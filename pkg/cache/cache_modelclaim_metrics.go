/*
Copyright 2026 The AIBrix Authors.
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
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// A target's identity is its pointer. Recreating it on binding changes fences off
// old work even when an engine wakes at the same address.
type modelClaimMetricsTarget struct {
	pod     *v1.Pod
	binding utils.ModelClaimBinding
	records utils.SyncMap[string, MetricRecord]

	mu          sync.Mutex // Serializes publication with retirement. Never held during a scrape.
	retired     bool
	scheduled   bool
	failures    int
	nextFetchAt time.Time
	metricPod   *v1.Pod // Pod metadata with the engine type resolved for this binding.
	emitted     map[string]map[string]string
}

func (pod *Pod) modelMetricStore(model string) *utils.SyncMap[string, MetricRecord] {
	if target := pod.claimMetrics[model]; target != nil {
		return &target.records
	}
	return &pod.ModelMetrics
}

func (c *Store) isModelClaimMetricsPod(pod *v1.Pod) bool {
	if pod == nil || c.disableModelClaimBindings {
		return false
	}
	for key := range pod.Annotations {
		if strings.HasPrefix(key, constants.ModelClaimPodAnnotationPrefix) {
			return true
		}
	}
	if pod.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue {
		return false
	}
	// Pool membership alone does not replace a statically advertised model's
	// metrics endpoint.
	_, hasServingModel := getModelNameFromPod(pod)
	return !hasServingModel
}

// reconcileModelClaimMetrics runs under the informer lock before publishing the
// new cache Pod. Preserve sibling targets during routine Pod and binding updates.
func (c *Store) reconcileModelClaimMetrics(old *Pod, pod *v1.Pod, bindings map[string]utils.ModelClaimBinding) map[string]*modelClaimMetricsTarget {
	var targets map[string]*modelClaimMetricsTarget
	if c.isModelClaimMetricsPod(pod) {
		targets = make(map[string]*modelClaimMetricsTarget, len(bindings))
		samePod := old != nil && old.UID == pod.UID && old.Status.PodIP == pod.Status.PodIP &&
			maps.Equal(metrics.DefaultMetricLabels(old.Pod, nil), metrics.DefaultMetricLabels(pod, nil))
		for model, binding := range bindings {
			if binding.State != constants.ModelClaimRoutingStateActive || binding.Port <= 0 {
				continue
			}
			if samePod {
				previous := old.claimMetrics[model]
				if previous != nil && previous.binding.Claim == binding.Claim && previous.binding.Port == binding.Port {
					targets[model] = previous
					continue
				}
			}
			targets[model] = &modelClaimMetricsTarget{
				pod:       pod,
				binding:   binding,
				metricPod: pod,
				emitted:   make(map[string]map[string]string),
			}
		}
	}
	if old != nil {
		for model, target := range old.claimMetrics {
			if targets[model] != target {
				target.retire()
			}
		}
	}
	return targets
}

func (target *modelClaimMetricsTarget) retire() {
	target.mu.Lock()
	defer target.mu.Unlock()

	target.retired = true
	target.clearMetrics()
}

// clearMetrics is called with the target locked.
func (target *modelClaimMetricsTarget) clearMetrics() {
	ctx := &types.RoutingContext{Model: target.binding.Model}
	for name, labels := range target.emitted {
		metrics.DeleteEngineMetricFromPrometheus(ctx, target.metricPod, name, labels)
	}
	clear(target.emitted)
	target.records.Range(func(key string, _ MetricRecord) bool {
		target.records.Delete(key)
		return true
	})

	// Only these engine series are used as inputs to rolling model metrics.
	// History for gateway traffic and other bindings on the pod stays intact.
	prefix := rateHistoryPodPrefix(target.pod.Namespace, target.pod.Name) + target.binding.Model + "/"
	rateCalculator.mu.Lock()
	for name := range recentMeanHistograms {
		delete(rateCalculator.history, prefix+RecentMeanMetricName(name))
	}
	delete(rateCalculator.history, prefix+metrics.PromptTokenTotal)
	delete(rateCalculator.history, prefix+metrics.GenerationTokenTotal)
	rateCalculator.mu.Unlock()
}

func (target *modelClaimMetricsTarget) emitMetric(name string, value metrics.MetricValue, labels map[string]string) {
	ctx := &types.RoutingContext{Model: target.binding.Model}
	previousLabels, emitted := target.emitted[name]
	sameSeries := emitted && maps.Equal(previousLabels, labels)
	if emitted && !sameSeries {
		metrics.DeleteEngineMetricFromPrometheus(ctx, target.metricPod, name, previousLabels)
	}
	if metrics.Metrics[name].MetricType.Raw == metrics.Counter {
		// Engine counters are snapshots. The exporter adds increments, so use
		// the previous raw sample from this binding before the cache is updated.
		increment := value.GetSimpleValue()
		previous, sampled := target.records.Load(target.binding.Model + "/" + name)
		if sameSeries && sampled && increment >= previous.Value.GetSimpleValue() {
			increment -= previous.Value.GetSimpleValue()
		} else {
			// Start the series at the current value after a counter reset or
			// when the binding first publishes this series.
			metrics.DeleteEngineMetricFromPrometheus(ctx, target.metricPod, name, labels)
		}
		value = &metrics.SimpleMetricValue{Value: increment}
	}
	metrics.EmitMetricToPrometheus(ctx, target.metricPod, name, value, labels)
	target.emitted[name] = labels
}

func (c *Store) enqueueModelClaimMetrics(pod *Pod, target *modelClaimMetricsTarget, now time.Time) {
	target.mu.Lock()
	defer target.mu.Unlock()

	switch {
	case target.retired || pod.Status.PodIP == "":
		return
	case now.Before(target.nextFetchAt):
		metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonBackoff)
	case target.scheduled:
		metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonAlreadyQueued)
	default:
		select {
		case c.podMetricsJobs <- podMetricsJob{Pod: pod, target: target}:
			target.scheduled = true
		default:
			metrics.IncrementPodMetricsEnqueueDropped(metrics.PodMetricsDropReasonQueueFull)
		}
	}
}

func (c *Store) currentModelClaimMetricsPod(target *modelClaimMetricsTarget) *Pod {
	pod, ok := c.metaPods.Load(utils.GeneratePodKey(target.pod.Namespace, target.pod.Name))
	if !ok || pod.claimMetrics[target.binding.Model] != target ||
		!utils.FilterReadyPod(pod.Pod) || c.isPodQuarantined(pod.Pod) {
		return nil
	}
	return pod
}

func (c *Store) scrapeModelClaimMetrics(queuedPod *Pod, target *modelClaimMetricsTarget) {
	var result *metrics.EngineMetricsResult
	var err error
	target.mu.Lock()
	engineType := metrics.GetEngineType(*target.metricPod)
	target.mu.Unlock()
	if c.currentModelClaimMetricsPod(target) != nil {
		ctx, cancel := context.WithTimeout(context.Background(), podMetricsFetchTimeout)
		defer cancel()
		endpoint := net.JoinHostPort(queuedPod.Status.PodIP, strconv.Itoa(target.binding.Port))
		result, err = c.engineMetricsFetcher.FetchModelTypedMetrics(ctx, endpoint, engineType, queuedPod.Name,
			target.binding.Model, c.getAllAvailableMetrics())
	}
	fetchedAt := time.Now()

	target.mu.Lock()
	defer target.mu.Unlock()
	target.scheduled = false
	pod := c.currentModelClaimMetricsPod(target)
	if target.retired || pod == nil {
		return
	}
	if err != nil {
		target.failures++
		target.nextFetchAt = fetchedAt.Add(podMetricsBackoffDelay(target.failures))
		metrics.IncrementPodMetricsFetchFailure(engineType, metrics.PodMetricsFetchFailureReasonFetchError)
		klog.V(4).InfoS("Failed to fetch ModelClaim engine metrics", "pod", klog.KObj(pod.Pod),
			"model", target.binding.Model, "port", target.binding.Port, "error", err)
		return
	}
	if result == nil {
		return
	}
	target.failures = 0
	target.nextFetchAt = time.Time{}
	if result.EngineType != engineType {
		target.clearMetrics()
		target.metricPod = target.pod.DeepCopy()
		if target.metricPod.Labels == nil {
			target.metricPod.Labels = make(map[string]string)
		}
		target.metricPod.Labels[constants.ModelLabelEngine] = result.EngineType
	}

	// Binding lifecycle events retire claim metrics as soon as the route stops
	// serving. The ordinary worker's KV-event sleep backstop uses the model
	// identity in Pod metadata, which does not identify a bound engine.
	for key, value := range result.ModelMetrics {
		model, name := parseModelMetricKey(key)
		sanitizeMetricValueLabels(pod, value)
		if !shouldSkipMetric(pod.Pod, name) {
			c.updateThroughputToksPerS(pod, model, name, value)
			c.updateRecentMean(pod, model, name, value)
			target.emitMetric(name, value, value.GetLabelValues())
		}
		target.records.Store(key, MetricRecord{Value: value, Timestamp: fetchedAt})
	}
	for _, err := range result.Errors {
		klog.V(4).InfoS("ModelClaim metric parsing error", "pod", klog.KObj(pod.Pod),
			"model", target.binding.Model, "error", err)
	}
}
