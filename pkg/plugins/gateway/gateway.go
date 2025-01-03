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
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/ssestream"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
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
	defaultRPM           = 100
	defaultTPMMultiplier = 1000
	routingStrategies    = []string{"random", "least-request", "throughput", "least-kv-cache", "least-busy-time", "least-latency"}
)

type Server struct {
	routers             map[string]routing.Router
	redisClient         *redis.Client
	ratelimiter         ratelimiter.RateLimiter
	client              kubernetes.Interface
	requestCountTracker map[string]int
	cache               *cache.Cache
}

func NewServer(redisClient *redis.Client, c kubernetes.Interface) *Server {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}
	r := ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)
	routers := map[string]routing.Router{
		"random":          routing.NewRandomRouter(),
		"least-request":   routing.NewLeastRequestRouter(),
		"throughput":      routing.NewThroughputRouter(),
		"least-kv-cache":  routing.NewLeastKvCacheRouter(),
		"least-busy-time": routing.NewLeastBusyTimeRouter(),
		"least-latency":   routing.NewLeastBusyTimeRouter(),
	}

	return &Server{
		routers:             routers,
		redisClient:         redisClient,
		ratelimiter:         r,
		client:              c,
		requestCountTracker: map[string]int{},
		cache:               cache,
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
	var rpm int64
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
			resp, user, rpm, routingStrategy = s.HandleRequestHeaders(ctx, requestID, req)

		case *extProcPb.ProcessingRequest_RequestBody:
			resp, targetPodIP, stream = s.HandleRequestBody(ctx, requestID, req, user, routingStrategy)

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = s.HandleResponseHeaders(ctx, requestID, req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseBody:
			resp = s.HandleResponseBody(ctx, requestID, req, user, rpm, targetPodIP, stream)

		default:
			klog.Infof("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			klog.Infof("send error %v", err)
		}
	}
}

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, int64, string) {
	klog.Info("\n\n")
	klog.InfoS("-- In RequestHeaders processing ...", "requestID", requestID)
	var username string
	var user utils.User
	var rpm int64
	var err error
	var errRes *extProcPb.ProcessingResponse

	h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
	for _, n := range h.RequestHeaders.Headers.Headers {
		if strings.ToLower(n.Key) == "user" {
			username = string(n.RawValue)
		}
	}

	routingStrategy, routingStrategyEnabled := GetRoutingStrategy(h.RequestHeaders.Headers.Headers)
	if routingStrategyEnabled && !validateRoutingStrategy(routingStrategy) {
		return generateErrorResponse(
			envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-incorrect-routing-strategy", RawValue: []byte(routingStrategy),
			}}}, ""), utils.User{}, rpm, routingStrategy
	}

	if username != "" {
		user, err = utils.GetUser(utils.User{Name: username}, s.redisClient)
		if err != nil {
			klog.ErrorS(err, "unable to process user info", "requestID", requestID, "username", username)
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-user-error", RawValue: []byte("true"),
				}}},
				err.Error()), utils.User{}, rpm, routingStrategy
		}

		rpm, errRes, err = s.checkLimits(ctx, user)
		if errRes != nil {
			klog.ErrorS(err, "error on checking limits", "requestID", requestID, "username", username)
			return errRes, utils.User{}, rpm, routingStrategy
		}
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-went-into-req-headers",
									RawValue: []byte("true"),
								},
							},
						},
					},
					ClearRouteCache: true,
				},
			},
		},
	}, user, rpm, routingStrategy
}

