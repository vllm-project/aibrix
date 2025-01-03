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
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/aibrix/aibrix/pkg/utils"

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
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	dto "github.com/prometheus/client_model/go"
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
	PodMetrics        map[string]map[string]metrics.MetricValue            // pod_name: map[metric_name]metric_val
	PodModelMetrics   map[string]map[string]map[string]metrics.MetricValue // pod_name: map[model_name]map[metric_name]metric_val
	PodToModelMapping map[string]map[string]struct{}                       // pod_name: map[model_name]struct{}
	ModelToPodMapping map[string]map[string]*v1.Pod                        // model_name: map[pod_name]*v1.Pod
	requestTrace      map[string]map[string]int                            // model_name: map[Log2(input_token)-Log2(output_token)]request_count
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
		metrics.GPUCacheUsagePerc,
		metrics.CPUCacheUsagePerc,
		metrics.GPUCacheUsagePerc,
		metrics.CPUCacheUsagePerc,
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
			api, err := metrics.InitializePrometheusAPI(prometheusEndpoint, prometheusBasicAuthUsername, prometheusBasicAuthPassword)
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
			PodMetrics:        map[string]map[string]metrics.MetricValue{},
			PodModelMetrics:   map[string]map[string]map[string]metrics.MetricValue{},
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
	newPod := newObj.(*v1.Pod)

	oldModelName, oldOk := oldPod.Labels[modelIdentifier]
	newModelName, newOk := newPod.Labels[modelIdentifier]

	if !oldOk && !newOk {
		return // No model information to track in either old or new pod
	}

	// Remove old mappings if present
	if oldOk {
		delete(c.Pods, oldPod.Name)
		c.deletePodAndModelMapping(oldPod.Name, oldModelName)
	}

	// Add new mappings if present
	if newOk {
		c.Pods[newPod.Name] = newPod
		c.addPodAndModelMapping(newPod.Name, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
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
		if len(models) != 0 {
			c.PodToModelMapping[podName] = models
		} else {
			delete(c.PodToModelMapping, podName)
		}
	}

	if pods, ok := c.ModelToPodMapping[modelName]; ok {
		delete(pods, podName)
		if len(pods) != 0 {
			c.ModelToPodMapping[modelName] = pods
		} else {
			delete(c.ModelToPodMapping, modelName)
		}
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

func (c *Cache) GetPodMetric(podName, metricName string) (metrics.MetricValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podMetrics, ok := c.PodMetrics[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the podMetrics cache")
	}

	metricVal, ok := podMetrics[metricName]
	if !ok {
		return nil, fmt.Errorf("no metric available for %v", metricName)
	}

	return metricVal, nil
}

func (c *Cache) GetPodModelMetric(podName, modelName string, metricName string) (metrics.MetricValue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	podMetrics, ok := c.PodModelMetrics[podName]
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the podMetrics cache")
	}

	modelMetrics, ok := podMetrics[modelName]
	if !ok {
		return nil, fmt.Errorf("model does not exist in the podMetrics cache")
	}

	metricVal, ok := modelMetrics[metricName]
	if !ok {
		return nil, fmt.Errorf("no metric available for %v", metricName)
	}

	return metricVal, nil
}

// Update `PodMetrics` and `PodModelMetrics` according to the metric scope
func (c *Cache) updatePodRecord(podName string, modelName string, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue) error {
	if scope == metrics.PodMetricScope {
		if modelName != "" {
			return fmt.Errorf("modelName should be empty for scope %v", scope)
		}
		c.PodMetrics[podName][metricName] = metricValue
	} else if scope == metrics.PodModelMetricScope {
		if modelName == "" {
			return fmt.Errorf("modelName should not be empty for scope %v", scope)
		}
		if len(c.PodModelMetrics[podName][modelName]) == 0 {
			c.PodModelMetrics[podName][modelName] = map[string]metrics.MetricValue{}
		}
		c.PodModelMetrics[podName][modelName][metricName] = metricValue
	} else {
		return fmt.Errorf("scope %v is not supported", scope)
	}
	return nil
}

func (c *Cache) queryUpdatePromQLMetrics(metric metrics.Metric, queryLabels map[string]string, podName string, modelName string, metricName string) error {
	scope := metric.MetricScope
	query := metrics.BuildQuery(metric.PromQL, queryLabels)
	// Querying metrics
	result, warnings, err := c.prometheusApi.Query(context.Background(), query, time.Now())
	if err != nil {
		// Skip this model fetching if an error is thrown
		return fmt.Errorf("Error executing query: %v", err)
	}
	if len(warnings) > 0 {
		klog.Warningf("Warnings: %v\n", warnings)
	}

	klog.Infof("Query Result:%v\n", result)
	// Update metrics
	metricValue := &metrics.PrometheusMetricValue{Result: &result}
	err = c.updatePodRecord(podName, modelName, metricName, scope, metricValue)
	if err != nil {
		return fmt.Errorf("Failed to update metrics %s from prometheus %s: %v", metricName, podName, err)
	}
	klog.V(5).InfoS("Successfully parsed metrics from prometheus", "metric", metricName, "model", modelName, "PodName", podName, "Port", podPort, "metricValue", metricValue)
	return nil
}

func (c *Cache) updateSimpleMetricFromRawMetrics(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range counterGaugeMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.Warningf("Cannot find %v in the metric list", metricName)
			continue
		}

		// TODO: we should refact metricName to fit other engine
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.Warningf("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")

			metricValue, err := metrics.GetCounterGaugeValue(familyMetric, metricFamily.GetType())
			if err != nil {
				klog.Errorf("failed to parse metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			err = c.updatePodRecord(podName, modelName, metricName, scope, &metrics.SimpleMetricValue{Value: metricValue})
			if err != nil {
				klog.Errorf("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}
	}
}

func (c *Cache) updateHistogramMetricFromRawMetrics(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range histogramMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.Warningf("Cannot find %v in the metric list", metricName)
			continue
		}

		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.Warningf("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			metricValue, err := metrics.GetHistogramValue(familyMetric)
			if err != nil {
				klog.Errorf("failed to parse metrics %s from pod %s %s %d: %v", metricName, pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			histogramValue := &metrics.HistogramMetricValue{
				Sum:     metricValue.Sum,
				Count:   metricValue.Count,
				Buckets: metricValue.Buckets,
			}
			err = c.updatePodRecord(podName, modelName, metricName, scope, histogramValue)
			if err != nil {
				klog.Errorf("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)

		}
	}
}

func (c *Cache) updateQueryLabelMetricFromRawMetrics(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name

	for _, labelMetricName := range labelQueryMetricNames {
		metric, exists := metrics.Metrics[labelMetricName]
		if !exists {
			klog.Warningf("Cannot find %v in the metric list", labelMetricName)
			continue
		}
		rawMetricName := metric.RawMetricName
		scope := metric.MetricScope
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", rawMetricName)]
		if !exists {
			klog.Warningf("Cannot find %v in the pod metrics", rawMetricName)
			continue
		}
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			labelValue, _ := metrics.GetLabelValueForKey(familyMetric, labelMetricName)
			err := c.updatePodRecord(podName, modelName, labelMetricName, scope, &metrics.LabelValueMetricValue{Value: labelValue})
			if err != nil {
				klog.Errorf("Failed to update metrics %s from pod %s %s %d: %v", labelMetricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", labelMetricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", labelValue)
		}
	}
}

func (c *Cache) updateMetricFromPromQL(pod *v1.Pod) {
	podName := pod.Name

	for _, metricName := range prometheusMetricNames {
		queryLabels := map[string]string{
			"instance": fmt.Sprintf("%s:%d", pod.Status.PodIP, podPort),
		}
		metric, ok := metrics.Metrics[metricName]
		if !ok {
			klog.Warningf("Cannot find %v in the metric list", metricName)
			continue
		}
		scope := metric.MetricScope
		if scope == metrics.PodMetricScope {
			err := c.queryUpdatePromQLMetrics(metric, queryLabels, podName, "", metricName)
			if err != nil {
				klog.Errorf("Failed to query and update PromQL metrics: %v", err)
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if modelNames, ok := c.PodToModelMapping[podName]; ok {
				for modelName := range modelNames {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetrics(metric, queryLabels, podName, modelName, metricName)
					if err != nil {
						klog.Errorf("Failed to query and update PromQL metrics: %v", err)
						continue
					}
				}
			} else {
				klog.Warningf("Cannot find model names for pod %s", podName)
			}
		} else {
			klog.Warningf("Scope %v is not supported", scope)
		}
	}
}

func (c *Cache) updatePodMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, pod := range c.Pods {
		// Only scrape healthy Pod
		if pod.Status.PodIP == "" || utils.IsPodTerminating(pod) || !utils.IsPodReady(pod) {
			continue
		}
		podName := pod.Name
		if len(c.PodMetrics[podName]) == 0 {
			c.PodMetrics[podName] = map[string]metrics.MetricValue{}
		}
		if len(c.PodModelMetrics[podName]) == 0 {
			c.PodModelMetrics[podName] = make(map[string]map[string]metrics.MetricValue)
		}

		// We should use the primary container port. In the future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", pod.Status.PodIP, podPort)
		allMetrics, err := metrics.ParseMetricsURL(url)
		if err != nil {
			klog.Warningf("Error parsing metric families: %v\n", err)
		}

		// parse counterGaugeMetricsNames
		c.updateSimpleMetricFromRawMetrics(pod, allMetrics)

		// parse histogramMetrics
		c.updateHistogramMetricFromRawMetrics(pod, allMetrics)

		// parse QueryLabel metrics
		c.updateQueryLabelMetricFromRawMetrics(pod, allMetrics)

		if c.prometheusApi == nil {
			klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
			continue
		}
		// parse prometheus metrics
		c.updateMetricFromPromQL(pod)

	}
}

func (c *Cache) updateModelMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.prometheusApi == nil {
		klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
		return
	}

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
