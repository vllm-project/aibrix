/*
Copyright 2026 The Aibrix Team.

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
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

var podGatewayMetricLabelNames = func() []string {
	labels := metrics.DefaultMetricLabels(&v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{{}},
		},
	}, &types.RoutingContext{})
	labelNames := make([]string, 0, len(labels))
	for name := range labels {
		labelNames = append(labelNames, name)
	}
	sort.Strings(labelNames)
	return labelNames
}()

// gatewayMetricLabelValues caches labels on the immutable Pod metadata. Pod
// updates replace the cache object, so model, engine and role changes get new
// labels without retaining historical series. The once also supports concurrent
// first scrapes of pods constructed directly rather than through addPodLocked.
func (pod *Pod) gatewayMetricLabelValues() []string {
	pod.gatewayMetricLabelsOnce.Do(func() {
		modelName, _ := getModelNameFromPod(pod.Pod)
		labels := metrics.DefaultMetricLabels(pod.Pod, &types.RoutingContext{Model: modelName})
		pod.gatewayMetricLabels = make([]string, len(podGatewayMetricLabelNames))
		for i, name := range podGatewayMetricLabelNames {
			pod.gatewayMetricLabels[i] = labels[name]
		}
	})
	return pod.gatewayMetricLabels
}

// podGatewayMetricsCollector exports this gateway's running-request estimate
// for ready, non-quarantined pods. It retains no historical series.
type podGatewayMetricsCollector struct {
	store *Store
	desc  *prometheus.Desc
}

func newPodGatewayMetricsCollector(store *Store) *podGatewayMetricsCollector {
	return &podGatewayMetricsCollector{
		store: store,
		desc: prometheus.NewDesc(metrics.RealtimeNumRequestsRunning,
			"This gateway's estimate of in-flight requests for the target pod, including recent counts from other gateways",
			podGatewayMetricLabelNames, nil),
	}
}

func (c *podGatewayMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *podGatewayMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.store.snapshotNow()
	c.store.metaPods.Range(func(_ string, pod *Pod) bool {
		if pod == nil || pod.Pod == nil || !utils.FilterReadyPod(pod.Pod) || c.store.isPodQuarantined(pod.Pod) {
			return true
		}
		value, ready := c.store.runningRequestsEstimate(pod, now)
		if !ready {
			return true
		}
		metric, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, value, pod.gatewayMetricLabelValues()...)
		if err != nil {
			klog.ErrorS(err, "Skipping pod gauge with invalid labels", "metric", c.desc, "namespace", pod.Namespace, "pod", pod.Name)
			return true
		}
		ch <- metric
		return true
	})
}

func initPodGatewayMetricsCollector(store *Store, registerer prometheus.Registerer, stopCh <-chan struct{}) error {
	collector := newPodGatewayMetricsCollector(store)
	if err := registerer.Register(collector); err != nil {
		return err
	}
	if stopCh != nil {
		go func() {
			<-stopCh
			registerer.Unregister(collector)
		}()
	}
	return nil
}
