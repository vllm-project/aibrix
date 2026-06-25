/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapi "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	defaultAIBrixNamespace   = "aibrix-system"
	metricHeaderErr          = "metric-header-err"
	gatewayRespBody          = "gateway_rsp_body"
	gatewayRespHeaders       = "gateway_rsp_headers"
	gatewayReqBody           = "gateway_req_body"
	defaultHTTPRouteCacheTTL = 30 * time.Second
	envHTTPRouteCacheTTL     = "AIBRIX_HTTPROUTE_CACHE_TTL"
)

type httpRouteCacheEntry struct {
	err       error
	expiresAt time.Time
}

type Server struct {
	redisClient         *redis.Client
	ratelimiter         ratelimiter.RateLimiter
	modelRateLimiter    ratelimiter.RateLimiter
	client              kubernetes.Interface
	gatewayClient       gatewayapi.Interface
	requestCountTracker map[string]int
	cache               cache.Cache
	httpServer          *http.Server
	httprouteCache      sync.Map
	httprouteCacheTTL   time.Duration
	httprouteSFGroup    singleflight.Group
	// Broadcast channel for server-initiated shutdown
	shutdownCh   <-chan struct{}
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

type processState struct {
	ctx              context.Context
	requestID        string
	user             utils.User
	rpm              int64
	traceTerm        int64
	respErrorCode    int
	model            string
	metricLabel      string
	routerCtx        *types.RoutingContext
	lastRespHeaders  []*configPb.HeaderValueOption
	stream           bool
	isRespError      bool
	isGatewayRspDone bool
	completed        bool
	trackedModel     string
	span             trace.Span
	ttftSpan         trace.Span
}

var podName = os.Getenv("POD_NAME")
var tracer = otel.Tracer("envoy-ext-proc-server")
var gatewayInFlightCount int64
var gatewayModelInFlight sync.Map // model -> *int64

func (st *processState) trackModelInFlight() {
	if st.model == "" || st.trackedModel == st.model {
		return
	}
	if st.trackedModel != "" {
		st.releaseModelInFlight()
	}
	st.trackedModel = st.model
	val, _ := gatewayModelInFlight.LoadOrStore(st.model, new(int64))
	n := atomic.AddInt64(val.(*int64), 1)
	metrics.SetGaugeMetric(
		metrics.GatewayModelInFlight,
		metrics.GetMetricHelp(metrics.GatewayModelInFlight),
		float64(n),
		[]string{"gateway_pod", "model"},
		podName,
		st.model,
	)
}

func (st *processState) releaseModelInFlight() {
	if st.trackedModel == "" {
		return
	}
	modelToRelease := st.trackedModel
	st.trackedModel = ""
	val, ok := gatewayModelInFlight.Load(modelToRelease)
	if !ok {
		return
	}
	n := atomic.AddInt64(val.(*int64), -1)
	metrics.SetGaugeMetric(
		metrics.GatewayModelInFlight,
		metrics.GetMetricHelp(metrics.GatewayModelInFlight),
		float64(n),
		[]string{"gateway_pod", "model"},
		podName,
		modelToRelease,
	)
}

func httpRouteCacheTTL() time.Duration {
	if v := os.Getenv(envHTTPRouteCacheTTL); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHTTPRouteCacheTTL
}

func NewServer(redisClient *redis.Client, client kubernetes.Interface, gatewayClient gatewayapi.Interface) *Server {
	c, err := cache.Get()
	if err != nil {
		panic(err)
	}
	var r ratelimiter.RateLimiter
	var mr ratelimiter.RateLimiter
	if redisClient != nil {
		r = ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)
		mr = ratelimiter.NewRedisAccountRateLimiter("aibrix_model", redisClient, 1*time.Second)
	} else {
		r = ratelimiter.NewNoopRateLimiter()
		mr = ratelimiter.NewNoopRateLimiter()
	}

	// Initialize the routers
	routing.Init()

	shutdown := make(chan struct{})
	return &Server{
		redisClient:         redisClient,
		ratelimiter:         r,
		modelRateLimiter:    mr,
		client:              client,
		gatewayClient:       gatewayClient,
		requestCountTracker: map[string]int{},
		cache:               c,
		httprouteCacheTTL:   httpRouteCacheTTL(),
		shutdownCh:          shutdown,
		shutdown:            shutdown,
	}
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	st := &processState{
		ctx:       srv.Context(),
		requestID: uuid.New().String(),
	}

	n := atomic.AddInt64(&gatewayInFlightCount, 1)
	metrics.SetGaugeMetric(metrics.GatewayInFlight, metrics.GetMetricHelp(metrics.GatewayInFlight), float64(n), []string{"gateway_pod"}, podName)
	defer func() {
		st.releaseModelInFlight()
		if st.span != nil {
			st.span.End()
		}
		if st.ttftSpan != nil {
			st.ttftSpan.End()
		}
		remaining := atomic.AddInt64(&gatewayInFlightCount, -1)
		metrics.SetGaugeMetric(metrics.GatewayInFlight, metrics.GetMetricHelp(metrics.GatewayInFlight), float64(remaining), []string{"gateway_pod"}, podName)
	}()

	klog.InfoS("processing request", "requestID", st.requestID)
	labels := map[string]string{"pod_name": podName}
	metrics.EmitMetricToPrometheus(&types.RoutingContext{}, nil, metrics.GatewayRequestTotal, &metrics.SimpleMetricValue{Value: 1.0}, labels)

	for {
		if err := s.processOnce(srv, st); err != nil {
			return err
		}
		// Proactively break the loop if the response is fully processed.
		// This allows Envoy to gracefully close the stream and send 0\r\n\r\n.
		if st.completed {
			klog.V(4).InfoS("request actively finished, breaking ext_proc stream", "requestID", st.requestID)
			if st.model != "" {
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200")
			}
			s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
			return nil
		}
	}
}

