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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	crdinformers "github.com/vllm-project/aibrix/pkg/client/informers/externalversions"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	dto "github.com/prometheus/client_model/go"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	v1alpha1 "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	v1alpha1scheme "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/scheme"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/client-go/kubernetes/scheme"
)

var once sync.Once

type ModelRouterProvider func(modelName string) (types.Router, error)

// type global
type Cache struct {
	mu                sync.RWMutex
	redisClient       *redis.Client
	prometheusApi     prometheusv1.API
	initialized       bool
	subscribers       []metrics.MetricSubscriber
	metrics           map[string]interface{}
	metaPods          utils.SyncMap[string, *Pod]           // pod_name: *PodMeta
	metaModels        utils.SyncMap[string, *Model]         // model_name: *Model
	requestTrace      *utils.SyncMap[string, *RequestTrace] // model_name: *RequestTrace
	numRequestsTraces int32                                 // counter for requestTrace

	// buffer for sync map operations
	bufferPod   *Pod
	bufferModel *Model
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

func NewTestCacheWithPods(pods []*v1.Pod) *Cache {
	c := &Cache{}
	for _, pod := range pods {
		pod.Labels = make(map[string]string)
		pod.Labels[modelIdentifier] = "modelName"
		c.addPod(pod)
	}
	return c
}

func NewTestCacheWithPodsMetrics(pods []*v1.Pod, podMetrics map[string]map[string]metrics.MetricValue) *Cache {
	c := NewTestCacheWithPods(pods)
	c.metaPods.Range(func(podName string, metaPod *Pod) bool {
		if metrics, ok := podMetrics[podName]; ok {
			for metricName, metric := range metrics {
				metaPod.Metrics.Store(metricName, metric)
			}
		}
		return true
	})
	return c
}

func NewCache(config *rest.Config, stopCh <-chan struct{}) *Cache {
	return NewGatewayCache(config, stopCh, nil)
}

func NewMetadataCache(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Cache {
	return NewGatewayCache(config, stopCh, redisClient)
}

func NewGatewayCache(config *rest.Config, stopCh <-chan struct{}, redisClient *redis.Client) *Cache {
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
			initialized:   true,
			redisClient:   redisClient,
			prometheusApi: prometheusApi,
			requestTrace:  &utils.SyncMap[string, *RequestTrace]{},
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
	pod := obj.(*v1.Pod)
	// only track pods with model deployments
	modelName, ok := pod.Labels[modelIdentifier]
	if !ok {
		// klog.InfoS("ignored pod without model label", "name", pod.Name)
		return
	}
	// ignore worker pods
	nodeType, ok := pod.Labels[nodeType]
	if ok && nodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "name", pod.Name)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	metaPod := c.addPodLocked(pod)
	c.addPodAndModelMappingLocked(metaPod, modelName)

	klog.V(4).Infof("POD CREATED: %s/%s", pod.Namespace, pod.Name)
	go c.debugInfo()
}

func (c *Cache) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	_, oldOk := oldPod.Labels[modelIdentifier]
	_, existed := c.metaPods.Load(oldPod.Name) // Make sure nothing left.
	newModelName, newOk := newPod.Labels[modelIdentifier]

	if !oldOk && !existed && !newOk {
		// klog.InfoS("ignored pod without model label", "old pod", oldPod.Name, "old pod existence", existed, "new pod", newPod.Name)
		return // No model information to track in either old or new pod
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// TODO: No in place update is handled here

	// Remove old mappings if present, no adapter will be inherited. (Adapters will be rescaned and readded later)
	if oldOk || existed {
		odlMetaPod := c.deletePodLocked(oldPod.Name)
		if odlMetaPod != nil {
			for _, modelName := range odlMetaPod.Models.Array() {
				c.deletePodAndModelMappingLocked(odlMetaPod.Name, modelName, 1)
			}
		}
	}

	// ignore worker pods
	oldNodeType := oldPod.Labels[nodeType]
	newNodeType := newPod.Labels[nodeType]
	if oldNodeType == nodeWorker || newNodeType == nodeWorker {
		klog.InfoS("ignored ray worker pod", "old pod", oldPod.Name, "new pod", newPod.Name)
		return
	}

	// Add new mappings if present
	if newOk {
		metaPod := c.addPodLocked(newPod)
		c.addPodAndModelMappingLocked(metaPod, newModelName)
	}

	klog.V(4).Infof("POD UPDATED: %s/%s %s", newPod.Namespace, newPod.Name, newPod.Status.Phase)
	go c.debugInfo()
}

func (c *Cache) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	_, ok := pod.Labels[modelIdentifier]
	_, existed := c.metaPods.Load(pod.Name) // Make sure nothing left.
	if !ok && !existed {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// delete base model and associated lora models on this pod
	metaPod := c.deletePodLocked(pod.Name)
	if metaPod != nil {
		for _, modelName := range metaPod.Models.Array() {
			c.deletePodAndModelMappingLocked(pod.Name, modelName, 1)
		}
	}

	klog.V(4).Infof("POD DELETED: %s/%s", pod.Namespace, pod.Name)
	go c.debugInfo()
}

func (c *Cache) addModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, model.Name)
	}

	klog.V(4).Infof("MODELADAPTER CREATED: %s/%s", model.Namespace, model.Name)
	go c.debugInfo()
}

