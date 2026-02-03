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
	"strings"
	"time"

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
	var user utils.User
	var rpm, traceTerm int64
	var respErrorCode int
	var model, metricLabel string
	var routerCtx *types.RoutingContext
	var stream, isRespError, isGatewayRspDone bool
	ctx := srv.Context()
	requestID := uuid.New().String()
	completed := false
	resp := &extProcPb.ProcessingResponse{}

	klog.InfoS("processing request", "requestID", requestID)

	for {
		select {
		case <-s.shutdownCh:
			// Always emit a server-shutdown metric; use "unknown" if model not yet parsed
			modelTag := GetModelTag(model)
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "aibrix_gateway_server_shutdown", "503")
			klog.InfoS("server shutdown requested; draining request", "request_id", requestID, "model", model)
			s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
			return status.Error(codes.Unavailable, "server shutdown in progress")

		case <-ctx.Done():
			// Client canceled or deadline exceeded; use fallback "unknown"
			modelTag := GetModelTag(model)
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "context_cancelled", "499")
			klog.ErrorS(ctx.Err(), "context cancelled", "request_id", requestID, "model", model)
			s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
			return ctx.Err()
		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			select {
			// check for shutdown
			case <-s.shutdownCh:
				modelTag := GetModelTag(model)
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, modelTag, "aibrix_gateway_server_shutdown", "503")
				klog.InfoS("server shutdown requested; stream closed (EOF) during shutdown drain", "requestID", requestID, "model", model)
				s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
				return status.Error(codes.Unavailable, "server shutdown in progress")
			default:
			}

			// EOF at completion is normal
			if completed {
				if model != "" {
					s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, model, "gateway_request_success", "200")
				}
				klog.V(2).InfoS("stream closed (EOF): completed", "requestID", requestID, "model", model)
				s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
				return nil
			}

			// client closed stream (EOF)
			if model != "" {
				s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, model, "client_cancelled_eof", "499")
			}
			klog.ErrorS(nil, "client closed stream (EOF) before completion", "requestID", requestID, "model", model)
			s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
			return io.EOF
		}
		if err != nil {
			// Normal stream closure by envoy proxy
			st, ok := status.FromError(err)
			if ok && st.Code() == codes.Canceled {
				if model != "" {
					s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, model, "gateway_request_success", "200")
				}
				s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
				return status.Error(codes.Canceled, "request canceled")
			}

			// Record failed request metric for other gRPC errors
			if ok {
				if model != "" {
					s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, model, "gateway_request_fail", fmt.Sprintf("%d", st.Code()))
				}
				klog.ErrorS(err, "error receiving stream from Envoy extproc", "requestID", requestID, "model", model, "grpc_code", st.Code(), "grpc_message", st.Message())
				s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
				return st.Err()
			}
			// Not a gRPC status error; fallback with context
			klog.ErrorS(err, "error receiving stream from Envoy extproc (non-gRPC)", "requestID", requestID)
			return status.Errorf(codes.Unknown, "recv stream error: %v", err)
		}

		resp = nil
		switch v := req.Request.(type) {

		case *extProcPb.ProcessingRequest_RequestHeaders:
			resp, user, rpm, routerCtx = s.HandleRequestHeaders(ctx, requestID, req)
			if routerCtx != nil {
				ctx = routerCtx
				model = routerCtx.Model
			}
			metricLabel = "gateway_req_headers"

		case *extProcPb.ProcessingRequest_RequestBody:
			resp, model, routerCtx, stream, traceTerm = s.HandleRequestBody(ctx, requestID, req, user)
			metricLabel = gatewayReqBody

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp, isRespError, respErrorCode = s.HandleResponseHeaders(ctx, requestID, model, req)
			if isRespError {
				switch respErrorCode {
				case 500:
					// for error code 500, ProcessingRequest_ResponseBody is not invoked
					resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID, "Internal server error")
				case 401:
					// Early return due to unauthorized or canceled context we noticed.
					resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID, "Incorrect API key provided")
				}
			}
			metricLabel = gatewayRespHeaders

		case *extProcPb.ProcessingRequest_ResponseBody:
			if isRespError {
				resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID,
					string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody()))
			} else {
				resp, completed = s.HandleResponseBody(ctx, requestID, req, user, rpm, model, stream, traceTerm, completed)
			}
			metricLabel = gatewayRespBody

		default:
			klog.Infof("Unknown Request type %+v\n", v)
		}

		if resp == nil {
			klog.ErrorS(nil, "no ProcessingResponse generated for message", "requestID", requestID, "msg_type", fmt.Sprintf("%T", req.Request))
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, model, "no_response_err", "500")
			s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
			return status.Errorf(codes.Internal, "no response generated for %T", req.Request)
		}

		if model != "" && resp.GetImmediateResponse() == nil {
			if model != "" && resp.GetImmediateResponse() == nil {
				if metricLabel != gatewayRespBody {
					s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, model, metricLabel+"_success", "200")
				}
				if metricLabel == gatewayRespBody && completed && !isGatewayRspDone {
					isGatewayRspDone = true
					s.emitMetricsCounterHelper(metrics.GatewayRequestModelSuccessTotal, model, metricLabel+"_success", "200")
				}
			}
		}

		if model != "" && resp.GetImmediateResponse() != nil {
			statusCode := fmt.Sprintf("%d", int(resp.GetImmediateResponse().Status.GetCode()))
			metricFail := getMetricErr(resp.GetImmediateResponse(), metricLabel)
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, model, metricFail+"_fail", statusCode)
		}

		if err := srv.Send(resp); err != nil && len(model) > 0 {
			klog.ErrorS(err, "gateway fail to send response to envoy-proxy", "requestID", requestID)
			s.emitMetricsCounterHelper(metrics.GatewayRequestModelFailTotal, model, "send_envoy_proxy", "499")
			s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
			if routerCtx != nil {
				routerCtx.Delete()
			}

			// Optional: if it's context or connection-related, don’t retry
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
				klog.Warning("Stream already closed by client", "requestID", requestID)
			}
			return err
		}
	}
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
		resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders(),
		errMsg, errorCode, "")
}

func (s *Server) emitMetricsCounterHelper(metricName, model, status, statusCode string) {
	labelNames, labelValues := buildGatewayPodMetricLabels(model, status, statusCode)
	metrics.EmitMetricToPrometheus(metricName, &metrics.SimpleMetricValue{Value: 1.0}, labelNames, labelValues)
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