func (s *Server) HandleRequestBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, routingStrategy string) (*extProcPb.ProcessingResponse, string, bool) {
	klog.InfoS("-- In RequestBody processing ...", "requestID", requestID)
	var model, targetPodIP string
	var ok, stream bool

	var jsonMap map[string]interface{}

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	if err := json.Unmarshal(body.RequestBody.GetBody(), &jsonMap); err != nil {
		klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "requestBody", string(body.RequestBody.GetBody()))
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-request-body-processing-error", RawValue: []byte("true")}}},
			"error processing request body"), targetPodIP, stream
	}

	if model, ok = jsonMap["model"].(string); !ok || model == "" {
		klog.ErrorS(nil, "model error in request", "requestID", requestID, "jsonMap", jsonMap)
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-no-model", RawValue: []byte(model)}}},
			fmt.Sprintf("no model in request body or model %s does not exist", model)), targetPodIP, stream
	}

	// early reject the request if model doesn't exist.
	if !s.cache.CheckModelExists(model) {
		klog.ErrorS(nil, "model doesn't exist in cache, probably wrong model name", "requestID", requestID, "jsonMap", jsonMap)
		return generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-no-model", RawValue: []byte(model)}}},
			fmt.Sprintf("model %s does not exist", model)), targetPodIP, stream
	}

	stream, ok = jsonMap["stream"].(bool)
	if stream && ok {
		streamOptions, ok := jsonMap["stream_options"].(map[string]interface{})
		if !ok {
			return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-stream-options", RawValue: []byte("stream options not set")}}},
				"error processing request body"), targetPodIP, stream
		}
		includeUsage, ok := streamOptions["include_usage"].(bool)
		if !includeUsage || !ok {
			return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-stream-options-include-usage", RawValue: []byte("include usage for stream options not set")}}},
				"error processing request body"), targetPodIP, stream
		}
	}

	headers := []*configPb.HeaderValueOption{}
	if routingStrategy == "" {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      "model",
				RawValue: []byte(model),
			},
		})
		klog.InfoS("request start", "requestID", requestID, "model", model)
	} else {
		pods, err := s.cache.GetPodsForModel(model)
		if len(pods) == 0 || err != nil {
			return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-no-model-deployment", RawValue: []byte("true")}}},
				fmt.Sprintf("error on getting pods for model %s", model)), targetPodIP, stream
		}

		klog.InfoS("debug", "pods", pods)
		targetPodIP, err = s.selectTargetPod(ctx, routingStrategy, pods, model)
		klog.InfoS("debug", "targetPodIP", targetPodIP)
		if err != nil {
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-error-routing", RawValue: []byte("true")}}},
				"error on selecting target pod"), targetPodIP, stream
		}

		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      "routing-strategy",
				RawValue: []byte(routingStrategy),
			},
		},
			&configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      "target-pod",
					RawValue: []byte(targetPodIP),
				},
			})
		klog.InfoS("request start", "requestID", requestID, "model", model, "routingStrategy", routingStrategy, "targetPodIP", targetPodIP)
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

func (s *Server) HandleResponseHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, targetPodIP string) *extProcPb.ProcessingResponse {
	klog.InfoS("-- In ResponseHeaders processing ...", "requestID", requestID)

	headers := []*configPb.HeaderValueOption{{
		Header: &configPb.HeaderValue{
			Key:      "x-went-into-resp-headers",
			RawValue: []byte("true"),
		},
	}}
	if targetPodIP != "" {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      "target-pod",
				RawValue: []byte(targetPodIP),
			},
		})
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					ClearRouteCache: true,
				},
			},
		},
	}
}

func (s *Server) HandleResponseBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, targetPodIP string, stream bool) *extProcPb.ProcessingResponse {
	klog.InfoS("-- In ResponseBody processing ...", "requestID", requestID)
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)

	var res openai.ChatCompletion
	var model string
	var usage openai.CompletionUsage
	headers := []*configPb.HeaderValueOption{}

	if stream {
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
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-streaming-error", RawValue: []byte("true"),
				}}},
				err.Error())
		}
	} else {
		if err := json.Unmarshal(b.ResponseBody.Body, &res); err != nil {
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-error-response-unmarshal", RawValue: []byte("true"),
				}}},
				err.Error())
		}
		model = res.Model
		usage = res.Usage
	}
	var requestEnd string
	if usage.TotalTokens != 0 {
		defer func() {
			go func() {
				s.cache.AddRequestTrace(model, usage.PromptTokens, usage.CompletionTokens)
			}()
		}()

		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), res.Usage.TotalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: "x-error-update-tpm", RawValue: []byte("true"),
					}}},
					err.Error())
			}

			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "x-update-rpm",
						RawValue: []byte(fmt.Sprintf("%d", rpm)),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "x-update-tpm",
						RawValue: []byte(fmt.Sprintf("%d", tpm)),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+"rpm: %s, tpm: %s", rpm, tpm)
		}
		if targetPodIP != "" {
			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      "target-pod",
						RawValue: []byte(targetPodIP),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+", targetPod: %s", targetPodIP)
		}
		klog.Infof("request end, requestID: %s - %s", requestID, requestEnd)
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

