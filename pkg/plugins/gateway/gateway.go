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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/aibrix/aibrix/pkg/cache"
	routing "github.com/aibrix/aibrix/pkg/plugins/gateway/algorithms"
	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/aibrix/aibrix/pkg/utils"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	routingStrategies = []string{"random", "least-request", "throughput"}
)

const (
	DefaultRPM           = 100
	DefaultTPMMultiplier = 1000
	InternalServerError  = envoyTypePb.StatusCode_InternalServerError
)

type Server struct {
	mu          sync.RWMutex
	routers     map[string]routing.Router
	redisClient *redis.Client
	ratelimiter ratelimiter.RateLimiter
	cache       *cache.Cache
}

func NewServer(redisClient *redis.Client) *Server {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}
	r := ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)
	routers := map[string]routing.Router{
		"random":        routing.NewRandomRouter(),
		"least-request": routing.NewLeastRequestRouter(),
		"throughput":    routing.NewThroughputRouter(),
	}

	return &Server{
		routers:     routers,
		redisClient: redisClient,
		ratelimiter: r,
		cache:       cache,
	}
}

type HealthServer struct{}

func (s *HealthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *HealthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "watch is not implemented")
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	var user utils.User
	var routingStrategy, targetPodIP string
	var stream bool
	ctx := srv.Context()
	requestID := uuid.New().String()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		resp := &extProcPb.ProcessingResponse{}
		switch v := req.Request.(type) {

		case *extProcPb.ProcessingRequest_RequestHeaders:
			resp, user, routingStrategy = s.HandleRequestHeaders(ctx, requestID, req)

		case *extProcPb.ProcessingRequest_RequestBody:
			resp, targetPodIP, stream = s.HandleRequestBody(ctx, requestID, req, user, routingStrategy)

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{
							HeaderMutation: &extProcPb.HeaderMutation{
								SetHeaders: []*configPb.HeaderValueOption{},
							},
						},
					},
				},
			}

		case *extProcPb.ProcessingRequest_ResponseBody:
			resp = s.HandleResponseBody(ctx, requestID, req, user, targetPodIP, stream)

		default:
			klog.Infof("Unknown Request type %+v\n", v)
			resp = buildErrorResponse(envoyTypePb.StatusCode_NotFound, "unknown processing request to gateway", "x-unknown-processing-request", "true")
		}

		if err := srv.Send(resp); err != nil {
			klog.Infof("send error %v", err)
		}
	}
}

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, string) {
	klog.V(4).InfoS("-- In RequestHeaders processing ...", "requestID", requestID)

	h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
	routingStrategy, routingStrategyEnabled := getRoutingStrategy(h.RequestHeaders.Headers.Headers)
	if routingStrategyEnabled && !validateRoutingStrategy(routingStrategy) {
		errMsg := fmt.Sprintf("invalid routing strategy: %s", routingStrategy)
		klog.ErrorS(nil, errMsg, "requestID", requestID)
		return buildErrorResponse(envoyTypePb.StatusCode_BadRequest,
			errMsg, "x-invalid-routing-strategy", routingStrategy), utils.User{}, routingStrategy
	}

	user, errRes, err := s.validateUserConfig(ctx, h.RequestHeaders.Headers.Headers)
	if errRes != nil {
		klog.ErrorS(err, "error on validating user config", "requestID", requestID, "username", user.Name)
		return errRes, user, routingStrategy
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: buildEnvoyProxyHeaders([]*configPb.HeaderValueOption{}, "x-went-into-req-headers", "true"),
					},
					ClearRouteCache: true,
				},
			},
		},
	}, user, routingStrategy
}

func (s *Server) HandleRequestBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, routingStrategy string) (*extProcPb.ProcessingResponse, string, bool) {
	klog.V(4).InfoS("-- In RequestBody processing ...", "requestID", requestID)
	var model, targetPodIP string
	var ok, stream bool

	var jsonMap map[string]interface{}

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	if err := json.Unmarshal(body.RequestBody.GetBody(), &jsonMap); err != nil {
		klog.ErrorS(err, "error to unmarshal request body", "requestID", requestID, "requestBody", string(body.RequestBody.GetBody()))
		return buildErrorResponse(InternalServerError, "error processing request body", "x-error-request-body-processing", "true"), targetPodIP, stream
	}

	if model, ok = jsonMap["model"].(string); !ok || model == "" { // || !s.cache.CheckModelExists(model) # enable when dynamic lora is enabled
		klog.ErrorS(nil, "model error in request", "requestID", requestID, "jsonMap", jsonMap)
		return buildErrorResponse(InternalServerError,
			fmt.Sprintf("no model in request body or model %s does not exist", model), "x-missing-model", "true"), targetPodIP, stream
	}

	stream, ok = jsonMap["stream"].(bool)
	if stream && ok {
		streamOptions, ok := jsonMap["stream_options"].(map[string]interface{})
		if !ok {
			return buildErrorResponse(InternalServerError, "stream_options is missing", "x-missing-stream-options", "true"), targetPodIP, stream
		}
		includeUsage, ok := streamOptions["include_usage"].(bool)
		if !includeUsage || !ok {
			return buildErrorResponse(InternalServerError, "stream_options is incorrect", "x-error-stream-options-include-usage", "true"), targetPodIP, stream
		}
	}

	headers := []*configPb.HeaderValueOption{}
	switch {
	case routingStrategy == "":
		headers = buildEnvoyProxyHeaders(headers, "model", model)
		klog.InfoS("request start", "requestID", requestID, "model", model, "user", user.Name)
	case routingStrategy != "":
		pods, err := s.cache.GetPodsForModel(model)
		if len(pods) == 0 || err != nil {
			return buildErrorResponse(InternalServerError,
				fmt.Sprintf("error on getting pods for model %s", model),
				"x-no-model-deployment", "true"), targetPodIP, stream
		}
		targetPodIP, err = s.routers[routingStrategy].Route(ctx, pods)
		if err != nil {
			return buildErrorResponse(InternalServerError,
				fmt.Sprintf("error on selecting target pod for routingStrategy: %s", routingStrategy),
				"x-error-routing", "true"), targetPodIP, stream
		}

		headers = buildEnvoyProxyHeaders(headers, "routing-strategy", routingStrategy, "target-pod", targetPodIP)
		klog.InfoS("request start", "requestID", requestID, "model", model, "user", user.Name, "routingStrategy", routingStrategy, "targetPodIP", targetPodIP)
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}, targetPodIP, stream
}

