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
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	defaultHTTPRouteErrorTTL = 2 * time.Second
	httpRouteLookupTimeout   = 5 * time.Second
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
	disableRateLimiting bool
	priorityTier        bool
	apiKeyAuth          *apiKeyAuthConfig
	client              kubernetes.Interface
	gatewayClient       gatewayapi.Interface
	requestCountTracker map[string]int
	cache               cache.Cache
	routerManager       *routing.RouterManager
	inFlightObserver    func(int)
	wakeRequester       modelWakeRequester
	httpServer          *http.Server
	httprouteCache      sync.Map
	httprouteCacheTTL   time.Duration
	httprouteErrorTTL   time.Duration
	httprouteSFGroup    singleflight.Group
	// asyncJobs resolves the public job ids handed out for asynchronous APIs
	// (currently vLLM-Omni's Videos API) back to the pod that owns the job. It
	// reads and writes its store directly on every operation: there is no
	// per-replica job cache, so any replica can answer a follow-up for a job
	// another replica created, with no reconciliation window. Built lazily by
	// asyncJobRegistry() when not injected.
	asyncJobs     AsyncJobRegistry
	asyncJobsOnce sync.Once
	// Broadcast channel for server-initiated shutdown
	shutdownCh   <-chan struct{}
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// HasRequestBuffers is a test-observation API: it reports whether an in-flight
// request retains body state in either production buffer. sync.Map makes the
// point-in-time lookup safe concurrently with Process cleanup; callers should
// invoke it after Process returns when asserting terminal cleanup.
func HasRequestBuffers(requestID string) bool {
	_, requestPresent := requestBuffers.Load(requestID)
	_, streamPresent := streamBuffers.Load(requestID)
	return requestPresent || streamPresent
}

// recvResult is one (message, error) pair produced by a stream's srv.Recv().
type recvResult struct {
	req *extProcPb.ProcessingRequest
	err error
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
	// recvCh carries the result of the srv.Recv() this stream already has in
	// flight. It is kept across processOnce calls because a call can return
	// without consuming its message (PD fail-fast), and ext_proc requires that
	// exactly one Recv be outstanding on a stream at a time.
	recvCh chan recvResult
	// prefillFailFastDone records that the PD prefill failure has been handled
	// - not merely that the wakeup fired. The wakeup is a closed channel,
	// which stays ready forever, so the loop must stop selecting on it once it
	// has decided what to do, or a failure it deliberately ignored
	// (bad_response) would spin. Until then it stays armed, so a wakeup that
	// loses the tie to a message Envoy is waiting on fires again next time
	// round.
	prefillFailFastDone bool
	requestDone         sync.Once
	rootSpan            trace.Span // main span
	inferenceSpan       trace.Span // routing completion to final response body
	firstRespSpan       trace.Span // routing completion to first response body chunk
	toLastRespSpan      trace.Span // first response body chunk to stream completion
}

var podName = os.Getenv("POD_NAME")
var tracer = otel.Tracer("envoy-ext-proc-server")

func (st *processState) trackModelInFlight() {
	if st.model == "" || st.trackedModel == st.model {
		return
	}
	if st.trackedModel != "" {
		st.releaseModelInFlight()
	}
	st.trackedModel = st.model
	labelNames, labelValues := st.modelInFlightLabels()
	metrics.IncGaugeMetric(
		metrics.GatewayModelInFlight,
		metrics.GetMetricHelp(metrics.GatewayModelInFlight),
		labelNames,
		labelValues...,
	)
}

func (st *processState) releaseModelInFlight() {
	if st.trackedModel == "" {
		return
	}
	modelToRelease := st.trackedModel
	st.trackedModel = ""
	labelNames, labelValues := st.modelInFlightLabelsFor(modelToRelease)
	metrics.DecGaugeMetric(
		metrics.GatewayModelInFlight,
		metrics.GetMetricHelp(metrics.GatewayModelInFlight),
		labelNames,
		labelValues...,
	)
}

func (st *processState) modelInFlightLabels() ([]string, []string) {
	return st.modelInFlightLabelsFor(st.model)
}