func (c *Cache) updateModelAdapter(oldObj interface{}, newObj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldModel := oldObj.(*modelv1alpha1.ModelAdapter)
	newModel := newObj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range oldModel.Status.Instances {
		c.deletePodAndModelMappingLocked(pod, oldModel.Name, 0)
	}

	for _, pod := range newModel.Status.Instances {
		c.addPodAndModelMappingLockedByName(pod, newModel.Name)
	}

	klog.V(4).Infof("MODELADAPTER UPDATED. %s/%s %s", oldModel.Namespace, oldModel.Name, newModel.Status.Phase)
	go c.debugInfo()
}

func (c *Cache) deleteModelAdapter(obj interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	model := obj.(*modelv1alpha1.ModelAdapter)
	for _, pod := range model.Status.Instances {
		c.deletePodAndModelMappingLocked(pod, model.Name, 0)
	}

	klog.V(4).Infof("MODELADAPTER DELETED: %s/%s", model.Namespace, model.Name)
	go c.debugInfo()
}

func (c *Cache) addPodLocked(pod *v1.Pod) *Pod {
	if c.bufferPod == nil {
		c.bufferPod = &Pod{
			Pod:    pod,
			Models: NewRegistry[string](),
		}
	} else {
		c.bufferPod.Pod = pod
	}
	metaPod, loaded := c.metaPods.LoadOrStore(pod.Name, c.bufferPod)
	if !loaded {
		c.bufferPod = nil
	}
	return metaPod
}

func (c *Cache) addPodAndModelMappingLockedByName(podName, modelName string) {
	pod, ok := c.metaPods.Load(podName)
	if !ok {
		klog.Errorf("pod %s does not exist in internal-cache", podName)
		return
	}

	c.addPodAndModelMappingLocked(pod, modelName)
}

func (c *Cache) addPodAndModelMappingLocked(metaPod *Pod, modelName string) {
	if c.bufferModel == nil {
		c.bufferModel = &Model{
			Pods: NewRegistryWithArrayProvider(func(arr []*v1.Pod) *utils.PodArray { return &utils.PodArray{Pods: arr} }),
		}
	}
	metaModel, loaded := c.metaModels.LoadOrStore(modelName, c.bufferModel)
	if !loaded {
		c.bufferModel = nil
	}

	metaPod.Models.Store(modelName, modelName)
	metaModel.Pods.Store(metaPod.Name, metaPod.Pod)
}

func (c *Cache) deletePodLocked(podName string) *Pod {
	metaPod, _ := c.metaPods.LoadAndDelete(podName)
	return metaPod
}