func (s *Server) HandleResponseBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, targetPodIP string, stream bool) *extProcPb.ProcessingResponse {
	klog.V(4).InfoS("-- In ResponseBody processing ...", "requestID", requestID)
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)

	var res openai.ChatCompletion
	var model string
	var usage openai.CompletionUsage
	headers := []*configPb.HeaderValueOption{}

	switch stream {
	case true:
		t := &http.Response{
			Body: io.NopCloser(bytes.NewReader(b.ResponseBody.GetBody())),
		}
		streaming := ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(t), nil)
		for streaming.Next() {
			evt := streaming.Current()
			if len(evt.Choices) == 0 {
				model = evt.Model
				usage = evt.Usage
			}
		}
		if err := streaming.Err(); err != nil {
			klog.ErrorS(err, "error at chat completion chunk streaming", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			return buildErrorResponse(InternalServerError, err.Error(), "x-error-chatcompletionchunk-streaming", "true")
		}
	case false:
		if err := json.Unmarshal(b.ResponseBody.Body, &res); err != nil {
			klog.ErrorS(err, "error to unmarshal chat completion response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			return buildErrorResponse(InternalServerError, err.Error(), "x-error-chatcompletion-unmarshal", "true")
		}
		model = res.Model
		usage = res.Usage
	}

	// TODO refactor into separate post response
	if usage.TotalTokens != 0 {
		var rpm, tpm int64
		var err error
		defer func() {
			go func() {
				s.cache.AddRequestTrace(model, usage.PromptTokens, usage.CompletionTokens)
			}()
		}()

		if user.Name != "" {
			s.mu.Lock()
			rpm, err = s.getRPM(ctx, user.Name)
			if err != nil {
				klog.ErrorS(err, "error to read current rpm", "requestID", requestID, "username", user.Name)
				return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error(), "x-error-get-rpm", "true")
			}

			tpm, err = s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), int64(usage.TotalTokens))
			if err != nil {
				klog.ErrorS(err, "error to increment current tpm", "requestID", requestID, "username", user.Name)
				return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, err.Error(), "x-error-update-tpm", "true")
			}
			s.mu.Unlock()

			headers = buildEnvoyProxyHeaders(headers, "username", user.Name, "x-updated-rpm", strconv.Itoa(int(rpm)), "x-updated-tpm", strconv.Itoa(int(tpm)))
		}
		if targetPodIP != "" {
			headers = buildEnvoyProxyHeaders(headers, "target-pod", targetPodIP)
		}
		klog.InfoS("request end", "requestID", requestID, "rpm", rpm, "tpm", tpm, "target-pod", targetPodIP)
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}
}

func (s *Server) CheckLimits(ctx context.Context, user utils.User) (*extProcPb.ProcessingResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if user.Rpm == 0 {
		user.Rpm = int64(DefaultRPM)
	}
	if user.Tpm == 0 {
		user.Tpm = user.Rpm * int64(DefaultTPMMultiplier)
	}

	code, err := s.checkRPM(ctx, user.Name, user.Rpm)
	if err != nil {
		return buildErrorResponse(code, err.Error(), "x-exceeded-rpm", "true", "username", user.Name), err
	}

	code, err = s.checkTPM(ctx, user.Name, user.Tpm)
	if err != nil {
		return buildErrorResponse(code, err.Error(), "x-exceeded-tpm", "true", "username", user.Name), err
	}

	code, err = s.incrRPM(ctx, user.Name)
	if err != nil {
		return buildErrorResponse(code, err.Error(), "x-error-incr-rpm", "true", "username", user.Name), err
	}

	return nil, nil
}

func (s *Server) getRPM(ctx context.Context, username string) (int64, error) {
	return s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", username))
}

func (s *Server) checkRPM(ctx context.Context, username string, rpmLimit int64) (envoyTypePb.StatusCode, error) {
	rpmCurrent, err := s.getRPM(ctx, username)
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get current RPM")
	}

	if rpmCurrent >= rpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("exceeded RPM_LIMIT: %v", rpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) incrRPM(ctx context.Context, username string) (envoyTypePb.StatusCode, error) {
	_, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", username), 1)
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to increment current RPM")
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) checkTPM(ctx context.Context, username string, tpmLimit int64) (envoyTypePb.StatusCode, error) {
	tpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_TPM_CURRENT", username))
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get current TPM")
	}

	if tpmCurrent >= tpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("exceeded TPM_LIMIT: %v", tpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}
