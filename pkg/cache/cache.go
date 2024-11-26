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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	crdinformers "github.com/aibrix/aibrix/pkg/client/informers/externalversions"
	"github.com/redis/go-redis/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/aibrix/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/aibrix/aibrix/pkg/client/clientset/versioned/scheme"
	"github.com/aibrix/aibrix/pkg/metrics"
	"github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/config"
	"k8s.io/client-go/kubernetes/scheme"
)

var once sync.Once

// type global
type Cache struct {
	mu                sync.RWMutex
	redisClient       *redis.Client
	prometheusApi     prometheusv1.API
	initialized       bool
	subscribers       []metrics.MetricSubscriber
	metrics           map[string]interface{}
	ModelMetrics      map[string]map[string]interface{}
	Pods              map[string]*v1.Pod
	PodMetrics        map[string]map[string]*metrics.MetricValue // pod_name: map[metric_name]metric_val
	PodToModelMapping map[string]map[string]struct{}             // pod_name: map[model_name]struct{}
	ModelToPodMapping map[string]map[string]*v1.Pod              // model_name: map[pod_name]*v1.Pod
	requestTrace      map[string]map[string]int                  // model_name: map[Log2(input_token)-Log2(output_token)]request_count
}

const (
	modelIdentifier                       = "model.aibrix.ai/name"
	podPort                               = 8000
	defaultPodMetricRefreshIntervalInMS   = 50
	expireWriteRequestTraceIntervalInMins = 10
	keyWriteRequestTraceIntervalInSeconds = "meta_interval_sec"
	writeRequestTraceIntervalInSeconds    = 10
	keyPrecisionRequestTrace              = "meta_precision"
	precisionRequestTrace                 = 0.1
	keyVersionRequestTrace                = "meta_v"
	versionRequestTrace                   = 2
)

var (
	instance                Cache
	counterGaugeMetricNames = []string{
		metrics.NumRequestsRunning,
		metrics.NumRequestsWaiting,
		metrics.NumRequestsSwapped,
		metrics.AvgPromptThroughputToksPerS,
		metrics.AvgGenerationThroughputToksPerS,
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
	}

	podMetricRefreshIntervalInMilliseconds = getPodMetricRefreshInterval()
)

func getPodMetricRefreshInterval() time.Duration {
	value, exists := os.LookupEnv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS")
	if exists {
		intValue, err := strconv.Atoi(value)
		if err != nil {
			klog.V(4).Infof("Invalid AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS: %s, falling back to default", value)
		} else {
			klog.V(4).Infof("Using env value for refresh interval: %d ms", intValue)
			return time.Duration(intValue)
		}
	}
	klog.V(4).Infof("Using default refresh interval: %d ms", defaultPodMetricRefreshIntervalInMS)
	return time.Duration(defaultPodMetricRefreshIntervalInMS)
}

func GetCache() (*Cache, error) {
	if !instance.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return &instance, nil
}

// LoadEnv loads an environment variable or returns a default value if not set.
func LoadEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		klog.Warningf("Environment variable %s is not set, using default value: %s", key, defaultValue)
		return defaultValue
	}
	return value
}

