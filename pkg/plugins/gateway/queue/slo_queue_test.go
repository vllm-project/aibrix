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

package queue

import (
	"context"
	"math"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
)

type fakeOutputPredictor struct {
	reply int
}

func (f *fakeOutputPredictor) Predict(int) int {
	return f.reply
}
func (f *fakeOutputPredictor) AddTrace(inputTokens, outputTokens int, cnt int32) {
}

func newTestRequest(requestID string, predictor types.OutputPredictor) *types.RoutingContext {
	req := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("test"), "test-model", "hello world", requestID, "")
	req.SetOutputPreditor(predictor)
	return req
}

var _ = Describe("SLOQueue", func() {
	var (
		g       = &fakeOutputPredictor{reply: 100}
		profile = &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 0}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{E2E: 5.0},
		}
	)

	It("should map the test request onto the zero-throughput cell", func() {
		req := newTestRequest("req-1", g)
		features, err := req.Features()
		Expect(err).NotTo(HaveOccurred())
		signature, err := profile.GetSignature(features...)
		Expect(err).NotTo(HaveOccurred())
		Expect(signature).To(Equal([]int{1, 1}))
	})

	It("should return NaN when throughput is zero and the queue holds only the head request", func() {
		q := &SLOQueue{}
		sub := NewSimpleQueue[*types.RoutingContext](4)
		req := newTestRequest("req-1", g)
		Expect(sub.Enqueue(req, time.Now())).To(Succeed())
		rank, err := q.queueRank(time.Now(), req, sub, profile)
		Expect(math.IsNaN(rank)).To(BeTrue(), "expected rank to be NaN, got %v", rank)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should return +Inf when throughput is zero and other requests are queued", func() {
		q := &SLOQueue{}
		sub := NewSimpleQueue[*types.RoutingContext](4)

		req := newTestRequest("req-1", g)
		Expect(sub.Enqueue(req, time.Now())).To(Succeed())

		req1 := newTestRequest("req-2", g)
		Expect(sub.Enqueue(req1, time.Now())).To(Succeed())
		rank1, err := q.queueRank(time.Now(), req, sub, profile)
		Expect(math.IsInf(rank1, 1)).To(BeTrue(), "expected rank to be +Inf, got %v", rank1)
		Expect(err).NotTo(HaveOccurred())
	})
})