// deletePodAndModelMapping delete mappings between pods and model by specified names.
// If ignoreMapping > 0, podToModel mapping will be ignored.
// If ignoreMapping < 0, modelToPod mapping will be ignored
func (c *Cache) deletePodAndModelMappingLocked(podName, modelName string, ignoreMapping int) {
	if ignoreMapping <= 0 {
		if metaPod, ok := c.metaPods.Load(podName); ok {
			metaPod.Models.Delete(modelName)
			// PodToModelMapping entry should only be deleted during pod deleting.
		}
	}

	if ignoreMapping >= 0 {
		if meta, ok := c.metaModels.Load(modelName); ok {
			meta.Pods.Delete(podName)
			if meta.Pods.Len() == 0 {
				c.metaModels.Delete(modelName)
			}
		}
	}
}

func (c *Cache) debugInfo() {
	c.metaPods.Range(func(podName string, pod *Pod) bool {
		klog.V(4).Infof("pod: %s, podIP: %v, models: %s", podName, pod.Status.PodIP, strings.Join(pod.Models.Array(), " "))
		pod.Metrics.Range(func(metricName string, metricVal metrics.MetricValue) bool {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
			return true
		})
		pod.ModelMetrics.Range(func(metricName string, metricVal metrics.MetricValue) bool {
			klog.V(5).Infof("%v_%v_%v", podName, metricName, metricVal)
			return true
		})
		return true
	})
	c.metaModels.Range(func(modelName string, meta *Model) bool {
		var podList strings.Builder
		for _, pod := range meta.Pods.Registry.Array() {
			podList.WriteString(pod.Name)
			podList.WriteByte(' ')
		}
		klog.V(4).Infof("model: %s, pods: %v", modelName, podList)
		return true
	})
}

func (c *Cache) GetPod(podName string) (*v1.Pod, error) {
	metaPod, ok := c.metaPods.Load(podName)
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return metaPod.Pod, nil
}

// Do not call this directly, for debug purpose and less efficient.
func (c *Cache) GetPods() []*v1.Pod {
	pods := make([]*v1.Pod, 0, c.metaPods.Len())
	c.metaPods.Range(func(_ string, metaPod *Pod) bool {
		pods = append(pods, metaPod.Pod)
		return true
	})
	return pods
}

func (c *Cache) GetPodsForModel(modelName string) (*utils.PodArray, error) {
	meta, ok := c.metaModels.Load(modelName)
	if !ok {
		return nil, fmt.Errorf("model does not exist in the cache: %s", modelName)
	}

	return meta.Pods.Array(), nil
}

func (c *Cache) GetModels() []string {
	return c.metaModels.Keys()
}

func (c *Cache) GetModelsForPod(podName string) ([]string, error) {
	metaPod, ok := c.metaPods.Load(podName)
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return metaPod.Models.Array(), nil
}

func (c *Cache) CheckModelExists(modelName string) bool {
	_, ok := c.metaModels.Load(modelName)

	return ok
}

func (c *Cache) GetPodMetric(podName, metricName string) (metrics.MetricValue, error) {
	metaPod, ok := c.metaPods.Load(podName)
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return c.getPodMetricImpl(podName, &metaPod.Metrics, metricName)
}

func (c *Cache) GetPodModelMetric(podName, modelName string, metricName string) (metrics.MetricValue, error) {
	metaPod, ok := c.metaPods.Load(podName)
	if !ok {
		return nil, fmt.Errorf("pod does not exist in the cache: %s", podName)
	}

	return c.getPodMetricImpl(podName, &metaPod.ModelMetrics, c.getPodModelMetricName(modelName, metricName))
}

func (c *Cache) getPodMetricImpl(podName string, metricStore *utils.SyncMap[string, metrics.MetricValue], metricName string) (metrics.MetricValue, error) {
	metricVal, ok := metricStore.Load(metricName)
	if !ok {
		return nil, fmt.Errorf("no metric available for %s - %v", podName, metricName)
	}

	return metricVal, nil
}

