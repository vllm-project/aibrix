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
	"log"
	"strings"

	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/gateway/rate_limiter"
	routing "github.com/aibrix/aibrix/pkg/plugins/gateway/routing_algorithms"
	podutils "github.com/aibrix/aibrix/pkg/utils"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

type Server struct {
	ratelimiter ratelimiter.AccountRateLimiter
	client      kubernetes.Interface
}

func NewServer(r ratelimiter.AccountRateLimiter, c kubernetes.Interface) *Server {
	return &Server{
		ratelimiter: r,
		client:      c,
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
			resp, user, targetPodIP = s.HandleRequestHeaders(ctx, req)

		case *extProcPb.ProcessingRequest_RequestBody:
			resp = s.HandleRequestBody(req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = s.HandleResponseHeaders(req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseBody:
			resp = s.HandleResponseBody(ctx, req, user)

		default:
			log.Printf("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			log.Printf("send error %v", err)
		}
	}
}

func (s *Server) HandleRequestHeaders(ctx context.Context, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, string, string) {
	log.Println("--- In RequestHeaders processing ...")
	var user, model, targetPodIP string
	r := req.Request
	h := r.(*extProcPb.ProcessingRequest_RequestHeaders)

	log.Printf("Headers: %+v\n", h)
	log.Printf("EndOfStream: %v\n", h.RequestHeaders.EndOfStream)

	for _, n := range h.RequestHeaders.Headers.Headers {
		if strings.ToLower(n.Key) == "user" {
			user = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "model" {
			model = string(n.RawValue)
		}
		if strings.ToLower(n.Key) == "target-pod" {
			targetPodIP = string(n.RawValue)
		}
	}

	klog.Infof("user: %v", user)

	// TODO (varun): add check if user exists in backend storage
	// if no user name present in the request headers
	if user == "" {
		klog.Infoln("user does not exists")
		return nil, user, targetPodIP
	}
	code, err := s.checkRPM(ctx, user)
	if err != nil {
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: code,
					},
					Details: err.Error(),
					Headers: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-rpm-exceeded",
									RawValue: []byte("true"),
								},
							},
						},
					},
				},
			},
		}, user, targetPodIP
	}

	code, err = s.checkTPM(ctx, user)
	if err != nil {
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: code,
					},
					Details: err.Error(),
					Headers: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-tpm-exceeded",
									RawValue: []byte("true"),
								},
							},
						},
					},
				},
			},
		}, user, targetPodIP
	}

	pods, err := s.client.CoreV1().Pods(podutils.NAMESPACE).List(ctx, v1.ListOptions{
		LabelSelector: fmt.Sprintf("model.aibrix.ai=%s", model),
	})
	if err != nil {
		klog.Error(err)
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: code,
					},
					Details: err.Error(),
					Headers: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-routing-error",
									RawValue: []byte("true"),
								},
							},
						},
					},
				},
			},
		}, user, targetPodIP
	}

	// TODO (varun): evaluate how to enable selection of routing algorithm
	route := routing.NewRandomRouter()
	targetPodIP, _ = route.Get(ctx, pods.Items)
	headers := []*configPb.HeaderValueOption{{
		Header: &configPb.HeaderValue{
			Key:      "x-went-into-req-headers",
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
		ModeOverride: &filterPb.ProcessingMode{
			ResponseHeaderMode: filterPb.ProcessingMode_SEND,
			RequestBodyMode:    filterPb.ProcessingMode_NONE,
		},
	}

	return resp, user, targetPodIP
}

func (s *Server) HandleRequestBody(req *extProcPb.ProcessingRequest, targetPodIP string) *extProcPb.ProcessingResponse {
	log.Println("--- In RequestBody processing")

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
	log.Println("--- In ResponseHeaders processing")

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-went-into-resp-headers",
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
					ClearRouteCache: true,
				},
			},
		},
	}
}

func (s *Server) HandleResponseBody(ctx context.Context, req *extProcPb.ProcessingRequest, user string) *extProcPb.ProcessingResponse {
	log.Println("--- In ResponseBody processing")

	r := req.Request
	b := r.(*extProcPb.ProcessingRequest_ResponseBody)

	var res openai.CompletionResponse
	if err := json.Unmarshal(b.ResponseBody.Body, &res); err != nil {
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: envoyTypePb.StatusCode_InternalServerError,
					},
					Details: err.Error(),
				},
			},
		}
	}

	rpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", user), 1)
	if err != nil {
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: envoyTypePb.StatusCode_InternalServerError,
					},
					Details: err.Error(),
					Body:    "post query: error on updating rpm",
				},
			},
		}
	}
	tpm, err := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), int64(res.Usage.TotalTokens))
	if err != nil {
		return &extProcPb.ProcessingResponse{
			Response: &extProcPb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extProcPb.ImmediateResponse{
					Status: &envoyTypePb.HttpStatus{
						Code: envoyTypePb.StatusCode_InternalServerError,
					},
					Details: err.Error(),
					Body:    "post query: error on updating tpm",
				},
			},
		}
	}
	klog.Infof("Updated RPM: %v, TPM: %v for user: %v", rpm, tpm, user)

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									Key:      "x-updated-rpm",
									RawValue: []byte(fmt.Sprintf("%d", rpm)),
								},
							},
							{
								Header: &configPb.HeaderValue{
									Key:      "x-updated-tpm",
									RawValue: []byte(fmt.Sprintf("%d", tpm)),
								},
							},
						},
					},
				},
			},
		},
	}
}

func (s *Server) checkRPM(ctx context.Context, user string) (envoyTypePb.StatusCode, error) {
	rpmLimit, err := s.ratelimiter.GetLimit(ctx, fmt.Sprintf("%v_RPM_LIMIT", user))
	if err != nil {
		klog.Error(err)
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get requests per minute limit for user: %v", user)
	}
	rpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", user))
	if err != nil {
		klog.Error(err)
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get requests per minute current for user: %v", user)
	}
	klog.Infof("rmpCurrent: %v, rpmLimit: %v", rpmCurrent, rpmLimit)
	if rpmCurrent >= rpmLimit {
		err := fmt.Errorf("requests per limit of:%v, reached for user: %v", rpmLimit, user)
		klog.Errorln(err)
		return envoyTypePb.StatusCode_TooManyRequests, err
	}

	return envoyTypePb.StatusCode_OK, nil
}

func (s *Server) checkTPM(ctx context.Context, user string) (envoyTypePb.StatusCode, error) {
	tpmLimit, err := s.ratelimiter.GetLimit(ctx, fmt.Sprintf("%v_TPM_LIMIT", user))
	if err != nil {
		klog.Error(err)
		return envoyTypePb.StatusCode_InternalServerError, fmt.Errorf("fail to get tokens per minute limit for user: %v", user)
	}
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
