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
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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
)

type Server struct {
	redisClient         *redis.Client
	ratelimiter         ratelimiter.RateLimiter
	client              kubernetes.Interface
	gatewayClient       gatewayapi.Interface
	requestCountTracker map[string]int
	cache               cache.Cache
	metricsServer       *metrics.Server
	httpClient          *http.Client
	forwardingConfig    map[string]string

	// Stream management for forwarding requests
	activeForwards utils.SyncMap[string, *forwardingRequest]
}

func NewServer(redisClient *redis.Client, client kubernetes.Interface, gatewayClient gatewayapi.Interface, forwardingAPIs map[string]string) *Server {
	c, err := cache.Get()
	if err != nil {
		panic(err)
	}
	r := ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)

	// Initialize the routers
	routing.Init()

	// Initialize HTTP client with timeout
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	return &Server{
		redisClient:         redisClient,
		ratelimiter:         r,
		client:              client,
		gatewayClient:       gatewayClient,
		requestCountTracker: map[string]int{},
		cache:               c,
		metricsServer:       nil,
		httpClient:          httpClient,
		forwardingConfig:    forwardingAPIs,
	}
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	var user utils.User
	var rpm, traceTerm int64
	var respErrorCode int
	var model string
	var routerCtx *types.RoutingContext
	var stream, isRespError bool
	ctx := srv.Context()
	requestID := uuid.New().String()
	completed := false
	resp := &extProcPb.ProcessingResponse{}
	var chanStreamingResponse <-chan *extProcPb.ProcessingResponse

	klog.InfoS("processing request", "requestID", requestID)

	defer func() {
		// Clean up forward requests
		if streamReq, ok := s.activeForwards.LoadAndDelete(requestID); ok {
			streamReq.DoneForward(ctx.Err())
			return
		}
		if len(model) == 0 {
			return
		}

		// Clean up model requests
		s.cache.DoneRequestCount(routerCtx, requestID, model, traceTerm)
		if routerCtx != nil {
			routerCtx.Delete()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if chanStreamingResponse != nil {
			// Stream started, we need to stream all response.
			for resp := range chanStreamingResponse {
				if err := srv.Send(resp); err != nil {
					klog.ErrorS(err, "Error on streaming response", "requestID", requestID)

					// Optional: if it's context or connection-related, don’t retry
					if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
						klog.Warning("Stream already closed by client", "requestID", requestID)
					}
					return err
				}
			}
		}

		req, err := srv.Recv()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		switch v := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			resp, user, rpm, routerCtx = s.HandleRequestHeaders(ctx, requestID, req)
			if routerCtx != nil {
				ctx = routerCtx
			}

		case *extProcPb.ProcessingRequest_RequestBody:
			resp, model, routerCtx, stream, traceTerm = s.HandleRequestBody(ctx, requestID, req, user)
			if s.isContinueAndReplaceResponse(resp) {
				// Response of forwarded request will be streamed.
				if streamingRequest, ok := s.activeForwards.Load(requestID); !ok {
					resp = generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
						[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
							Key: HeaderErrorExtendAPI, RawValue: []byte("true")}}},
						"can not load streaming request")
				} else {
					chanStreamingResponse = streamingRequest.responseChan
				}
			} else if routerCtx != nil {
				ctx = routerCtx
			}

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp, isRespError, respErrorCode = s.HandleResponseHeaders(ctx, requestID, model, req)
			if isRespError && respErrorCode == 500 {
				// for error code 500, ProcessingRequest_ResponseBody is not invoked
				resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID, "")
			}

			if isRespError && respErrorCode == 401 {
				// Early return due to unauthorized or canceled context we noticed.
				resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID, `{"error":"unauthorized"}`)
			}

		case *extProcPb.ProcessingRequest_ResponseBody:
			if isRespError {
				resp = s.responseErrorProcessing(ctx, resp, respErrorCode, model, requestID,
					string(req.Request.(*extProcPb.ProcessingRequest_ResponseBody).ResponseBody.GetBody()))
			} else {
				resp, completed = s.HandleResponseBody(ctx, requestID, req, user, rpm, model, stream, traceTerm, completed)
			}
		default:
			klog.Infof("Unknown Request type %+v\n", v)
			resp = generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorUnexpected, RawValue: []byte("true")}}},
				fmt.Sprintf("Unknown extProcPb request type %v", v))
		}

		if err := srv.Send(resp); err != nil && len(model) > 0 {
			klog.ErrorS(nil, err.Error(), "requestID", requestID)

			// Optional: if it's context or connection-related, don’t retry
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "EOF") {
				klog.Warning("Stream already closed by client", "requestID", requestID)
			}

			return err
		}
	}
}

func (s *Server) selectTargetPod(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	router, err := routing.Select(ctx)
	if err != nil {
		return "", err
	}

	if pods.Len() == 0 {
		return "", fmt.Errorf("no pods for routing")
	}
	readyPods := utils.FilterRoutablePods(pods.All())
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods for routing")
	}
	if len(readyPods) == 1 {
		ctx.SetTargetPod(readyPods[0])
		return ctx.TargetAddress(), nil
	}

	return router.Route(ctx, &utils.PodArray{Pods: readyPods})
}

// validateHTTPRouteStatus checks if httproute object exists and validates its conditions are true
func (s *Server) validateHTTPRouteStatus(ctx context.Context, model string) error {
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
	httprouteErr := s.validateHTTPRouteStatus(ctx, model)
	if errMsg != "" && httprouteErr != nil {
		errMsg = fmt.Sprintf("%s. %s", errMsg, httprouteErr.Error())
	} else if errMsg == "" && httprouteErr != nil {
		errMsg = httprouteErr.Error()
	}
	klog.ErrorS(nil, "request end", "requestID", requestID, "errorCode", respErrorCode, "errorMessage", errMsg)
	return generateErrorResponse(
		envoyTypePb.StatusCode(respErrorCode),
		resp.GetResponseHeaders().GetResponse().GetHeaderMutation().GetSetHeaders(),
		errMsg)
}

func (s *Server) isContinueAndReplaceResponse(resp *extProcPb.ProcessingResponse) bool {
	// First, get the RequestHeaders message from the "oneof" response.
	// The generated GetRequestHeaders() returns nil if the response is
	// not of this type.
	if rh := resp.GetRequestHeaders(); rh != nil {
		// Next, get the CommonResponse from within the HeadersResponse.
		if cr := rh.GetResponse(); cr != nil {
			// Finally, check the status enum.
			return cr.GetStatus() == extProcPb.CommonResponse_CONTINUE_AND_REPLACE
		}
	}
	// If any of the nested fields were nil, or if the status did not match,
	// it's not the response we're looking for.
	return false
}