func (st *processState) modelInFlightLabelsFor(model string) ([]string, []string) {
	loraAdapter := ""
	if st.routerCtx != nil {
		if m := st.routerCtx.MetricModel(); m != "" {
			model = m
		}
		loraAdapter = st.routerCtx.MetricLoraAdapter()
	}
	return []string{"gateway_pod", "model", "lora_adapter"},
		[]string{podName, model, loraAdapter}
}

// finishRequestCount and finishRequestTrace are the only production lifecycle
// completion points. A request can reach several terminal paths in quick
// succession (for example, response completion followed by EOF), so cache
// bookkeeping must be finalized exactly once while routerCtx is still owned by
// this processState.
func (s *Server) finishRequestCount(st *processState) {
	st.requestDone.Do(func() {
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
	})
}

func (s *Server) finishRequestTrace(st *processState, usage TokenUsage) {
	st.requestDone.Do(func() {
		s.cache.DoneRequestTrace(
			st.routerCtx,
			st.requestID,
			st.model,
			usage.PromptTokens,
			usage.CompletionTokens,
			st.traceTerm,
		)
	})
}

func httpRouteCacheTTL() time.Duration {
	if v := os.Getenv(envHTTPRouteCacheTTL); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHTTPRouteCacheTTL
}

// ServerOptions configures optional dependencies for a Server.
type ServerOptions struct {
	Cache         cache.Cache
	RouterManager *routing.RouterManager
	// DisableRateLimiting disables AIBrix user and model quota enforcement while
	// leaving Redis available to other gateway features.
	DisableRateLimiting bool
	// PriorityTier forwards the tier a request declares through the
	// x-aibrix-priority-tier header as the priority on the upstream vLLM
	// request. Off by default: requests are forwarded unchanged unless a
	// deployment opts in. See gateway_req_priority.go.
	PriorityTier bool
	// InFlightObserver receives test/diagnostic lifecycle deltas (+1/-1). The
	// callback must be non-blocking and non-panicking because it runs on the
	// request processing path and is not recovered by Gateway.
	InFlightObserver func(int)
}

func NewServer(redisClient *redis.Client, client kubernetes.Interface, gatewayClient gatewayapi.Interface) *Server {
	return NewServerWithOptions(redisClient, client, gatewayClient, ServerOptions{})
}

