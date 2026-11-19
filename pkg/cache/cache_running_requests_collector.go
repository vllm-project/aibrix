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
)

// podRunningRequestsCollector exports only current pods with a fresh computed
// cross-gateway estimate. Unlike a GaugeVec, it retains no historical series.
type podRunningRequestsCollector struct {
	store      *Store
	desc       *prometheus.Desc
	labelNames []string
	maxAge     time.Duration
	now        func() time.Time
}

func newPodRunningRequestsCollector(store *Store) *podRunningRequestsCollector {
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
	return &podRunningRequestsCollector{
		store:      store,
		desc:       prometheus.NewDesc(metrics.RealtimeNumRequestsRunning, metrics.GetMetricHelp(metrics.RealtimeNumRequestsRunning), labelNames, nil),
		labelNames: labelNames,
		// Allow several refresh cycles and one fetch timeout before withholding
		// an estimate. This does not change the routing cache's freshness policy.
		maxAge: max(5*time.Second, 3*podMetricRefreshInterval, podMetricsFetchTimeout+podMetricRefreshInterval),
		now:    time.Now,
	}
}

func (c *podRunningRequestsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *podRunningRequestsCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	c.store.metaPods.Range(func(_ string, pod *Pod) bool {
		if pod == nil || pod.Pod == nil {
			return true
		}
		record, ok := pod.Metrics.Load(metrics.RealtimeNumRequestsRunning)
		if !ok || record.Value == nil || record.Timestamp.IsZero() || now.Sub(record.Timestamp) > c.maxAge {
			return true
		}
		modelName, _ := getModelNameFromPod(pod.Pod)
		labels := metrics.DefaultMetricLabels(pod.Pod, &types.RoutingContext{Model: modelName})
		labelValues := make([]string, len(c.labelNames))
		for i, name := range c.labelNames {
			labelValues[i] = labels[name]
		}
		metric, err := prometheus.NewConstMetric(c.desc, prometheus.GaugeValue, record.Value.GetSimpleValue(), labelValues...)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(c.desc, err)
			return true
		}
		ch <- metric
		return true
	})
}

func initPodRunningRequestsCollector(store *Store, registerer prometheus.Registerer, stopCh <-chan struct{}) error {
	collector := newPodRunningRequestsCollector(store)
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