func (c *Cache) getPodModelMetricName(modelName string, metricName string) string {
	return fmt.Sprintf("%s/%s", modelName, metricName)
}

// Update `PodMetrics` and `PodModelMetrics` according to the metric scope
// TODO: replace in-place metric update podMetrics and podModelMetrics to fresh copy for preventing stale metric keys
func (c *Cache) updatePodRecord(pod *Pod, modelName string, metricName string, scope metrics.MetricScope, metricValue metrics.MetricValue) error {
	if scope == metrics.PodMetricScope {
		if modelName != "" {
			return fmt.Errorf("modelName should be empty for scope %v", scope)
		}
		pod.Metrics.Store(metricName, metricValue)
	} else if scope == metrics.PodModelMetricScope {
		if modelName == "" {
			return fmt.Errorf("modelName should not be empty for scope %v", scope)
		}
		pod.ModelMetrics.Store(c.getPodModelMetricName(modelName, metricName), metricValue)
	} else {
		return fmt.Errorf("scope %v is not supported", scope)
	}
	return nil
}

func (c *Cache) queryUpdatePromQLMetrics(metric metrics.Metric, queryLabels map[string]string, pod *Pod, modelName string, metricName string) error {
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
	err = c.updatePodRecord(pod, modelName, metricName, scope, metricValue)
	if err != nil {
		return fmt.Errorf("failed to update metrics %s from prometheus %s: %v", metricName, pod.Name, err)
	}
	klog.V(5).InfoS("Successfully parsed metrics from prometheus", "metric", metricName, "model", modelName, "PodName", pod.Name, "Port", podPort, "metricValue", metricValue)
	return nil
}

func (c *Cache) updateSimpleMetricFromRawMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily) {
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

			err = c.updatePodRecord(pod, modelName, metricName, scope, &metrics.SimpleMetricValue{Value: metricValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)
		}
	}
}

func (c *Cache) updateHistogramMetricFromRawMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily) {
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
			err = c.updatePodRecord(pod, modelName, metricName, scope, histogramValue)
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", metricName, podName, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", metricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", metricValue)

		}
	}
}

func (c *Cache) updateQueryLabelMetricFromRawMetrics(pod *Pod, allMetrics map[string]*dto.MetricFamily) {
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
			err := c.updatePodRecord(pod, modelName, labelMetricName, scope, &metrics.LabelValueMetricValue{Value: labelValue})
			if err != nil {
				klog.V(4).Infof("Failed to update metrics %s from pod %s %s %d: %v", labelMetricName, pod.Name, pod.Status.PodIP, podPort, err)
				continue
			}

			klog.V(5).InfoS("Successfully parsed metrics", "metric", labelMetricName, "model", modelName, "PodIP", pod.Status.PodIP, "Port", podPort, "metricValue", labelValue)
		}
	}
}

