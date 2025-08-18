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

package metrics

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

func TestProxy(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics Suite")
}

// MetricFetcherRecorder records url used for fetching metrics.
type MetricFetcherRecorder struct {
	RestMetricsFetcher

	url string
}

func NewMetricFetcherRecorder() *MetricFetcherRecorder {
	recorder := &MetricFetcherRecorder{}
	recorder.RestMetricsFetcher.testURLSetter = func(url string) {
		recorder.url = url
	}
	return recorder
}

func GetDomainMetricSource0() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "http",
		Endpoint:         "example.com",
		Path:             "/metrics",
	}
}

func GetDomainMetricSource1() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "http",
		Endpoint:         "example.com:8080",
		Path:             "/metrics",
	}
}

func GetDomainMetricSource2() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "https",
		Endpoint:         "example.com",
		Port:             "8000",
		Path:             "metrics",
	}
}

func GetDomainMetricSource3() autoscalingv1alpha1.MetricSource {
	return autoscalingv1alpha1.MetricSource{
		MetricSourceType: "domain",
		ProtocolType:     "https",
		Endpoint:         "example.com:8080",
		Port:             "8000",
		Path:             "metrics",
	}
}

var _ = Describe("KPAMetricsClient", func() {
	It("should client fetch metrics from correct url", func() {
		recorder := NewMetricFetcherRecorder()
		client := NewKPAMetricsClient(recorder, 0, 0)

		_, err := client.GetMetricFromSource(context.Background(), GetDomainMetricSource0())
		Expect(err).To(BeNil())
		Expect(recorder.url).To(Equal("http://example.com/metrics"))

		_, err = client.GetMetricFromSource(context.Background(), GetDomainMetricSource1())
		Expect(err).To(BeNil())
		Expect(recorder.url).To(Equal("http://example.com:8080/metrics"))

		_, err = client.GetMetricFromSource(context.Background(), GetDomainMetricSource2())
		Expect(err).To(BeNil())
		Expect(recorder.url).To(Equal("https://example.com:8000/metrics"))

		_, err = client.GetMetricFromSource(context.Background(), GetDomainMetricSource3())
		Expect(err).To(BeNil())
		Expect(recorder.url).To(Equal("https://example.com:8080/metrics"))
	})

})
