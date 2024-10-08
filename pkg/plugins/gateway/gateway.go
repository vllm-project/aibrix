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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	openai "github.com/sashabaranov/go-openai"
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
		"random":        routing.NewRandomRouter(),
		"least-request": routing.NewLeastRequestRouter(r),
		"throughput":    routing.NewThroughputRouter(r),
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
	var user, targetPodIP string
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
			resp, user, targetPodIP = s.HandleRequestHeaders(ctx, requestID, req)

		case *extProcPb.ProcessingRequest_RequestBody:
			resp = s.HandleRequestBody(req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = s.HandleResponseHeaders(req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseBody:
			resp = s.HandleResponseBody(ctx, requestID, req, user, targetPodIP)

		default:
			klog.Infof("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			klog.Infof("send error %v", err)
		}
	}
}

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, string, string) {
	klog.Info("--- In RequestHeaders processing ...")
	var username, model, routingStrategy, targetPodIP string
	r := req.Request
	h := r.(*extProcPb.ProcessingRequest_RequestHeaders)

	for _, n := range h.RequestHeaders.Headers.Headers {
		if strings.ToLower(n.Key) == "user" {
			username = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "model" {
			model = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "routing-strategy" {
			routingStrategy = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "target-pod" {
			targetPodIP = string(n.RawValue)
		}
	}

	if username != "" {
		user, err := utils.GetUser(utils.User{Name: username}, s.redisClient)
		if err != nil {
			return generateErrorResponse(
				envoyTypePb.StatusCode_Forbidden,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-user-missing", RawValue: []byte("true"),
				}}},
				fmt.Sprintf("pre query: username is missing: %v", err.Error())), username, targetPodIP
		}

		errRes := s.checkLimits(ctx, requestID, user)
		if errRes != nil {
			return errRes, user.Name, targetPodIP
		}
	}

	headers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				Key:      "x-went-into-req-headers",
				RawValue: []byte("true"),
			},
		},
		// TODO (varun): refactor this part with model name input from request body
		// {
		// 	Header: &configPb.HeaderValue{
		// 		Key:      "x-updated-rpm",
		// 		RawValue: []byte(fmt.Sprintf("%d", rpm)),
		// 	},
		// },
	}
	if routingStrategy != "" {
		pods, err := s.cache.GetPodsForModel(model)
		if len(pods) == 0 || err != nil {
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-no-model-deployment", RawValue: []byte("true"),
				}}},
				fmt.Sprintf("pre query: no models are deployed: %v", err.Error())), username, targetPodIP
		}

		targetPodIP, err = s.selectTargetPod(ctx, routingStrategy, pods)
		if targetPodIP == "" || err != nil {
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-select-target-pod", RawValue: []byte("true"),
				}}},
				fmt.Sprintf("pre query: error on selecting target pod: %v", err.Error())), username, targetPodIP
		}

		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      "target-pod",
				RawValue: []byte(targetPodIP),
			},
		})
		klog.Infof("RequestStart %s: SelectedTargetPodIP: %s", requestID, targetPodIP)
	}

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					ClearRouteCache: true,
				},
			},
		},
	}

	return resp, username, targetPodIP
}

func (s *Server) HandleRequestBody(req *extProcPb.ProcessingRequest, targetPodIP string) *extProcPb.ProcessingResponse {
	klog.Info("--- In RequestBody processing")

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-went-into-req-body",
									RawValue: []byte("true"),
								},
							},
							{
								Header: &configPb.HeaderValue{
									Key:      "target-pod",
									RawValue: []byte(targetPodIP),
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *Server) HandleResponseHeaders(req *extProcPb.ProcessingRequest, targetPodIP string) *extProcPb.ProcessingResponse {
	klog.Info("--- In ResponseHeaders processing")

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

func (s *Server) HandleResponseBody(ctx context.Context, reqeustID string, req *extProcPb.ProcessingRequest, user string, targetPodIP string) *extProcPb.ProcessingResponse {
	klog.Infof("--- In ResponseBody processing %s", reqeustID)

	r := req.Request
	b := r.(*extProcPb.ProcessingRequest_ResponseBody)

	var res openai.CompletionResponse
	if err := json.Unmarshal(b.ResponseBody.Body, &res); err != nil {
		return generateErrorResponse(
			envoyTypePb.StatusCode_InternalServerError,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-error-response-unmarshal", RawValue: []byte("true"),
			}}},
			err.Error())
	}

	if user != "" {
		tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), int64(res.Usage.TotalTokens))
		if err != nil {
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: "x-error-update-tpm", RawValue: []byte("true"),
				}}},
				fmt.Sprintf("post query: error on updating tpm: %v", err.Error()))
		}
		klog.Infof("RequestEnd %s: TPM: %v for user: %v", reqeustID, tpm, user)
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							// TODO (varun): refactor with read model name from body
							// {
							// 	Header: &configPb.HeaderValue{
							// 		Key:      "x-updated-tpm",
							// 		RawValue: []byte(fmt.Sprintf("%d", tpm)),
							// 	},
							// },
						},
					},
				},
			},
		},
	}
}