func (s *Server) checkLimits(ctx context.Context, user utils.User) (int64, *extProcPb.ProcessingResponse, error) {
	if user.Rpm == 0 {
		user.Rpm = int64(defaultRPM)
	}
	if user.Tpm == 0 {
		user.Tpm = user.Rpm * int64(defaultTPMMultiplier)
	}

	code, err := s.checkRPM(ctx, user.Name, user.Rpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-rpm-error", RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	rpm, code, err := s.incrRPM(ctx, user.Name)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-error-incr-rpm", RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	code, err = s.checkTPM(ctx, user.Name, user.Tpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-tpm-exceeded", RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	return rpm, nil, nil
}

func (s *Server) checkRPM(ctx context.Context, username string, rpmLimit int64) (envoyTypePb.StatusCode, error) {
	rpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", username))
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get RPM for user: %v", username)
	}

	if rpmCurrent >= rpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("user: %v has exceeded RPM: %v", username, rpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) incrRPM(ctx context.Context, username string) (int64, envoyTypePb.StatusCode, error) {
	rpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", username), 1)
	if err != nil {
		return rpm, envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to increment RPM for user: %v", username)
	}

	return rpm, envoyTypePb.StatusCode_OK, nil
}

func (s *Server) checkTPM(ctx context.Context, username string, tpmLimit int64) (envoyTypePb.StatusCode, error) {
	tpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_TPM_CURRENT", username))
	if err != nil {
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get TPM for user: %v", username)
	}

	if tpmCurrent >= tpmLimit {
		return envoyTypePb.StatusCode_TooManyRequests, fmt.Errorf("user: %v has exceeded TPM: %v", username, tpmLimit)
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) selectTargetPod(ctx context.Context, routingStrategy string, pods map[string]*v1.Pod, model string) (string, error) {
	var route routing.Router
	switch routingStrategy {
	case "least-request":
		route = s.routers[routingStrategy]
	case "throughput":
		route = s.routers[routingStrategy]
	case "least-kv-cache":
		route = s.routers[routingStrategy]
	case "least-busy-time":
		route = s.routers[routingStrategy]
	case "least-latency":
		route = s.routers[routingStrategy]
	default:
		route = s.routers["random"]
	}

	return route.Route(ctx, pods, model)
}

func validateRoutingStrategy(routingStrategy string) bool {
	routingStrategy = strings.TrimSpace(routingStrategy)
	return slices.Contains(routingStrategies, routingStrategy)
}

func generateErrorResponse(statusCode envoyTypePb.StatusCode, headers []*configPb.HeaderValueOption, body string) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{
					Code: statusCode,
				},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: headers,
				},
				Body: body,
			},
		},
	}
}

// GetRoutingStrategy retrieves the routing strategy from the headers or environment variable
// It returns the routing strategy value and whether custom routing strategy is enabled.
func GetRoutingStrategy(headers []*configPb.HeaderValue) (string, bool) {
	var routingStrategy string
	routingStrategyEnabled := false

	// Check headers for routing strategy
	for _, header := range headers {
		if strings.ToLower(header.Key) == "routing-strategy" {
			routingStrategy = string(header.RawValue)
			routingStrategyEnabled = true
			break // Prioritize header value over environment variable
		}
	}

	// If header not set, check environment variable
	if !routingStrategyEnabled {
		if value, exists := utils.CheckEnvExists("ROUTING_ALGORITHM"); exists {
			routingStrategy = value
			routingStrategyEnabled = true
		}
	}

	return routingStrategy, routingStrategyEnabled
}
