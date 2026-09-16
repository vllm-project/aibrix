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
	req.SetOutputPredictor(predictor)
	return req
}

var _ = Describe("SLOQueue", func() {
	var (
		predictor       = &fakeOutputPredictor{reply: 100}
		zeroTputProfile = &cache.ModelGPUProfile{
			// Indexes are stored in log2 space: ModelGPUProfile.Unmarshal converts them
			// when loading a JSON profile, and GetSignature compares log2(feature) against
			// them. A hand-built profile must supply them already converted, so {0, 1}
			// below means 1 and 2 tokens.
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 0}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{E2E: 5.0},
		}
		nonZeroTputProfile = &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{E2E: 5.0},
		}
	)

	It("should map the test request onto the zero-throughput cell", func() {
		req := newTestRequest("req-1", predictor)
		features, err := req.Features()
		Expect(err).NotTo(HaveOccurred())
		signature, err := zeroTputProfile.GetSignature(features...)
		Expect(err).NotTo(HaveOccurred())
		Expect(signature).To(Equal([]int{1, 1}))
	})

	It("should return ErrorSLOFailureRequest when the profile reports zero throughput", func() {
		q := &SLOQueue{}
		sub := NewSimpleQueue[*types.RoutingContext](4)
		req := newTestRequest("req-1", predictor)
		Expect(sub.Enqueue(req, time.Now())).To(Succeed())
		rank, err := q.queueRank(time.Now(), req, sub, zeroTputProfile)
		Expect(err).To(MatchError(cache.ErrorSLOFailureRequest))
		Expect(rank).To(BeZero())
	})

	It("should return a finite rank when the profile reports non-zero throughput", func() {
		q := &SLOQueue{}
		sub := NewSimpleQueue[*types.RoutingContext](4)

		req := newTestRequest("req-1", predictor)
		Expect(sub.Enqueue(req, time.Now())).To(Succeed())

		req1 := newTestRequest("req-2", predictor)
		Expect(sub.Enqueue(req1, time.Now())).To(Succeed())
		rank1, err := q.queueRank(req.RequestTime, req, sub, nonZeroTputProfile)
		Expect(err).NotTo(HaveOccurred())
		Expect(rank1).To(BeNumerically("~", -3.9, 0.01))
	})

	It("should return a negative rank when no time has elapsed and the profile meets the SLO", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)

		rank, err := q.rank(req.RequestTime, req, nonZeroTputProfile)
		Expect(err).NotTo(HaveOccurred())
		Expect(rank).To(BeNumerically("~", -4.0, 0.01))
	})

	It("should increase the rank by the elapsed time", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)

		rank, err := q.rank(req.RequestTime.Add(2*time.Second), req, nonZeroTputProfile)
		Expect(err).NotTo(HaveOccurred())
		Expect(rank).To(BeNumerically("~", -2.0, 0.01))
	})

	It("should return errNoSLO when the profile has no SLO configured", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)
		nonSLOProfile := &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
		}
		rank, err := q.rank(req.RequestTime, req, nonSLOProfile)
		Expect(err).To(MatchError(errNoSLO))
		Expect(rank).To(BeZero())
	})

	It("should prefer TPOT when all four SLOs are configured", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)
		profile := &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{TPOT: 0.1, TTFT: 0.5, TPAT: 0.01, E2E: 9},
		}
		_, expected, target, err := q.rankImpl(req.RequestTime, req, profile)
		Expect(err).NotTo(HaveOccurred())
		Expect(expected).To(BeNumerically("~", 1.0, 0.01))
		Expect(target).To(BeNumerically("~", 10.5, 0.01))
	})

	It("should fall back to TTFT when TPOT is not configured", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)
		profile := &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			TTFT:    [][]float64{{0.2, 0.2}, {0.2, 0.2}},
			SLOs:    cache.ModelSLOs{TTFT: 0.5, TPAT: 0.01, E2E: 9},
		}
		_, expected, target, err := q.rankImpl(req.RequestTime, req, profile)
		Expect(err).NotTo(HaveOccurred())
		Expect(expected).To(BeNumerically("~", 0.2, 0.01))
		Expect(target).To(BeNumerically("~", 0.5, 0.01))
	})

	It("should fall back to TPAT when neither TPOT nor TTFT is configured", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)
		profile := &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{TPAT: 0.01, E2E: 9},
		}
		_, expected, target, err := q.rankImpl(req.RequestTime, req, profile)
		Expect(err).NotTo(HaveOccurred())
		Expect(expected).To(BeNumerically("~", 1.0, 0.01))
		promptLen, err := req.PromptLength()
		Expect(err).NotTo(HaveOccurred())
		Expect(target).To(BeNumerically("~", 0.01*(float64(promptLen+100)), 0.01))
	})

	It("should use E2E when only E2E is configured", func() {
		q := &SLOQueue{}
		req := newTestRequest("req-1", predictor)
		profile := &cache.ModelGPUProfile{
			Indexes: [][]float64{{0, 1}, {0, 0.5}},
			Tputs:   [][]float64{{10, 10}, {10, 10}},
			E2E:     [][]float64{{1, 1}, {1, 1}},
			SLOs:    cache.ModelSLOs{E2E: 9},
		}
		_, _, target, err := q.rankImpl(req.RequestTime, req, profile)
		Expect(err).NotTo(HaveOccurred())
		Expect(target).To(BeNumerically("~", 9, 0.01))
	})
})
