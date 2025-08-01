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

package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

var (
	store = &Store{} // Global cache store instance
	once  sync.Once  // Singleton pattern control lock
)

// InitOptions configures the cache initialization behavior
type InitOptions struct {
	// EnableKVSync configures whether to start the ZMQ KV event sync
	EnableKVSync bool

	// RedisClient is required for KVSync and other features. Can be nil.
	RedisClient *redis.Client

	// ModelRouterProvider is needed only by the gateway. Can be nil.
	ModelRouterProvider ModelRouterProviderFunc
}

const (
	// For output predictor
	maxInputTokens  = 1024 * 1024       // 1M
	maxOutputTokens = 1024 * 1024       // 1M
	movingWindow    = 240 * time.Second // keep window same with window size of GPU optimizer.
)

// Store contains core data structures and components of the caching system
type Store struct {
	mu                  sync.RWMutex            // Read-write lock for concurrency safety
	initialized         bool                    // Initialization status flag
	redisClient         *redis.Client           // Redis client instance
	prometheusApi       prometheusv1.API        // Prometheus API client
	modelRouterProvider ModelRouterProviderFunc // Function to get model router

	// Metrics related fields
	subscribers         []metrics.MetricSubscriber // List of metric subscribers
	metrics             map[string]any             // Generic metric storage
	pendingLoadProvider CappedLoadProvider         // Provider that defines load in terms of pending requests.
	numRequestsTraces   int32                      // Request trace counter

	// Request trace fields
	enableTracing bool                                  // Default to load from enableGPUOptimizerTracing, can be configured.
	requestTrace  *utils.SyncMap[string, *RequestTrace] // Request trace data (model_name -> *RequestTrace)

	// Pod related storage
	metaPods utils.SyncMap[string, *Pod] // pod_namespace/pod_name -> *Pod

	// Model related storage
	metaModels utils.SyncMap[string, *Model] // model_name -> *Model

	// Deploymnent related storage
	enableProfileCaching bool                                    // Default to load from enableModelGPUProfileCaching, can be configured.
	deploymentProfiles   utils.SyncMap[string, *ModelGPUProfile] // aibrix:profile_[model_name]_[deployment_name] -> *ModelGPUProfile

	// buffer for sync map operations
	bufferPod   *Pod
	bufferModel *Model

	// podMetricsWorkerCount
	// Number of concurrent workers used to update Pod metrics
	podMetricsWorkerCount int

	// podMetricsJobs Channel for sending Pod metrics update jobs to workers
	podMetricsJobs chan *Pod

	// Sync prefix indexer - only created when KV sync is enabled
	syncPrefixIndexer *syncindexer.SyncPrefixHashTable

	// KV event management - optional enhancement
	kvEventManager *KVEventManager
}

// Get retrieves the cache instance
// Returns:
//
//	Cache: Cache interface instance
//	error: Returns error if cache is not initialized
func Get() (Cache, error) {
	if !store.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return store, nil
}

// New creates a new cache store instance
// Parameters:
//
//	redisClient: Redis client instance
//	prometheusApi: Prometheus API client
//
// Returns:
//
//	Store: Initialized cache store instance
func New(redisClient *redis.Client, prometheusApi prometheusv1.API, modelRouterProvider ModelRouterProviderFunc) *Store {

	store = &Store{
		initialized:           true,
		redisClient:           redisClient,
		prometheusApi:         prometheusApi,
		enableTracing:         enableGPUOptimizerTracing,
		requestTrace:          &utils.SyncMap[string, *RequestTrace]{},
		modelRouterProvider:   modelRouterProvider,
		podMetricsWorkerCount: defaultPodMetricsWorkerCount,
		podMetricsJobs:        make(chan *Pod, 100), // Initialize the job channel with a buffer size of 100
		enableProfileCaching:  enableModelGPUProfileCaching,
	}

	// Start podMetrics worker pool
	for w := 0; w < store.podMetricsWorkerCount; w++ {
		go store.worker(store.podMetricsJobs)
	}

	return store
}

// NewForTest initializes the cache store for testing purposes, it can be repeated call for reset.
func NewForTest() *Store {
	store := &Store{
		initialized:          true,
		enableTracing:        enableGPUOptimizerTracing,
		enableProfileCaching: enableModelGPUProfileCaching,
	}
	if store.enableTracing {
		store.requestTrace = &utils.SyncMap[string, *RequestTrace]{}
	}
	if store.enableProfileCaching {
		initProfileCache(store, nil, true)
	}
	return store
}

func NewWithPodsForTest(pods []*v1.Pod, model string) *Store {
	return InitWithPods(NewForTest(), pods, model)
}

func NewWithPodsMetricsForTest(pods []*v1.Pod, model string, podMetrics map[string]map[string]metrics.MetricValue) *Store {
	return InitWithPodsMetrics(InitWithPods(NewForTest(), pods, model), podMetrics)
}

