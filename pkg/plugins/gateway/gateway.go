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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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

const (
	HeaderErrorInvalidRouting = "x-error-invalid-routing-strategy"

	// General Error Headers
	HeaderErrorUser                  = "x-error-user"
	HeaderErrorRouting               = "x-error-routing"
	HeaderErrorRequestBodyProcessing = "x-error-request-body-processing"
	HeaderErrorResponseUnmarshal     = "x-error-response-unmarshal"
	HeaderErrorResponseUnknown       = "x-error-response-unknown"

	// Model & Deployment Headers
	HeaderErrorNoModelInRequest = "x-error-no-model-in-request"
	HeaderErrorNoModelBackends  = "x-error-no-model-backends"

	// Streaming Headers
	HeaderErrorStreaming                 = "x-error-streaming"
	HeaderErrorNoStreamOptions           = "x-error-no-stream-options"
	HeaderErrorStreamOptionsIncludeUsage = "x-error-no-stream-options-include-usage"

	// Request & Target Headers
	HeaderWentIntoReqHeaders = "x-went-into-req-headers"
	HeaderTargetPod          = "target-pod"
	HeaderRoutingStrategy    = "routing-strategy"

	// RPM & TPM Update Errors
	HeaderUpdateTPM        = "x-update-tpm"
	HeaderUpdateRPM        = "x-update-rpm"
	HeaderErrorRPMExceeded = "x-error-rpm-exceeded"
	HeaderErrorTPMExceeded = "x-error-tpm-exceeded"
	HeaderErrorIncrRPM     = "x-error-incr-rpm"
	HeaderErrorIncrTPM     = "x-error-incr-tpm"

	// Rate Limiting defaults
	DefaultRPM           = 100
	DefaultTPMMultiplier = 1000

	// Envs
	EnvRoutingAlgorithm = "ROUTING_ALGORITHM"

	// Router names
	RouterRandom        = "random"
	RouterLeastRequest  = "least-request"
	RouterThroughput    = "throughput"
	RouterPrefixCache   = "prefix-cache"
	RouterLeastKvCache  = "least-kv-cache"
	RouterLeastBusyTime = "least-busy-time"
	RouterLeastLatency  = "least-latency"
)

var (
	routingStrategies = []string{"random", "least-request", "throughput", "prefix-cache", "least-kv-cache", "least-busy-time", "least-latency"}

	ErrorUnknownResponse = errors.New("unknown response")

	requestBuffers sync.Map // Thread-safe map to track buffers per request
)

// routerConstructors maps router names to their initialization functions.
var routerConstructors = map[string]func() (routing.Router, error){
	RouterRandom:        func() (routing.Router, error) { return routing.NewRandomRouter() },
	RouterLeastRequest:  func() (routing.Router, error) { return routing.NewLeastRequestRouter() },
	RouterThroughput:    func() (routing.Router, error) { return routing.NewThroughputRouter() },
	RouterPrefixCache:   func() (routing.Router, error) { return routing.NewPrefixCacheRouter() },
	RouterLeastKvCache:  func() (routing.Router, error) { return routing.NewLeastKvCacheRouter() },
	RouterLeastBusyTime: func() (routing.Router, error) { return routing.NewLeastBusyTimeRouter() },
	RouterLeastLatency:  func() (routing.Router, error) { return routing.NewLeastExpectedLatencyRouter() },
}

type Server struct {
	routers             map[string]routing.Router
	redisClient         *redis.Client
	ratelimiter         ratelimiter.RateLimiter
	client              kubernetes.Interface
	requestCountTracker map[string]int
	cache               *cache.Cache
}

func NewServer(redisClient *redis.Client, client kubernetes.Interface) *Server {
	c, err := cache.GetCache()
	if err != nil {
		panic(err)
	}
	r := ratelimiter.NewRedisAccountRateLimiter("aibrix", redisClient, 1*time.Minute)
	routers := initializeRouters()

	return &Server{
		routers:             routers,
		redisClient:         redisClient,
		ratelimiter:         r,
		client:              client,
		requestCountTracker: map[string]int{},
		cache:               c,
	}
}

