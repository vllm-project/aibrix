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
	"strconv"
	"sync"
	"sync/atomic"
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
	"github.com/aibrix/aibrix/pkg/utils"
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
	requestTrace      *sync.Map                                            // model_name: RequestTrace
	numRequestsTraces int32                                                // counter for requestTrace
	pendingRequests   *sync.Map                                            // model_name: *int32
}

type Block struct {
	modelToPods    map[string]map[string]time.Time // model_name: map[pod_name]pod_last_access_time
	lastAccessTime time.Time                       //block_last_access_time
}

const (
	modelIdentifier                       = "model.aibrix.ai/name"
	nodeType                              = "ray.io/node-type"
	nodeWorker                            = "worker"
	podPort                               = 8000
	defaultPodMetricRefreshIntervalInMS   = 50
	expireWriteRequestTraceIntervalInMins = 10
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

	// TODO: add a helper function for get methods.
	podMetricRefreshInterval = getPodMetricRefreshInterval()
)

func getPodMetricRefreshInterval() time.Duration {
	value := utils.LoadEnv("AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS", "")
	if value != "" {
		intValue, err := strconv.Atoi(value)
		if err != nil || intValue <= 0 {
			klog.Infof("invalid AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS: %s, falling back to default", value)
		} else {
			klog.Infof("using AIBRIX_POD_METRIC_REFRESH_INTERVAL_MS env value for pod metrics refresh interval: %d ms", intValue)
			return time.Duration(intValue) * time.Millisecond
		}
	}
	klog.Infof("using default refresh interval: %d ms", defaultPodMetricRefreshIntervalInMS)
	return defaultPodMetricRefreshIntervalInMS * time.Millisecond
}

