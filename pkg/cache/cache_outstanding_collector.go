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
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// podOutstandingCollector reads the local request ledger at scrape time. It
// deliberately bypasses the metric cache, which can contain a cross-gateway
// estimate and stops refreshing when engine metric fetches fail.
type podOutstandingCollector struct {
	store *Store
	desc  *prometheus.Desc
}

func newPodOutstandingCollector(store *Store, gatewayName string) *podOutstandingCollector {
	return &podOutstandingCollector{
		store: store,
		desc: prometheus.NewDesc(
			"gateway_pod_outstanding_requests",
			"Requests routed by this gateway to a pod that have not completed.",
			[]string{"namespace", "pod", "model_name"},
			prometheus.Labels{"gateway_pod": gatewayName},
		),
	}
}

func (c *podOutstandingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *podOutstandingCollector) Collect(ch chan<- prometheus.Metric) {
	c.store.metaPods.Range(func(_ string, pod *Pod) bool {
		if pod == nil || pod.Pod == nil {
			return true
		}
		modelName, _ := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue,
			float64(atomic.LoadInt32(&pod.runningRequests)), pod.Namespace, pod.Name, modelName)
		return true
	})
}

func initPodOutstandingCollector(store *Store, registerer prometheus.Registerer, stopCh <-chan struct{}) error {
	collector := newPodOutstandingCollector(store, gatewayPodName)
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