// NewServerWithOptions constructs a Gateway server with optional dependencies.
// A supplied Cache without a RouterManager creates an isolated cache-aware
// manager; with no options, production global cache and routing behavior apply.
func NewServerWithOptions(redisClient *redis.Client, client kubernetes.Interface, gatewayClient gatewayapi.Interface, options ServerOptions) *Server {
	c := options.Cache
	if c == nil {
		var err error
		c, err = cache.Get()
		if err != nil {
			panic(err)
		}
	}
	var r ratelimiter.RateLimiter
	var mr ratelimiter.RateLimiter
	if redisClient != nil && !options.DisableRateLimiting {
		r = ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)
		mr = ratelimiter.NewRedisAccountRateLimiter("aibrix_model", redisClient, 1*time.Second)
	} else {
		r = ratelimiter.NewNoopRateLimiter()
		mr = ratelimiter.NewNoopRateLimiter()
	}

	routerManager := options.RouterManager
	if routerManager == nil {
		if options.Cache != nil {
			routerManager = routing.NewRouterManagerWithCache(c)
		} else {
			routing.Init()
			routerManager = routing.DefaultRouterManager()
		}
	}
	routerManager.Init()

	shutdown := make(chan struct{})
	s := &Server{
		redisClient:         redisClient,
		ratelimiter:         r,
		modelRateLimiter:    mr,
		disableRateLimiting: options.DisableRateLimiting,
		priorityTier:        options.PriorityTier,
		apiKeyAuth:          loadAPIKeyAuthConfig(),
		client:              client,
		gatewayClient:       gatewayClient,
		requestCountTracker: map[string]int{},
		cache:               c,
		routerManager:       routerManager,
		inFlightObserver:    options.InFlightObserver,
		wakeRequester:       newRuntimeModelWakeRequester(nil, defaultModelClaimRuntimePort),
		httprouteCacheTTL:   httpRouteCacheTTL(),
		httprouteErrorTTL:   defaultHTTPRouteErrorTTL,
		shutdownCh:          shutdown,
		shutdown:            shutdown,
	}
	s.asyncJobs = newAsyncJobRegistryForClient(redisClient, c)
	if sar, err := routerManager.Lookup(routing.RouterSessionAffinity); err == nil {
		if rb, ok := sar.(routing.RedisBackedRouter); ok {
			rb.Start(shutdown, redisClient)
		}
	}
	return s
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) (err error) {
	// Process is also reachable without the server's stream interceptor, so it
	// recovers panics on its own as well.
	defer func() {
		if r := recover(); r != nil {
			err = recoverStreamPanic(r, ProcessFullMethod)
		}
	}()

	rootSpan := trace.SpanFromContext(srv.Context())
	requestID := uuid.New().String()
	if rootSpan.SpanContext().HasTraceID() {
		requestID = rootSpan.SpanContext().TraceID().String()
	}

	st := &processState{
		ctx:       srv.Context(),
		requestID: requestID,
		rootSpan:  rootSpan,
	}

	metrics.IncGaugeMetric(metrics.GatewayInFlight, metrics.GetMetricHelp(metrics.GatewayInFlight), []string{"gateway_pod"}, podName)
	if s.inFlightObserver != nil {
		s.inFlightObserver(1)
	}
	defer func() {
		// This is a fallback for any terminal path that did not explicitly finish
		// bookkeeping. A non-empty model and routing context mean request-body
		// processing progressed far enough that AddRequestCount may have run.
		if st.model != "" && st.routerCtx != nil {
			s.finishRequestCount(st)
		}
		requestBuffers.Delete(st.requestID)
		streamBuffers.Delete(st.requestID)
		// end spans created by this server
		for _, span := range []trace.Span{st.toLastRespSpan, st.firstRespSpan, st.inferenceSpan} {
			if span != nil {
				span.End()
			}
		}
		st.releaseModelInFlight()
		metrics.DecGaugeMetric(metrics.GatewayInFlight, metrics.GetMetricHelp(metrics.GatewayInFlight), []string{"gateway_pod"}, podName)
		if s.inFlightObserver != nil {
			s.inFlightObserver(-1)
		}
		// routerCtx must remain valid until every completion path above has
		// returned. Returning it to the pool earlier lets another request reset
		// the same object while this Process still holds the pointer.
		if st.routerCtx != nil {
			st.routerCtx.Delete()
			st.routerCtx = nil
		}
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
			if st.toLastRespSpan != nil {
				st.toLastRespSpan.End()
				st.toLastRespSpan = nil
			}
			klog.V(4).InfoS("request actively finished, breaking ext_proc stream", "requestID", st.requestID)
			if st.model != "" && !st.isGatewayRspDone {
				st.isGatewayRspDone = true
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200", st.routerCtx)
			}
			s.finishRequestCount(st)
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
	//
	// The channel outlives the call: a PD fail-fast that decides not to end the
	// stream returns without consuming the message, and the next iteration must
	// wait on the same in-flight Recv rather than start a second one.
	if st.recvCh == nil {
		ch := make(chan recvResult, 1)
		st.recvCh = ch
		go func() {
			req, err := srv.Recv()
			ch <- recvResult{req, err}
		}()
	}

	// Arm the PD prefill-failure wakeup only once the routing context exists -
	// it is assigned while handling RequestHeaders, and the prefill goroutine
	// that records the failure is only started later, from RequestBody - and
	// only until the failure has been acted on. PrefillFailed() is nil-safe and
	// a nil channel blocks forever, so a non-PD stream selects on exactly the
	// two cases it had before. A failure recorded before this point is not
	// missed either: it closes the channel, and a receive on an already-closed
	// channel is ready immediately.
	var prefillFailed <-chan struct{}
	if !st.prefillFailFastDone {
		prefillFailed = st.routerCtx.PrefillFailed()
	}

	// ctx.Done() is intentionally omitted here: gRPC unblocks Recv when the
	// stream context is cancelled, so handleRecvError handles that path.
	// preRecvCheck covers the case where ctx is already done before we spawn.
	var req *extProcPb.ProcessingRequest
	select {
	case r := <-st.recvCh:
		st.recvCh = nil
		if r.err != nil {
			return s.handleRecvError(st, r.err)
		}
		req = r.req
	case <-prefillFailed:
		// A message that has already arrived wins the tie: Envoy is waiting on
		// a reply for it, and it may be the decode leg's response headers,
		// which change which half of the fail-fast handling applies.
		select {
		case r := <-st.recvCh:
			st.recvCh = nil
			if r.err != nil {
				return s.handleRecvError(st, r.err)
			}
			req = r.req
		default:
			// Disarmed only here, where the failure is actually handled.
			// Setting it for the branch above would drop the wakeup on the
			// floor: the failure would never be acted on, and the stream
			// would go back to waiting out the decode pod's bootstrap
			// timeout - the hang this whole path exists to prevent. Losing
			// the tie costs nothing instead: the wakeup channel is closed,
			// so the next iteration sees it ready again and fires fail-fast
			// then, by which time a decode response among those buffered
			// messages has been recorded and the after_response half applies.
			st.prefillFailFastDone = true
			return s.handlePrefillFailFast(srv, st)
		}
	case <-s.shutdownCh:
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503", st.routerCtx)
			s.finishRequestCount(st)
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
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503", st.routerCtx)
			s.finishRequestCount(st)
		}
		klog.ErrorS(nil, "server shutdown requested; draining request", "request_id", st.requestID, "model", st.model)
		return status.Error(codes.Unavailable, "server shutdown in progress")

	// Client cancelled or deadline exceeded
	case <-st.ctx.Done():
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "context_cancelled", "499", st.routerCtx)
			s.finishRequestCount(st)
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
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "aibrix_gateway_server_shutdown", "503", st.routerCtx)
				s.finishRequestCount(st)
			}
			klog.ErrorS(nil, "server shutdown requested; stream closed (EOF) during shutdown drain", "requestID", st.requestID, "model", st.model)
			return status.Error(codes.Unavailable, "server shutdown in progress")

		default:
		}

		// Fallback: if proactive exit in Process was skipped (should not happen normally)
		if st.completed {
			if st.model != "" && !st.isGatewayRspDone {
				st.isGatewayRspDone = true
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200", st.routerCtx)
			}
			klog.V(2).InfoS("stream closed (EOF): completed", "requestID", st.requestID, "model", st.model)
			s.finishRequestCount(st)
			return nil
		}

		// client closed stream (EOF)
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "client_cancelled_eof", "499", st.routerCtx)
		}
		klog.ErrorS(nil, "client closed stream (EOF) before completion", "requestID", st.requestID, "model", st.model)
		s.finishRequestCount(st)
		return io.EOF
	}

	// Normal stream closure by envoy proxy
	stErr, ok := status.FromError(err)
	if ok && stErr.Code() == codes.Canceled {
		if st.model != "" && !st.isGatewayRspDone {
			st.isGatewayRspDone = true
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200", st.routerCtx)
		}
		s.finishRequestCount(st)
		return status.Error(codes.Canceled, "request canceled")
	}

	// Record failed request metric for other gRPC errors
	if ok {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "gateway_request_fail", strconv.FormatUint(uint64(stErr.Code()), 10), st.routerCtx)
		}
		klog.ErrorS(err, "error receiving stream from Envoy extproc", "requestID", st.requestID, "model", st.model, "grpc_code", stErr.Code(), "grpc_message", stErr.Message())
		s.finishRequestCount(st)
		return stErr.Err()
	}

	if st.model != "" && st.routerCtx != nil {
		s.finishRequestCount(st)
	}
	klog.ErrorS(err, "error receiving stream from Envoy extproc (non-gRPC)", "requestID", st.requestID)
	return status.Errorf(codes.Unknown, "recv stream error: %v", err)
}

