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
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const (
	// When the engine's HTTP proxy is separated from the engine itself,
	// the request port and metrics port may differ, so a dedicated metrics port is required.
	MetricPortLabel                     = constants.ModelLabelMetricPort
	engineLabel                         = constants.ModelLabelEngine
	defaultMetricPort                   = 8000
	defaultEngineLabelValue             = "vllm"
	defaultPodMetricRefreshIntervalInMS = 1000
	defaultPodMetricsWorkerCount        = 10
	defaultPodMetricsQueuePerWorker     = 10
	defaultPodMetricsFetchTimeoutInMS   = 5000
	defaultPromQueryIntervalInMS        = 200
	defaultPromQueryTimeoutInMS         = 2000
	undefinedMetricLabelValue           = "undefined"
	podMetricsBackoffBaseDelay          = time.Second
	podMetricsBackoffMaxDelay           = 30 * time.Second
)

var (
	counterGaugeMetricNames = []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.EngineSleepState,
		metrics.HTTPRequestTotal,
		metrics.NumPrefillPreallocQueueReqs,
		metrics.NumDecodePreallocQueueReqs,

		metrics.KVCacheUsagePerc,
		metrics.NixlNumFailedTransfers,
		metrics.NixlNumFailedNotifications,
		metrics.PrefixCacheQueriesTotal,
		metrics.PrefixCacheHitTotal,
		metrics.ExternalPrefixCacheHitsTotal,
		metrics.ExternalPrefixCacheQueriesTotal,

		metrics.NumRequestsSwapped,
		metrics.PromptTokenTotal,
		metrics.GenerationTokenTotal,
		metrics.AvgPromptThroughputToksPerS,
		metrics.AvgGenerationThroughputToksPerS,
		metrics.GPUCacheUsagePerc,
		metrics.CPUCacheUsagePerc,
		metrics.EngineUtilization,
		metrics.KVCacheCapacityTokens,
	}

	// histogram metric example - time_to_first_token_seconds, _sum, _bucket _count.
	histogramMetricNames = []string{
		metrics.IterationTokensTotal,
		metrics.TimeToFirstTokenSeconds,
		metrics.TimePerOutputTokenSeconds,
		metrics.E2ERequestLatencySeconds,
		metrics.RequestQueueTimeSeconds,
		metrics.RequestInferenceTimeSeconds,
		metrics.RequestDecodeTimeSeconds,
		metrics.RequestPrefillTimeSeconds,
		metrics.HTTPRequestDurationSeconds,
		metrics.PerStageReqLatencySeconds,
		metrics.HTTPRequestDurationHighRSeconds,
		metrics.RequestPromptTokens,
		metrics.RequestGenerationTokens,

		metrics.NixlXferTimeSeconds,
		metrics.NixlPostTimeSeconds,
		metrics.NixlBytesTransferred,
		metrics.NixlNumDescriptors,
	}

	prometheusMetricNames = []string{
		metrics.DrainRate1m,
		metrics.P95TTFT5m,
		metrics.P95TTFT5mPod,
		metrics.AvgTTFT5mPod,
		metrics.P95TPOT5mPod,
		metrics.AvgTPOT5mPod,
		metrics.AvgPromptToksPerReq,
		metrics.AvgGenerationToksPerReq,
		metrics.AvgE2ELatencyPod,
		metrics.AvgRequestsPerMinPod,
		metrics.AvgPromptThroughputToksPerMinPod,
		metrics.AvgGenerationThroughputToksPerMinPod,
	}

	labelQueryMetricNames = []string{
		metrics.MaxLora,
		metrics.KVCacheNumGPUBlocks,
		metrics.KVCacheBlockSize,
		metrics.WaitingLoraAdapters,
		metrics.RunningLoraAdapters,
	}
	podMetricRefreshInterval = loadPodMetricRefreshInterval()
	podMetricsFetchTimeout   = loadPodMetricsFetchTimeout()
	promQueryInterval        = time.Duration(utils.LoadEnvInt("AIBRIX_PROMETHEUS_QUERY_INTERVAL_MS", defaultPromQueryIntervalInMS)) * time.Millisecond
	promQueryTimeout         = time.Duration(utils.LoadEnvInt("AIBRIX_PROMETHEUS_QUERY_TIMEOUT_MS", defaultPromQueryTimeoutInMS)) * time.Millisecond
)

type podMetricsBackoffState struct {
	mu          sync.Mutex
	uid         string
	failures    int
	nextFetchAt time.Time
}

type podMetricsSchedulingState struct {
	mu     sync.Mutex
	uid    string
	status podMetricsSchedulingStatus
}

type podMetricsSchedulingStatus string

const (
	podMetricsQueued   podMetricsSchedulingStatus = "queued"
	podMetricsInFlight podMetricsSchedulingStatus = "in_flight"
)

func loadPodMetricRefreshInterval() time.Duration {
	return time.Duration(utils.LoadEnvInt("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", defaultPodMetricRefreshIntervalInMS)) * time.Millisecond
}

func loadPodMetricsWorkerCount() int {
	workerCount := utils.LoadEnvInt("AIBRIX_POD_METRICS_WORKER_COUNT", defaultPodMetricsWorkerCount)
	if workerCount <= 0 {
		return defaultPodMetricsWorkerCount
	}
	return workerCount
}

func loadPodMetricsJobQueueSize(workerCount int) int {
	defaultQueueSize := workerCount * defaultPodMetricsQueuePerWorker
	queueSize := utils.LoadEnvInt("AIBRIX_POD_METRICS_JOB_QUEUE_SIZE", defaultQueueSize)
	if queueSize <= 0 {
		return defaultQueueSize
	}
	return queueSize
}

func loadPodMetricsFetchTimeout() time.Duration {
	timeout := utils.LoadEnvInt("AIBRIX_POD_METRICS_FETCH_TIMEOUT_MS", defaultPodMetricsFetchTimeoutInMS)
	if timeout <= 0 {
		timeout = defaultPodMetricsFetchTimeoutInMS
	}
	return time.Duration(timeout) * time.Millisecond
}