func (s *Server) processOnce(srv extProcPb.ExternalProcessor_ProcessServer, st *processState) error {
	if err := s.preRecvCheck(st); err != nil {
		return err
	}

	// Run Recv in a goroutine so we can interrupt it if shutdown or context
	// cancellation arrives while the stream is idle. Envoy keeps ext_proc
	// streams open indefinitely between requests, so a bare srv.Recv() would
	// block GracefulStop() forever on rollout.
	type recvResult struct {
		req *extProcPb.ProcessingRequest
		err error
	}
	ch := make(chan recvResult, 1)
	go func() {
		req, err := srv.Recv()
		ch <- recvResult{req, err}
	}()

	// ctx.Done() is intentionally omitted here: gRPC unblocks Recv when the
	// stream context is cancelled, so handleRecvError handles that path.
	// preRecvCheck covers the case where ctx is already done before we spawn.
	var req *extProcPb.ProcessingRequest
	select {
	case r := <-ch:
		if r.err != nil {
			return s.handleRecvError(st, r.err)
		}
		req = r.req
	case <-s.shutdownCh:
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503")
			s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		}
		klog.ErrorS(nil, "server shutdown requested; aborting blocked Recv", "request_id", st.requestID, "model", st.model)
		return status.Error(codes.Unavailable, "server shutdown in progress")
	}

	resp, err := s.handleProcessingRequest(st, req)
	if err != nil {
		return err
	}
	return s.sendProcessingResponse(srv, st, resp)
}

func (s *Server) preRecvCheck(st *processState) error {
	select {
	case <-s.shutdownCh:
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503")
			s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		}
		klog.ErrorS(nil, "server shutdown requested; draining request", "request_id", st.requestID, "model", st.model)
		return status.Error(codes.Unavailable, "server shutdown in progress")

	// Client cancelled or deadline exceeded
	case <-st.ctx.Done():
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "context_cancelled", "499")
			s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		}
		klog.ErrorS(st.ctx.Err(), "context cancelled", "request_id", st.requestID, "model", st.model)
		return st.ctx.Err()

	default:
		return nil
	}
}

