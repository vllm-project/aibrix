/*
Copyright 2025 The Aibrix Team.

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

package server

import (
	"context"
	"net"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"k8s.io/klog/v2"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/handler"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// Server holds the gRPC and HTTP servers for the console backend.
type Server struct {
	grpcServer      *grpc.Server
	httpServer      *http.Server
	store           store.Store
	gatewayEndpoint string
}

// New creates a new console Server.
func New(gatewayEndpoint string) *Server {
	return &Server{
		store:           store.NewMemoryStore(),
		gatewayEndpoint: gatewayEndpoint,
	}
}

// StartGRPC starts the gRPC server on the given address.
func (s *Server) StartGRPC(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.grpcServer = grpc.NewServer()

	// Register all service handlers
	pb.RegisterDeploymentServiceServer(s.grpcServer, handler.NewDeploymentHandler(s.store))
	pb.RegisterJobServiceServer(s.grpcServer, handler.NewJobHandler(s.store))
	pb.RegisterModelServiceServer(s.grpcServer, handler.NewModelHandler(s.store))
	pb.RegisterAPIKeyServiceServer(s.grpcServer, handler.NewAPIKeyHandler(s.store))
	pb.RegisterSecretServiceServer(s.grpcServer, handler.NewSecretHandler(s.store))
	pb.RegisterQuotaServiceServer(s.grpcServer, handler.NewQuotaHandler(s.store))

	// Enable gRPC reflection for debugging
	reflection.Register(s.grpcServer)

	klog.Infof("gRPC server listening on %s", addr)
	return s.grpcServer.Serve(lis)
}

// StartHTTP starts the grpc-gateway HTTP server on the given address,
// connecting to the gRPC server at grpcAddr.
func (s *Server) StartHTTP(httpAddr, grpcAddr string) error {
	ctx := context.Background()

	mux := runtime.NewServeMux()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	// Register all gRPC-gateway handlers
	for _, registerFn := range []func(context.Context, *runtime.ServeMux, string, []grpc.DialOption) error{
		pb.RegisterDeploymentServiceHandlerFromEndpoint,
		pb.RegisterJobServiceHandlerFromEndpoint,
		pb.RegisterModelServiceHandlerFromEndpoint,
		pb.RegisterAPIKeyServiceHandlerFromEndpoint,
		pb.RegisterSecretServiceHandlerFromEndpoint,
		pb.RegisterQuotaServiceHandlerFromEndpoint,
	} {
		if err := registerFn(ctx, mux, grpcAddr, opts); err != nil {
			return err
		}
	}

	// Register playground SSE proxy as a custom route
	playgroundHandler := handler.NewPlaygroundHandler(s.gatewayEndpoint)
	if err := mux.HandlePath("POST", "/api/v1/playground/chat/completions", playgroundHandler.HandleChatCompletion); err != nil {
		return err
	}

	// Wrap with CORS middleware
	httpHandler := corsMiddleware(mux)

	s.httpServer = &http.Server{
		Addr:    httpAddr,
		Handler: httpHandler,
	}

	klog.Infof("HTTP gateway listening on %s", httpAddr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops both servers.
func (s *Server) Shutdown(ctx context.Context) {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.httpServer != nil {
		_ = s.httpServer.Shutdown(ctx)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}