func podMetricsBackoffKey(pod *Pod) string {
	return utils.GeneratePodKey(pod.Namespace, pod.Name)
}

func podMetricsBackoffDelay(failures int) time.Duration {
	if failures <= 0 {
		return podMetricsBackoffBaseDelay
	}
	if failures >= 6 {
		return podMetricsBackoffMaxDelay
	}
	delay := podMetricsBackoffBaseDelay
	for i := 1; i < failures; i++ {
		delay *= 2
	}
	return delay
}

// MetricSnapshot represents a metric value at a specific timestamp
type MetricSnapshot struct {
	Value float64
	// Count is the observation count of a histogram whose Sum is Value (see
	// calculateMean1m); it stays 0 for counters.
	Count     float64
	Timestamp time.Time
}

// RateCalculator manages historical metric values for rate calculation
type RateCalculator struct {
	mu      sync.RWMutex
	history map[string][]MetricSnapshot // key: "namespace/podName/modelName/metricName"
	// outputRate holds each pod's completion-counter history, keyed by utils.GeneratePodKey. One
	// entry per pod, so deleting a pod's entry does not scan the map.
	outputRate map[string][]outputRateSnapshot
	maxAge     time.Duration // Maximum age to keep snapshots
	maxCount   int           // Maximum number of snapshots to keep
}

// rateHistoryPodPrefix returns the prefix of every rate history key of a pod. The history
// is process-wide, so the key carries the namespace: same-named pods in different
// namespaces (e.g. StatefulSet or LeaderWorkerSet replicas) must not share a series.
func rateHistoryPodPrefix(namespace, podName string) string {
	return utils.GeneratePodKey(namespace, podName) + "/"
}

// rateHistoryKey is the history key of a pod's metric series. calculateRate1m passes an
// empty modelName, since the counters it reads are pod-level.
func rateHistoryKey(pod *Pod, modelName, metricName string) string {
	return rateHistoryPodPrefix(pod.Namespace, pod.Name) + modelName + "/" + metricName
}

// PurgeEntriesForPod removes all history entries of the pod namespace/podName.
// Call this when a pod is deleted to prevent unbounded map growth in high-churn clusters.
func (r *RateCalculator) PurgeEntriesForPod(namespace, podName string) {
	prefix := rateHistoryPodPrefix(namespace, podName)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.history {
		if strings.HasPrefix(k, prefix) {
			delete(r.history, k)
		}
	}
	delete(r.outputRate, utils.GeneratePodKey(namespace, podName))
}

var (
	// Global rate calculator instance
	rateCalculator = &RateCalculator{
		history:  make(map[string][]MetricSnapshot),
		maxAge:   5 * time.Minute, // Keep 5 minutes of history
		maxCount: 20,              // Keep max 20 snapshots per metric
	}

	prometheusBasicAuthOnce sync.Once
	prometheusBasicAuthUser string
	prometheusBasicAuthPass string
)

func initPrometheusAPI(kubeConfig *rest.Config) prometheusv1.API {
	prometheusEndpoint := utils.LoadEnv("PROMETHEUS_ENDPOINT", "")
	loadPrometheusBasicAuth(kubeConfig)

	// Initialize Prometheus API
	var prometheusApi prometheusv1.API
	if prometheusEndpoint != "" {
		api, err := metrics.InitializePrometheusAPI(prometheusEndpoint, prometheusBasicAuthUser, prometheusBasicAuthPass)
		if err != nil {
			klog.Errorf("Error initializing Prometheus API: %v", err)
		} else {
			prometheusApi = api
			klog.Infof("Prometheus API initialized successfully")
		}
	}
	return prometheusApi
}

// loadPrometheusBasicAuth initializes Prometheus basic auth credentials exactly once (via sync.Once).
// It loads from a Kubernetes Secret when PROMETHEUS_BASIC_AUTH_SECRET_NAME is set; otherwise it falls back to env vars.
// The resulting values are stored in package-level variables prometheusBasicAuthUser/prometheusBasicAuthPass.
func loadPrometheusBasicAuth(kubeConfig *rest.Config) {
	prometheusBasicAuthOnce.Do(func() {
		secretName := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_SECRET_NAME", "")
		if secretName == "" {
			prometheusBasicAuthUser = utils.LoadEnv("PROMETHEUS_BASIC_AUTH_USERNAME", "")
			prometheusBasicAuthPass = utils.LoadEnv("PROMETHEUS_BASIC_AUTH_PASSWORD", "")
			return
		}

		secretNamespace := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_SECRET_NAMESPACE", utils.NAMESPACE)
		// Default to "username" and "password" if keys are not specified
		usernameKey := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_USERNAME_KEY", "username")
		passwordKey := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_PASSWORD_KEY", "password")

		if kubeConfig == nil {
			klog.Warningf("Prometheus basic auth secret %s/%s is not loaded due to nil kubeConfig", secretNamespace, secretName)
			return
		}
		clientset, err := kubernetes.NewForConfig(kubeConfig)
		if err != nil {
			klog.ErrorS(err, "Failed to create Kubernetes client for Prometheus basic auth secret")
			return
		}

		secret, err := clientset.CoreV1().Secrets(secretNamespace).Get(context.Background(), secretName, metav1.GetOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to read Prometheus basic auth secret", "namespace", secretNamespace, "name", secretName)
			return
		}
		if b, ok := secret.Data[usernameKey]; ok {
			prometheusBasicAuthUser = strings.TrimSpace(string(b))
		}
		if b, ok := secret.Data[passwordKey]; ok {
			prometheusBasicAuthPass = strings.TrimSpace(string(b))
		}
	})
}

func (c *Store) getPodMetricImpl(podName string, metricStore *utils.SyncMap[string, MetricRecord], metricName string) (MetricRecord, error) {
	record, ok := metricStore.Load(metricName)
	if !ok {
		return MetricRecord{}, &MetricNotFoundError{CacheError: ErrorTypeMetricNotFound, PodName: podName, MetricName: metricName}
	}
	return record, nil
}

func (c *Store) getPodModelMetricName(modelName string, metricName string) string {
	// Concatenation, not fmt.Sprintf: this runs for every pod a routing strategy scores.
	return modelName + "/" + metricName
}