func (s *Server) handleRecvError(st *processState, err error) error {
	if err == io.EOF {
		select {
		// check for shutdown
		case <-s.shutdownCh:
			if st.model != "" {
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503")
				s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
			}
			klog.ErrorS(nil, "server shutdown requested; stream closed (EOF) during shutdown drain", "requestID", st.requestID, "model", st.model)
			return status.Error(codes.Unavailable, "server shutdown in progress")

		default:
		}

		// Fallback: if proactive exit in Process was skipped (should not happen normally)
		if st.completed {
			if st.model != "" {
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200")
			}
			klog.V(2).InfoS("stream closed (EOF): completed", "requestID", st.requestID, "model", st.model)
			s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
			return nil
		}

		// client closed stream (EOF)
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "client_cancelled_eof", "499")
		}
		klog.ErrorS(nil, "client closed stream (EOF) before completion", "requestID", st.requestID, "model", st.model)
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return io.EOF
	}

	// Normal stream closure by envoy proxy
	stErr, ok := status.FromError(err)
	if ok && stErr.Code() == codes.Canceled {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200")
		}
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return status.Error(codes.Canceled, "request canceled")
	}

	// Record failed request metric for other gRPC errors
	if ok {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "gateway_request_fail", strconv.FormatUint(uint64(stErr.Code()), 10))
		}
		klog.ErrorS(err, "error receiving stream from Envoy extproc", "requestID", st.requestID, "model", st.model, "grpc_code", stErr.Code(), "grpc_message", stErr.Message())
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return stErr.Err()
	}

	klog.ErrorS(err, "error receiving stream from Envoy extproc (non-gRPC)", "requestID", st.requestID)
	return status.Errorf(codes.Unknown, "recv stream error: %v", err)
}

func (s *Server) handleProcessingRequest(st *processState, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, error) {
	var resp *extProcPb.ProcessingResponse

	switch req.Request.(type) {
	case *extProcPb.ProcessingRequest_RequestHeaders:
		st.ctx, st.span = tracer.Start(st.ctx, "ExtProc_HTTP_Request")

		resp, st.user, st.rpm, st.routerCtx = s.HandleRequestHeaders(st.ctx, st.requestID, req)
		if st.routerCtx != nil {
			st.model = st.routerCtx.Model
			st.requestID = st.routerCtx.RequestID
		}
		st.metricLabel = "gateway_req_headers"

	case *extProcPb.ProcessingRequest_RequestBody:
		resp, st.model, st.stream, st.traceTerm = s.HandleRequestBody(st.ctx, st.routerCtx, st.requestID, req, st.user)
		st.metricLabel = gatewayReqBody
		// create a ttftSpan to collect time from reqBody to first respBody
		_, st.ttftSpan = tracer.Start(st.ctx, "Wait_For_LLM_First_Token")

	case *extProcPb.ProcessingRequest_ResponseHeaders:
		resp, st.isRespError, st.respErrorCode = s.HandleResponseHeaders(st.ctx, st.routerCtx, st.requestID, st.model, req)
		st.lastRespHeaders = resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
		if st.isRespError {
			resp = s.responseForResponseHeaderError(st, resp)
		}
		st.metricLabel = gatewayRespHeaders

	case *extProcPb.ProcessingRequest_ResponseBody:
		// stop collecting on first resp only
		if st.ttftSpan != nil {
			st.ttftSpan.End()
			st.ttftSpan = nil
		}
		if st.isRespError {
			body := string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody())
			resp = s.responseErrorProcessingWithHeaders(st.ctx, st.routerCtx, st.lastRespHeaders, st.respErrorCode, st.model, st.requestID, body)
		} else {
			resp, st.completed = s.HandleResponseBody(st.ctx, st.routerCtx, st.requestID, req, st.user, st.rpm, st.model, st.stream, st.traceTerm, st.completed)
		}
		st.metricLabel = gatewayRespBody

	default:
		klog.InfoS("unknown request type", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
	}

	st.trackModelInFlight()

	if resp == nil {
		klog.ErrorS(nil, "no ProcessingResponse generated for message", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "no_response_err", "500")
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return nil, status.Errorf(codes.Internal, "no response generated for %T", req.Request)
	}

	if st.model == "" {
		return resp, nil
	}

	if resp.GetImmediateResponse() == nil {
		if st.metricLabel != gatewayRespBody {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, st.metricLabel+"_success", "200")
			return resp, nil
		}
		if st.completed && !st.isGatewayRspDone {
			st.isGatewayRspDone = true
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, st.metricLabel+"_success", "200")
		}
		return resp, nil
	}

	statusCode := strconv.Itoa(int(resp.GetImmediateResponse().GetStatus().GetCode()))
	metricFail := getMetricErr(resp.GetImmediateResponse(), st.metricLabel)
	s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, metricFail+"_fail", statusCode)

	return resp, nil
}

func (s *Server) responseForResponseHeaderError(st *processState, resp *extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	switch st.respErrorCode {
	case 500:
		return s.responseErrorProcessing(st.ctx, st.routerCtx, resp, st.respErrorCode, st.model, st.requestID, "Internal server error")
	case 401:
		return s.responseErrorProcessing(st.ctx, st.routerCtx, resp, st.respErrorCode, st.model, st.requestID, "Incorrect API key provided")
	default:
		return resp
	}
}

