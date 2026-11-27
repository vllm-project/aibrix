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
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof" // registers the profiling handlers that startPprofServer serves
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/filters"
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
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/drain"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/statesync"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"google.golang.org/grpc/health"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	defaultGRPCMaxMessageSizeBytes = 4 * 1024 * 1024
	envGRPCMaxMessageSizeBytes     = "AIBRIX_GRPC_MAX_MESSAGE_SIZE_BYTES"
	envDisableRateLimiting         = "AIBRIX_DISABLE_RATE_LIMITING"
	envPriorityTierEnabled         = "AIBRIX_PRIORITY_TIER_ENABLED"
)

var (
	grpcAddr        string
	httpAddr        string
	metricsAddr     string // deprecated: use httpAddr
	standalone      bool
	checkRedis      bool
	endpointsConfig string
	pprofAddr       string
)

type kubeAPIOptions struct {
	qps   float64
	burst int
}

type modelDiscoveryOptions struct {
	watchModelAdapters bool
	watchModelClaims   bool
	listMode           string
	maxAge             time.Duration
}

func (o *modelDiscoveryOptions) addFlags(fs *flag.FlagSet) {
	fs.BoolVar(&o.watchModelAdapters, "watch-model-adapters", true,
		"List and watch ModelAdapter resources in Kubernetes discovery.")
	fs.BoolVar(&o.watchModelClaims, "watch-model-claims", true,
		"List and watch ModelClaim resources and use their Pod bindings in Kubernetes discovery.")
	fs.StringVar(&o.listMode, "model-list-mode", string(gateway.ModelListKnown),
		"Models returned by /v1/models: known or ready-pods.")
	fs.DurationVar(&o.maxAge, "model-list-discovery-max-age", 0,
		"Maximum age of a verified Kubernetes model-list snapshot; 0 disables verification.")
}

func (o modelDiscoveryOptions) validate() error {
	if o.listMode != string(gateway.ModelListKnown) && o.listMode != string(gateway.ModelListReadyPods) {
		return fmt.Errorf("invalid --model-list-mode %q: expected known or ready-pods", o.listMode)
	}
	if o.maxAge < 0 {
		return fmt.Errorf("--model-list-discovery-max-age must be nonnegative")
	}
	return nil
}

type discoveryCheckOptions struct {
	interval             time.Duration
	unreachableThreshold time.Duration
}

func (o *discoveryCheckOptions) addFlags(fs *flag.FlagSet) {
	fs.DurationVar(&o.interval, "discovery-check-interval", 10*time.Second,
		"Interval of the check that this replica can still read Pods from the Kubernetes API server; "+
			"each check is bounded by half of it. 0 disables the check; otherwise at least 1s. Ignored in standalone mode.")
	fs.DurationVar(&o.unreachableThreshold, "discovery-unreachable-threshold", 30*time.Second,
		"Age of the last successful reachability check at which aibrix_gateway_discovery_unreachable becomes 1. "+
			"Keep it above normal watch reconnect delays; it must be at least twice --discovery-check-interval.")
}

func (o discoveryCheckOptions) validate() error {
	if o.interval < 0 {
		return fmt.Errorf("--discovery-check-interval must be nonnegative, got %v", o.interval)
	}
	// Each check gets half the interval; below a second that leaves too little
	// for a LIST through a loaded API server, and adds load for no benefit.
	if o.interval > 0 && o.interval < time.Second {
		return fmt.Errorf("--discovery-check-interval must be 0 or at least 1s, got %v", o.interval)
	}
	// A healthy check may run for half an interval, so a scrape during it can
	// see an age of up to 1.5 intervals; twice the interval leaves a margin.
	if o.interval > 0 && o.unreachableThreshold < 2*o.interval {
		return fmt.Errorf("--discovery-unreachable-threshold (%v) must be at least twice --discovery-check-interval (%v)",
			o.unreachableThreshold, o.interval)
	}
	return nil
}