// InitModelRouterProvider initializes the cache store with model router provider for testing purposes, it can be repeated call for reset.
// Call this function before InitWithPods for expected behavior.
func InitWithModelRouterProvider(st *Store, modelRouterProvider ModelRouterProviderFunc) *Store {
	st.modelRouterProvider = modelRouterProvider
	return st
}

// InitWithRequestTrace initializes the cache store with request trace.
func InitWithRequestTrace(st *Store) *Store {
	if !st.enableTracing {
		st.enableTracing = true
		st.requestTrace = &utils.SyncMap[string, *RequestTrace]{}
	}
	return st
}

// InitWithRequestTrace initializes the cache store with request trace.
func InitWithProfileCache(st *Store) *Store {
	if !st.enableProfileCaching {
		st.enableProfileCaching = true
		initProfileCache(st, nil, true)
	}
	return st
}

// InitWithPods initializes the cache store with pods for testing purposes, it can be repeated call for reset.
func InitWithPods(st *Store, pods []*v1.Pod, model string) *Store {
	for _, pod := range pods {
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[modelIdentifier] = model
		st.addPod(pod)
	}
	return st
}

// InitWithAsyncPods initializes the cache store with pods initialized in an async way, this simulate the timeline of how store initializes
func InitWithAsyncPods(st *Store, pods []*v1.Pod, model string) <-chan *Store {
	ret := make(chan *Store, 1)
	var wait sync.WaitGroup
	for _, pod := range pods {
		wait.Add(1)
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[modelIdentifier] = model
		go func() {
			st.addPod(pod)
			wait.Done()
		}()
	}
	go func() {
		wait.Wait()
		ret <- st
		close(ret)
	}()
	return ret
}

// InitWithPods initializes the cache store with pods metrics for testing purposes, it can be repeated call for reset.
func InitWithPodsMetrics(st *Store, podMetrics map[string]map[string]metrics.MetricValue) *Store {
	st.metaPods.Range(func(key string, metaPod *Pod) bool {
		_, podName, ok := utils.ParsePodKey(key)
		if !ok {
			return true
		}
		if podmetrics, ok := podMetrics[podName]; ok {
			for metricName, metric := range podmetrics {
				if err := st.updatePodRecord(metaPod, "", metricName, metrics.PodMetricScope, metric); err != nil {
					return false
				}
			}
		}
		return true
	})
	return st
}

// InitForTest initialize the global store object for testing.
func InitForTest() *Store {
	store = NewForTest()
	return store
}

// InitWithOptions initializes the cache store with configurable behavior
// Parameters:
//
//	config: Kubernetes configuration
//	stopCh: Stop signal channel
//	opts: Configuration options for initialization
//
// Returns:
//
//	*Store: Pointer to initialized store instance
func InitWithOptions(config *rest.Config, stopCh <-chan struct{}, opts InitOptions) *Store {
	once.Do(func() {
		// Log initialization based on configuration
		var service string
		if opts.EnableKVSync {
			service = "gateway"
		} else if opts.RedisClient != nil {
			service = "metadata"
		} else {
			service = "controllers"
		}

		klog.InfoS("initialize cache",
			"service", service,
			"enableKVSync", opts.EnableKVSync,
			"hasRedisClient", opts.RedisClient != nil,
			"hasModelRouterProvider", opts.ModelRouterProvider != nil,
			"enableModelGPUProfileCaching", enableModelGPUProfileCaching,
			"enableGPUOptimizerTracing", enableGPUOptimizerTracing)

		// Configure cache components based on service needs
		if service == "metadata" || service == "controllers" {
			enableGPUOptimizerTracing = false
			enableModelGPUProfileCaching = false
		}

		// Create store with provided dependencies
		store = New(opts.RedisClient, initPrometheusAPI(), opts.ModelRouterProvider)

		// Initialize cache components
		if err := initCacheInformers(store, config, stopCh); err != nil {
			panic(err)
		}
		initMetricsCache(store, stopCh)

		// Initialize profile cache if enabled
		if store.enableProfileCaching {
			initProfileCache(store, stopCh, false)
		}

		// Initialize trace cache if enabled and Redis is available
		if store.enableTracing && opts.RedisClient != nil {
			initTraceCache(opts.RedisClient, stopCh)
		}

		// Initialize KV event sync if enabled
		if opts.EnableKVSync {
			if opts.RedisClient == nil {
				klog.Fatalf("InitOptions: EnableKVSync is true but RedisClient is nil")
			}
			if err := store.initKVEventSync(); err != nil {
				klog.Errorf("Failed to initialize KV event sync: %v", err)
				// Continue without KV sync - this is not a fatal error
			}
		}
	})

	return store
}

