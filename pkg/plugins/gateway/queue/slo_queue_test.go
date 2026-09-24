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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

type fakePodList struct{ deployments []string }

func (p fakePodList) Len() int                          { return 0 }
func (p fakePodList) All() []*corev1.Pod                { return nil }
func (p fakePodList) Indexes() []string                 { return p.deployments }
func (p fakePodList) ListByIndex(string) []*corev1.Pod  { return nil }
func (p fakePodList) ListPortsForPod() map[string][]int { return nil }

type fakeRouter struct{}

func (fakeRouter) Route(ctx *types.RoutingContext, _ types.PodList) (string, error) {
	ctx.SetTargetPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fake-target", Namespace: "default"}})
	return "10.0.0.1:8000", nil
}

func newRankedTestRequest(requestID string, predictedOutput int, age time.Duration) *types.RoutingContext {
	return rankedRequestAt(requestID, predictedOutput, time.Now(), age)
}

func newTestSLOQueue(model string, requests map[string]*types.RoutingContext) *SLOQueue {
	provider := func(*types.RoutingContext) (types.Router, error) { return fakeRouter{}, nil }
	return newTestSLOQueueWithProvider(model, requests, provider)
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
		sub := NewSimpleQueue[*types.QueueEntry](4)
		req := newTestRequest("req-1", predictor)
		Expect(sub.Enqueue(types.NewQueueEntry(req, time.Now()), time.Now())).To(Succeed())
		rank, err := q.queueRank(time.Now(), req, sub, zeroTputProfile)
		Expect(err).To(MatchError(cache.ErrorSLOFailureRequest))
		Expect(rank).To(BeZero())
	})

	It("should return a finite rank when the profile reports non-zero throughput", func() {
		q := &SLOQueue{}
		sub := NewSimpleQueue[*types.QueueEntry](4)

		req := newTestRequest("req-1", predictor)
		Expect(sub.Enqueue(types.NewQueueEntry(req, time.Now()), time.Now())).To(Succeed())

		req1 := newTestRequest("req-2", predictor)
		Expect(sub.Enqueue(types.NewQueueEntry(req1, time.Now()), time.Now())).To(Succeed())
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
		promptLen, promptLenErr := req.PromptLength()
		Expect(promptLenErr).NotTo(HaveOccurred())
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

	It("should require Peek before Dequeue", func() {
		q := &SLOQueue{}

		req, err := q.Dequeue(time.Now())
		Expect(err).To(MatchError("call SLOQueue.Peek first"))
		Expect(req).To(BeNil())
	})

	It("should fail closed when the peeked subqueue is gone", func() {
		q := &SLOQueue{lastCandidateSubKey: "missing-sub"}

		req, err := q.Dequeue(time.Now())
		Expect(err).To(MatchError("subqueue missing-sub not found"))
		Expect(req).To(BeNil())

		// The failed dequeue still consumes the Peek state, so the next call goes
		// back to the Peek-first guard.
		req, err = q.Dequeue(time.Now())
		Expect(err).To(MatchError("call SLOQueue.Peek first"))
		Expect(req).To(BeNil())
	})

	It("should return the SLO routing error recorded by Peek", func() {
		q := &SLOQueue{lastCandidateError: cache.ErrorSLOFailureRequest}
		req := newTestRequest("req-1", predictor)

		address, err := q.Route(req, nil)
		Expect(err).To(MatchError(cache.ErrorSLOFailureRequest))
		Expect(address).To(BeEmpty())
	})

	It("should group requests into the same key when features fall in the same log2 bucket", func() {
		q := &SLOQueue{}

		f1 := types.RequestFeatures{100, 100}
		b1 := q.featuresKey(f1)
		Expect(f1).To(Equal(types.RequestFeatures{100, 100}))

		f2 := types.RequestFeatures{120, 120}
		b2 := q.featuresKey(f2)
		Expect(f2).To(Equal(types.RequestFeatures{120, 120}))

		f3 := types.RequestFeatures{1000, 1000}
		b3 := q.featuresKey(f3)
		Expect(f3).To(Equal(types.RequestFeatures{1000, 1000}))

		Expect(b1).To(Equal(b2))
		Expect(b1).NotTo(Equal(b3))
	})

	It("should order candidates by rank, then arrival time, then subqueue key", func() {
		now := time.Now()
		newCandidate := func(subKey string, rank float64, requestTime time.Time) *candidateRouterRequest {
			req := newTestRequest("req-"+subKey, predictor)
			req.RequestTime = requestTime
			return &candidateRouterRequest{
				QueueEntry: types.NewQueueEntry(req, requestTime),
				SubKey:     subKey,
				Profiles:   []*candidateProfiles{{Rank: rank, Key: "dep-a"}},
			}
		}

		q := &SLOQueue{}
		higher := newCandidate("c", 1.0, now)
		early := newCandidate("a", 0.0, now.Add(-2*time.Second))
		late := newCandidate("b", 0.0, now.Add(-time.Second))
		Expect(q.candidateLess(higher, early)).To(BeTrue())
		Expect(q.candidateLess(early, higher)).To(BeFalse())
		Expect(q.candidateLess(early, late)).To(BeTrue())
		Expect(q.candidateLess(late, early)).To(BeFalse())

		// Same rank and same arrival time: the subqueue key keeps the order total.
		sameTime := now.Add(-3 * time.Second)
		keyA := newCandidate("a", 0.0, sameTime)
		keyB := newCandidate("b", 0.0, sameTime)
		Expect(q.candidateLess(keyA, keyB)).To(BeTrue())
		Expect(q.candidateLess(keyB, keyA)).To(BeFalse())
	})

})