func (s *Server) sendProcessingResponse(srv extProcPb.ExternalProcessor_ProcessServer, st *processState, resp *extProcPb.ProcessingResponse) error {
	if err := srv.Send(resp); err != nil && len(st.model) > 0 {
		klog.ErrorS(err, "gateway fail to send response to envoy-proxy", "requestID", st.requestID)
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "send_envoy_proxy", "499")
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		// Manually delete routerCtx on error paths;
		// HandleResponseBody() and HandleResponseHeaders() will handle it on the happy path.
		if st.routerCtx != nil {
			st.routerCtx.Delete()
		}

		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
			klog.Warning("Stream already closed by client", "requestID", st.requestID)
		}
		return err
	}
	return nil
}

func (s *Server) selectTargetPod(ctx context.Context, routeCtx *types.RoutingContext, pods types.PodList, externalFilterExpr string) (string, error) {
	var span trace.Span
	_, span = tracer.Start(ctx, "selectTargetPod")
	defer span.End()

	router, err := routing.Select(routeCtx)
	if err != nil {
		return "", err
	}

	if pods.Len() == 0 {
		return "", fmt.Errorf("no pods for routing")
	}
	readyPods := utils.FilterRoutablePods(pods.All())

	// filter pod by header 'external-filter'
	readyPods, err = utils.FilterPodsByLabelSelector(readyPods, externalFilterExpr)
	if err != nil {
		return "", fmt.Errorf("filter pods by label selector failed: %v", err)
	}

	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods for routing")
	}
	if len(readyPods) == 1 && len(utils.GetPortsForPod(readyPods[0])) <= 1 {
		routeCtx.SetTargetPod(readyPods[0])
		return routeCtx.TargetAddress(), nil
	}
	utils.CryptoShuffle(readyPods)

	return router.Route(routeCtx, &utils.PodArray{Pods: readyPods})
}

// validateHTTPRouteStatus checks if httproute object exists and validates its conditions are true.
// Results are cached with a TTL (default 30s, configurable via AIBRIX_HTTPROUTE_CACHE_TTL) to
// avoid hammering the Kubernetes API on every request.
func (s *Server) validateHTTPRouteStatus(ctx context.Context, model string) error {
	// Skip validation in standalone mode (no gateway client)
	if s.gatewayClient == nil {
		return nil
	}

	if cached, ok := s.httprouteCache.Load(model); ok {
		entry := cached.(httpRouteCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.err
		}
	}

	// Use singleflight to collapse concurrent cache-miss requests for the same
	// model into a single Kubernetes API call, preventing thundering herd on
	// cache expiry under high load.
	v, err, _ := s.httprouteSFGroup.Do(model, func() (interface{}, error) {
		// Re-check cache inside the group: a previous waiter may have already
		// populated it while we were queued.
		if cached, ok := s.httprouteCache.Load(model); ok {
			entry := cached.(httpRouteCacheEntry)
			if time.Now().Before(entry.expiresAt) {
				return entry.err, nil
			}
		}

		name := fmt.Sprintf("%s-router", model)
		httproute, err := s.gatewayClient.GatewayV1().HTTPRoutes(defaultAIBrixNamespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.httprouteCache.Store(model, httpRouteCacheEntry{err: err, expiresAt: time.Now().Add(s.httprouteCacheTTL)})
			}
			return nil, err
		}

		errMsg := []string{}
		for _, status := range httproute.Status.Parents {
			if len(status.Conditions) == 0 {
				errMsg = append(errMsg, fmt.Sprintf("httproute: %s/%s, does not have valid status", defaultAIBrixNamespace, name))
				break
			}
			for _, condition := range status.Conditions {
				if condition.Type == string(gatewayv1.RouteConditionAccepted) &&
					condition.Reason != string(gatewayv1.RouteReasonAccepted) {
					errMsg = append(errMsg, fmt.Sprintf("httproute: %s/%s, route is not accepted: %s.", defaultAIBrixNamespace, name, condition.Reason))
				} else if condition.Type == string(gatewayv1.RouteConditionResolvedRefs) &&
					condition.Reason != string(gatewayv1.RouteReasonResolvedRefs) {
					errMsg = append(errMsg, fmt.Sprintf("httproute: %s/%s, route's object references are not resolved: %s.", defaultAIBrixNamespace, name, condition.Reason))
				}
			}
		}

		var result error
		if len(errMsg) > 0 {
			result = errors.New(strings.Join(errMsg, ", "))
		}
		s.httprouteCache.Store(model, httpRouteCacheEntry{err: result, expiresAt: time.Now().Add(s.httprouteCacheTTL)})
		return result, nil
	})

	if err != nil {
		return err
	}
	if v != nil {
		return v.(error)
	}
	return nil
}