// InitializePrometheusAPI initializes the Prometheus API client.
func InitializePrometheusAPI(endpoint string, username string, password string) (prometheusv1.API, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("prometheus endpoint is not provided")
	}

	client, err := api.NewClient(api.Config{
		Address: endpoint,
		RoundTripper: config.NewBasicAuthRoundTripper(config.NewInlineSecret(username),
			config.NewInlineSecret(password), api.DefaultRoundTripper),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	return prometheusv1.NewAPI(client), nil
}

func NewCache(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Cache {
	once.Do(func() {
		if err := v1alpha1scheme.AddToScheme(scheme.Scheme); err != nil {
			panic(err)
		}

		k8sClientSet, err := kubernetes.NewForConfig(config)
		if err != nil {
			panic(err)
		}

		crdClientSet, err := v1alpha1.NewForConfig(config)
		if err != nil {
			panic(err)
		}

		factory := informers.NewSharedInformerFactoryWithOptions(k8sClientSet, 0)
		crdFactory := crdinformers.NewSharedInformerFactoryWithOptions(crdClientSet, 0)

		podInformer := factory.Core().V1().Pods().Informer()
		modelInformer := crdFactory.Model().V1alpha1().ModelAdapters().Informer()

		defer runtime.HandleCrash()
		factory.Start(stopCh)
		crdFactory.Start(stopCh)

		if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced, modelInformer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}

		// Load environment variables
		prometheusEndpoint := LoadEnv("PROMETHEUS_ENDPOINT", "")
		prometheusBasicAuthUsername := LoadEnv("PROMETHEUS_BASIC_AUTH_USERNAME", "")
		prometheusBasicAuthPassword := LoadEnv("PROMETHEUS_BASIC_AUTH_PASSWORD", "")

		// Initialize Prometheus API
		var prometheusApi prometheusv1.API
		if prometheusEndpoint != "" {
			api, err := InitializePrometheusAPI(prometheusEndpoint, prometheusBasicAuthUsername, prometheusBasicAuthPassword)
			if err != nil {
				klog.Errorf("Error initializing Prometheus API: %v", err)
			} else {
				prometheusApi = api
				klog.Infof("Prometheus API initialized successfully")
			}
		}

		instance = Cache{
			initialized:       true,
			redisClient:       redisClient,
			prometheusApi:     prometheusApi,
			Pods:              map[string]*v1.Pod{},
			PodMetrics:        map[string]map[string]*metrics.MetricValue{},
			PodToModelMapping: map[string]map[string]struct{}{},
			ModelToPodMapping: map[string]map[string]*v1.Pod{},
			requestTrace:      map[string]map[string]int{},
		}

		if _, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addPod,
			UpdateFunc: instance.updatePod,
			DeleteFunc: instance.deletePod,
		}); err != nil {
			panic(err)
		}

		if _, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    instance.addModelAdapter,
			UpdateFunc: instance.updateModelAdapter,
			DeleteFunc: instance.deleteModelAdapter,
		}); err != nil {
			panic(err)
		}

		ticker := time.NewTicker(podMetricRefreshIntervalInMilliseconds * time.Millisecond)
		go func() {
			for {
				select {
				case <-ticker.C:
					instance.updatePodMetrics()
					instance.updateModelMetrics()
					instance.debugInfo()
				case <-stopCh:
					ticker.Stop()
					return
				}
			}
		}()

		traceTicker := time.NewTicker(writeRequestTraceIntervalInSeconds * time.Second)
		go func() {
			if redisClient == nil {
				return
			}
			for {
				select {
				case <-traceTicker.C:
					if len(instance.requestTrace) == 0 {
						continue
					}
					t := time.Now().Unix()
					roundT := t - t%writeRequestTraceIntervalInSeconds
					instance.writeRequestTraceToStorage(roundT)
				case <-stopCh:
					ticker.Stop()
					return
				}
			}
		}()
	})

	return &instance
}

func (c *Cache) addPod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	// only track pods with model deployments
	modelName, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}

	c.Pods[pod.Name] = pod
	c.addPodAndModelMapping(pod.Name, modelName)
	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()
}

func (c *Cache) updatePod(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPod := oldObj.(*v1.Pod)
	oldModelName, ok := oldPod.Labels[modelIdentifier]
	if !ok {
		return
	}

	newPod := newObj.(*v1.Pod)
	newModelName, ok := oldPod.Labels[modelIdentifier]
	if !ok {
		return
	}

	delete(c.Pods, oldPod.Name)
	c.Pods[newPod.Name] = newPod
	c.deletePodAndModelMapping(oldPod.Name, oldModelName)
	c.addPodAndModelMapping(newPod.Name, newModelName)
	klog.V(4).Infof("POD UPDATED. %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.debugInfo()
}

func (c *Cache) deletePod(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	pod := obj.(*v1.Pod)
	_, ok := pod.Labels[modelIdentifier]
	if !ok {
		return
	}

	// delete base model and associated lora models on this pod
	if models, ok := c.PodToModelMapping[pod.Name]; ok {
		for modelName := range models {
			c.deletePodAndModelMapping(pod.Name, modelName)
		}
	}
	delete(c.PodToModelMapping, pod.Name)
	delete(c.Pods, pod.Name)
	delete(c.PodMetrics, pod.Name)

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfo()
}

func (c *Cache) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMapping(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Cache) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)

	for _, pod := range oldModel.Status.Instances {
		c.deletePodAndModelMapping(pod, oldModel.Name)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMapping(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	c.debugInfo()
}

func (c *Cache) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMapping(pod, model.Name)
	}
	delete(c.ModelToPodMapping, model.Name)

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	c.debugInfo()
}

