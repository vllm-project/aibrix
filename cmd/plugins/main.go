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
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	healthserver "github.com/vllm-project/aibrix/pkg/plugins/gateway/health"
	"github.com/vllm-project/aibrix/pkg/utils"
)

var (
	grpcAddr                string
	metricsAddr             string
	profilingAddr           string
	enableLeaderElection    bool
	leaderElectionID        string
	leaderElectionNamespace string
)

func main() {
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":50052", "The address the gRPC server binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&profilingAddr, "profiling-bind-address", ":6061", "The address the profiling endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false, "Enable leader election for high availability")
	flag.StringVar(&leaderElectionID,
		"leader-election-id", "gateway-plugin-lock", "Name of the lease resource for leader election")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace",
		"aibrix-system", "Namespace for leader election lease (default: same as pod)")

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
	kvSyncEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)
	remoteTokenizerEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)

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

	isLeader := &atomic.Bool{}
	isLeader.Store(false)

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	defer leaderCancel()

	if enableLeaderElection {
		klog.Info("Leader election enabled")

		// Get pod info for lease
		podName := os.Getenv("POD_NAME")
		if podName == "" {
			podName = string(uuid.NewUUID())
		}
		if leaderElectionNamespace == "" {
			podNamespace := os.Getenv("POD_NAMESPACE")
			if podNamespace == "" {
				// Read from file (in-cluster mode)
				nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
				if err != nil {
					klog.Fatalf("Failed to read namespace from file: %v", err)
				}
				podNamespace = string(nsBytes)
			}
			leaderElectionNamespace = podNamespace
		}

		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      leaderElectionID,
				Namespace: leaderElectionNamespace,
			},
			Client: k8sClient.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: podName,
			},
		}

		leConfig := leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					klog.Info("This instance is now the leader!")
					isLeader.Store(true)
				},
				OnStoppedLeading: func() {
					klog.Info("This instance is no longer the leader, initiating graceful shutdown...")
					// Cancel the leader context to stop leader-specific operations
					leaderCancel()
					// Exit the process to let Kubernetes restart it
					os.Exit(0)
				},
				OnNewLeader: func(identity string) {
					if identity == podName {
						klog.Info("Still the leader")
					} else {
						klog.Infof("New leader elected: %s", identity)
					}
				},
			},
			ReleaseOnCancel: true,
		}

		leaderElector, err := leaderelection.NewLeaderElector(leConfig)
		if err != nil {
			klog.Fatalf("Failed to create leader elector: %v", err)
		}

		go func() {
			leaderElector.Run(leaderCtx)
		}()
	} else {
		// Single instance mode, all instances are leaders
		isLeader.Store(true)
		klog.Info("Single instance mode enabled, this instance is always the leader")
	}

	// Setup gRPC server with custom health server
	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, gatewayServer)

	newHealthServer := healthserver.NewHealthServer(isLeader, enableLeaderElection)
	healthpb.RegisterHealthServer(s, newHealthServer)

	klog.Info("starting gRPC server on " + grpcAddr)

	profilingServer := &http.Server{
		Addr: profilingAddr,
	}
	go func() {
		if err := profilingServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("failed to setup profiling on %s: %v", profilingAddr, err)
		}
	}()

	// Create graceful shutdown function
	gracefulShutdown := func() {
		klog.Info("Initiating graceful shutdown...")

		s.GracefulStop()
		klog.Info("gRPC server stopped")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := profilingServer.Shutdown(ctx); err != nil {
			klog.Errorf("Error shutting down profiling server: %v", err)
		}
		klog.Info("Profiling server stopped")

		gatewayServer.Shutdown()
		klog.Info("Gateway server stopped")

		if err := redisClient.Close(); err != nil {
			klog.Warningf("Error closing Redis client during shutdown: %v", err)
		}
		klog.Info("Redis client closed")

		leaderCancel()
		klog.Info("Leader context cancelled")
		klog.Info("Graceful shutdown completed")
	}

	go func() {
		if err := s.Serve(lis); err != nil {
			klog.Errorf("gRPC server error: %v", err)
		}
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	if enableLeaderElection {
		// In leader election mode: wait for either signal or losing leadership
		select {
		case sig := <-signalCh:
			klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		case <-leaderCtx.Done():
			klog.Info("Leader context cancelled (lost leadership), initiating shutdown...")
		}
		gracefulShutdown()
		os.Exit(0)
	} else {
		// In single instance mode: wait for shutdown signal
		sig := <-signalCh
		klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		gracefulShutdown()
		os.Exit(0)
	}
}