// initializeRouters initialize different routing algorithms, consider to initialize the router in lazy way
func initializeRouters() map[string]routing.Router {
	routers := make(map[string]routing.Router)
	for name, constructor := range routerConstructors {
		router, err := constructor()
		if err != nil {
			klog.Warningf("failed to initialize router %s: %v", name, err)
			continue
		}
		routers[name] = router
	}
	return routers
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
	var rpm, traceTerm int64
	var model, routingStrategy, targetPodIP string
	var stream bool
	ctx := srv.Context()
	requestID := uuid.New().String()
	completed := false

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
			resp, model, targetPodIP, stream, traceTerm = s.HandleRequestBody(ctx, requestID, req, user, routingStrategy)

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = s.HandleResponseHeaders(ctx, requestID, req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseBody:
			resp, completed = s.HandleResponseBody(ctx, requestID, req, user, rpm, model, targetPodIP, stream, traceTerm, completed)

		default:
			klog.Infof("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			klog.Infof("send error %v", err)
		}
	}
}

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, int64, string) {
	klog.Info("\n")
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

	routingStrategy, routingStrategyEnabled := getRoutingStrategy(h.RequestHeaders.Headers.Headers)
	if routingStrategyEnabled && !validateRoutingStrategy(routingStrategy) {
		klog.ErrorS(nil, "incorrect routing strategy", "routing-strategy", routingStrategy)
		return generateErrorResponse(
			envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorInvalidRouting, RawValue: []byte(routingStrategy),
			}}}, "incorrect routing strategy"), utils.User{}, rpm, routingStrategy
	}

	if username != "" {
		user, err = utils.GetUser(utils.User{Name: username}, s.redisClient)
		if err != nil {
			klog.ErrorS(err, "unable to process user info", "requestID", requestID, "username", username)
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorUser, RawValue: []byte("true"),
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
									Key:      HeaderWentIntoReqHeaders,
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

func (s *Server) HandleRequestBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, routingStrategy string) (*extProcPb.ProcessingResponse, string, string, bool, int64) {
	klog.InfoS("-- In RequestBody processing ...", "requestID", requestID)
	var model, targetPodIP string
	var ok, stream bool
	var term int64 // Identify the trace window

	var jsonMap map[string]interface{}

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)
	if err := json.Unmarshal(body.RequestBody.GetBody(), &jsonMap); err != nil {
		klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "requestBody", string(body.RequestBody.GetBody()))
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorRequestBodyProcessing, RawValue: []byte("true")}}},
			"error processing request body"), model, targetPodIP, stream, term
	}

	if model, ok = jsonMap["model"].(string); !ok || model == "" {
		klog.ErrorS(nil, "model error in request", "requestID", requestID, "jsonMap", jsonMap)
		return generateErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelInRequest, RawValue: []byte(model)}}},
			"no model in request body"), model, targetPodIP, stream, term
	}

	// early reject the request if model doesn't exist.
	if !s.cache.CheckModelExists(model) {
		klog.ErrorS(nil, "model doesn't exist in cache, probably wrong model name", "requestID", requestID, "model", model)
		return generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte(model)}}},
			fmt.Sprintf("model %s does not exist", model)), model, targetPodIP, stream, term
	}

	// early reject if no pods are ready to accept request for a model
	pods, err := s.cache.GetPodsForModel(model)
	if len(pods) == 0 || len(utils.FilterReadyPods(pods)) == 0 || err != nil {
		klog.ErrorS(err, "no ready pod available", "requestID", requestID, "model", model)
		return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte("true")}}},
			fmt.Sprintf("error on getting pods for model %s", model)), model, targetPodIP, stream, term
	}

	stream, ok = jsonMap["stream"].(bool)
	if ok && stream {
		if errRes := validateStreamOptions(requestID, user, jsonMap); errRes != nil {
			return errRes, model, targetPodIP, stream, term
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
		message, extErr := getRequestMessage(jsonMap)
		if err != nil {
			return extErr, model, targetPodIP, stream, term
		}

		targetPodIP, err = s.selectTargetPod(ctx, routingStrategy, pods, model, message)
		if targetPodIP == "" || err != nil {
			klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingStrategy", routingStrategy, "model", model)
			return generateErrorResponse(
				envoyTypePb.StatusCode_ServiceUnavailable,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorRouting, RawValue: []byte("true")}}},
				"error on selecting target pod"), model, targetPodIP, stream, term
		}

		headers = append(headers,
			&configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      HeaderRoutingStrategy,
					RawValue: []byte(routingStrategy),
				},
			},
			&configPb.HeaderValueOption{
				Header: &configPb.HeaderValue{
					Key:      HeaderTargetPod,
					RawValue: []byte(targetPodIP),
				},
			})
		klog.InfoS("request start", "requestID", requestID, "model", model, "routingStrategy", routingStrategy, "targetPodIP", targetPodIP)
	}

	term = s.cache.AddRequestCount(requestID, model)

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
	}, model, targetPodIP, stream, term
}