func GetCache() (*Cache, error) {
	if !instance.initialized {
		return nil, errors.New("cache is not initialized")
	}
	return &instance, nil
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

		instance = Cache{
			initialized:       true,
			redisClient:       redisClient,
			prometheusApi:     prometheusApi,
			Pods:              map[string]*v1.Pod{},
			PodMetrics:        map[string]map[string]metrics.MetricValue{},
			PodModelMetrics:   map[string]map[string]map[string]metrics.MetricValue{},
			PodToModelMapping: map[string]map[string]struct{}{},
			ModelToPodMapping: map[string]map[string]*v1.Pod{},
			requestTrace:      &sync.Map{},
			pendingRequests:   &sync.Map{},
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

		ticker := time.NewTicker(podMetricRefreshInterval)
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

		tickerOffset := time.Duration(time.Now().UnixNano()) % RequestTraceWriteInterval
		var traceAlignmentTimer *time.Timer
		// TODO: Using ticker may be a problem if writeRequestTraceToStorage takes too long.
		var traceTicker *time.Ticker
		// To limit the offset of each tick, we align the ticker by waiting for a round
		// Very small offset usually will not be a problem, because it take some time to write to the Redis.
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
				// Wait for alignment
				<-traceAlignmentTimer.C
				traceAlignmentTimer = nil

				// TODO: Clean up data if necessary.

				// Start ticker
				traceTicker = time.NewTicker(RequestTraceWriteInterval)
			}
			klog.Infof("trace ticker start at %s", time.Now())
			for {
				select {
				case <-traceTicker.C:
					if atomic.LoadInt32(&instance.numRequestsTraces) == 0 {
						continue
					}
					t := time.Now().Unix()
					roundT := t - t%int64(RequestTraceWriteInterval/time.Second)
					instance.writeRequestTraceToStorage(roundT)
				case <-stopCh:
					traceTicker.Stop()
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
	// ignore worker pods
	nodeType, ok := pod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", pod.Name)
		return
	}

	c.Pods[pod.Name] = pod
	c.addPodAndModelMappingLocked(pod.Name, modelName)
	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfoLocked()
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

	// ignore worker pods
	nodeType, ok := oldPod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", oldPod.Name)
		return
	}

	// ignore worker pods
	nodeType, ok = newPod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", newPod.Name)
		return
	}

	// Add new mappings if present
	if newOk {
		c.Pods[newPod.Name] = newPod
		c.addPodAndModelMappingLocked(newPod.Name, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	c.debugInfoLocked()
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
	delete(c.Pods, pod.Name)
	delete(c.PodMetrics, pod.Name)
	delete(c.PodModelMetrics, pod.Name)

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	c.debugInfoLocked()
}

func (c *Cache) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMappingLocked(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	c.debugInfoLocked()
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
		c.addPodAndModelMappingLocked(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	c.debugInfoLocked()
}

func (c *Cache) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMapping(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	c.debugInfoLocked()
}

func (c *Cache) addPodAndModelMappingLocked(podName, modelName string) {
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
	c.mu.RLock()
	defer c.mu.RUnlock()

	c.debugInfoLocked()
}

func (c *Cache) debugInfoLocked() {
	for _, pod := range c.Pods {
		klog.V(4).Infof("pod: %s, podIP: %v", pod.Name, pod.Status.PodIP)
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
	for podName, metrics := range c.PodMetrics {
		for metricName, metricVal := range metrics {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
		}
	}
	for podName, models := range c.PodModelMetrics {
		for modelName, metrics := range models {
			for metricName, metricVal := range metrics {
				klog.V(5).Infof("%v_%v_%v_%v", podName, modelName, metricName, metricVal)
			}
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
// TODO: replace in-place metric update podMetrics and podModelMetrics to fresh copy for preventing stale metric keys
func (c *Cache) updatePodRecordLocked(podName string, modelName string, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue) error {
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

func (c *Cache) queryUpdatePromQLMetricsLocked(metric metrics.Metric, queryLabels map[string]string, podName string, modelName string, metricName string) error {
	scope := metric.MetricScope
	query := metrics.BuildQuery(metric.PromQL, queryLabels)
	// Querying metrics
	result, warnings, err := c.prometheusApi.Query(context.Background(), query, time.Now())
	if err != nil {
		// Skip this model fetching if an error is thrown
		return fmt.Errorf("error executing query: %v", err)
	}
	if len(warnings) > 0 {
		klog.V(4).Infof("Warnings: %v\n", warnings)
	}

	// Update metrics
	metricValue := &metrics.PrometheusMetricValue{Result: &result}
	err = c.updatePodRecordLocked(podName, modelName, metricName, scope, metricValue)
	if err != nil {
		return fmt.Errorf("failed to update metrics %s from prometheus %s: %v", metricName, podName, err)
	}
	klog.V(5).InfoS("Successfully parsed metrics from prometheus", "metric", metricName, "model", modelName, "PodName", podName, "Port", podPort, "metricValue", metricValue)
	return nil
}

func (c *Cache) updateSimpleMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range counterGaugeMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}

		// TODO: we should refact metricName to fit other engine
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")

			metricValue, err := metrics.GetCounterGaugeValue(familyMetric, metricFamily.GetType())
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			err = c.updatePodRecordLocked(podName, modelName, metricName, scope, &metrics.SimpleMetricValue{Value: metricValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}
	}
}

func (c *Cache) updateHistogramMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name
	for _, metricName := range histogramMetricNames {
		metric, exists := metrics.Metrics[metricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}

		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", metricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", metricName)
			continue
		}
		scope := metric.MetricScope
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			metricValue, err := metrics.GetHistogramValue(familyMetric)
			if err != nil {
				klog.V(4).Infof("failed to parse metrics %s from pod %s %s %d: %v", metricName, pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			histogramValue := &metrics.HistogramMetricValue{
				Sum:     metricValue.Sum,
				Count:   metricValue.Count,
				Buckets: metricValue.Buckets,
			}
			err = c.updatePodRecordLocked(podName, modelName, metricName, scope, histogramValue)
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)

		}
	}
}

func (c *Cache) updateQueryLabelMetricFromRawMetricsLocked(pod *v1.Pod, allMetrics map[string]*dto.MetricFamily) {
	podName := pod.Name

	for _, labelMetricName := range labelQueryMetricNames {
		metric, exists := metrics.Metrics[labelMetricName]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the metric list", labelMetricName)
			continue
		}
		rawMetricName := metric.RawMetricName
		scope := metric.MetricScope
		metricFamily, exists := allMetrics[fmt.Sprintf("vllm:%s", rawMetricName)]
		if !exists {
			klog.V(4).Infof("Cannot find %v in the pod metrics", rawMetricName)
			continue
		}
		for _, familyMetric := range metricFamily.Metric {
			modelName, _ := metrics.GetLabelValueForKey(familyMetric, "model_name")
			labelValue, _ := metrics.GetLabelValueForKey(familyMetric, labelMetricName)
			err := c.updatePodRecordLocked(podName, modelName, labelMetricName, scope, &metrics.LabelValueMetricValue{Value: labelValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", labelMetricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", labelMetricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", labelValue)
		}
	}
}

func (c *Cache) updateMetricFromPromQLLocked(pod *v1.Pod) {
	podName := pod.Name

	for _, metricName := range prometheusMetricNames {
		queryLabels := map[string]string{
			"instance": fmt.Sprintf("%s:%d", pod.Status.PodIP, podPort),
		}
		metric, ok := metrics.Metrics[metricName]
		if !ok {
			klog.V(4).Infof("Cannot find %v in the metric list", metricName)
			continue
		}
		scope := metric.MetricScope
		if scope == metrics.PodMetricScope {
			err := c.queryUpdatePromQLMetricsLocked(metric, queryLabels, podName, "", metricName)
			if err != nil {
				klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if modelNames, ok := c.PodToModelMapping[podName]; ok {
				for modelName := range modelNames {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetricsLocked(metric, queryLabels, podName, modelName, metricName)
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

func (c *Cache) updatePodMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	readyPods := utils.FilterReadyPods(c.Pods)
	if len(readyPods) == 0 {
		return
	}

	for _, pod := range readyPods {
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
			klog.V(4).Infof("Error parsing metric families: %v\n", err)
		}

		// parse counterGaugeMetricsNames
		c.updateSimpleMetricFromRawMetricsLocked(pod, allMetrics)

		// parse histogramMetrics
		c.updateHistogramMetricFromRawMetricsLocked(pod, allMetrics)

		// parse QueryLabel metrics
		c.updateQueryLabelMetricFromRawMetricsLocked(pod, allMetrics)

		if c.prometheusApi == nil {
			klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
			continue
		}
		// parse prometheus metrics
		c.updateMetricFromPromQLLocked(pod)
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

func (c *Cache) AddRequestCount(requestID string, modelName string) (traceTerm int64) {
	success := false
	for {
		trace := c.getRequestTrace(modelName)
		// TODO: use non-empty key if we have output prediction to decide buckets early.
		if traceTerm, success = trace.AddRequest(requestID, ""); success {
			break
		}
		// In case AddRequest return false, it has been recycled and we want to retry.
	}

	newPendingCounter := int32(0)
	pPendingCounter, _ := c.pendingRequests.LoadOrStore(modelName, &newPendingCounter)
	atomic.AddInt32(pPendingCounter.(*int32), 1)
	return
}

func (c *Cache) DoneRequestCount(requestID string, modelName string, traceTerm int64) {
	pPendingCounter, ok := c.pendingRequests.Load(modelName)
	if ok {
		atomic.AddInt32(pPendingCounter.(*int32), -1)
	}

	// DoneRequest only works for current term, no need to retry.
	c.getRequestTrace(modelName).DoneRequest(requestID, traceTerm)
}

func (c *Cache) AddRequestTrace(requestID string, modelName string, inputTokens, outputTokens int64) {
	traceKey := c.getTraceKey(inputTokens, outputTokens)
	for {
		trace := c.getRequestTrace(modelName)
		if trace.AddRequestTrace(requestID, traceKey) {
			break
		}
		// In case DoneRequest return false, it has been recycled and we want to retry.
	}
}

func (c *Cache) DoneRequestTrace(requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	pPendingCounter, ok := c.pendingRequests.Load(modelName)
	if ok {
		atomic.AddInt32(pPendingCounter.(*int32), -1)
	}

	traceKey := c.getTraceKey(inputTokens, outputTokens)
	for {
		trace := c.getRequestTrace(modelName)
		if trace.DoneRequestTrace(requestID, traceKey, traceTerm) {
			break
		}
		// In case DoneRequest return false, it has been recycled and we want to retry.
	}
}

func (c *Cache) getRequestTrace(modelName string) *RequestTrace {
	trace := NewRequestTrace(time.Now().UnixNano())
	newer, loaded := c.requestTrace.LoadOrStore(modelName, trace)
	if loaded {
		trace.Recycle()
	} else {
		atomic.AddInt32(&c.numRequestsTraces, 1)
	}
	return newer.(*RequestTrace)
}

func (c *Cache) getTraceKey(inputTokens, outputTokens int64) (traceKey string) {
	if inputTokens > 0 && outputTokens > 0 {
		inputIndex := int64(math.Round(math.Log2(float64(inputTokens)) / RequestTracePrecision)) // Round to the nearest precision and convert to int
		outputIndex := int64(math.Round(math.Log2(float64(outputTokens)) / RequestTracePrecision))
		traceKey = fmt.Sprintf("%v:%v", inputIndex, outputIndex)

		klog.V(5).Infof("inputTokens: %v, inputIndex: %v, outputTokens: %v, outputIndex: %v",
			inputTokens, inputIndex, outputTokens, outputIndex)
	}
	return
}

func (c *Cache) writeRequestTraceToStorage(roundT int64) {
	// Save and reset trace context, atomicity is guaranteed.
	var requestTrace *sync.Map
	numTraces := atomic.LoadInt32(&c.numRequestsTraces)
	requestTrace, c.requestTrace = c.requestTrace, &sync.Map{}
	numResetTo := int32(0)
	// TODO: Adding a unit test here.
	for !atomic.CompareAndSwapInt32(&c.numRequestsTraces, numTraces, numResetTo) {
		// If new traces added to reset map, assert updatedNumTraces >= numTraces regardless duplication.
		updatedNumTraces := atomic.LoadInt32(&c.numRequestsTraces)
		numTraces, numResetTo = updatedNumTraces, updatedNumTraces-numTraces
	}

	requestTrace.Range(func(iModelName, iTrace any) bool {
		modelName := iModelName.(string)
		trace := iTrace.(*RequestTrace)
		requestTrace.Store(modelName, nil) // Simply assign nil instead of delete

		trace.Lock()
		pending := int32(0)
		if pCounter, loaded := c.pendingRequests.Load(modelName); loaded {
			pending = atomic.LoadInt32(pCounter.(*int32))
		}
		traceMap := trace.ToMapLocked(pending)
		trace.RecycleLocked()
		trace.Unlock()

		value, err := json.Marshal(traceMap)
		if err != nil {
			klog.ErrorS(err, "error to marshall request trace for redis set")
			return true
		}

		key := fmt.Sprintf("aibrix:%v_request_trace_%v", modelName, roundT)
		if _, err = c.redisClient.Set(context.Background(), key, value, expireWriteRequestTraceIntervalInMins*time.Minute).Result(); err != nil {
			klog.Error(err)
		}
		return true
	})

	klog.V(5).Infof("writeRequestTraceWithKey: %v", roundT)
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
