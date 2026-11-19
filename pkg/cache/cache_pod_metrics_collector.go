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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// podGaugeSpec supplies one gauge's descriptor and read-only value calculation.
// read must support concurrent scrapes without network I/O or state updates.
// A false ready result withholds only this gauge for the pod.
type podGaugeSpec struct {
	desc *prometheus.Desc
	read func(store *Store, pod *Pod, now time.Time) (value float64, ready bool)
}

// podGatewayMetricsCollector exports gauges for current pods using the shared
// gateway labels. Its gauge definitions are fixed at construction; each owns its
// readiness and freshness policy. It retains no historical series.
type podGatewayMetricsCollector struct {
	store      *Store
	gauges     []podGaugeSpec
	labelNames []string
}

func newPodGatewayMetricsCollector(store *Store) *podGatewayMetricsCollector {
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
	return &podGatewayMetricsCollector{
		store:      store,
		labelNames: labelNames,
		// Add a descriptor and read function here to expose another pod gauge.
		// Pod traversal, labels, registration and series lifecycle stay shared.
		gauges: []podGaugeSpec{{
			desc: prometheus.NewDesc(metrics.RealtimeNumRequestsRunning,
				"This gateway's estimate of in-flight requests for the target pod, including recent counts from other gateways",
				labelNames, nil),
			read: (*Store).runningRequestsEstimate,
		}},
	}
}

func (c *podGatewayMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, gauge := range c.gauges {
		ch <- gauge.desc
	}
}

func (c *podGatewayMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.store.snapshotNow()
	c.store.metaPods.Range(func(_ string, pod *Pod) bool {
		if pod == nil || pod.Pod == nil {
			return true
		}
		var labelValues []string
		for _, gauge := range c.gauges {
			value, ready := gauge.read(c.store, pod, now)
			if !ready {
				continue
			}
			if labelValues == nil {
				modelName, _ := getModelNameFromPod(pod.Pod)
				labels := metrics.DefaultMetricLabels(pod.Pod, &types.RoutingContext{Model: modelName})
				labelValues = make([]string, len(c.labelNames))
				for i, name := range c.labelNames {
					labelValues[i] = labels[name]
				}
			}
			metric, err := prometheus.NewConstMetric(gauge.desc, prometheus.GaugeValue, value, labelValues...)
			if err != nil {
				klog.ErrorS(err, "Skipping pod gauge with invalid labels", "metric", gauge.desc, "namespace", pod.Namespace, "pod", pod.Name)
				continue
			}
			ch <- metric
		}
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