func (s *Server) HandleResponseHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, targetPodIP string) *extProcPb.ProcessingResponse {
	klog.InfoS("-- In ResponseHeaders processing ...", "requestID", requestID)

	headers := []*configPb.HeaderValueOption{{
		Header: &configPb.HeaderValue{
			Key:      HeaderWentIntoReqHeaders,
			RawValue: []byte("true"),
		},
	}}
	if targetPodIP != "" {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      HeaderTargetPod,
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

func (s *Server) HandleResponseBody(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest, user utils.User, rpm int64, model string, targetPodIP string, stream bool, traceTerm int64, hasCompleted bool) (*extProcPb.ProcessingResponse, bool) {
	b := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)
	klog.InfoS("-- In ResponseBody processing ...", "requestID", requestID, "endOfStream", b.ResponseBody.EndOfStream)

	var res openai.ChatCompletion
	var usage openai.CompletionUsage
	var promptTokens, completionTokens int64
	var headers []*configPb.HeaderValueOption
	complete := hasCompleted

	defer func() {
		// Wrapped in a function to delay the evaluation of parameters. Using complete to make sure DoneRequestTrace only call once for a request.
		if !hasCompleted && complete && b.ResponseBody.EndOfStream {
			s.cache.DoneRequestTrace(requestID, model, promptTokens, completionTokens, traceTerm)
		}
	}()

	if stream {
		t := &http.Response{
			Body: io.NopCloser(bytes.NewReader(b.ResponseBody.GetBody())),
		}
		streaming := ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(t), nil)
		for streaming.Next() {
			evt := streaming.Current()
			if len(evt.Choices) == 0 {
				// Do not overwrite model, res can be empty.
				usage = evt.Usage
			}
		}
		if err := streaming.Err(); err != nil {
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorStreaming, RawValue: []byte("true"),
				}}},
				err.Error()), complete
		}
	} else {
		// Use request ID as a key to store per-request buffer
		// Retrieve or create buffer
		buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
		buffer := buf.(*bytes.Buffer)
		// Append data to per-request buffer
		buffer.Write(b.ResponseBody.Body)

		if !b.ResponseBody.EndOfStream {
			// Partial data received, wait for more chunks, we just return a common response here.
			return &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}, complete
		}

		// Last part received, process the full response
		finalBody := buffer.Bytes()
		// Clean up the buffer after final processing
		requestBuffers.Delete(requestID)

		if err := json.Unmarshal(finalBody, &res); err != nil {
			klog.ErrorS(err, "error to unmarshal response", "requestID", requestID, "responseBody", string(b.ResponseBody.GetBody()))
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorResponseUnmarshal, RawValue: []byte("true"),
				}}},
				err.Error()), complete
		} else if len(res.Model) == 0 {
			msg := ErrorUnknownResponse.Error()
			responseBodyContent := string(b.ResponseBody.GetBody())
			if len(responseBodyContent) != 0 {
				msg = responseBodyContent
			}
			klog.ErrorS(err, "unexpected response", "requestID", requestID, "responseBody", responseBodyContent)
			complete = true
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorResponseUnknown, RawValue: []byte("true"),
				}}},
				msg), complete
		}
		// Do not overwrite model, res can be empty.
		usage = res.Usage
	}

	var requestEnd string
	if usage.TotalTokens != 0 {
		complete = true
		// Update promptTokens and completeTokens
		promptTokens = usage.PromptTokens
		completionTokens = usage.CompletionTokens
		// Count token per user.
		if user.Name != "" {
			tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), res.Usage.TotalTokens)
			if err != nil {
				return generateErrorResponse(
					envoyTypePb.StatusCode_InternalServerError,
					[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
						Key: HeaderErrorIncrTPM, RawValue: []byte("true"),
					}}},
					err.Error()), complete
			}

			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderUpdateRPM,
						RawValue: []byte(fmt.Sprintf("%d", rpm)),
					},
				},
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderUpdateTPM,
						RawValue: []byte(fmt.Sprintf("%d", tpm)),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+"rpm: %s, tpm: %s, ", rpm, tpm)
		}

		if targetPodIP != "" {
			headers = append(headers,
				&configPb.HeaderValueOption{
					Header: &configPb.HeaderValue{
						Key:      HeaderTargetPod,
						RawValue: []byte(targetPodIP),
					},
				},
			)
			requestEnd = fmt.Sprintf(requestEnd+"targetPod: %s", targetPodIP)
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
	}, complete
}

func (s *Server) checkLimits(ctx context.Context, user utils.User) (int64, *extProcPb.ProcessingResponse, error) {
	if user.Rpm == 0 {
		user.Rpm = int64(DefaultRPM)
	}
	if user.Tpm == 0 {
		user.Tpm = user.Rpm * int64(DefaultTPMMultiplier)
	}

	code, err := s.checkRPM(ctx, user.Name, user.Rpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorRPMExceeded, RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	rpm, code, err := s.incrRPM(ctx, user.Name)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorIncrRPM, RawValue: []byte("true"),
			}}},
			err.Error()), err
	}

	code, err = s.checkTPM(ctx, user.Name, user.Tpm)
	if err != nil {
		return 0, generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorTPMExceeded, RawValue: []byte("true"),
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

func (s *Server) selectTargetPod(ctx context.Context, routingStrategy string, pods map[string]*v1.Pod, model, message string) (string, error) {
	var route routing.Router
	switch routingStrategy {
	case "least-request":
		route = s.routers[routingStrategy]
	case "throughput":
		route = s.routers[routingStrategy]
	case "prefix-cache":
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

	return route.Route(ctx, pods, model, message)
}
