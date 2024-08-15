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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ratelimiter "github.com/aibrix/aibrix/pkg/plugins/ext_proc/rate_limiter"
	"github.com/coocood/freecache"
	redis "github.com/redis/go-redis/v9"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	cache    *freecache.Cache
	cacheKey = []byte("key")
)

type server struct {
	ratelimiter ratelimiter.AccountRateLimiter
}
type healthServer struct{}

func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "watch is not implemented")
}

func (s *server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	var user string
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
			r := req.Request
			h := r.(*extProcPb.ProcessingRequest_RequestHeaders)
			klog.Infof("In RequestHeaders pricessing, Request: %+v, EndOfStream: %+v", r, h.RequestHeaders.EndOfStream)

			for _, n := range h.RequestHeaders.Headers.Headers {
				if strings.ToLower(n.Key) == "user" {
					user = string(n.RawValue)
				}
			}

			klog.Infof("user: %v", user)

			// TODO (varun): add check if user exists in backend storage
			// if no user name present in the request headers
			if user == "" {
				klog.Infoln("user does not exists")
				return status.Errorf(codes.PermissionDenied, "user does not exists")
			}
			code, err := s.checkRPM(ctx, user)
			if err != nil {
				return status.Errorf(code, err.Error())
			}

			code, err = s.checkTPM(ctx, user)
			if err != nil {
				return status.Errorf(code, err.Error())
			}

			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{
							HeaderMutation: &extProcPb.HeaderMutation{
								SetHeaders: []*configPb.HeaderValueOption{},
							},
						},
					},
				},
				ModeOverride: &filterPb.ProcessingMode{
					ResponseHeaderMode: filterPb.ProcessingMode_DEFAULT,
					RequestBodyMode:    filterPb.ProcessingMode_NONE,
					ResponseBodyMode:   filterPb.ProcessingMode_STREAMED,
				},
			}

		case *extProcPb.ProcessingRequest_RequestBody:
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestBody{
					RequestBody: &extProcPb.BodyResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{
						Response: &extProcPb.CommonResponse{},
					},
				},
			}

		case *extProcPb.ProcessingRequest_ResponseBody:
			klog.Info("In ResponseBody")
			r := req.Request
			b := r.(*extProcPb.ProcessingRequest_ResponseBody)

			var res openai.CompletionResponse
			json.Unmarshal(b.ResponseBody.Body, &res)

			rpm, _ := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_RPM_CURRENT", user), 1)
			tpm, _ := s.ratelimiter.Incr(ctx, fmt.Sprintf("%v_TPM_CURRENT", user), int64(res.Usage.TotalTokens))
			klog.Infof("Updated RPM: %v, TPM: %v for user: %v", rpm, tpm, user)

		default:
			log.Printf("Unknown Request type %+v\n", v)
		}

		if err := srv.Send(resp); err != nil {
			log.Printf("send error %v", err)
		}
	}
}

func (s *server) checkRPM(ctx context.Context, user string) (codes.Code, error) {
	rpmLimit, err := s.ratelimiter.GetLimit(ctx, fmt.Sprintf("%v_RPM_LIMIT", user))
	if err != nil {
		klog.Error(err)
		return codes.Internal, fmt.Errorf("fail to get requests per minute limit for user: %v", user)
	}
	rpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_RPM_CURRENT", user))
	if err != nil {
		klog.Error(err)
		return codes.Internal, fmt.Errorf("fail to get requests per minute current for user: %v", user)
	}
	klog.Infof("rmpCurrent: %v, rpmLimit: %v", rpmCurrent, rpmLimit)
	if rpmCurrent > rpmLimit {
		err := fmt.Errorf("requests per limit of:%v, reached for user: %v", rpmLimit, user)
		klog.Errorln(err)
		return codes.ResourceExhausted, err
	}

	return codes.OK, nil
}

func (s *server) checkTPM(ctx context.Context, user string) (codes.Code, error) {
	tpmLimit, err := s.ratelimiter.GetLimit(ctx, fmt.Sprintf("%v_TPM_LIMIT", user))
	if err != nil {
		klog.Error(err)
		return codes.Internal, fmt.Errorf("fail to get tokens per minute limit for user: %v", user)
	}
	tpmCurrent, err := s.ratelimiter.Get(ctx, fmt.Sprintf("%v_TPM_CURRENT", user))
	if err != nil {
		klog.Error(err)
		return codes.Internal, fmt.Errorf("fail to get tokens per minute current for user: %v", user)
	}
	klog.Infof("tpmCurrent: %v, tpmLimit: %v", tpmCurrent, tpmLimit)
	if tpmCurrent > tpmLimit {
		err := fmt.Errorf("tokens per limit of:%v, reached for user: %v", tpmLimit, user)
		klog.Errorln(err)
		return codes.ResourceExhausted, err
	}

	return codes.OK, nil
}

// Create Redis Client
var (
	host = getEnv("REDIS_HOST", "localhost")
	port = string(getEnv("REDIS_PORT", "6379"))
)

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

const (
	RPM_LIMIT = 4
	TPM_LIMIT = 1000
)

func main() {

	// Connect to Redis
	client := redis.NewClient(&redis.Options{
		Addr: host + ":" + port,
		DB:   0, // Default DB
	})

	// Ping the Redis server to check the connection
	pong, err := client.Ping(context.Background()).Result()
	if err != nil {
		log.Fatal("Error connecting to Redis:", err)
	}
	fmt.Println("Connected to Redis:", pong)

	// grpc server init
	lis, err := net.Listen("tcp", ":50052")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	extProcPb.RegisterExternalProcessorServer(s, &server{
		ratelimiter: ratelimiter.NewRedisAccountRateLimiter("aibrix", client, 1*time.Minute),
	})
	healthPb.RegisterHealthServer(s, &healthServer{})

	log.Println("Starting gRPC server on port :50052")

	// shutdown
	var gracefulStop = make(chan os.Signal)
	signal.Notify(gracefulStop, syscall.SIGTERM)
	signal.Notify(gracefulStop, syscall.SIGINT)
	go func() {
		sig := <-gracefulStop
		log.Printf("caught sig: %+v", sig)
		log.Println("Wait for 1 second to finish processing")
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	s.Serve(lis)
}