// StartHTTPServer starts the gateway's HTTP server with metrics and API handlers.
// In local/standalone mode, Envoy routes /v1/models here since there is no metadata service.
// In standard K8s deployment, Envoy routes /v1/models to the metadata service instead,
// so the /v1/models handler here is never reached — no conflict.
func (s *Server) StartHTTPServer(addr string) error {
	if s.httpServer != nil {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/v1/models", s.handleListModels)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %v", addr, err)
	}

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	klog.InfoS("Starting HTTP server", "address", addr)
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "Failed to start HTTP server")
		}
	}()

	return nil
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintf(w, `{"error":"method not allowed"}`)
		return
	}

	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	type modelListResponse struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}

	models := s.cache.ListModels()
	data := make([]modelObject, len(models))
	for i, m := range models {
		data[i] = modelObject{ID: m, Object: "model", Created: 0, OwnedBy: "aibrix"}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(modelListResponse{Object: "list", Data: data}); err != nil {
		klog.ErrorS(err, "failed to encode model list response")
	}
}

func (s *Server) Shutdown() {
	if s.shutdown != nil {
		s.shutdownOnce.Do(func() { close(s.shutdown) })
	}
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			klog.ErrorS(err, "Error stopping HTTP server")
		}
	}
}

func (s *Server) responseErrorProcessing(ctx context.Context, routingCtx *types.RoutingContext, resp *extProcPb.ProcessingResponse, respErrorCode int,
	model, requestID, errMsg string) *extProcPb.ProcessingResponse {
	headers := resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	return s.responseErrorProcessingWithHeaders(ctx, routingCtx, headers, respErrorCode, model, requestID, errMsg)
}

func (s *Server) responseErrorProcessingWithHeaders(ctx context.Context, routingCtx *types.RoutingContext, headers []*configPb.HeaderValueOption, respErrorCode int,
	model, requestID, errMsg string) *extProcPb.ProcessingResponse {
	var httprouteErr error
	// if use pd route Algorithm, we don't check httproute status
	if routingCtx == nil || routingCtx.Algorithm != routing.RouterPD {
		httprouteErr = s.validateHTTPRouteStatus(ctx, model)
	}

	_, span := tracer.Start(ctx, "responseErrorProcessingWithHeaders")
	defer span.End()

	if errMsg != "" && httprouteErr != nil {
		errMsg = fmt.Sprintf("%s. %s", errMsg, httprouteErr.Error())
	} else if errMsg == "" && httprouteErr != nil {
		errMsg = httprouteErr.Error()
	}
	klog.ErrorS(nil, "request end", "requestID", requestID, "errorCode", respErrorCode, "errorMessage", errMsg)

	// Determine appropriate error code based on HTTP status
	errorCode := ""
	if respErrorCode == 401 {
		errorCode = ErrorCodeInvalidAPIKey
	} else if respErrorCode == 503 {
		errorCode = ErrorCodeServiceUnavailable
	}

	return generateErrorResponse(
		envoyTypePb.StatusCode(respErrorCode),
		headers,
		errMsg, errorCode, "")
}

func (s *Server) emitMetricsCounterHelper(metricName, model, status, statusCode string) {
	labels := buildGatewayPodMetricLabels(model, status, statusCode)
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, nil, metricName, &metrics.SimpleMetricValue{Value: 1.0}, labels)
}

func getMetricErr(resp *extProcPb.ImmediateResponse, metricLabel string) string {
	if resp == nil {
		return metricLabel
	}
	var headerValue string
	for _, opt := range resp.GetHeaders().GetSetHeaders() {
		if opt.Header != nil && opt.Header.Key == metricHeaderErr {
			if opt.Header.Value != "" {
				headerValue = opt.Header.Value
			} else {
				headerValue = string(opt.Header.RawValue)
			}
			break
		}
	}

	if headerValue != "" {
		return metricLabel + "_" + headerValue
	}
	return metricLabel
}
