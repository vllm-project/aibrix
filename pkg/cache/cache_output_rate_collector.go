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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// podOutputRateCollector exports, at scrape time, the per-request output-token rate this gateway
// measured for each pod, how old that estimate is, and the share of the pod's requests that
// finished without output tokens. It reads the pod records, so a deleted pod leaves no series,
// and a pod without an estimate has none.
type podOutputRateCollector struct {
	store    *Store
	now      func() time.Time
	rate     *prometheus.Desc
	rateAge  *prometheus.Desc
	fraction *prometheus.Desc
}

func newPodOutputRateCollector(store *Store, gatewayName string) *podOutputRateCollector {
	labels := []string{"target_namespace", "target_pod", "model"}
	constLabels := prometheus.Labels{"gateway_pod": gatewayName}
	return &podOutputRateCollector{
		store: store,
		now:   time.Now,
		rate: prometheus.NewDesc(
			"gateway_pod_output_tokens_per_request_second",
			"Output tokens per second of one request's time on the pod, over requests this gateway routed (EWMA of ~1-minute windows). Not updated while the pod completes too few requests; see gateway_pod_output_rate_age_seconds.",
			labels, constLabels),
		rateAge: prometheus.NewDesc(
			"gateway_pod_output_rate_age_seconds",
			"Seconds since gateway_pod_output_tokens_per_request_second was last updated for the pod.",
			labels, constLabels),
		fraction: prometheus.NewDesc(
			"gateway_pod_requests_without_output_tokens_fraction",
			"Share of the requests this gateway routed to the pod that finished without output tokens, in the last ~1-minute window in which any finished. They are not part of gateway_pod_output_tokens_per_request_second.",
			labels, constLabels),
	}
}

func (c *podOutputRateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.rate
	ch <- c.rateAge
	ch <- c.fraction
}

func (c *podOutputRateCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	c.store.metaPods.Range(func(_ string, pod *Pod) bool {
		if pod == nil || pod.Pod == nil {
			return true
		}
		modelName, _ := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
		emit := func(desc *prometheus.Desc, value float64) {
			metric, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, pod.Namespace, pod.Name, modelName)
			if err != nil {
				ch <- prometheus.NewInvalidMetric(desc, err)
				return
			}
			ch <- metric
		}
		if record, ok := pod.Metrics.Load(metrics.RealtimeOutputTokensPerRequestSecondEWMA); ok {
			emit(c.rate, record.Value.GetSimpleValue())
			emit(c.rateAge, now.Sub(record.Timestamp).Seconds())
		}
		if record, ok := pod.Metrics.Load(metrics.RealtimeRequestsWithoutOutputTokensFraction1m); ok {
			emit(c.fraction, record.Value.GetSimpleValue())
		}
		return true
	})
}

func initPodOutputRateCollector(store *Store, registerer prometheus.Registerer, stopCh <-chan struct{}) error {
	collector := newPodOutputRateCollector(store, gatewayPodName)
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