func (c *Cache) updateMetricFromPromQL(pod *Pod) {
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
			err := c.queryUpdatePromQLMetrics(metric, queryLabels, pod, "", metricName)
			if err != nil {
				klog.V(4).Infof("Failed to query and update PromQL metrics: %v", err)
				continue
			}
		} else if scope == metrics.PodModelMetricScope {
			if pod.Models.Len() > 0 {
				for _, modelName := range pod.Models.Array() {
					queryLabels["model_name"] = modelName
					err := c.queryUpdatePromQLMetrics(metric, queryLabels, pod, modelName, metricName)
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
	c.metaPods.Range(func(podName string, metaPod *Pod) bool {
		if !utils.FilterReadyPod(metaPod.Pod) {
			// Skip unready pod
			return true
		}

		// We should use the primary container port. In the future, we can decide whether to use sidecar container's port
		url := fmt.Sprintf("http://%s:%d/metrics", metaPod.Status.PodIP, podPort)
		allMetrics, err := metrics.ParseMetricsURL(url)
		if err != nil {
			klog.V(4).Infof("Error parsing metric families: %v\n", err)
		}

		// parse counterGaugeMetricsNames
		c.updateSimpleMetricFromRawMetrics(metaPod, allMetrics)

		// parse histogramMetrics
		c.updateHistogramMetricFromRawMetrics(metaPod, allMetrics)

		// parse QueryLabel metrics
		c.updateQueryLabelMetricFromRawMetrics(metaPod, allMetrics)

		if c.prometheusApi == nil {
			klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
			return true
		}
		// parse prometheus metrics
		c.updateMetricFromPromQL(metaPod)
		return true
	})
}

func (c *Cache) updateModelMetrics() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.prometheusApi == nil {
		klog.V(4).InfoS("Prometheus api is not initialized, PROMETHEUS_ENDPOINT is not configured, skip fetching prometheus metrics")
		return
	}
}

func (c *Cache) AddRequestCount(ctx *types.RoutingContext, requestID string, modelName string) (traceTerm int64) {
	success := false
	for {
		trace := c.getRequestTrace(modelName)
		// TODO: use non-empty key if we have output prediction to decide buckets early.
		if traceTerm, success = trace.AddRequest(requestID, ""); success {
			break
		}
		// In case AddRequest return false, it has been recycled and we want to retry.
	}

	meta, ok := c.metaModels.Load(modelName)
	if ok {
		atomic.AddInt32(&meta.pendingRequests, 1)
	}
	return
}

func (c *Cache) DoneRequestCount(ctx *types.RoutingContext, requestID string, modelName string, traceTerm int64) {
	meta, ok := c.metaModels.Load(modelName)
	if ok {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	// DoneRequest only works for current term, no need to retry.
	c.getRequestTrace(modelName).DoneRequest(requestID, traceTerm)
}

func (c *Cache) DoneRequestTrace(ctx *types.RoutingContext, requestID string, modelName string, inputTokens, outputTokens, traceTerm int64) {
	meta, ok := c.metaModels.Load(modelName)
	if ok {
		atomic.AddInt32(&meta.pendingRequests, -1)
	}

	var traceKey string
	for {
		trace := c.getRequestTrace(modelName)
		if traceKey, ok = trace.DoneRequestTrace(requestID, inputTokens, outputTokens, traceKey, traceTerm); ok {
			break
		}
		// In case DoneRequest return false, it has been recycled and we want to retry.
	}
	klog.V(5).Infof("inputTokens: %v, outputTokens: %v, trace key: %s", inputTokens, outputTokens, traceKey)
}

func (c *Cache) getRequestTrace(modelName string) *RequestTrace {
	trace := NewRequestTrace(time.Now().UnixNano())
	newer, loaded := c.requestTrace.LoadOrStore(modelName, trace)
	if loaded {
		trace.Recycle()
	} else {
		atomic.AddInt32(&c.numRequestsTraces, 1)
	}
	return newer
}

func (c *Cache) writeRequestTraceToStorage(roundT int64) {
	// Save and reset trace context, atomicity is guaranteed.
	var requestTrace *utils.SyncMap[string, *RequestTrace]
	numTraces := atomic.LoadInt32(&c.numRequestsTraces)
	requestTrace, c.requestTrace = c.requestTrace, &utils.SyncMap[string, *RequestTrace]{}
	numResetTo := int32(0)
	// TODO: Adding a unit test here.
	for !atomic.CompareAndSwapInt32(&c.numRequestsTraces, numTraces, numResetTo) {
		// If new traces added to reset map, assert updatedNumTraces >= numTraces regardless duplication.
		updatedNumTraces := atomic.LoadInt32(&c.numRequestsTraces)
		numTraces, numResetTo = updatedNumTraces, updatedNumTraces-numTraces
	}

	requestTrace.Range(func(modelName string, trace *RequestTrace) bool {
		requestTrace.Store(modelName, nil) // Simply assign nil instead of delete

		trace.Lock()
		pending := int32(0)
		if meta, loaded := c.metaModels.Load(modelName); loaded {
			pending = atomic.LoadInt32(&meta.pendingRequests)
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