func (c *Store) updatePodMetrics() {
	now := time.Now()
	c.metaPods.Range(func(key string, metaPod *Pod) bool {
		if !utils.FilterReadyPod(metaPod.Pod) {
			// Skip unready pod
			return true
		}
		if c.isPodQuarantined(metaPod.Pod) {
			klog.V(4).InfoS("Pod is quarantined, skipping pod metrics update",
				"pod", metaPod.Name, "namespace", metaPod.Namespace)
			return true
		}
		if metaPod.claimMetrics != nil {
			if len(metaPod.claimMetrics) != 0 {
				c.updateGatewayTrackedPodMetrics(metaPod)
			}
			for _, target := range metaPod.claimMetrics {
				c.enqueueModelClaimMetrics(metaPod, target, now)
			}
			return true
		}
		if c.shouldSkipPodMetricsFetch(metaPod, now) {
			metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonBackoff)
			klog.V(4).InfoS("Pod metrics fetch is in backoff, skipping pod metrics update",
				"pod", metaPod.Name, "namespace", metaPod.Namespace)
			return true
		}
		if !c.tryMarkPodMetricsQueued(metaPod) {
			metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonAlreadyQueued)
			klog.V(4).InfoS("Pod metrics fetch is already queued or in flight, skipping duplicate pod metrics update",
				"pod", metaPod.Name, "namespace", metaPod.Namespace)
			return true
		}
		// Non-blocking send: if the worker pool is saturated (all workers busy
		// and channel buffer full), skip this pod. It will be retried on the
		// next refresh cycle. This prevents the metrics refresh
		// goroutine from blocking indefinitely when workers are stuck on slow
		// or unreachable engine pods.
		select {
		case c.podMetricsJobs <- podMetricsJob{Pod: metaPod}:
		default:
			c.finishPodMetricsScheduling(metaPod)
			metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonQueueFull)
			klog.V(4).InfoS("Metrics worker pool saturated, skipping pod metrics update",
				"pod", metaPod.Name)
		}
		return true
	})
}

type podMetricsJob struct {
	*Pod
	target *modelClaimMetricsTarget
}

func (c *Store) worker(jobs <-chan podMetricsJob) {
	for job := range jobs {
		if job.target != nil {
			c.scrapeModelClaimMetrics(job.Pod, job.target)
			continue
		}
		pod := job.Pod
		c.markPodMetricsInFlight(pod)
		ctx, cancel := context.WithTimeout(context.Background(), podMetricsFetchTimeout)
		podMetricPort := getPodMetricPort(pod)
		func() {
			defer c.finishPodMetricsScheduling(pod)
			defer cancel()

			// Use centralized typed metrics fetcher for better engine abstraction and error handling
			metricsToFetch := c.getAllAvailableMetrics()
			endpoint := fmt.Sprintf("%s:%d", pod.Status.PodIP, podMetricPort)
			engineType := metrics.GetEngineType(*pod.Pod)
			identifier := pod.Name
			result, err := c.engineMetricsFetcher.FetchAllTypedMetrics(ctx, endpoint, engineType, identifier, metricsToFetch)
			// Serialize publication with informer updates so a deleted or replaced
			// pod cannot publish an in-flight scrape after its gauges are removed.
			c.mu.RLock()
			defer c.mu.RUnlock()
			current, ok := c.metaPods.Load(podMetricsBackoffKey(pod))
			if !ok || current != pod {
				return
			}
			if err != nil {
				metrics.IncrementPodMetricsFetchFailure(engineType, metrics.PodMetricsFetchFailureReasonFetchError)
				c.recordPodMetricsFetchFailure(pod, time.Now())
				klog.V(4).InfoS("Failed to fetch typed metrics from engine pod",
					"pod", pod.Name, "podIP", pod.Status.PodIP, "port", podMetricPort, "error", err)
				return
			}
			fetchedAt := time.Now()
			c.recordPodMetricsFetchSuccess(pod)

			for metricName, metricValue := range result.Metrics {
				sanitizeMetricValueLabels(pod, metricValue)
				if shouldSkipMetric(pod.Pod, metricName) {
					continue
				}
				metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: ""}, pod.Pod, metricName, metricValue, metricValue.GetLabelValues())
			}

			for metricName, metricValue := range result.ModelMetrics {
				sanitizeMetricValueLabels(pod, metricValue)
				// Model names may contain "/" (HuggingFace-style ids), so this
				// must split on the last "/" like parseModelMetricKey, not the
				// first: strings.SplitN(metricName, "/", 2) previously cut a
				// slash-bearing model name in half and left metric holding
				// part of the model name instead (review on #2735).
				model, metric := parseModelMetricKey(metricName)
				if model == "" {
					continue
				}

				model = resolveMetricModelName(pod, model)

				if shouldSkipMetric(pod.Pod, metric) {
					continue
				}

				c.updateThroughputToksPerS(pod, model, metric, metricValue)
				c.updateRecentMean(pod, model, metric, metricValue)
				metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, metric, metricValue, metricValue.GetLabelValues())

				if metric == metrics.EngineSleepState && c.kvEventManager != nil {
					// The prefix router and the KV-event subscription both key
					// on the pod-metadata model name (constants.ModelNameFromMetadata),
					// not the Prometheus-reported model_name label resolved
					// above: those can differ (HF path vs model.aibrix.ai/name),
					// and RemovePrefix silently no-ops on a (modelName, loraID)
					// it never subscribed under (review on #2735).
					if podModelName, ok := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations); ok {
						podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
						c.kvEventManager.CheckSleepStateBackstop(ctx, podKey, podModelName, podLoraID(pod), metricValue.GetSimpleValue())
					}
				}
			}
			// Update pod metrics using typed results
			c.updatePodMetricsFromTypedResultAt(pod, result, fetchedAt)

			c.updateGatewayTrackedPodMetrics(pod)

			// Handle Prometheus-based metrics separately (these require PromQL queries)
			if c.prometheusApi != nil {
				c.enqueuePromQL(pod)
			} else {
				klog.V(4).InfoS("Prometheus API not initialized, skipping PromQL metrics", "pod", pod.Name)
			}

			// Log successful processing
			klog.V(5).InfoS("Successfully processed metrics for pod",
				"pod", pod.Name,
				"podMetrics", len(result.Metrics),
				"modelMetrics", len(result.ModelMetrics),
				"errors", len(result.Errors))
		}()
	}
}

