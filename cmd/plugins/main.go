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
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/health"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

var (
	grpcAddr    string
	metricsAddr string
)

func main() {
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":50052", "The address the gRPC server binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	redisClient := utils.GetRedisClient()
	defer func() {
		if err := redisClient.Close(); err != nil {
			klog.Warningf("Error closing Redis client: %v", err)
		}
	}()

	stopCh := make(chan struct{})
	defer close(stopCh)
	var config *rest.Config
	var err error

	// ref: https://github.com/kubernetes-sigs/controller-runtime/issues/878#issuecomment-1002204308
	kubeConfig := flag.Lookup("kubeconfig").Value.String()
	if kubeConfig == "" {
		klog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}

	if err != nil {
		panic(err)
	}

	// Initialize cache with KV sync enabled for gateway
	kvSyncEnabled, _ := strconv.ParseBool(utils.LoadEnv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false"))
	remoteTokenizerEnabled, _ := strconv.ParseBool(utils.LoadEnv("AIBRIX_USE_REMOTE_TOKENIZER", "false"))

	cache.InitWithOptions(config, stopCh, cache.InitOptions{
		EnableKVSync:        kvSyncEnabled && remoteTokenizerEnabled,
		RedisClient:         redisClient,
		ModelRouterProvider: routing.ModelRouterFactory,
	})

	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error creating kubernetes client: %v", err)
	}

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}
	gatewayK8sClient, err := versioned.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error on creating gateway k8s client: %v", err)
	}

	gatewayServer := gateway.NewServer(redisClient, k8sClient, gatewayK8sClient)

	if err := gatewayServer.StartMetricsServer(metricsAddr); err != nil {
		klog.Fatalf("Failed to start metrics server: %v", err)
	}
	klog.Infof("Started metrics server on %s", metricsAddr)

	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, gatewayServer)

	healthCheck := health.NewServer()
	healthPb.RegisterHealthServer(s, healthCheck)
	healthCheck.SetServingStatus("gateway-plugin", healthPb.HealthCheckResponse_SERVING)

	klog.Info("starting gRPC server on " + grpcAddr)

	go func() {
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			klog.Fatalf("failed to setup profiling: %v", err)
		}
	}()

	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-gracefulStop
		klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		gatewayServer.Shutdown()
		s.GracefulStop()
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		panic(err)
	}
}
