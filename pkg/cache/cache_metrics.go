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
	"strings"
	"sync"
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
)

const (
	// When the engine's HTTP proxy is separated from the engine itself,
	// the request port and metrics port may differ, so a dedicated metrics port is required.
	MetricPortLabel                     = constants.ModelLabelMetricPort
	modelLabel                          = constants.ModelLabelName
	defaultMetricPort                   = 8000
	defaultEngineLabelValue             = "vllm"
	defaultPodMetricRefreshIntervalInMS = 50
	defaultPodMetricsWorkerCount        = 10
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
		metrics.WaitingLoraAdapters,
		metrics.RunningLoraAdapters,
	}
	podMetricRefreshInterval = time.Duration(utils.LoadEnvInt("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", defaultPodMetricRefreshIntervalInMS)) * time.Millisecond
)

// MetricSnapshot represents a metric value at a specific timestamp
type MetricSnapshot struct {
	Value     float64
	Timestamp time.Time
}

// RateCalculator manages historical metric values for rate calculation
type RateCalculator struct {
	mu       sync.RWMutex
	history  map[string][]MetricSnapshot // key: "podName/modelName/metricName"
	maxAge   time.Duration               // Maximum age to keep snapshots
	maxCount int                         // Maximum number of snapshots to keep
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

func (c *Store) getPodMetricImpl(podName string, metricStore *utils.SyncMap[string, metrics.MetricValue], metricName string) (metrics.MetricValue, error) {
	metricVal, ok := metricStore.Load(metricName)
	if !ok {
		return nil, &MetricNotFoundError{
			CacheError: ErrorTypeMetricNotFound,
			PodName:    podName,
			MetricName: metricName,
		}
	}

	return metricVal, nil
}

func (c *Store) getPodModelMetricName(modelName string, metricName string) string {
	return fmt.Sprintf("%s/%s", modelName, metricName)
}

func (c *Store) updatePodMetrics() {
	c.metaPods.Range(func(key string, metaPod *Pod) bool {
		if !utils.FilterReadyPod(metaPod.Pod) {
			// Skip unready pod
			return true
		}
		c.podMetricsJobs <- metaPod // Send the job to the worker pool
		return true
	})
}

func (c *Store) worker(jobs <-chan *Pod) {
	for pod := range jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		podMetricPort := getPodMetricPort(pod)

		// Use centralized typed metrics fetcher for better engine abstraction and error handling
		metricsToFetch := c.getAllAvailableMetrics()
		endpoint := fmt.Sprintf("%s:%d", pod.Status.PodIP, podMetricPort)
		engineType := metrics.GetEngineType(*pod.Pod)
		identifier := pod.Name
		result, err := c.engineMetricsFetcher.FetchAllTypedMetrics(ctx, endpoint, engineType, identifier, metricsToFetch)
		if err != nil {
			klog.V(4).InfoS("Failed to fetch typed metrics from engine pod",
				"pod", pod.Name, "podIP", pod.Status.PodIP, "port", podMetricPort, "error", err)
			cancel()
			continue
		}

		for metricName, metricValue := range result.Metrics {
			if shouldSkipMetric(pod.Name, metricName) {
				continue
			}
			metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: ""}, pod.Pod, metricName, metricValue, metricValue.GetLabelValues())
		}

		for metricName, metricValue := range result.ModelMetrics {
			parts := strings.SplitN(metricName, "/", 2)
			if len(parts) != 2 {
				continue
			}
			model := parts[0]
			metric := parts[1]

			if shouldSkipMetric(pod.Name, metric) {
				continue
			}

			var rateMetricName string
			if strings.Contains(pod.Name, "prefill") && metric == metrics.PromptTokenTotal {
				rateMetricName = metrics.AvgPromptThroughputToksPerS
			} else if strings.Contains(pod.Name, "decode") && metric == metrics.GenerationTokenTotal {
				rateMetricName = metrics.AvgGenerationThroughputToksPerS
			}
			if rateMetricName != "" {
				perSecRate := c.calculatePerSecondRate(pod, model, metric, metricValue.GetSimpleValue())
				if perSecRate >= 0 {
					rateValue := &metrics.SimpleMetricValue{Value: perSecRate}
					metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, rateMetricName, rateValue, metricValue.GetLabelValues())
					_ = c.updatePodRecord(pod, model, rateMetricName, metrics.PodModelMetricScope, rateValue)
					klog.V(4).InfoS("get metric per sec rate", "metric", rateMetricName, "raw_value", metricValue.GetSimpleValue(), "per_sec_rate", rateValue.GetSimpleValue())
				}
			}
			metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, pod.Pod, metric, metricValue, metricValue.GetLabelValues())
		}
		// Update pod metrics using typed results
		c.updatePodMetricsFromTypedResult(pod, result)

		// Handle Prometheus-based metrics separately (these require PromQL queries)
		if c.prometheusApi != nil {
			c.updateMetricFromPromQL(ctx, pod)
		} else {
			klog.V(4).InfoS("Prometheus API not initialized, skipping PromQL metrics", "pod", pod.Name)
		}

		// Log successful processing
		klog.V(5).InfoS("Successfully processed metrics for pod",
			"pod", pod.Name,
			"podMetrics", len(result.Metrics),
			"modelMetrics", len(result.ModelMetrics),
			"errors", len(result.Errors))

		cancel()
	}
}