func (c *Store) updateGatewayTrackedPodMetrics(pod *Pod) {
	c.syncRunningRequestsGlobally(pod)
	c.updateRealtimeRunningRequestsDrainRate1m(pod)
	c.updateRealtimeOutputTokenRateEWMA(pod)
	c.updateRealtimeOutputTokensPerRequestSecondEWMA(pod)
}

func (c *Store) tryMarkPodMetricsQueued(pod *Pod) bool {
	key := podMetricsBackoffKey(pod)
	state, loaded := c.podMetricsScheduling.LoadOrStore(key, &podMetricsSchedulingState{
		uid:    string(pod.UID),
		status: podMetricsQueued,
	})
	if !loaded {
		return true
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uid != string(pod.UID) {
		c.podMetricsScheduling.Store(key, &podMetricsSchedulingState{
			uid:    string(pod.UID),
			status: podMetricsQueued,
		})
		return true
	}
	return false
}

func (c *Store) markPodMetricsInFlight(pod *Pod) {
	key := podMetricsBackoffKey(pod)
	state, ok := c.podMetricsScheduling.Load(key)
	if !ok {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uid == string(pod.UID) {
		state.status = podMetricsInFlight
	}
}

func (c *Store) finishPodMetricsScheduling(pod *Pod) {
	key := podMetricsBackoffKey(pod)
	state, ok := c.podMetricsScheduling.Load(key)
	if !ok {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uid == string(pod.UID) {
		c.podMetricsScheduling.CompareAndDelete(key, state)
	}
}

func (c *Store) clearPodMetricsScheduling(namespace, name string) {
	c.podMetricsScheduling.Delete(utils.GeneratePodKey(namespace, name))
}

func (c *Store) shouldSkipPodMetricsFetch(pod *Pod, now time.Time) bool {
	key := podMetricsBackoffKey(pod)
	state, ok := c.podMetricsBackoff.Load(key)
	if !ok {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uid != string(pod.UID) {
		c.podMetricsBackoff.CompareAndDelete(key, state)
		return false
	}
	return now.Before(state.nextFetchAt)
}

func (c *Store) recordPodMetricsFetchFailure(pod *Pod, now time.Time) {
	key := podMetricsBackoffKey(pod)
	state, ok := c.podMetricsBackoff.Load(key)
	if !ok {
		state, _ = c.podMetricsBackoff.LoadOrStore(key, &podMetricsBackoffState{uid: string(pod.UID)})
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if !c.isCurrentPodMetricsPod(pod) {
		if state.uid == string(pod.UID) {
			c.podMetricsBackoff.CompareAndDelete(key, state)
		}
		return
	}
	if state.uid != string(pod.UID) {
		state.uid = string(pod.UID)
		state.failures = 0
	}
	state.failures++
	state.nextFetchAt = now.Add(podMetricsBackoffDelay(state.failures))
}

func (c *Store) recordPodMetricsFetchSuccess(pod *Pod) {
	c.clearPodMetricsBackoffForPod(pod)
}

func (c *Store) clearPodMetricsBackoffForPod(pod *Pod) {
	key := podMetricsBackoffKey(pod)
	state, ok := c.podMetricsBackoff.Load(key)
	if !ok {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.uid == string(pod.UID) {
		c.podMetricsBackoff.CompareAndDelete(key, state)
	}
}

func (c *Store) clearPodMetricsBackoff(namespace, name string) {
	c.podMetricsBackoff.Delete(utils.GeneratePodKey(namespace, name))
}

func (c *Store) isCurrentPodMetricsPod(pod *Pod) bool {
	current, ok := c.metaPods.Load(utils.GeneratePodKey(pod.Namespace, pod.Name))
	return ok && string(current.UID) == string(pod.UID)
}

func (c *Store) updateMetricFromPromQL(ctx context.Context, pod *Pod) (queryErr error) {
	podName := pod.Name
	podMetricPort := getPodMetricPort(pod)
	for _, metricName := range prometheusMetricNames {
		queryLabels := map[string]string{
			"instance": fmt.Sprintf("%s:%d", pod.Status.PodIP, podMetricPort),
		}
		metric, ok := metrics.Metrics[metricName]
		if !ok {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}
		scope := metric.MetricScope
		if scope == metrics.PodMetricScope {
			err := c.queryUpdatePromQLMetrics(ctx, metric, queryLabels, pod, "", metricName, podMetricPort)
			if err != nil {
				klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
				if queryErr == nil {
					queryErr = err
				}
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if pod.Models.Len() > 0 {
				for _, modelName := range pod.Models.Array() {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetrics(ctx, metric, queryLabels, pod, modelName, metricName, podMetricPort)
					if err != nil {
						klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
						if queryErr == nil {
							queryErr = err
						}
						continue
					}
				}
			} else {
				klog.V(4).Infof("Cannot find model names for pod %s", podName)
			}
		} else {
			klog.V(4).Infof("Scope %v is not supported", scope)
		}
	}
	return queryErr
}

func (c *Store) queryUpdatePromQLMetrics(ctx context.Context, metric metrics.Metric, queryLabels map[string]string, pod *Pod, modelName string, metricName string, podMetricPort int) error {
	scope := metric.MetricScope
	query := metrics.BuildQuery(metric.PromQL, queryLabels)
	// Querying metrics
	result, warnings, err := c.prometheusApi.Query(ctx, query, time.Now())
	if err != nil {
		metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: modelName}, pod.Pod, metrics.PrometheusQueryFail, &metrics.SimpleMetricValue{Value: 1.0}, nil)
		// Skip this model fetching if an error is thrown
		return fmt.Errorf("error executing query: %v", err)
	}
	if len(warnings) > 0 {
		klog.V(4).Infof("Warnings: %v\n", warnings)
	}

	// Update metrics
	metricValue := &metrics.PrometheusMetricValue{Result: &result}
	err = c.updatePodRecord(pod, modelName, metricName, scope, metricValue)
	if err != nil {
		return fmt.Errorf("failed to update metrics %s from prometheus %s: %v", metricName, pod.Name, err)
	}
	klog.V(5).InfoS("Successfully parsed metrics from prometheus", "metric", metricName, "model", modelName, "PodName", pod.Name, "Port", podMetricPort, "metricValue", metricValue)
	return nil
}

// Update `PodMetrics` and `PodModelMetrics` according to the metric scope
// TODO: replace in-place metric update podMetrics and podModelMetrics to fresh copy for preventing stale metric keys
func (c *Store) updatePodRecord(pod *Pod, modelName string, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue) error {
	return c.updatePodRecordAt(pod, modelName, metricName, scope, metricValue, time.Now())
}

func (c *Store) updatePodRecordAt(pod *Pod, modelName, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue, now time.Time) error {
	record := MetricRecord{Value: metricValue, Timestamp: now}
	switch scope {
	case metrics.PodMetricScope:
		pod.Metrics.Store(metricName, record)
	case metrics.PodModelMetricScope:
		if modelName == "" {
			var ok bool
			modelName, ok = constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
			if !ok {
				return fmt.Errorf("modelName should not be empty for scope %v", scope)
			}
		}
		pod.modelMetricStore(modelName).Store(c.getPodModelMetricName(modelName, metricName), record)
	default:
		return fmt.Errorf("scope %v is not supported", scope)
	}
	return nil
}

func copyPodMetricInventory(source, destination *Pod) {
	source.Metrics.Range(func(metricName string, record MetricRecord) bool {
		destination.Metrics.Store(metricName, record)
		return true
	})
	source.ModelMetrics.Range(func(key string, record MetricRecord) bool {
		destination.ModelMetrics.Store(key, record)
		return true
	})
}

func (c *Store) deletePodGaugeMetrics(pod *Pod) {
	if pod == nil {
		return
	}
	pod.Metrics.Range(func(metricName string, record MetricRecord) bool {
		deletePodGaugeMetric(pod.Pod, "", metricName, record.Value.GetLabelValues())
		return true
	})
	pod.ModelMetrics.Range(func(key string, record MetricRecord) bool {
		modelName, metricName := parseModelMetricKey(key)
		deletePodGaugeMetric(pod.Pod, modelName, metricName, record.Value.GetLabelValues())
		return true
	})
}

func deletePodGaugeMetric(pod *v1.Pod, modelName, metricName string, extra map[string]string) {
	definition, ok := metrics.Metrics[metricName]
	if !ok || definition.MetricType.Raw == metrics.Counter {
		return
	}
	metrics.DeleteEngineMetricFromPrometheus(&types.RoutingContext{Model: modelName}, pod, metricName, extra)
}

type modelReplicaState struct {
	pod       *v1.Pod
	modelName string
}

const pdRoleIdentifier = "role-name"

// pdRole returns the PD role of pod, "prefill" or "decode", or "" for neither. The
// role-name label is what the PD router groups pods by, so it decides. Pods whose
// label is missing or holds another value fall back to the pod name, which is how
// the role was detected before; that keeps every pod matched today matched.
func pdRole(pod *v1.Pod) string {
	if pod == nil {
		return ""
	}
	switch pod.Labels[pdRoleIdentifier] {
	case "prefill":
		return "prefill"
	case "decode":
		return "decode"
	}
	switch {
	case strings.Contains(pod.Name, "prefill"):
		return "prefill"
	case strings.Contains(pod.Name, "decode"):
		return "decode"
	}
	return ""
}

func isPodWithHTTPServer(pod *v1.Pod) bool {
	podGroupIndex, exists := pod.Labels[podGroupIndex]
	if !exists {
		return true
	}
	return podGroupIndex == "0"
}

func (c *Store) updateModelMetrics() {
	c.updateModelReplicaMetrics()
}

// updateModelReplicaMetrics emits model_replicas=1 for each ready, routable engine pod and
// removes stale series when pods leave the cache. A quarantined pod (PodQuarantineChecker)
// is treated as gone: it is left out of the active set, so its series is removed below.
func (c *Store) updateModelReplicaMetrics() {
	active := make(map[string]struct{})

	c.metaPods.Range(func(key string, metaPod *Pod) bool {
		pod := metaPod.Pod
		if pod == nil || !utils.FilterReadyPod(pod) || !isPodWithHTTPServer(pod) || c.isPodQuarantined(pod) {
			return true
		}

		modelName, ok := getModelNameFromPod(pod)
		if !ok || modelName == "" {
			return true
		}

		extras := map[string]string{"model_name": modelName}
		metrics.EmitMetricToPrometheus(
			&types.RoutingContext{Model: modelName},
			pod,
			metrics.ModelReplicas,
			&metrics.SimpleMetricValue{Value: 1.0},
			extras,
		)
		c.modelReplicaEmitted.Store(key, modelReplicaState{pod: pod, modelName: modelName})
		active[key] = struct{}{}
		return true
	})

	c.modelReplicaEmitted.Range(func(key string, state modelReplicaState) bool {
		if _, ok := active[key]; ok {
			return true
		}
		extras := map[string]string{"model_name": state.modelName}
		metrics.DeleteGaugeMetricForPod(
			metrics.ModelReplicas,
			&types.RoutingContext{Model: state.modelName},
			state.pod,
			extras,
		)
		c.modelReplicaEmitted.Delete(key)
		return true
	})
}

func (c *Store) aggregateMetrics() {
	if c.metrics == nil {
		c.metrics = make(map[string]any)
	}
	for _, subscriber := range c.subscribers {
		for _, metric := range subscriber.SubscribedMetrics() {
			if _, exists := c.metrics[metric]; !exists {
				// TODO: refactor to
				c.metrics[metric] = "yes"
			}
		}
	}
}

// getAllAvailableMetrics returns all metrics that can be fetched from engine pods
func (c *Store) getAllAvailableMetrics() []string {
	var allMetrics []string

	// Add all the metrics we currently process
	allMetrics = append(allMetrics, counterGaugeMetricNames...)
	allMetrics = append(allMetrics, histogramMetricNames...)
	allMetrics = append(allMetrics, labelQueryMetricNames...)

	return allMetrics
}

// updateThroughputToksPerS derives a per-second token throughput rate from a cumulative token counter
// and stores it under AvgPromptThroughputToksPerS (prefill pods) or AvgGenerationThroughputToksPerS
// (decode pods). No-ops for pods that don't match either role or metric name.
func (c *Store) updateThroughputToksPerS(pod *Pod, model, metric string, metricValue metrics.MetricValue) {
	var rateMetricName string
	role := pdRole(pod.Pod)
	if role == "prefill" && metric == metrics.PromptTokenTotal {
		rateMetricName = metrics.AvgPromptThroughputToksPerS
	} else if role == "decode" && metric == metrics.GenerationTokenTotal {
		rateMetricName = metrics.AvgGenerationThroughputToksPerS
	}
	if rateMetricName == "" {
		return
	}
	perSecRate := c.calculatePerSecondRate(pod, model, metric, metricValue.GetSimpleValue())
	if perSecRate >= 0 {
		rateValue := &metrics.SimpleMetricValue{Value: perSecRate, Labels: metricValue.GetLabelValues()}
		if target := pod.claimMetrics[model]; target != nil {
			target.emitMetric(rateMetricName, rateValue, metricValue.GetLabelValues())
		} else {
			metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, rateMetricName, rateValue, metricValue.GetLabelValues())
		}
		_ = c.updatePodRecord(pod, model, rateMetricName, metrics.PodModelMetricScope, rateValue)
		klog.V(4).InfoS("get metric per sec rate", "metric", rateMetricName, "raw_value", metricValue.GetSimpleValue(), "per_sec_rate", rateValue.GetSimpleValue())
	}
}

// syncRunningRequestsGlobally updates the routing metric cache with the same estimate that
// the scrape-time collector computes directly. The collector must not read this cache slot:
// request start/finish also write it with the local count and refresh its timestamp.
func (c *Store) syncRunningRequestsGlobally(pod *Pod) {
	total, ready := c.runningRequestsEstimate(pod, c.snapshotNow())
	if !ready {
		return
	}
	totalValue := &metrics.SimpleMetricValue{Value: total}
	if err := c.updatePodRecord(pod, "", metrics.RealtimeNumRequestsRunning,
		metrics.PodMetricScope, totalValue); err != nil {
		klog.V(4).ErrorS(err, "failed to update global running requests", "pod", pod.Name)
	}
}

// runningRequestsEstimate adds the local atomic counter to fresh remote contributions without
// accessing Redis or the metric cache. An active reader takes precedence over the snapshot;
// unread reader pods and an uninitialized snapshot withhold the estimate. Once initialized,
// freshCount excludes expired remote contributions, falling back to the local count.
func (c *Store) runningRequestsEstimate(pod *Pod, now time.Time) (float64, bool) {
	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	var remoteRunning float64
	if reader := c.remoteRunningReader.Load(); reader != nil {
		count, seen := reader.freshCount(podKey, now)
		if !seen {
			return 0, false
		}
		remoteRunning = float64(count)
	} else {
		snap := c.gatewayRemoteRunning.Load()
		if snap == nil {
			return 0, false
		}
		remoteRunning = float64(snap.freshCount(podKey, now))
	}

	localRunning := float64(atomic.LoadInt32(&pod.runningRequests))
	total := localRunning + remoteRunning
	if klog.V(5).Enabled() {
		klog.V(5).InfoS("running requests aggregation", "pod", pod.Name, "local", localRunning, "remote", remoteRunning, "total", total)
	}

	return total, true
}

// updateRealtimeRunningRequestsDrainRate1m computes the 1-minute rolling rate at which completed
// requests are draining on the pod and stores it under RealtimeRunningRequestsDrainRate1m. The
// gateway exports the stored value at scrape time (podGatewayMetricsCollector), so a deleted or
// relabeled pod leaves no stale series. Computed for all pods; the PD-disaggregation decode
// router scores on it.
func (c *Store) updateRealtimeRunningRequestsDrainRate1m(pod *Pod) {
	completed := float64(atomic.LoadInt64(&pod.completedRequests))
	drainRate := c.calculateRate1m(pod, "completed_requests", completed)
	if drainRate >= 0 {
		rateValue := &metrics.SimpleMetricValue{Value: drainRate}
		// The gauge describes the whole pod rather than one request, so the collector takes
		// its model label from the pod's own metadata: the model.aibrix.ai/name label, or the
		// annotation of the same name for model names a label cannot carry (#3501).
		_ = c.updatePodRecord(pod, "", metrics.RealtimeRunningRequestsDrainRate1m, metrics.PodMetricScope, rateValue)
		klog.V(4).InfoS("Updating drain rate metric", "pod", pod.Name,
			"completed_requests", completed, metrics.RealtimeRunningRequestsDrainRate1m, drainRate)
	}
}

// outputTokenRateEWMATau is the time constant of the output-token-rate EWMA: a step change in a
// pod's throughput is ~63% reflected after this long. The 1-minute windowed rate it smooths moves
// in discrete steps as its baseline snapshot advances, so this keeps the capacity estimate (and
// with it the load-balance score) from jumping between scrapes.
const outputTokenRateEWMATau = 10 * time.Second

// updateRealtimeOutputTokenRateEWMA maintains the pod's observed output-token throughput
// (tokens/sec: EWMA of the 1-minute completion-token rate) under RealtimeOutputTokenRateEWMA. It is
// the capacity estimate of the load-balance router (algorithms/load_balance.go). Like the request
// drain rate, it is gateway-tracked, so it is available for every engine.
//
// A window with no completed output tokens (idle pod, or usage not reported) says nothing about how
// fast the replica is, so the previous estimate is kept instead of decayed toward zero. Decaying
// would make a briefly idle replica look weak and starve it of the traffic that re-measures it.
func (c *Store) updateRealtimeOutputTokenRateEWMA(pod *Pod) {
	completed := float64(atomic.LoadInt64(&pod.completedOutputTokens))
	rate := c.calculateRate1m(pod, "completed_output_tokens", completed)
	if rate <= 0 {
		return
	}

	ewma := rate
	if prev, ok := pod.Metrics.Load(metrics.RealtimeOutputTokenRateEWMA); ok {
		alpha := 1 - math.Exp(-podMetricRefreshInterval.Seconds()/outputTokenRateEWMATau.Seconds())
		ewma = alpha*rate + (1-alpha)*prev.Value.GetSimpleValue()
	}
	_ = c.updatePodRecord(pod, "", metrics.RealtimeOutputTokenRateEWMA, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: ewma})
	klog.V(4).InfoS("Updating output token rate metric", "pod", pod.Name,
		"completed_output_tokens", completed, "rate_1m", rate, metrics.RealtimeOutputTokenRateEWMA, ewma)
}

const (
	outputRateMinSamplesEnv = "AIBRIX_OUTPUT_RATE_MIN_SAMPLES"
	// From the runs on #3491 (one model, 32 output tokens, short unique prompts): once a pod's
	// first seconds are left out, the per-request rate of its first k completed requests is within
	// 10% of its steady value from k = 8, for all 70 same-speed (run, pod) pairs and at the 95th
	// percentile of the 14 heterogeneous ones. It covers estimation noise for that workload only.
	// A fresh engine's first requests are slower than its steady state, and no sample count this
	// small averages that out. A pod that completes fewer requests than this per window never
	// refreshes its estimate.
	outputRateMinSamplesDefault = 8
)

// outputRateMinSamplesFromEnv is the process default of Store.minOutputRateSamples.
var outputRateMinSamplesFromEnv = utils.LoadEnvNonNegativeInt(outputRateMinSamplesEnv, outputRateMinSamplesDefault)

// minOutputRateSamples is how many requests with known output tokens a pod must have completed
// within the rate window for the window to update its per-request rate. 0 lets every window with
// a completed request update it.
func (c *Store) minOutputRateSamples() int {
	if c.outputRateMinSamples != nil {
		return *c.outputRateMinSamples
	}
	return outputRateMinSamplesFromEnv
}

// readOutputRateCounters reads the completion counters as one consistent set. donePodStats
// updates them under the pod's stats lock, so they are read under it too: a metrics tick never
// sees a request's tokens without its time or its sample.
func (c *Store) readOutputRateCounters(pod *Pod) outputRateCounters {
	mu := c.podStatsLockFor(utils.GeneratePodKey(pod.Namespace, pod.Name))
	mu.Lock()
	defer mu.Unlock()
	return outputRateCounters{
		tokens:      float64(atomic.LoadInt64(&pod.completedOutputTokens)),
		busySeconds: float64(atomic.LoadInt64(&pod.completedOutputBusyNanos)) / float64(time.Second),
		requests:    float64(atomic.LoadInt64(&pod.completedOutputRequests)),
		finished:    float64(atomic.LoadInt64(&pod.completedRequests)),
	}
}

// ewmaWeight is the weight of a new sample in an EWMA with time constant
// outputTokenRateEWMATau, elapsed after the previous one.
func ewmaWeight(elapsed time.Duration) float64 {
	return 1 - math.Exp(-elapsed.Seconds()/outputTokenRateEWMATau.Seconds())
}

// updateRealtimeOutputTokensPerRequestSecondEWMA maintains two pod metrics from one window of the
// gateway-tracked completion counters.
//
// RealtimeOutputTokensPerRequestSecondEWMA is the pod's per-request decode rate: the output tokens
// completed in the window divided by the time those requests spent on the pod. Doubling a pod's
// traffic doubles both terms, so below saturation it does not depend on how traffic is split,
// which RealtimeOutputTokenRateEWMA does (#3450). A faster replica finishes each request sooner
// and measures higher. When requests queue on a saturated replica their time on the pod grows and
// the estimate falls. No router reads it yet.
//
// What it does not see:
//   - A request that finishes without output tokens is in neither term: aborted, timed out,
//     failed, or completed without usage. The last includes every streaming request sent
//     without stream_options.include_usage, which the gateway only requires of TPM-limited
//     users. So RealtimeRequestsWithoutOutputTokensFraction1m is not a failure rate: on a pod
//     whose clients stream without usage it reads near 1 and the rate is never updated, and
//     with mixed clients the rate describes only the requests that reported usage. A pod whose
//     slowest requests are abandoned reads faster than it is; the fraction makes that visible.
//   - It is one value per pod. A pod that serves several models (ModelClaim) mixes them.
//   - Time on the pod is the whole request, from addPodStats to donePodStats, so it includes
//     queueing and prompt processing. Pods that receive different request mixes read differently
//     even on the same hardware.
//
// Both values can be old. The rate is updated only by a window in which at least
// minOutputRateSamples requests with known output tokens completed, and the fraction only by a
// window in which a request finished. Otherwise the previous value stays, with the timestamp of
// its last update: an idle pod, or one below that request rate, keeps its last values
// indefinitely. A reader must check the record's Timestamp (GetMetricRecordByPod) and decide how
// old a value it accepts.
//
// The EWMA weighs a new window by the time since the estimate was last updated, so after
// skipped windows the old estimate counts for less.
func (c *Store) updateRealtimeOutputTokensPerRequestSecondEWMA(pod *Pod) {
	d, ok := c.outputRateDeltas1m(pod, c.readOutputRateCounters(pod))
	if !ok {
		return
	}
	now := time.Now()

	if d.finished > 0 {
		without := math.Min(1, math.Max(0, 1-d.requests/d.finished))
		_ = c.updatePodRecordAt(pod, "", metrics.RealtimeRequestsWithoutOutputTokensFraction1m, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: without}, now)
	}

	if d.tokens <= 0 || d.busySeconds <= 0 || d.requests < float64(c.minOutputRateSamples()) {
		return
	}
	rate := d.tokens / d.busySeconds

	ewma := rate
	if prev, ok := pod.Metrics.Load(metrics.RealtimeOutputTokensPerRequestSecondEWMA); ok {
		elapsed := now.Sub(prev.Timestamp)
		if elapsed < podMetricRefreshInterval {
			elapsed = podMetricRefreshInterval
		}
		alpha := ewmaWeight(elapsed)
		ewma = alpha*rate + (1-alpha)*prev.Value.GetSimpleValue()
	}
	_ = c.updatePodRecordAt(pod, "", metrics.RealtimeOutputTokensPerRequestSecondEWMA, metrics.PodMetricScope, &metrics.SimpleMetricValue{Value: ewma}, now)
	klog.V(4).InfoS("Updating per-request output token rate metric", "pod", pod.Name,
		"window_output_tokens", d.tokens, "window_busy_seconds", d.busySeconds, "window_requests", d.requests,
		"window_finished", d.finished, "rate_1m", rate,
		metrics.RealtimeOutputTokensPerRequestSecondEWMA, ewma)
}

// updatePodMetricsFromTypedResult processes the typed metrics result and updates pod storage.
// Precondition: labels in result must already be sanitized via sanitizeMetricValueLabels.
// The production caller worker() sanitizes every MetricValue in result before invoking this
// function, so labels stored in pod.Metrics / pod.ModelMetrics carry resolved model_name /
// engine_type values rather than the "undefined" sentinel emitted by some engines (e.g. trtllm).
func (c *Store) updatePodMetricsFromTypedResult(pod *Pod, result *metrics.EngineMetricsResult) {
	c.updatePodMetricsFromTypedResultAt(pod, result, time.Now())
}

func (c *Store) updatePodMetricsFromTypedResultAt(pod *Pod, result *metrics.EngineMetricsResult, now time.Time) {
	// Process pod-scoped metrics
	for metricName, metricValue := range result.Metrics {
		if metricDef, exists := metrics.Metrics[metricName]; exists {
			err := c.updatePodRecordAt(pod, "", metricName, metricDef.MetricScope, metricValue, now)
			if err != nil {
				klog.V(4).InfoS("Failed to update pod metric",
					"pod", pod.Name, "metric", metricName, "error", err)
			}
		}
	}

	// Process model-scoped metrics
	for modelMetricKey, metricValue := range result.ModelMetrics {
		// modelMetricKey format: "model/metric"
		modelName, metricName := parseModelMetricKey(modelMetricKey)

		modelName = resolveMetricModelName(pod, modelName)

		if metricDef, exists := metrics.Metrics[metricName]; exists {
			err := c.updatePodRecordAt(pod, modelName, metricName, metricDef.MetricScope, metricValue, now)
			if err != nil {
				klog.V(4).InfoS("Failed to update model metric",
					"pod", pod.Name, "model", modelName, "metric", metricName, "error", err)
			}
		}
	}

	// Log any errors from the fetching process
	for _, err := range result.Errors {
		klog.V(4).InfoS("Metric fetching error", "pod", pod.Name, "error", err)
	}
}

// parseModelMetricKey parses a key like "model/metric" into model name and metric name
func parseModelMetricKey(key string) (modelName, metricName string) {
	separator := strings.LastIndexByte(key, '/')
	if separator < 0 {
		return "", key // Fallback if parsing fails
	}
	return key[:separator], key[separator+1:]
}

// podLoraID mirrors kvevent's own extractLoraID: the pod's LoRA identity
// (constants.GetLoraID) if it declares one and it parses, else -1. Kept in
// sync with that convention since CheckSleepStateBackstop's purge must scope
// to the same (modelName, loraID, podKey) the AllBlocksCleared event handler
// already uses for the same pod.
func podLoraID(pod *Pod) int64 {
	if loraStr := constants.GetLoraID(pod.Labels); loraStr != "" {
		if parsed, err := strconv.ParseInt(loraStr, 10, 64); err == nil {
			return parsed
		}
	}
	return -1
}

func resolveMetricModelName(pod *Pod, modelName string) string {
	if !isUndefinedMetricLabelValue(modelName) {
		return modelName
	}

	if podModel, ok := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations); ok {
		klog.V(4).InfoS("Using pod metadata as model name fallback",
			"pod", pod.Name, "originalModel", modelName, "resolvedModel", podModel)
		return podModel
	}

	return modelName
}

func resolveMetricEngineType(pod *Pod, engineType string) string {
	if !isUndefinedMetricLabelValue(engineType) {
		return engineType
	}

	if podEngine, err := getPodLabel(pod, engineLabel); err == nil && podEngine != "" {
		klog.V(4).InfoS("Using pod label as engine type fallback",
			"pod", pod.Name, "originalEngineType", engineType, "resolvedEngineType", podEngine)
		return podEngine
	}

	return engineType
}

func sanitizeMetricValueLabels(pod *Pod, metricValue metrics.MetricValue) {
	switch value := metricValue.(type) {
	case *metrics.SimpleMetricValue:
		value.Labels = sanitizeMetricLabels(pod, value.Labels)
	case *metrics.HistogramMetricValue:
		value.Labels = sanitizeMetricLabels(pod, value.Labels)
	}
}

func sanitizeMetricLabels(pod *Pod, labelValues map[string]string) map[string]string {
	if len(labelValues) == 0 {
		return labelValues
	}

	// Key-existence check is required: isUndefinedMetricLabelValue("") is true, so a missing
	// key would otherwise falsely trigger the alloc path even though we never add it back.
	modelVal, hasModel := labelValues["model_name"]
	engineVal, hasEngine := labelValues["engine_type"]
	modelNeedsFix := hasModel && isUndefinedMetricLabelValue(modelVal)
	engineNeedsFix := hasEngine && isUndefinedMetricLabelValue(engineVal)

	if !modelNeedsFix && !engineNeedsFix {
		return labelValues
	}

	sanitized := make(map[string]string, len(labelValues))
	for key, value := range labelValues {
		sanitized[key] = value
	}
	if modelNeedsFix {
		sanitized["model_name"] = resolveMetricModelName(pod, modelVal)
	}
	if engineNeedsFix {
		sanitized["engine_type"] = resolveMetricEngineType(pod, engineVal)
	}

	return sanitized
}

func isUndefinedMetricLabelValue(value string) bool {
	return value == "" || value == undefinedMetricLabelValue
}
