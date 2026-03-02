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

	"github.com/vllm-project/aibrix/apps/console/api/config"
	"github.com/vllm-project/aibrix/apps/console/api/server"
)

func main() {
	// Allow CLI flags to override env vars
	grpcAddr := flag.String("grpc-addr", "", "gRPC server address (overrides GRPC_ADDR env)")
	httpAddr := flag.String("http-addr", "", "HTTP gateway address (overrides HTTP_ADDR env)")
	gatewayEndpoint := flag.String("gateway-endpoint", "",
		"AIBrix gateway endpoint (overrides GATEWAY_ENDPOINT env)")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	// Load configuration from environment
	cfg := config.Load()

	// Apply CLI flag overrides
	if *grpcAddr != "" {
		cfg.GRPCAddr = *grpcAddr
	}
	if *httpAddr != "" {
		cfg.HTTPAddr = *httpAddr
	}
	if *gatewayEndpoint != "" {
		cfg.GatewayEndpoint = *gatewayEndpoint
	}

	srv := server.New(cfg)

	// Start gRPC server
	go func() {
		if err := srv.StartGRPC(cfg.GRPCAddr); err != nil {
			klog.Fatalf("Failed to start gRPC server: %v", err)
		}
	}()

	// Start HTTP gateway (connects to the gRPC server)
	go func() {
		if err := srv.StartHTTP(cfg.HTTPAddr, cfg.GRPCAddr); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to start HTTP gateway: %v", err)
		}
	}()

	klog.Infof("AIBrix Console started: gRPC=%s HTTP=%s store=%s auth=%s",
		cfg.GRPCAddr, cfg.HTTPAddr, cfg.StoreType, cfg.AuthMode)

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	klog.Info("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
