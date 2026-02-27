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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
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
	defaultAIBrixNamespace = "aibrix-system"
	metricHeaderErr        = "metric-header-err"
	gatewayRespBody        = "gateway_rsp_body"
	gatewayRespHeaders     = "gateway_rsp_headers"
	gatewayReqBody         = "gateway_req_body"
)

type Server struct {
	redisClient         *redis.Client
	ratelimiter         ratelimiter.RateLimiter
	client              kubernetes.Interface
	gatewayClient       gatewayapi.Interface
	requestCountTracker map[string]int
	cache               cache.Cache
	metricsServer       *metrics.Server
	// Broadcast channel for server-initiated shutdown
	shutdownCh <-chan struct{}
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
}

var podName = os.Getenv("POD_NAME")

func NewServer(redisClient *redis.Client, client kubernetes.Interface, gatewayClient gatewayapi.Interface) *Server {
	c, err := cache.Get()
	if err != nil {
		panic(err)
	}
	r := ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)

	// Initialize the routers
	routing.Init()

	return &Server{
		redisClient:         redisClient,
		ratelimiter:         r,
		client:              client,
		gatewayClient:       gatewayClient,
		requestCountTracker: map[string]int{},
		cache:               c,
		metricsServer:       nil,
	}
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	st := &processState{
		ctx:       srv.Context(),
		requestID: uuid.New().String(),
	}

	klog.InfoS("processing request", "requestID", st.requestID)

	for {
		if err := s.processOnce(srv, st); err != nil {
			return err
		}
	}
}

func (s *Server) processOnce(srv extProcPb.ExternalProcessor_ProcessServer, st *processState) error {
	if err := s.preRecvCheck(st); err != nil {
		return err
	}

	req, err := srv.Recv()
	if err != nil {
		return s.handleRecvError(st, err)
	}

	resp, err := s.handleProcessingRequest(st, req)
	if err != nil {
		return err
	}

	s.emitProcessMetrics(st, resp)
	return s.sendProcessingResponse(srv, st, resp)
}

func (s *Server) preRecvCheck(st *processState) error {
	select {
	case <-s.shutdownCh:
		modelTag := GetModelTag(st.model)
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "aibrix_gateway_server_shutdown", "503")
		klog.InfoS("server shutdown requested; draining request", "request_id", st.requestID, "model", st.model)
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return status.Error(codes.Unavailable, "server shutdown in progress")
	case <-st.ctx.Done():
		modelTag := GetModelTag(st.model)
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "context_cancelled", "499")
		klog.ErrorS(st.ctx.Err(), "context cancelled", "request_id", st.requestID, "model", st.model)
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return st.ctx.Err()
	default:
		return nil
	}
}

func (s *Server) handleRecvError(st *processState, err error) error {
	if err == io.EOF {
		return s.handleEOF(st)
	}
	return s.handleNonEOFRecvError(st, err)
}

func (s *Server) handleEOF(st *processState) error {
	select {
	case <-s.shutdownCh:
		modelTag := GetModelTag(st.model)
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "aibrix_gateway_server_shutdown", "503")
		klog.InfoS("server shutdown requested; stream closed (EOF) during shutdown drain", "requestID", st.requestID, "model", st.model)
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return status.Error(codes.Unavailable, "server shutdown in progress")
	default:
	}

	if st.completed {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200")
		}
		klog.V(2).InfoS("stream closed (EOF): completed", "requestID", st.requestID, "model", st.model)
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return nil
	}

	if st.model != "" {
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "client_cancelled_eof", "499")
	}
	klog.ErrorS(nil, "client closed stream (EOF) before completion", "requestID", st.requestID, "model", st.model)
	s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
	return io.EOF
}

func (s *Server) handleNonEOFRecvError(st *processState, err error) error {
	stErr, ok := status.FromError(err)
	if ok && stErr.Code() == codes.Canceled {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, "gateway_request_success", "200")
		}
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return status.Error(codes.Canceled, "request canceled")
	}

	if ok {
		if st.model != "" {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "gateway_request_fail", fmt.Sprintf("%d", stErr.Code()))
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
		resp, st.user, st.rpm, st.routerCtx = s.HandleRequestHeaders(st.ctx, st.requestID, req)
		if st.routerCtx != nil {
			st.ctx = st.routerCtx
			st.model = st.routerCtx.Model
		}
		st.metricLabel = "gateway_req_headers"

	case *extProcPb.ProcessingRequest_RequestBody:
		resp, st.model, st.routerCtx, st.stream, st.traceTerm = s.HandleRequestBody(st.ctx, st.requestID, req, st.user)
		st.metricLabel = gatewayReqBody

	case *extProcPb.ProcessingRequest_ResponseHeaders:
		resp, st.isRespError, st.respErrorCode = s.HandleResponseHeaders(st.ctx, st.requestID, st.model, req)
		st.lastRespHeaders = resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
		if st.isRespError {
			resp = s.responseForResponseHeaderError(st, resp)
		}
		st.metricLabel = gatewayRespHeaders

	case *extProcPb.ProcessingRequest_ResponseBody:
		if st.isRespError {
			body := string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody())
			resp = s.responseErrorProcessingWithHeaders(st.ctx, st.lastRespHeaders, st.respErrorCode, st.model, st.requestID, body)
		} else {
			resp, st.completed = s.HandleResponseBody(st.ctx, st.requestID, req, st.user, st.rpm, st.model, st.stream, st.traceTerm, st.completed)
		}
		st.metricLabel = gatewayRespBody

	default:
		klog.InfoS("unknown request type", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
	}

	if resp == nil {
		klog.ErrorS(nil, "no ProcessingResponse generated for message", "requestID", st.requestID, "msg_type", fmt.Sprintf("%T", req.Request))
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "no_response_err", "500")
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
		return nil, status.Errorf(codes.Internal, "no response generated for %T", req.Request)
	}

	return resp, nil
}