// initMetricsCache initializes metrics cache update loop
// Parameters:
//
//	store: Cache store instance
//	stopCh: Stop signal channel
func initMetricsCache(store *Store, stopCh <-chan struct{}) {
	ticker := time.NewTicker(podMetricRefreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// Periodically update metrics
				store.updatePodMetrics()
				store.updateModelMetrics()
				if klog.V(5).Enabled() {
					store.debugInfo()
				}
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// initMetricsCache initializes metrics cache update loop
// Parameters:
//
//	store: Cache store instance
//	stopCh: Stop signal channel
func initProfileCache(store *Store, stopCh <-chan struct{}, forTesting bool) {
	store.pendingLoadProvider = newPendingLoadProvider(store)
	if forTesting {
		return
	}
	// Skip initialization below during testing
	ticker := time.NewTicker(defaultModelGPUProfileRefreshInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// Periodically update metrics
				store.updateDeploymentProfiles(context.Background())
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

// initTraceCache initializes request tracing cache
// Parameters:
//
//	redisClient: Redis client instance
//	stopCh: Stop signal channel
func initTraceCache(redisClient *redis.Client, stopCh <-chan struct{}) {
	// Calculate time offset for window alignment
	tickerOffset := time.Duration(time.Now().UnixNano()) % RequestTraceWriteInterval
	var traceAlignmentTimer *time.Timer
	var traceTicker *time.Ticker

	// Select alignment method based on offset
	if tickerOffset > MaxRequestTraceIntervalOffset {
		traceAlignmentTimer = time.NewTimer(RequestTraceWriteInterval - tickerOffset)
	} else {
		traceTicker = time.NewTicker(RequestTraceWriteInterval)
	}

	go func() {
		if redisClient == nil {
			return
		}
		if traceAlignmentTimer != nil {
			// Wait for time window alignment
			<-traceAlignmentTimer.C
			traceAlignmentTimer = nil
			traceTicker = time.NewTicker(RequestTraceWriteInterval)
		}
		klog.Infof("trace ticker start at %s", time.Now())
		for {
			select {
			case <-traceTicker.C:
				// Periodically write trace data to storage
				if atomic.LoadInt32(&store.numRequestsTraces) == 0 {
					continue
				}
				t := time.Now().Unix()
				roundT := t - t%int64(RequestTraceWriteInterval/time.Second)
				store.writeRequestTraceToStorage(roundT)
			case <-stopCh:
				traceTicker.Stop()
				return
			}
		}
	}()
}

// initKVEventSync initializes the KV event synchronization system
func (s *Store) initKVEventSync() error {
	klog.Info("Initializing KV event synchronization")

	// Check if KV sync should be enabled
	kvSyncValue := utils.LoadEnv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false")
	kvSyncEnabled, _ := strconv.ParseBool(kvSyncValue)
	remoteTokenValue := utils.LoadEnv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
	remoteTokenizerEnabled, _ := strconv.ParseBool(remoteTokenValue)

	// Early return if not enabled
	if !kvSyncEnabled {
		klog.Info("KV event sync is disabled")
		return nil
	}

	if !remoteTokenizerEnabled {
		klog.Warning("KV sync requires remote tokenizer, feature disabled")
		return nil
	}

	// Track initialization state for cleanup
	var initialized bool
	defer func() {
		if !initialized {
			s.cleanupKVEventSync()
		}
	}()

	// Create and validate event manager first
	s.kvEventManager = NewKVEventManager(s)
	if s.kvEventManager == nil {
		return fmt.Errorf("failed to create KV event manager")
	}

	// Validate configuration before allocating more resources
	if err := s.kvEventManager.validateConfiguration(); err != nil {
		return fmt.Errorf("invalid KV event sync configuration: %w", err)
	}

	// Create sync indexer after validation passes
	s.syncPrefixIndexer = syncindexer.NewSyncPrefixHashTable()
	if s.syncPrefixIndexer == nil {
		return fmt.Errorf("failed to create sync prefix indexer")
	}

	// Start event manager
	if err := s.kvEventManager.Start(); err != nil {
		return fmt.Errorf("failed to start KV event sync: %w", err)
	}

	// Mark as successfully initialized
	initialized = true
	klog.Info("KV event synchronization initialized successfully")

	return nil
}

// cleanupKVEventSync cleans up partially initialized KV event sync resources
func (s *Store) cleanupKVEventSync() {
	klog.Info("Cleaning up KV event sync resources")

	// Stop event manager if it exists
	if s.kvEventManager != nil {
		s.kvEventManager.Stop()
		s.kvEventManager = nil
	}

	// Clear sync indexer
	if s.syncPrefixIndexer != nil {
		s.syncPrefixIndexer.Close()
		s.syncPrefixIndexer = nil
	}
}

// GetSyncPrefixIndexer returns the sync prefix hash indexer
func (s *Store) GetSyncPrefixIndexer() *syncindexer.SyncPrefixHashTable {
	// Return sync indexer only if KV sync is enabled
	// Router will fall back to original indexer if this returns nil
	return s.syncPrefixIndexer
}

// Close gracefully shuts down the cache store
func (s *Store) Close() {
	klog.Info("Closing cache store")

	// Clean up KV event sync resources
	s.cleanupKVEventSync()

	// Other cleanup can be added here in the future
}