var _ = Describe("SLOQueue Peek failure isolation", func() {
	const model = "test-model"

	BeforeEach(func() {
		// Hand-built profiles store indexes in log2 space: output buckets split at 1 and 8 tokens.
		indexes := [][]float64{{0, 3}, {0}}
		installProfiles(model,
			&cache.ModelGPUProfile{
				Deployment: "dep-good",
				Indexes:    indexes,
				E2E:        [][]float64{{1.0}, {5.0}},
				SLOs:       cache.ModelSLOs{E2E: 5.0},
			},
			&cache.ModelGPUProfile{
				Deployment: "dep-bad",
				Indexes:    indexes,
				E2E:        [][]float64{{1.0}},
				SLOs:       cache.ModelSLOs{E2E: 5.0},
			},
			&cache.ModelGPUProfile{
				Deployment: "dep-noslo",
				Indexes:    indexes,
				E2E:        [][]float64{{1.0}, {5.0}},
			},
		)
	})

	It("should keep SLO ranking when a single (request, profile) rank fails", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"early": newRankedTestRequest("req-early", 2, time.Second),
			"late":  newRankedTestRequest("req-late", 16, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-good", "dep-bad"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.RequestID).To(Equal("req-late"))
	})

	It("should fall back to FIFO when no profile provides ranking for any request", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"early": newRankedTestRequest("req-early", 2, time.Second),
			"late":  newRankedTestRequest("req-late", 16, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-noslo"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.RequestID).To(Equal("req-early"))
	})

	It("should drop only the candidate that no profile can rank", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			// dep-bad only has E2E data for the first output bucket, so this request
			// cannot be ranked on any profile and its candidate must be dropped.
			"unrankable": newRankedTestRequest("req-unrankable", 16, 7*time.Second),
			// Same deployment, first bucket: rankable, and it arrives after the
			// unrankable FIFO head.
			"rankable": newRankedTestRequest("req-rankable", 1, 6*time.Second),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-bad"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.RequestID).To(Equal("req-rankable"))
	})

	It("should keep serving ranked candidates when multiple (request, profile) pairs fail", func() {
		q := newTestSLOQueue(model, map[string]*types.RoutingContext{
			"waiting": newRankedTestRequest("req-waiting", 2, 2500*time.Millisecond),
			"urgent":  newRankedTestRequest("req-urgent", 16, 500*time.Millisecond),
			"new":     newRankedTestRequest("req-new", 64, 0),
		})

		picked, err := q.Peek(time.Now(), fakePodList{deployments: []string{"dep-good", "dep-bad"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.RequestID).To(Equal("req-urgent"))
	})
})