func (c *Store) updateMetricFromPromQL(ctx context.Context, pod *Pod) {
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
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if pod.Models.Len() > 0 {
				for _, modelName := range pod.Models.Array() {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetrics(ctx, metric, queryLabels, pod, modelName, metricName, podMetricPort)
					if err != nil {
						klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
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
	if scope == metrics.PodMetricScope {
		pod.Metrics.Store(metricName, metricValue)
	} else if scope == metrics.PodModelMetricScope {
		var err error
		if modelName == "" {
			modelName, err = getPodLabel(pod, modelLabel)
			if err != nil {
				return fmt.Errorf("modelName should not be empty for scope %v", scope)
			}
		}
		pod.ModelMetrics.Store(c.getPodModelMetricName(modelName, metricName), metricValue)
	} else {
		return fmt.Errorf("scope %v is not supported", scope)
	}
	return nil
}

func (c *Store) updateModelMetrics() {
	// c.mu.Lock()
	// defer c.mu.Unlock()

	// if c.prometheusApi == nil {
	// 	klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
	// 	return
	// }
}

func (c *Store) aggregateMetrics() {
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

// updatePodMetricsFromTypedResult processes the typed metrics result and updates pod storage
func (c *Store) updatePodMetricsFromTypedResult(pod *Pod, result *metrics.EngineMetricsResult) {
	// Process pod-scoped metrics
	for metricName, metricValue := range result.Metrics {
		if metricDef, exists := metrics.Metrics[metricName]; exists {
			err := c.updatePodRecord(pod, "", metricName, metricDef.MetricScope, metricValue)
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

		// Calculate per-second rate for token metrics, only for decode pods generation tokens
		if strings.Contains(pod.Name, "decode") && metricName == metrics.GenerationTokenTotal {
			if simpleValue, ok := metricValue.(*metrics.SimpleMetricValue); ok {
				perSecRate := c.calculatePerSecondRate(pod, modelName, metricName, simpleValue.Value)
				if perSecRate >= 0 { // Only store valid rates (negative means insufficient data)
					rateMetricName := metrics.AvgGenerationThroughputToksPerS
					rateValue := &metrics.SimpleMetricValue{Value: perSecRate}
					_ = c.updatePodRecord(pod, modelName, rateMetricName, metrics.PodModelMetricScope, rateValue)
					klog.V(4).InfoS("Updating model metric", "pod", pod.Name, "model", modelName,
						"generation_token_total", metricValue,
						"avg_generation_throughput_toks_per_s", rateValue)
				}
			}
		}

		if metricDef, exists := metrics.Metrics[metricName]; exists {
			err := c.updatePodRecord(pod, modelName, metricName, metricDef.MetricScope, metricValue)
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
	modelName, metricName, found := strings.Cut(key, "/")
	if !found {
		return "", key // Fallback if parsing fails
	}
	return modelName, metricName
}
