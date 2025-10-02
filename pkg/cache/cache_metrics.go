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
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// When the engine's HTTP proxy is separated from the engine itself,
	// the request port and metrics port may differ, so a dedicated metrics port is required.
	MetricPortLabel                     = constants.ModelLabelMetricPort
	engineLabel                         = constants.ModelLabelEngine
	portLabel                           = constants.ModelLabelPort
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
		metrics.NumRequestsSwapped,
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
	}

	prometheusMetricNames = []string{
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

func initPrometheusAPI() prometheusv1.API {
	// Load environment variables
	prometheusEndpoint := utils.LoadEnv("PROMETHEUS_ENDPOINT", "")
	prometheusBasicAuthUsername := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_USERNAME", "")
	prometheusBasicAuthPassword := utils.LoadEnv("PROMETHEUS_BASIC_AUTH_PASSWORD", "")

	// Initialize Prometheus API
	var prometheusApi prometheusv1.API
	if prometheusEndpoint != "" {
		api, err := metrics.InitializePrometheusAPI(prometheusEndpoint, prometheusBasicAuthUsername, prometheusBasicAuthPassword)
		if err != nil {
			klog.Errorf("Error initializing Prometheus API: %v", err)
		} else {
			prometheusApi = api
			klog.Infof("Prometheus API initialized successfully")
		}
	}
	return prometheusApi
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

// Deprecated: updateSimpleMetricFromRawMetrics is kept for backward compatibility.
// Use updatePodMetricsFromTypedResult instead.
func (c *Store) updateSimpleMetricFromRawMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	podMetricPort := getPodMetricPort(pod)
	for _, metricName := range counterGaugeMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}

		// TODO: we should refact metricName to fit other engine
		metricFamily, exists := c.fetchMetrics(pod, allMetrics, metricName)
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")

			metricValue, err := metrics.GetCounterGaugeValue(familyMetric, metricFamily.GetType())
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podMetricPort, err)
				continue
			}

			err = c.updatePodRecord(pod, modelName, metricName, scope, &metrics.SimpleMetricValue{Value: metricValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podMetricPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podMetricPort, "metricValue", metricValue)
		}
	}
}

// Deprecated: updateHistogramMetricFromRawMetrics is kept for backward compatibility.
// Use updatePodMetricsFromTypedResult instead.
func (c *Store) updateHistogramMetricFromRawMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	podMetricPort := getPodMetricPort(pod)
	for _, metricName := range histogramMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}
		metricFamily, exists := c.fetchMetrics(pod, allMetrics, metricName)
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			metricValue, err := metrics.GetHistogramValue(familyMetric)
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, pod.Name, pod.Status.PodIP, podMetricPort, err)
				continue
			}

			histogramValue := &metrics.HistogramMetricValue{
				Sum:     metricValue.Sum,
				Count:   metricValue.Count,
				Buckets: metricValue.Buckets,
			}
			err = c.updatePodRecord(pod, modelName, metricName, scope, histogramValue)
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podMetricPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podMetricPort, "metricValue", metricValue)
		}
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

func (c *Store) fetchMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily, labelMetricName string) (*dto.MetricFamily, bool) {
	metric, exists := metrics.Metrics[labelMetricName]
	if !exists {
		klog.V(4).Infof("Cannot find labelMetricName %v in collected metrics names", labelMetricName)
		return nil, false
	}
	engineType, err := getPodLabel(pod, engineLabel)
	if engineType == "" {
		klog.V(4).Infof(err.Error())
		engineType = defaultEngineLabelValue
	}
	rawMetricName, ok := metric.EngineMetricsNameMapping[engineType]
	if !ok {
		klog.V(4).Infof("Cannot find engine type %v mapping for metrics %v", engineType, labelMetricName)
		return nil, false
	}
	metricFamily, exists := allMetrics[rawMetricName]
	if !exists {
		klog.V(4).Infof("Cannot find raw metrics %v, engine type %v", rawMetricName, engineType)
		return nil, false
	}
	return metricFamily, true
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

// Helper methods for centralized typed metrics processing

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