func (o modelDiscoveryOptions) kubernetesProvider(config *rest.Config) *discovery.KubernetesProvider {
	provider := discovery.NewKubernetesProvider(config).WithModelAdapters(o.watchModelAdapters)
	if o.watchModelClaims {
		provider = provider.WithModelClaims()
	}
	return provider
}

func startEarlyModelListGate(enabled bool, addr string) (*gateway.ModelListGate, error) {
	if !enabled {
		return nil, nil
	}
	gate := &gateway.ModelListGate{}
	if err := gate.StartHTTPServer(addr); err != nil {
		return nil, err
	}
	return gate, nil
}

func startGatewayHTTPServer(gate *gateway.ModelListGate, server *gateway.Server, addr string) error {
	// A Pod that has never handled an RPC must still expose a valid idle sample
	// so the preStop hook can finish without waiting for its entire budget.
	metrics.SetGaugeMetric(metrics.GatewayInFlight, metrics.GetMetricHelp(metrics.GatewayInFlight),
		0, []string{"gateway_pod"}, metrics.GatewayPodName())
	if gate != nil {
		gate.SetServer(server)
		return nil
	}
	return server.StartHTTPServer(addr)
}

func (o *kubeAPIOptions) addFlags(fs *flag.FlagSet) {
	fs.Float64Var(
		&o.qps,
		"kube-api-qps",
		float64(rest.DefaultQPS),
		"Maximum QPS for the core Kubernetes and Gateway API clients; must be greater than zero.",
	)
	fs.IntVar(
		&o.burst,
		"kube-api-burst",
		rest.DefaultBurst,
		"Maximum burst for the core Kubernetes and Gateway API clients; must be greater than zero.",
	)
}

func (o kubeAPIOptions) validate() error {
	// Validate the effective float32 value used by rest.Config as well, so that
	// conversion cannot turn a positive QPS into zero or infinity.
	qps := float32(o.qps)
	if !(qps > 0) || math.IsInf(float64(qps), 0) {
		return fmt.Errorf("--kube-api-qps must be finite and greater than zero as a float32, got %v", o.qps)
	}
	if o.burst <= 0 {
		return fmt.Errorf("--kube-api-burst must be greater than zero, got %d", o.burst)
	}
	return nil
}

func (o kubeAPIOptions) applyTo(config *rest.Config) {
	config.QPS = float32(o.qps)
	config.Burst = o.burst
}

func checkRedisConnectivity(ctx context.Context, retryInterval time.Duration, check func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := check(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		klog.Infof("Waiting for Redis: %v", err)
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func runRedisConnectivityCheck(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("--check-redis-timeout must be nonnegative")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return checkRedisConnectivity(ctx, 2*time.Second, utils.CheckRedisConnectivity)
}

func checkRedisConnectivityOrExit(timeout time.Duration) {
	if err := runRedisConnectivityCheck(timeout); err != nil {
		klog.Exitf("Redis connectivity check stopped: %v", err)
	}
}

// firstError returns the first non-nil error, so flag groups are validated in
// the order they are listed.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "prestop" {
		os.Exit(drain.Run(os.Args[2:]))
	}
	runGateway()
}