func (c *Cache) addPodAndModelMapping(podName, modelName string) {
	pod, ok := c.Pods[podName]
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	models, ok := c.PodToModelMapping[podName]
	if !ok {
		c.PodToModelMapping[podName] = map[string]struct{}{
			modelName: {},
		}
	} else {
		models[modelName] = struct{}{}
		c.PodToModelMapping[podName] = models
	}

	pods, ok := c.ModelToPodMapping[modelName]
	if !ok {
		c.ModelToPodMapping[modelName] = map[string]*v1.Pod{
			podName: pod,
		}
	} else {
		pods[podName] = pod
		c.ModelToPodMapping[modelName] = pods
	}
}

func (c *Cache) deletePodAndModelMapping(podName, modelName string) {
	if models, ok := c.PodToModelMapping[podName]; ok {
		delete(models, modelName)
		c.PodToModelMapping[podName] = models
	}

	if pods, ok := c.ModelToPodMapping[modelName]; ok {
		delete(pods, podName)
		c.ModelToPodMapping[modelName] = pods
	}
}

func (c *Cache) debugInfo() {
	for _, pod := range c.Pods {
		klog.V(4).Infof("pod: %s, podIP: %v", pod.Name, pod.Status.PodIP)
	}
	for podName, metrics := range c.PodMetrics {
		for metricName, metricVal := range metrics {
			klog.V(4).Infof("%v_%v_%v", podName, metricName, metricVal)
		}
	}
	for podName, models := range c.PodToModelMapping {
		var modelList string
		for modelName := range models {
			modelList += modelName + " "
		}
		klog.V(4).Infof("pod: %s, models: %s", podName, modelList)
	}
	for modelName, pods := range c.ModelToPodMapping {
		var podList string
		for podName := range pods {
			podList += podName + " "
		}
		klog.V(4).Infof("model: %s, pods: %s", modelName, podList)
	}
	for inputIndex, output := range c.requestTrace {
		for outputIndex, requestCount := range output {
			klog.V(4).Infof("inputIndex: %v, outputIndex: %v, requestCount: %v", inputIndex, outputIndex, requestCount)
		}
	}
}