func (s *Server) handleProcessingRequest(st *processState, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, error) {
	var resp *extProcPb.ProcessingResponse

	switch req.Request.(type) {
	case *extProcPb.ProcessingRequest_RequestHeaders:
		resp, st.user, st.rpm, st.routerCtx, st.traceTerm = s.HandleRequestHeaders(st.ctx, st.requestID, st.rootSpan, req)
		if st.routerCtx != nil {
			st.model = st.routerCtx.Model
			st.routerCtx.Span = st.rootSpan
			st.requestID = st.routerCtx.RequestID // sync requestID if it was overridden by traceparent header
		}
		st.metricLabel = "gateway_req_headers"

	case *extProcPb.ProcessingRequest_RequestBody:
		resp, st.model, st.stream, st.traceTerm = s.HandleRequestBody(st.ctx, st.routerCtx, st.requestID, req, st.user)
		st.metricLabel = gatewayReqBody
		// ImmediateResponse means the request was rejected locally and never
		// entered the inference stage.
		if resp != nil && resp.GetImmediateResponse() == nil {
			_, st.inferenceSpan = tracer.Start(st.ctx, "llm.inference")
			if st.stream {
				_, st.firstRespSpan = tracer.Start(st.ctx, "llm.time_to_first_response_chunk")
			}
		}

	case *extProcPb.ProcessingRequest_ResponseHeaders:
		// The decode pod has started answering: from here on a late PD prefill
		// failure must not abort it, and must not fail a stream the client is
		// already being served on.
		st.routerCtx.MarkDecodeResponded()
		resp, st.isRespError, st.respErrorCode = s.HandleResponseHeaders(st.ctx, st.routerCtx, st.requestID, st.model, req)
		st.lastRespHeaders = resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
		if st.isRespError {
			s.finishRequestCount(st)
			resp = s.responseForResponseHeaderError(st, resp)
		}
		st.metricLabel = gatewayRespHeaders

	case *extProcPb.ProcessingRequest_ResponseBody:
		// Also marked here, not only on response headers: a filter chain
		// configured without the response-header callback delivers body chunks
		// as the first sign of life from the decode pod.
		st.routerCtx.MarkDecodeResponded()
		// Stop collecting on the first response body chunk.
		if st.firstRespSpan != nil {
			st.firstRespSpan.End()
			st.firstRespSpan = nil
			// after the first response body chunk arrives
			if st.stream && st.toLastRespSpan == nil {
				_, st.toLastRespSpan = tracer.Start(st.ctx, "llm.time_from_first_to_last_response_chunk")
			}
		}
		if st.isRespError {
			body := string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody())
			body = string(rewriteVideoJobErrorBody(st.routerCtx, []byte(body)))
			resp = s.responseErrorProcessingWithHeaders(st.ctx, st.routerCtx, st.lastRespHeaders, st.respErrorCode, st.model, st.requestID, body)
		} else {
			var usage TokenUsage
			resp, st.completed, usage = s.HandleResponseBody(st.ctx, st.routerCtx, st.requestID, req, st.user, st.rpm, st.model, st.stream, st.completed)
			if st.completed {
				s.finishRequestTrace(st, usage)
			}
		}
		st.metricLabel = gatewayRespBody

	default:
		klog.InfoS("unknown request type", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
	}

	st.trackModelInFlight()

	if resp == nil {
		klog.ErrorS(nil, "no ProcessingResponse generated for message", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "no_response_err", "500", st.routerCtx)
		s.finishRequestCount(st)
		return nil, status.Errorf(codes.Internal, "no response generated for %T", req.Request)
	}

	if st.model == "" {
		return resp, nil
	}

	if resp.GetImmediateResponse() == nil {
		if st.metricLabel != gatewayRespBody {
			return resp, nil
		}
		if st.completed && !st.isGatewayRspDone {
			st.isGatewayRspDone = true
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, st.metricLabel+"_success", "200", st.routerCtx)
		}
		return resp, nil
	}

	statusCode := strconv.Itoa(int(resp.GetImmediateResponse().GetStatus().GetCode()))
	metricFail := getMetricErr(resp.GetImmediateResponse(), st.metricLabel)
	s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, metricFail+"_fail", statusCode, st.routerCtx)

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
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "send_envoy_proxy", "499", st.routerCtx)
		s.finishRequestCount(st)

		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
			klog.Warning("Stream already closed by client", "requestID", st.requestID)
		}
		return err
	}
	return nil
}

