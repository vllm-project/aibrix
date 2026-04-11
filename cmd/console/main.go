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

package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/server"
)

var (
	grpcAddr        string
	httpAddr        string
	gatewayEndpoint string
)

func main() {
	flag.StringVar(&grpcAddr, "grpc-addr", ":50060", "The address the gRPC server binds to.")
	flag.StringVar(&httpAddr, "http-addr", ":8090", "The address the HTTP gateway binds to.")
	flag.StringVar(&gatewayEndpoint, "gateway-endpoint", "http://localhost:8888",
		"AIBrix gateway endpoint for playground proxy.")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	srv := server.New(gatewayEndpoint)

	// Start gRPC server
	go func() {
		if err := srv.StartGRPC(grpcAddr); err != nil {
			klog.Fatalf("Failed to start gRPC server: %v", err)
		}
	}()

	// Start HTTP gateway (connects to the gRPC server)
	go func() {
		if err := srv.StartHTTP(httpAddr, grpcAddr); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to start HTTP gateway: %v", err)
		}
	}()

	klog.Infof("AIBrix Console started: gRPC=%s HTTP=%s gateway=%s", grpcAddr, httpAddr, gatewayEndpoint)

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	klog.Info("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
