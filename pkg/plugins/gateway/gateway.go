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
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/utils"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

type Server struct {
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

	return &Server{
		redisClient:         redisClient,
		ratelimiter:         r,
		client:              client,
		requestCountTracker: map[string]int{},
		cache:               c,
	}
}

func (s *Server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	var user utils.User
	var rpm, traceTerm int64
	var respErrorCode int
	var model, routingStrategy, targetPodIP string
	var stream, isRespError bool
	ctx := srv.Context()
	requestID := uuid.New().String()
	completed := false

	klog.InfoS("Processing request", "requestID", requestID)

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
			resp, isRespError, respErrorCode = s.HandleResponseHeaders(ctx, requestID, req, targetPodIP)

		case *extProcPb.ProcessingRequest_ResponseBody:
			respBody := req.Request.(*extProcPb.ProcessingRequest_ResponseBody)
			if isRespError {
				klog.ErrorS(errors.New("request end"), string(respBody.ResponseBody.GetBody()), "requestID", requestID)
				generateErrorResponse(envoyTypePb.StatusCode(respErrorCode), nil, string(respBody.ResponseBody.GetBody()))
			} else {
				resp, completed = s.HandleResponseBody(ctx, requestID, req, user, rpm, model, targetPodIP, stream, traceTerm, completed)
			}
		default:
			klog.Infof("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			klog.Infof("send error %v", err)
		}
	}
}

func (s *Server) selectTargetPod(ctx context.Context, routingStrategy routing.Algorithms, pods map[string]*v1.Pod, model, message string) (string, error) {
	router, err := routing.Select(routingStrategy)()
	if err != nil {
		return "", err
	}
	return router.Route(ctx, pods, model, message)
}

func NewHealthCheckServer() *HealthServer {
	return &HealthServer{}
}

type HealthServer struct{}

func (s *HealthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *HealthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "watch is not implemented")
}