func runGateway() {
	var gracefulShutdownTimeout time.Duration
	var kubeAPI kubeAPIOptions
	kubeAPI.addFlags(flag.CommandLine)
	var modelDiscovery modelDiscoveryOptions
	var checkRedisTimeout time.Duration
	modelDiscovery.addFlags(flag.CommandLine)
	var discoveryCheck discoveryCheckOptions
	discoveryCheck.addFlags(flag.CommandLine)
	flag.StringVar(&grpcAddr, "grpc-bind-address", ":50052", "The address the gRPC server binds to.")
	flag.StringVar(&httpAddr, "http-bind-address", "", "The address the HTTP server binds to (metrics, /v1/models).")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "", "[Deprecated] Use --http-bind-address instead.")
	flag.BoolVar(&standalone, "standalone", false, "Run in standalone mode without Kubernetes.")
	flag.StringVar(&pprofAddr, "pprof-bind-address", "localhost:6060",
		"The address the pprof debug server binds to. Empty disables it.")
	flag.BoolVar(&checkRedis, "check-redis", false, "Check Redis connectivity and exit without starting the gateway.")
	flag.DurationVar(&checkRedisTimeout, "check-redis-timeout", 0,
		"Maximum wait for Redis with --check-redis; 0 waits indefinitely.")
	flag.StringVar(&endpointsConfig, "endpoints-config", "",
		"Path to endpoints config file (required in standalone mode).")
	flag.DurationVar(&gracefulShutdownTimeout, "graceful-shutdown-timeout", 20*time.Second,
		"Maximum wait for existing gRPC requests after SIGTERM, before forcing them to stop.")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()
	if gracefulShutdownTimeout <= 0 {
		klog.Fatal("--graceful-shutdown-timeout must be positive")
	}
	if err := firstError(kubeAPI.validate(), modelDiscovery.validate(), discoveryCheck.validate()); err != nil {
		klog.Fatal(err)
	}

	if checkRedis {
		checkRedisConnectivityOrExit(checkRedisTimeout)
		return
	}

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

	redisClients := utils.GetRedisClients()
	redisClient := redisClients.Shared()
	if redisClient == nil {
		if standalone {
			klog.Warning("Running without Redis: rate limiting and user auth disabled")
		} else {
			klog.Fatal("Redis is required in Kubernetes mode")
		}
	} else {
		defer func() {
			if err := redisClients.Close(); err != nil {
				klog.Warningf("Error closing Redis client: %v", err)
			}
		}()
	}

	// Keep cache workers alive until requests drain. sync.Once guards shutdown
	// cleanup and the normal-return defer against a double-close panic.
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

		kubeAPI.applyTo(config)

		k8sClient, err = kubernetes.NewForConfig(config)
		if err != nil {
			klog.Fatalf("Error creating kubernetes client: %v", err)
		}

		gatewayK8sClient, err = versioned.NewForConfig(config)
		if err != nil {
			klog.Fatalf("Error on creating gateway k8s client: %v", err)
		}
		discoveryProvider = modelDiscovery.kubernetesProvider(config)
	}
	modelListGate, err := startEarlyModelListGate(modelDiscovery.maxAge > 0, httpAddr)
	if err != nil {
		klog.Fatalf("Failed to start model-list HTTP server: %v", err)
	}
	defer modelListGate.Shutdown()

	// Initialize cache
	kvSyncEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)
	remoteTokenizerEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)

	cache.InitWithOptions(config, stopCh, cache.InitOptions{
		IsGateway:                     true,
		EnableKVSync:                  kvSyncEnabled && remoteTokenizerEnabled,
		RedisClients:                  redisClients,
		ModelRouterProvider:           routing.ModelRouterFactory,
		DiscoveryProvider:             discoveryProvider,
		DisableModelClaimPodBindings:  !standalone && !modelDiscovery.watchModelClaims,
		ModelListMaxAge:               modelDiscovery.maxAge,
		DiscoveryCheckInterval:        discoveryCheck.interval,
		DiscoveryUnreachableThreshold: discoveryCheck.unreachableThreshold,
	})

	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}

	gatewayServer := gateway.NewServerWithRedisClients(redisClients, k8sClient, gatewayK8sClient, gateway.ServerOptions{
		DisableRateLimiting: utils.LoadEnvBool(envDisableRateLimiting, false),
		PriorityTier:        utils.LoadEnvBool(envPriorityTierEnabled, false),
		ModelListMode:       gateway.ModelListMode(modelDiscovery.listMode),
		VerifyModelList:     modelDiscovery.maxAge > 0,
	})

	stateSyncEnabled := utils.LoadEnvBool("AIBRIX_STATESYNC_ENABLED", false)
	var syncManager *statesync.RedisSync
	if stateSyncEnabled {
		klog.InfoS("statesync enabled; starting cross-replica state sync")
		table := prefixcacheindexer.GetSharedPrefixHashTable()
		table.EnableDeltaSync()
		syncManager = statesync.New(redisClient)
		syncManager.Register(prefixcacheindexer.NewPrefixHashTableSyncable(table))
		syncManager.Start()
	} else {
		klog.InfoS("statesync disabled; set AIBRIX_STATESYNC_ENABLED=true to enable cross-replica state sync")
	}

	if err := startGatewayHTTPServer(modelListGate, gatewayServer, httpAddr); err != nil {
		klog.Fatalf("Failed to start HTTP server: %v", err)
	}
	klog.Infof("Started HTTP server on %s", httpAddr)

	var opts []grpc.ServerOption

	otelEnabled := utils.OTELEnabled()
	var telApp *utils.Telemetry
	if otelEnabled {
		otlpProtocol := utils.LoadEnv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		if otlpProtocol == "http/protobuf" {
			otlpProtocol = "http"
		}
		telApp, err = utils.InitOpenTelemetry("aibrix-gateway-plugin", otlpProtocol)
		if err != nil || telApp == nil {
			klog.Fatalf("Failed to initialize OpenTelemetry: %v", err)
		}
		opts = append(opts, grpc.StatsHandler(
			otelgrpc.NewServerHandler(
				otelgrpc.WithFilter(
					filters.Not(filters.HealthCheck()),
				),
			),
		))
	}

	grpcMaxMessageSize := utils.LoadEnvInt(envGRPCMaxMessageSizeBytes, defaultGRPCMaxMessageSizeBytes)
	opts = append(opts, grpc.MaxRecvMsgSize(grpcMaxMessageSize))

	// One panicking request must not take the process down with the requests in
	// flight on it, so recover in the stream interceptor and fail only that stream.
	opts = append(opts, grpc.StreamInterceptor(gateway.StreamPanicRecoveryInterceptor()))

	s := grpc.NewServer(opts...)

	extProcPb.RegisterExternalProcessorServer(s, gatewayServer)

	healthCheck := health.NewServer()
	healthPb.RegisterHealthServer(s, healthCheck)
	healthCheck.SetServingStatus("gateway-plugin", healthPb.HealthCheckResponse_SERVING)

	klog.Info("starting gRPC server on " + grpcAddr)

	startPprofServer(pprofAddr)

	klog.Infof("GOMAXPROCS is: %d", runtime.GOMAXPROCS(0))

	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(gracefulStop)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sig := <-gracefulStop
		klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		shutdownGateway(s, gatewayServer, healthCheck, gracefulShutdownTimeout)
		if syncManager != nil {
			syncManager.Stop()
		}
		modelListGate.Shutdown()
		if otelEnabled {
			telApp.Shutdown()
		}
		stopFn()
	}()

	if err := s.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		panic(err)
	}
	// Serve can return before the signal handler finishes its cleanup. Keep
	// Redis and cache workers available until that cleanup has completed.
	<-shutdownDone
}

// startPprofServer serves the profiling endpoints (http.DefaultServeMux, where
// net/http/pprof registers them) on addr in the background and returns the
// listener, or nil when addr is empty or cannot be bound. A bind failure is
// logged, not fatal: profiling is a debugging aid, and a port that another
// process holds, such as a second gateway on the same host in local mode, must
// not stop the gateway from starting.
func startPprofServer(addr string) net.Listener {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		klog.ErrorS(err, "pprof server disabled: cannot bind its address", "address", addr)
		return nil
	}
	klog.InfoS("pprof server listening", "address", ln.Addr().String())
	go func() {
		if err := http.Serve(ln, nil); err != nil && !errors.Is(err, net.ErrClosed) {
			klog.ErrorS(err, "pprof server stopped")
		}
	}()
	return ln
}