func (c *Cache) GetPod(podName string) (*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	pod, ok := c.Pods[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return pod, nil
}

func (c *Cache) GetPods() map[string]*v1.Pod {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.Pods
}

func (c *Cache) GetPodsForModel(modelName string) (map[string]*v1.Pod, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podsMap, ok := c.ModelToPodMapping[modelName]
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return podsMap, nil
}

func (c *Cache) GetModelsForPod(podName string) (map[string]struct{}, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	models, ok := c.PodToModelMapping[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return models, nil
}

func (c *Cache) CheckModelExists(modelName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.ModelToPodMapping[modelName]

	return ok
}

func (c *Cache) GetPodMetric(podName, metricName string) (*metrics.MetricValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	metrics, ok := c.PodMetrics[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the metrics cache")
	}

	metricVal, ok := metrics[metricName]
	if !ok {
		return nil, fmt.Errorf("no metric available for %v", metricName)
	}

	return metricVal, nil
}

func (c *Cache) updatePodMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pod := range c.Pods {
		if pod.Status.PodIP == "" {
			continue
		}
		podName := pod.Name
		if len(c.PodMetrics[podName]) == 0 {
			c.PodMetrics[podName] = map[string]*metrics.MetricValue{}
		}

		// We should use the primary container port. In future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, podPort)
		resp, err := http.Get(url)
		if err != nil {
			klog.Errorf("failed to fetch metrics from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, podPort, err)
			continue
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				klog.Errorf("Error closing response body: %v", err)
			}
		}()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.Errorf("failed to read response from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, podPort, err)
			continue
		}

		// the metrics should come from those router subscribers

		// parse counterGaugeMetricsNames
		for _, metricName := range counterGaugeMetricNames {
			metricValue, err := parseMetricFromBody(body, metricName)
			if err != nil {
				klog.Errorf("failed to parse metrics from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			c.PodMetrics[pod.Name][metricName] = &metrics.MetricValue{Value: metricValue}
			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}

		// parse histogramMetrics
		for _, metricName := range histogramMetricNames {
			metricValue, err := parseHistogramFromBody(body, metricName)
			if err != nil {
				klog.Errorf("failed to parse metrics from pod %s %s %d: %v", pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			c.PodMetrics[pod.Name][metricName] = &metrics.MetricValue{Histogram: metricValue}
			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}

		for _, metricName := range prometheusMetricNames {
			modelName := pod.Labels["model.aibrix.ai/name"]
			queryLabels := map[string]string{
				"model_name": modelName,
				"instance":   fmt.Sprintf("%s/%d", pod.Status.PodIP, podPort),
			}
			metric, ok := metrics.Metrics[metricName]
			if !ok {
				klog.Warningf("Cannot find %v in the metric list", metricName)
				continue
			}
			query := BuildQuery(metric.PromQL, queryLabels)
			// Querying metrics
			result, warnings, err := c.prometheusApi.Query(context.Background(), query, time.Now())
			if err != nil {
				// Skip this model fetching if an error is thrown
				klog.Warningf("Error executing query: %v", err)
				continue
			}
			if len(warnings) > 0 {
				klog.Warningf("Warnings: %v\n", warnings)
			}

			klog.Infof("Query Result:%v\n", result)
			// Update metrics
			c.PodMetrics[pod.Name][metricName] = &metrics.MetricValue{PrometheusResult: &result}
		}
	}
}

func (c *Cache) updateModelMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, metricName := range prometheusMetricNames {
		for modelName := range c.ModelToPodMapping {
			// Ensure ModelMetrics is initialized
			if c.ModelMetrics == nil {
				c.ModelMetrics = make(map[string]map[string]interface{})
			}

			// Ensure the map for the specific modelName is initialized
			if c.ModelMetrics[modelName] == nil {
				c.ModelMetrics[modelName] = make(map[string]interface{})
			}

			queryLabels := map[string]string{
				"model_name": modelName,
			}
			metric, ok := metrics.Metrics[metricName]
			if !ok {
				klog.Warningf("Cannot find %v in the metric list", metricName)
				continue
			}
			query := BuildQuery(metric.PromQL, queryLabels)
			// Querying metrics
			result, warnings, err := c.prometheusApi.Query(context.Background(), query, time.Now())
			if err != nil {
				// Skip this model fetching if an error is thrown
				klog.Warningf("Error executing query: %v", err)
				continue
			}
			if len(warnings) > 0 {
				klog.Warningf("Warnings: %v\n", warnings)
			}

			klog.Infof("Query Result:%v\n", result)
			// Update metrics
			c.ModelMetrics[modelName][metricName] = result
		}
	}
}

// BuildQuery builds a PromQL query by dynamically injecting labels
// It replaces placeholders in the query (e.g., ${key}) with actual values from queryLabels
// and appends any additional labels to the query.
func BuildQuery(queryTemplate string, queryLabels map[string]string) string {
	// Regular expression to find placeholders like ${key}
	placeholderPattern := regexp.MustCompile(`\$\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

	// Replace placeholders with actual values from queryLabels
	queryWithReplacements := placeholderPattern.ReplaceAllStringFunc(queryTemplate, func(match string) string {
		// Extract the key from ${key}
		key := placeholderPattern.FindStringSubmatch(match)[1]
		if value, exists := queryLabels[key]; exists {
			return value // Replace ${key} with its value
		}
		return match // Keep the placeholder if no match in queryLabels
	})

	// Collect all labels from queryLabels not already in the query
	existingLabels := map[string]bool{}
	matches := placeholderPattern.FindAllStringSubmatch(queryTemplate, -1)
	for _, match := range matches {
		existingLabels[match[1]] = true
	}

	var additionalLabels []string
	for key, value := range queryLabels {
		if !existingLabels[key] {
			additionalLabels = append(additionalLabels, fmt.Sprintf(`%s="%s"`, key, value))
		}
	}

	// If there are additional labels, append them to the query
	if len(additionalLabels) > 0 {
		labels := strings.Join(additionalLabels, ",")
		if strings.Contains(queryWithReplacements, "{") {
			// Add to existing label set
			queryWithReplacements = strings.Replace(queryWithReplacements, "{", fmt.Sprintf("{%s,", labels), 1)
		} else {
			// Create a new label set
			queryWithReplacements = fmt.Sprintf("%s{%s}", queryWithReplacements, labels)
		}
	}

	return queryWithReplacements
}

func parseMetricFromBody(body []byte, metricName string) (float64, error) {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "#") && strings.Contains(line, metricName) {
			// format is `http_requests_total 1234.56`
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("unexpected format for metric %s", metricName)
			}

			// parse to float64
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse metric value for %s: %v", metricName, err)
			}

			return value, nil
		}
	}
	return 0, fmt.Errorf("metrics %s not found", metricName)
}

func parseHistogramFromBody(body []byte, metricName string) (*metrics.HistogramMetric, error) {
	lines := strings.Split(string(body), "\n")
	histogram := &metrics.HistogramMetric{
		Buckets: make(map[string]float64),
	}
	found := false

	for _, line := range lines {
		if strings.Contains(line, metricName+"_sum") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return nil, fmt.Errorf("unexpected format for metric %s_sum", metricName)
			}
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse sum for metric %s: %v", metricName, err)
			}
			histogram.Sum = value
			found = true
		} else if strings.Contains(line, metricName+"_count") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return nil, fmt.Errorf("unexpected format for metric %s_count", metricName)
			}
			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse count for metric %s: %v", metricName, err)
			}
			histogram.Count = value
			found = true
		} else if strings.Contains(line, metricName+"_bucket") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return nil, fmt.Errorf("unexpected format for bucket in metric %s", metricName)
			}

			// Extract the bucket boundary (le="...")
			bucketBoundary := extractBucketBoundary(line)
			if bucketBoundary == "" {
				return nil, fmt.Errorf("failed to extract bucket boundary for metric %s", metricName)
			}

			value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse bucket value for %s: %v", metricName, err)
			}
			histogram.Buckets[bucketBoundary] = value
			found = true
		}
	}

	if !found {
		return nil, fmt.Errorf("metrics %s not found", metricName)
	}
	return histogram, nil
}

func extractBucketBoundary(line string) string {
	// Extract `le="value"` from bucket lines
	startIndex := strings.Index(line, `le="`)
	if startIndex == -1 {
		return ""
	}
	startIndex += len(`le="`)
	endIndex := strings.Index(line[startIndex:], `"`)
	if endIndex == -1 {
		return ""
	}
	return line[startIndex : startIndex+endIndex]
}

func (c *Cache) AddRequestTrace(modelName string, inputTokens, outputTokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	inputIndex := int64(math.Round(math.Log2(float64(inputTokens)) / precisionRequestTrace)) // Round to the nearest precision and convert to int
	outputIndex := int64(math.Round(math.Log2(float64(outputTokens)) / precisionRequestTrace))

	klog.V(5).Infof("inputTokens: %v, inputIndex: %v, outputTokens: %v, outputIndex: %v",
		inputTokens, inputIndex, outputTokens, outputIndex)

	if len(c.requestTrace[modelName]) == 0 {
		c.requestTrace[modelName] = map[string]int{}
		c.requestTrace[modelName][keyWriteRequestTraceIntervalInSeconds] = writeRequestTraceIntervalInSeconds
		c.requestTrace[modelName][keyPrecisionRequestTrace] = int(1 / precisionRequestTrace)
		c.requestTrace[modelName][keyVersionRequestTrace] = versionRequestTrace
	}

	c.requestTrace[modelName][fmt.Sprintf("%v:%v", inputIndex, outputIndex)] += 1
}

func (c *Cache) writeRequestTraceToStorage(roundT int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	defer func() {
		klog.V(5).Infof("writeRequestTraceWithKey: %v", roundT)
		c.requestTrace = map[string]map[string]int{}
	}()

	for modelName, trace := range c.requestTrace {
		key := fmt.Sprintf("aibrix:%v_request_trace_%v", modelName, roundT)
		value, err := json.Marshal(trace)
		if err != nil {
			klog.ErrorS(err, "error to marshall request trace for redis set")
			continue
		}

		if _, err = c.redisClient.Set(context.Background(), key, value, expireWriteRequestTraceIntervalInMins*time.Minute).Result(); err != nil {
			klog.Error(err)
		}
	}
}

func (c *Cache) AddSubscriber(subscriber metrics.MetricSubscriber) {
	c.subscribers = append(c.subscribers, subscriber)
	c.aggregateMetrics()
}

func (c *Cache) aggregateMetrics() {
	for _, subscriber := range c.subscribers {
		for _, metric := range subscriber.SubscribedMetrics() {
			if _, exists := c.metrics[metric]; !exists {
				// TODO: refactor to
				c.metrics[metric] = "yes"
			}
		}
	}
}
