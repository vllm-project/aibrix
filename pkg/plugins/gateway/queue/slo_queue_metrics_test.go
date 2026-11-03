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

package queue

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

type capturedCounterEmission struct {
	name        string
	value       float64
	labelNames  []string
	labelValues []string
}

// counterCapture records counter emissions while the test hook is installed.
type counterCapture struct {
	mu        sync.Mutex
	emissions []capturedCounterEmission
}

func startCounterCapture() (*counterCapture, func()) {
	capture := &counterCapture{}
	original := metrics.IncrementCounterMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name string, _ string, value float64, labelNames []string, labelValues ...string) {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.emissions = append(capture.emissions, capturedCounterEmission{
			name:        name,
			value:       value,
			labelNames:  append([]string(nil), labelNames...),
			labelValues: append([]string(nil), labelValues...),
		})
	}
	return capture, func() { metrics.IncrementCounterMetricFnForTest = original }
}

// labelValues returns every value emitted for one label of one metric.
func (c *counterCapture) labelValues(metricName, labelName string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var values []string
	for _, e := range c.emissions {
		if e.name != metricName {
			continue
		}
		for i, name := range e.labelNames {
			if name == labelName && i < len(e.labelValues) {
				values = append(values, e.labelValues[i])
			}
		}
	}
	return values
}

var _ = Describe("SLOQueue FIFO fallback metrics", func() {
	const model = "test-model"

	BeforeEach(func() {
		st := cache.InitForTest()
		// Hand-built profiles store indexes in log2 space, as in the other
		// SLOQueue specs: output buckets split at 1 and 8 tokens.
		indexes := [][]float64{{0, 3}, {0}}
		goodProfile := &cache.ModelGPUProfile{
			Deployment: "dep-good",
			Indexes:    indexes,
			E2E:        [][]float64{{1.0}, {5.0}},
			SLOs:       cache.ModelSLOs{E2E: 5.0},
		}
		noSLOProfile := &cache.ModelGPUProfile{
			Deployment: "dep-noslo",
			Indexes:    indexes,
			E2E:        [][]float64{{1.0}, {5.0}},
		}
		st.UpdateModelProfile(cache.ModelGPUProfileKey(model, "dep-good"), goodProfile, true)
		st.UpdateModelProfile(cache.ModelGPUProfileKey(model, "dep-noslo"), noSLOProfile, true)
	})

	It("should report no_profile when no deployment carries a profile", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"single": newRankedTestRequest("req-noprofile", 2, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-missing"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())

		capture, stop := startCounterCapture()
		defer stop()

		dequeued, err := q.Dequeue(time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(dequeued).To(BeIdenticalTo(picked))

		Expect(capture.labelValues(metrics.GatewayQueueFIFOFallbackTotal, "reason")).To(Equal([]string{"no_profile"}))
	})

	It("should report no_slo_info when available profiles carry no SLO information", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"single": newRankedTestRequest("req-noslo", 2, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-noslo"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())

		capture, stop := startCounterCapture()
		defer stop()

		dequeued, err := q.Dequeue(time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(dequeued).To(BeIdenticalTo(picked))

		Expect(capture.labelValues(metrics.GatewayQueueFIFOFallbackTotal, "reason")).To(Equal([]string{"no_slo_info"}))
	})

	It("should not report a fallback when a ranked candidate is dequeued", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"single": newRankedTestRequest("req-ranked", 2, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-good"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.HasRouted()).To(BeTrue())

		capture, stop := startCounterCapture()
		defer stop()

		dequeued, err := q.Dequeue(time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(dequeued).To(BeIdenticalTo(picked))

		Expect(capture.labelValues(metrics.GatewayQueueFIFOFallbackTotal, "reason")).To(BeEmpty())
	})

	It("should label a fallback from the entry, not from a recycled routing context", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"single": newRankedTestRequest("req-recycled", 2, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-missing"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())

		// Replay what requestPool does between two requests while the entry is still
		// queued: the context is reset for a different request.
		types.RecycleRoutingContextForTest(picked.RoutingContext, context.Background(),
			types.RoutingAlgorithm("test"), "recycled-model", "hello world", "req-recycled-next", "")

		capture, stop := startCounterCapture()
		defer stop()

		dequeued, err := q.Dequeue(time.Now())
		Expect(err).NotTo(HaveOccurred())
		Expect(dequeued).To(BeIdenticalTo(picked))

		Expect(capture.labelValues(metrics.GatewayQueueFIFOFallbackTotal, "reason")).To(Equal([]string{"no_profile"}))
		Expect(capture.labelValues(metrics.GatewayQueueFIFOFallbackTotal, "model")).To(Equal([]string{model}))
	})
})