func (s *Server) checkLimits(ctx context.Context, requestID string, user utils.User) *extProcPb.ProcessingResponse {
	if user.Rpm == 0 {
		user.Rpm = int64(defaultRPM)
	}
	if user.Tpm == 0 {
		user.Tpm = user.Rpm * int64(defaultTPMMultiplier)
	}

	code, err := s.checkRPM(ctx, user.Name, user.Rpm)
	if err != nil {
		return generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-rpm-exceeded", RawValue: []byte("true"),
			}}},
			fmt.Sprintf("pre query: error on checking rpm: %v", err.Error()))
	}

	rpm, code, err := s.incrRPM(ctx, user.Name)
	if err != nil {
		return generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-error-update-rpm", RawValue: []byte("true"),
			}}},
			fmt.Sprintf("pre query: error on updating rpm: %v", err.Error()))
	}
	klog.Infof("RequestStart %s: RPM: %v for user: %v", requestID, rpm, user.Name)

	code, err = s.checkTPM(ctx, user.Name, user.Tpm)
	if err != nil {
		return generateErrorResponse(
			code,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: "x-tpm-exceeded", RawValue: []byte("true"),
			}}},
			fmt.Sprintf("pre query: error on checking tpm: %v", err.Error()))
	}

	return nil
}

func (s *Server) checkRPM(ctx context.Context, user string, rpmLimit int64) (envoyTypePb.StatusCode, error) {
	rpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", user))
	if err != nil {
		klog.Error(err)
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get requests per minute current for user: %v", user)
	}
	klog.Infof("rmpCurrent: %v, rpmLimit: %v", rpmCurrent, rpmLimit)
	if rpmCurrent >= rpmLimit {
		err := fmt.Errorf("requests per limit of: %v, reached for user: %v", rpmLimit, user)
		klog.Errorln(err)
		return envoyTypePb.StatusCode_TooManyRequests, err
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) incrRPM(ctx context.Context, user string) (int64, envoyTypePb.StatusCode, error) {
	rpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", user), 1)
	if err != nil {
		return rpm, envoyTypePb.StatusCode_InternalServerError, err
	}

	klog.Infof("Updated RPM: %v for user: %v", rpm, user)
	return rpm, envoyTypePb.StatusCode_OK, nil
}

func (s *Server) checkTPM(ctx context.Context, user string, tpmLimit int64) (envoyTypePb.StatusCode, error) {
	tpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_TPM_CURRENT", user))
	if err != nil {
		klog.Error(err)
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get tokens per minute current for user: %v", user)
	}
	klog.Infof("tpmCurrent: %v, tpmLimit: %v", tpmCurrent, tpmLimit)
	if tpmCurrent >= tpmLimit {
		err := fmt.Errorf("tokens per limit of:%v, reached for user: %v", tpmLimit, user)
		klog.Errorln(err)
		return envoyTypePb.StatusCode_TooManyRequests, err
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) selectTargetPod(ctx context.Context, routingStrategy string, pods map[string]*v1.Pod) (string, error) {
	var route routing.Router
	switch routingStrategy {
	case "least-request":
		route = s.routers[routingStrategy]
	case "throughput":
		route = s.routers[routingStrategy]
	default:
		route = s.routers["random"]
	}

	return route.Route(ctx, pods)
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