func (s *Server) responseForResponseHeaderError(st *processState, resp *extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	switch st.respErrorCode {
	case 500:
		return s.responseErrorProcessing(st.ctx, resp, st.respErrorCode, st.model, st.requestID, "Internal server error")
	case 401:
		return s.responseErrorProcessing(st.ctx, resp, st.respErrorCode, st.model, st.requestID, "Incorrect API key provided")
	default:
		return resp
	}
}

func (s *Server) emitProcessMetrics(st *processState, resp *extProcPb.ProcessingResponse) {
	s.emitGatewayRequestTotalMetric(resp, st.model)
	if st.model == "" {
		return
	}

	if resp.GetImmediateResponse() == nil {
		if st.metricLabel != gatewayRespBody {
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, st.metricLabel+"_success", "200")
			return
		}
		if st.completed && !st.isGatewayRspDone {
			st.isGatewayRspDone = true
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, st.model, st.metricLabel+"_success", "200")
		}
		return
	}

	statusCode := fmt.Sprintf("%d", int(resp.GetImmediateResponse().Status.GetCode()))
	metricFail := getMetricErr(resp.GetImmediateResponse(), st.metricLabel)
	s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, metricFail+"_fail", statusCode)
}

func (s *Server) sendProcessingResponse(srv extProcPb.ExternalProcessor_ProcessServer, st *processState, resp *extProcPb.ProcessingResponse) error {
	if err := srv.Send(resp); err != nil && len(st.model) > 0 {
		klog.ErrorS(err, "gateway fail to send response to envoy-proxy", "requestID", st.requestID)
		s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, st.model, "send_envoy_proxy", "499")
		s.cache.DoneRequestCount(st.routerCtx, st.requestID, st.model, st.traceTerm)
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

func (s *Server) selectTargetPod(ctx *types.RoutingContext, pods types.PodList, externalFilterExpr string) (string, error) {
	router, err := routing.Select(ctx)
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
		ctx.SetTargetPod(readyPods[0])
		return ctx.TargetAddress(), nil
	}

	return router.Route(ctx, &utils.PodArray{Pods: readyPods})
}

// validateHTTPRouteStatus checks if httproute object exists and validates its conditions are true
func (s *Server) validateHTTPRouteStatus(ctx context.Context, model string) error {
	// Skip validation in standalone mode (no gateway client)
	if s.gatewayClient == nil {
		return nil
	}

	errMsg := []string{}
	name := fmt.Sprintf("%s-router", model)
	httproute, err := s.gatewayClient.GatewayV1().HTTPRoutes(defaultAIBrixNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

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

	if len(errMsg) == 0 {
		return nil
	}

	return errors.New(strings.Join(errMsg, ", "))
}

func (s *Server) StartMetricsServer(addr string) error {
	if s.metricsServer != nil {
		return nil
	}

	s.metricsServer = metrics.NewServer(addr)
	if err := s.metricsServer.Start(); err != nil {
		return fmt.Errorf("failed to start metrics server: %v", err)
	}

	return nil
}

func (s *Server) Shutdown() {
	if s.metricsServer != nil {
		if err := s.metricsServer.Stop(); err != nil {
			klog.ErrorS(err, "Error stopping metrics server")
		}
	}
}

func (s *Server) responseErrorProcessing(ctx context.Context, resp *extProcPb.ProcessingResponse, respErrorCode int,
	model, requestID, errMsg string) *extProcPb.ProcessingResponse {
	headers := resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	return s.responseErrorProcessingWithHeaders(ctx, headers, respErrorCode, model, requestID, errMsg)
}

func (s *Server) responseErrorProcessingWithHeaders(ctx context.Context, headers []*configPb.HeaderValueOption, respErrorCode int,
	model, requestID, errMsg string) *extProcPb.ProcessingResponse {
	var httprouteErr error
	routingCtx, ok := ctx.(*types.RoutingContext)
	// if use pd route Algorithm, we don't check httproute status
	if !ok || routingCtx.Algorithm != routing.RouterPD {
		httprouteErr = s.validateHTTPRouteStatus(ctx, model)
	}
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

func (s *Server) emitGatewayRequestTotalMetric(resp *extProcPb.ProcessingResponse, model string) {
	statusCode := "200"
	if resp.GetImmediateResponse() != nil {
		statusCode = fmt.Sprintf("%d", int(resp.GetImmediateResponse().Status.GetCode()))
	}
	labels := buildGatewayPodMetricLabels(model, "gateway_request_handled", statusCode)
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, nil, metrics.GatewayRequestTotal, &metrics.SimpleMetricValue{Value: 1.0}, labels)
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