func (s *Server) selectTargetPod(ctx context.Context, routeCtx *types.RoutingContext, pods types.PodList, externalFilterExpr string) (string, error) {
	var span trace.Span
	ctx, span = tracer.Start(ctx, "process.select_target_pod")
	defer span.End()

	if pods.Len() == 0 {
		return "", fmt.Errorf("no pods for routing")
	}
	readyPods := utils.FilterRoutablePods(pods.All())

	// Resolve the model config profile's routing knobs once per request: the
	// load-imbalance gate below and every strategy on the routing path read the
	// same values from the routing context (see
	// routingalgorithms.ResolveRoutingOverrides).
	routing.ResolveRoutingOverrides(routeCtx)

	if routeCtx.Span != nil {
		routeCtx.Span.SetAttributes(
			attribute.Int("candidate_pods", pods.Len()),
			attribute.Int("ready_pods", len(readyPods)),
			attribute.String("routing_strategy", string(routeCtx.Algorithm)),
		)
	}

	// filter pod by header 'external-filter'
	var err error
	readyPods, err = utils.FilterPodsByLabelSelector(readyPods, externalFilterExpr)
	if err != nil {
		return "", fmt.Errorf("filter pods by label selector failed: %v", err)
	}

	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods for routing")
	}

	if limit := replicaInflightLimit(routeCtx); limit > 0 {
		readyPods = s.filterSaturatedReplicaInflight(readyPods, limit)
		if len(readyPods) == 0 {
			return "", errReplicaInflightExceeded
		}
	}

	// Resolve exclusivity from the caller's raw algorithm string rather than comparing it
	// directly against "pd": the string may itself be a multi-strategy config that collapses
	// to a single exclusive strategy (e.g. "pd:1,least-request:1" resolves to "pd", and bare
	// "slo-pack-load"/"slo-least-load" are exclusive too), and a plain equality check would
	// miss those and wrongly narrow readyPods ahead of them.
	resolvedExclusive, isExclusive := routing.ResolveExclusiveStrategy(string(routeCtx.Algorithm))
	if isExclusive {
		if resolvedExclusive == string(routing.RouterPD) {
			engine, err := routing.ValidateAndGetLLMEngine(readyPods)
			if err != nil {
				return "", fmt.Errorf("engine validation failed for request %s: %w", routeCtx.RequestID, err)
			}
			routeCtx.Engine = engine
		}
	} else {
		// Apply the load-imbalance hotspot gate ahead of whichever strategy actually routes
		// this request. Exclusive strategies (pd, slo*) are exempted above: they manage their
		// own pod subsets (prefill/decode, SLO tiers), which a blanket running-request-count
		// filter would distort.
		readyPods = routing.ApplyLoadImbalanceGate(routeCtx, s.cache, readyPods)
	}

	if s.routerManager == nil {
		// Preserve compatibility for legacy tests that build Server literals.
		s.routerManager = routing.DefaultRouterManager()
	}
	router, err := s.routerManager.Select(routeCtx)
	if err != nil {
		return "", err
	}

	if len(readyPods) == 1 && len(utils.GetPortsForPod(readyPods[0])) <= 1 && !isExclusive {
		routeCtx.SetTargetPod(readyPods[0])
		// This fast path skips router.Route() entirely, so a router with state to persist
		// once a target pod is picked (e.g. session-affinity's Redis pin, or a multi-strategy
		// blend wrapping it) never gets that chance unless we run its post-route hook here too.
		if updater, ok := router.(types.PostRouteUpdater); ok {
			podList := &utils.PodArray{Pods: readyPods}
			if err := updater.PostRouteUpdate(routeCtx, podList, readyPods[0]); err != nil {
				klog.Warningf("post-route update failed for request %s: %v", routeCtx.RequestID, err)
			}
		}
		return routeCtx.TargetAddress(), nil
	}
	utils.CryptoShuffle(readyPods)
	// PD issues an additional HTTP request while routing. Pass the current span
	// to that request without changing Context semantics for other routers.
	if isExclusive && resolvedExclusive == string(routing.RouterPD) {
		routeCtx.Context = ctx
	}
	return router.Route(routeCtx, &utils.PodArray{Pods: readyPods})
}

