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
	"runtime"
	"sync"
	"syscall"

	_ "go.uber.org/automaxprocs"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/health"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

var (
	grpcAddr        string
	httpAddr        string
	metricsAddr     string // deprecated: use httpAddr
	standalone      bool
	endpointsConfig string
)

func main() {
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":50052", "The address the gRPC server binds to.")
	flag.StringVar(&httpAddr, "http-bind-address", "", "The address the HTTP server binds to (metrics, /v1/models).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "", "[Deprecated] Use --http-bind-address instead.")
	flag.BoolVar(&standalone, "standalone", false, "Run in standalone mode without Kubernetes.")
	flag.StringVar(&endpointsConfig, "endpoints-config", "",
		"Path to endpoints config file (required in standalone mode).")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	// Resolve HTTP bind address: prefer --http-bind-address, fall back to deprecated --metrics-bind-address
	if httpAddr == "" {
		if metricsAddr != "" {
			klog.Warning("--metrics-bind-address is deprecated, use --http-bind-address instead")
			httpAddr = metricsAddr
		} else {
			httpAddr = ":8080"
		}
	}

	// Validate standalone mode flags
	if standalone && endpointsConfig == "" {
		klog.Fatal("--endpoints-config is required when running in standalone mode")
	}

	redisClient := utils.GetRedisClient()
	if redisClient == nil {
		if standalone {
			klog.Warning("Running without Redis: rate limiting and user auth disabled")
		} else {
			klog.Fatal("Redis is required in Kubernetes mode")
		}
	} else {
		defer func() {
			if err := redisClient.Close(); err != nil {
				klog.Warningf("Error closing Redis client: %v", err)
			}
		}()
	}

	// register additional routing algorithms that need dependences
	if redisClient != nil {
		routing.RegisterPowerOfTwoRouter(redisClient)
	}

	// stopCh is closed either on normal return (via defer) or proactively in
	// the signal handler before calling os.Exit. sync.Once guards against a
	// double-close panic.
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(func() { close(stopCh) }) }
	defer stopFn()

	var config *rest.Config
	var k8sClient kubernetes.Interface
	var gatewayK8sClient versioned.Interface
	var discoveryProvider discovery.Provider

	if standalone {
		// Standalone mode: use file-based discovery
		klog.Info("Running in standalone mode")
		discoveryProvider = discovery.NewStaticProvider(endpointsConfig)
	} else {
		// Kubernetes mode: load config and create clients
		var err error
		kubeConfig := flag.Lookup("kubeconfig").Value.String()
		if kubeConfig == "" {
			klog.Info("using in-cluster configuration")
			config, err = rest.InClusterConfig()
		} else {
			klog.Infof("using configuration from '%s'", kubeConfig)
			config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
		}
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}

		k8sClient, err = kubernetes.NewForConfig(config)
		if err != nil {
			klog.Fatalf("Error creating kubernetes client: %v", err)
		}

		gatewayK8sClient, err = versioned.NewForConfig(config)
		if err != nil {
			klog.Fatalf("Error on creating gateway k8s client: %v", err)
		}
	}

	// Initialize cache
	kvSyncEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)
	remoteTokenizerEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)

	cache.InitWithOptions(config, stopCh, cache.InitOptions{
		EnableKVSync:        kvSyncEnabled && remoteTokenizerEnabled,
		RedisClient:         redisClient,
		ModelRouterProvider: routing.ModelRouterFactory,
		DiscoveryProvider:   discoveryProvider,
	})

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}

	gatewayServer := gateway.NewServer(redisClient, k8sClient, gatewayK8sClient)

	if err := gatewayServer.StartHTTPServer(httpAddr); err != nil {
		klog.Fatalf("Failed to start HTTP server: %v", err)
	}
	klog.Infof("Started HTTP server on %s", httpAddr)

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

	klog.Infof("GOMAXPROCS is: %d", runtime.GOMAXPROCS(0))

	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-gracefulStop
		klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		gatewayServer.Shutdown()
		s.GracefulStop()
		// Close stopCh so that cache ticker goroutines and other consumers tied
		// to this signal exit promptly; os.Exit below bypasses deferred calls.
		stopFn()
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		panic(err)
	}
}
