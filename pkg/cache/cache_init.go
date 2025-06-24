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
)

var (
	store = &Store{} // Global cache store instance
	once  sync.Once  // Singleton pattern control lock
)

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
	store = &Store{
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

// Init initializes the cache store (singleton pattern)
// Parameters:
//
//	config: Kubernetes configuration
//	stopCh: Stop signal channel
//	redisClient: Redis client instance
//
// Returns:
//
//	*Store: Pointer to initialized store instance
func Init(config *rest.Config, stopCh <-chan struct{}) *Store {
	// Configure cache components
	enableGPUOptimizerTracing = false
	enableModelGPUProfileCaching = false
	return InitForGateway(config, stopCh, nil, nil)
}

func InitForMetadata(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Store {
	// Configure cache components
	enableGPUOptimizerTracing = false
	enableModelGPUProfileCaching = false
	return InitForGateway(config, stopCh, redisClient, nil)
}

func InitForGateway(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client, modelRouterProvider ModelRouterProviderFunc) *Store {
	once.Do(func() {
		klog.InfoS("initialize cache for gateway",
			"enableModelGPUProfileCaching", enableModelGPUProfileCaching,
			"enableGPUOptimizerTracing", enableGPUOptimizerTracing)
		store = New(redisClient, initPrometheusAPI(), modelRouterProvider)

		// Initialize cache components
		if err := initCacheInformers(store, config, stopCh); err != nil {
			panic(err)
		}
		initMetricsCache(store, stopCh)
		if store.enableProfileCaching {
			initProfileCache(store, stopCh, false)
		}
		if store.enableTracing {
			initTraceCache(redisClient, stopCh)
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