// validateHTTPRouteStatus checks if httproute object exists and validates its conditions are true.
// Successful results are cached with a TTL (default 30s, configurable via
// AIBRIX_HTTPROUTE_CACHE_TTL). Transient errors use a short TTL so route
// creation and recovery are observed quickly without hammering the API server.
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

		name := utils.ModelRouterName(model)
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpRouteLookupTimeout)
		defer cancel()
		httproute, err := s.gatewayClient.GatewayV1().HTTPRoutes(defaultAIBrixNamespace).Get(lookupCtx, name, metav1.GetOptions{})
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				ttl := s.httprouteErrorTTL
				if ttl <= 0 {
					ttl = defaultHTTPRouteErrorTTL
				}
				s.httprouteCache.Store(model, httpRouteCacheEntry{err: err, expiresAt: time.Now().Add(ttl)})
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
		ttl := s.httprouteCacheTTL
		if result != nil {
			ttl = s.httprouteErrorTTL
			if ttl <= 0 {
				ttl = defaultHTTPRouteErrorTTL
			}
		}
		s.httprouteCache.Store(model, httpRouteCacheEntry{err: result, expiresAt: time.Now().Add(ttl)})
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
		_, _ = fmt.Fprintf(w, `{"error":"method not allowed"}`)
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
	// Match HandleRequestBody: HTTPRoute is only used when no explicit routing algorithm is set.
	// Do not validate HTTPRoute on errors for least-request, pd, random, etc. (avoids misleading
	// "httproute not found" appended to real upstream errors like 404).
	if routingCtx == nil || routingCtx.Algorithm == routing.RouterNotSet {
		httprouteErr = s.validateHTTPRouteStatus(ctx, model)
	}

	_, span := tracer.Start(ctx, "process.response_error_processing_with_headers")
	defer span.End()

	if errMsg != "" && httprouteErr != nil {
		errMsg = fmt.Sprintf("%s. %s", errMsg, httprouteErr.Error())
	} else if errMsg == "" && httprouteErr != nil {
		errMsg = httprouteErr.Error()
	}
	klog.ErrorS(nil, "request end", "requestID", requestID, "errorCode", respErrorCode, "errorMessage", errMsg)

	// On a non-2xx upstream status the body often carries the engine's own OpenAI-style
	// error. Normalize it to a single envelope and pass it through verbatim instead of
	// re-wrapping the raw JSON into error.message. The HTTP status stays the
	// authoritative respErrorCode; the body's own code is preserved inside the body.
	if normalizedBody, code, ok := normalizeUpstreamErrorBody([]byte(errMsg), respErrorCode); ok {
		errHeaders := append(append([]*configPb.HeaderValueOption{}, headers...),
			buildEnvoyProxyHeaders(nil, HeaderErrorResponseUnknown, "true")...)
		return buildErrorResponseWithBody(code, normalizedBody, errHeaders)
	}

	// Determine appropriate error code based on HTTP status
	errorCode := ""
	switch respErrorCode {
	case 401:
		errorCode = ErrorCodeInvalidAPIKey
	case 503:
		errorCode = ErrorCodeServiceUnavailable
	}

	return generateErrorResponse(
		envoyTypePb.StatusCode(respErrorCode),
		headers,
		errMsg, errorCode, "")
}

func (s *Server) emitMetricsCounterHelper(metricName, model, status, statusCode string, routingCtx *types.RoutingContext) {
	if routingCtx == nil {
		routingCtx = &types.RoutingContext{Model: model}
	}
	labels := buildGatewayPodMetricLabels(model, status, statusCode)
	metrics.EmitMetricToPrometheus(routingCtx, nil, metricName, &metrics.SimpleMetricValue{Value: 1.0}, labels)
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
