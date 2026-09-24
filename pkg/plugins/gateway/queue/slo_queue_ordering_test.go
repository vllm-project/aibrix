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
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// routeRecorder collects one label per subRoute attempt, so a test can assert
// which profiles a candidate was tried on and in which order.
type routeRecorder struct {
	calls []string
}

// recordingPodList serves exactly one pod per deployment index. A recorded
// attempt then tells both the routing style (single profile vs whole pod set)
// and which profile was tried.
type recordingPodList struct {
	deployments []string
}

func (p *recordingPodList) Len() int { return len(p.deployments) }

func (p *recordingPodList) All() []*corev1.Pod {
	pods := make([]*corev1.Pod, 0, len(p.deployments))
	for _, deployment := range p.deployments {
		pods = append(pods, p.pod(deployment))
	}
	return pods
}

func (p *recordingPodList) Indexes() []string { return p.deployments }

func (p *recordingPodList) ListByIndex(index string) []*corev1.Pod {
	for _, deployment := range p.deployments {
		if deployment == index {
			return []*corev1.Pod{p.pod(index)}
		}
	}
	return nil
}

func (p *recordingPodList) ListPortsForPod() map[string][]int { return nil }

func (p *recordingPodList) pod(deployment string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + deployment, Namespace: "default"}}
}

// recordingRouter records the pod set it was handed and either routes the
// request or declines with a transient capacity error, which makes Peek walk
// the remaining profiles and candidates.
type recordingRouter struct {
	recorder *routeRecorder
	route    bool
}

func (r recordingRouter) Route(ctx *types.RoutingContext, pods types.PodList) (string, error) {
	r.recorder.calls = append(r.recorder.calls, describePods(pods))
	if !r.route {
		return "", cache.ErrorLoadCapacityReached
	}
	ctx.SetTargetPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "target-pod", Namespace: "default"}})
	return "10.0.0.1:8000", nil
}

func describePods(pods types.PodList) string {
	if array, ok := pods.(*utils.PodArray); ok {
		names := make([]string, 0, len(array.Pods))
		for _, pod := range array.Pods {
			names = append(names, pod.Name)
		}
		return "profile:" + strings.Join(names, "+")
	}
	return "all:" + strings.Join(pods.Indexes(), "+")
}

func recordingProvider(recorder *routeRecorder, route bool) types.RouterProviderFunc {
	return func(*types.RoutingContext) (types.Router, error) {
		return recordingRouter{recorder: recorder, route: route}, nil
	}
}

// rankedRequestAt pins RequestTime to now-age so rank values are exact.
func rankedRequestAt(requestID string, predictedOutput int, now time.Time, age time.Duration) *types.RoutingContext {
	req := newRankedTestRequest(requestID, predictedOutput, 0)
	req.RequestTime = now.Add(-age)
	return req
}

// e2eProfile builds a profile whose expected E2E latency is one value for small
// outputs and another for large ones (the buckets split at 1 and 8 tokens).
func e2eProfile(deployment string, smallOutput, largeOutput, target float64) *cache.ModelGPUProfile {
	return &cache.ModelGPUProfile{
		Deployment: deployment,
		Indexes:    [][]float64{{0, 3}, {0}},
		E2E:        [][]float64{{smallOutput}, {largeOutput}},
		SLOs:       cache.ModelSLOs{E2E: target},
	}
}

func installProfiles(model string, profiles ...*cache.ModelGPUProfile) *cache.Store {
	st := cache.InitForTest()
	for _, profile := range profiles {
		st.UpdateModelProfile(cache.ModelGPUProfileKey(model, profile.Deployment), profile, true)
	}
	return st
}

func enqueueRequests(q *SLOQueue, key string, requests ...*types.RoutingContext) {
	sub := NewSimpleQueue[*types.QueueEntry](4)
	for _, req := range requests {
		Expect(sub.Enqueue(types.NewQueueEntry(req, time.Now()), time.Now())).To(Succeed())
	}
	q.subs.Store(key, sub)
}

func newTestSLOQueueWithOptions(model string, requests map[string]*types.RoutingContext, opts queueOptions, provider types.RouterProviderFunc) *SLOQueue {
	q, err := newSLOQueue(provider, model, opts)
	Expect(err).NotTo(HaveOccurred())
	for key, req := range requests {
		enqueueRequests(q, key, req)
	}
	return q
}

var _ = Describe("SLOQueue ordering contract", func() {
	const model = "ordering-model"
	const deployment = "dep-ordering"

	// Expected E2E latency: 1s for small outputs, 5s for large ones; target 5s.
	profile := e2eProfile(deployment, 1.0, 5.0, 5.0)

	BeforeEach(func() {
		installProfiles(model, profile)
	})

	It("keeps the shipped default switches", func() {
		Expect(defaultQueueOptions()).To(Equal(queueOptions{
			fifoOnNonSLOViolation:    false,
			queueOverallSLO:          false,
			monogenousGPURouting:     true,
			monogenousGPURoutingOnly: false,
		}))
	})

	It("serves the older candidate when all candidates rank the same", func() {
		now := time.Now()
		// Both candidates rank 0: 4s elapsed + 1s expected - 5s target, and
		// 0s elapsed + 5s expected - 5s target.
		oldReq := rankedRequestAt("req-old", 2, now, 4*time.Second)
		newReq := rankedRequestAt("req-new", 16, now, 0)
		q := newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{
			"old": oldReq,
			"new": newReq,
		}, defaultQueueOptions(), recordingProvider(&routeRecorder{}, true))

		oldRank, err := q.rank(now, oldReq, profile)
		Expect(err).NotTo(HaveOccurred())
		newRank, err := q.rank(now, newReq, profile)
		Expect(err).NotTo(HaveOccurred())
		Expect(oldRank).To(Equal(newRank))

		pods := &recordingPodList{deployments: []string{deployment}}
		for i := 0; i < 20; i++ {
			picked, err := q.Peek(now, pods)
			Expect(err).NotTo(HaveOccurred())
			Expect(picked).NotTo(BeNil())
			Expect(picked.RequestID).To(Equal("req-old"))
		}
	})

	It("breaks equal ranks by arrival time and then by subqueue key regardless of input order", func() {
		now := time.Now()
		newCandidate := func(subKey string, requestTime time.Time) *candidateRouterRequest {
			req := newTestRequest("req-"+subKey, &fakeOutputPredictor{reply: 2})
			req.RequestTime = requestTime
			return &candidateRouterRequest{
				QueueEntry: types.NewQueueEntry(req, requestTime),
				SubKey:     subKey,
				Profiles:   []*candidateProfiles{{Rank: 0.0, Key: deployment}},
			}
		}

		q := &SLOQueue{}
		older := newCandidate("b-older", now.Add(-time.Second))
		newer := newCandidate("a-newer", now)
		candidates := []*candidateRouterRequest{newer, older}
		sort.Slice(candidates, func(i, j int) bool { return q.candidateLess(candidates[i], candidates[j]) })
		Expect(candidates[0].SubKey).To(Equal("b-older"))

		// Same arrival time: the subqueue key is the final tie-break.
		sameTime := now.Add(-2 * time.Second)
		keyB := newCandidate("b", sameTime)
		keyA := newCandidate("a", sameTime)
		candidates = []*candidateRouterRequest{keyB, keyA}
		sort.Slice(candidates, func(i, j int) bool { return q.candidateLess(candidates[i], candidates[j]) })
		Expect(candidates[0].SubKey).To(Equal("a"))
	})
})

var _ = Describe("SLOQueue policy switches", func() {
	const model = "switch-model"

	It("tries profiles from the most relaxing one and stops before the first violating profile", func() {
		installProfiles(model,
			e2eProfile("dep-relaxed", 1.0, 1.0, 10.0),
			e2eProfile("dep-mid", 3.0, 3.0, 10.0),
			e2eProfile("dep-violating", 20.0, 20.0, 10.0),
		)
		now := time.Now()
		// Ranks: -7, -5 and +12, so the violating profile must never be tried.
		req := rankedRequestAt("req-relaxer", 16, now, 2*time.Second)
		recorder := &routeRecorder{}
		q := newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{"sub": req}, defaultQueueOptions(), recordingProvider(recorder, false))

		picked, err := q.Peek(now, &recordingPodList{deployments: []string{"dep-relaxed", "dep-mid", "dep-violating"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).To(BeNil())
		Expect(recorder.calls).To(Equal([]string{
			"profile:pod-dep-relaxed",
			"profile:pod-dep-mid",
		}))
	})

	It("treats the most-relaxing-profile-only switch as a refinement of monogenous routing", func() {
		installProfiles(model,
			e2eProfile("dep-relaxed", 1.0, 1.0, 10.0),
			e2eProfile("dep-violating", 20.0, 20.0, 10.0),
		)
		now := time.Now()
		req := rankedRequestAt("req-only", 16, now, 2*time.Second)
		recorder := &routeRecorder{}
		opts := defaultQueueOptions()
		opts.monogenousGPURouting = false
		opts.monogenousGPURoutingOnly = true
		q := newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{"sub": req}, opts, recordingProvider(recorder, false))

		picked, err := q.Peek(now, &recordingPodList{deployments: []string{"dep-relaxed", "dep-violating"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).To(BeNil())
		Expect(recorder.calls).To(Equal([]string{"profile:pod-dep-relaxed"}))
	})

	It("routes the whole pod set once when monogenous GPU routing is off", func() {
		installProfiles(model,
			e2eProfile("dep-relaxed", 1.0, 1.0, 10.0),
			e2eProfile("dep-violating", 20.0, 20.0, 10.0),
		)
		now := time.Now()
		req := rankedRequestAt("req-whole", 16, now, 2*time.Second)
		recorder := &routeRecorder{}
		opts := defaultQueueOptions()
		opts.monogenousGPURouting = false
		q := newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{"sub": req}, opts, recordingProvider(recorder, false))

		picked, err := q.Peek(now, &recordingPodList{deployments: []string{"dep-relaxed", "dep-violating"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).To(BeNil())
		Expect(recorder.calls).To(Equal([]string{"all:dep-relaxed+dep-violating"}))
	})

	It("serves both-negative candidates in arrival order only with fifoOnNonSLOViolation", func() {
		installProfiles(model, e2eProfile("dep-fifo", 1.0, 15.0, 20.0))
		now := time.Now()
		pods := &recordingPodList{deployments: []string{"dep-fifo"}}
		newQueue := func(opts queueOptions) *SLOQueue {
			// Ranks: 6s + 1s - 20s = -13 and 1s + 15s - 20s = -4, both negative.
			return newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{
				"old": rankedRequestAt("req-old-low", 2, now, 6*time.Second),
				"new": rankedRequestAt("req-new-high", 16, now, time.Second),
			}, opts, recordingProvider(&routeRecorder{}, true))
		}

		byRank, err := newQueue(defaultQueueOptions()).Peek(now, pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(byRank).NotTo(BeNil())
		Expect(byRank.RequestID).To(Equal("req-new-high"))

		opts := defaultQueueOptions()
		opts.fifoOnNonSLOViolation = true
		byFIFO, err := newQueue(opts).Peek(now, pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(byFIFO).NotTo(BeNil())
		Expect(byFIFO.RequestID).To(Equal("req-old-low"))
	})

	It("ranks a head against its whole subqueue only with queueOverallSLO", func() {
		const deployment = "dep-overall"
		installProfiles(model, &cache.ModelGPUProfile{
			Deployment: deployment,
			Indexes:    [][]float64{{0, 3}, {0}},
			E2E:        [][]float64{{1.0}, {5.0}},
			Tputs:      [][]float64{{0.5}, {0.5}},
			SLOs:       cache.ModelSLOs{E2E: 20.0},
		})
		now := time.Now()
		newQueue := func(opts queueOptions) *SLOQueue {
			q, err := newSLOQueue(recordingProvider(&routeRecorder{}, true), model, opts)
			Expect(err).NotTo(HaveOccurred())
			// The long subqueue holds four requests; at 0.5 RPS the head of the
			// long queue ranks -12.5 overall but -18.5 per request, while the
			// short queue head ranks -14 either way.
			enqueueRequests(q, "long",
				rankedRequestAt("req-long", 2, now, 500*time.Millisecond),
				rankedRequestAt("req-long-f1", 2, now, 0),
				rankedRequestAt("req-long-f2", 2, now, 0),
				rankedRequestAt("req-long-f3", 2, now, 0),
			)
			enqueueRequests(q, "short", rankedRequestAt("req-short", 16, now, time.Second))
			return q
		}
		pods := &recordingPodList{deployments: []string{deployment}}

		byRequest, err := newQueue(defaultQueueOptions()).Peek(now, pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(byRequest).NotTo(BeNil())
		Expect(byRequest.RequestID).To(Equal("req-short"))

		opts := defaultQueueOptions()
		opts.queueOverallSLO = true
		byQueue, err := newQueue(opts).Peek(now, pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(byQueue).NotTo(BeNil())
		Expect(byQueue.RequestID).To(Equal("req-long"))
	})

	It("serves the candidate whose most relaxing profile is closest to a violation", func() {
		installProfiles(model,
			e2eProfile("dep-a", 1.0, 1.0, 10.0),
			e2eProfile("dep-b", 7.0, 7.0, 10.0),
		)
		now := time.Now()
		// req-a ranks -8 / -2 and req-b ranks -7 / -1: the effective rank of
		// req-b is higher, and its most relaxing profile is dep-a.
		recorder := &routeRecorder{}
		q := newTestSLOQueueWithOptions(model, map[string]*types.RoutingContext{
			"a": rankedRequestAt("req-a", 16, now, time.Second),
			"b": rankedRequestAt("req-b", 16, now, 2*time.Second),
		}, defaultQueueOptions(), recordingProvider(recorder, true))

		picked, err := q.Peek(now, &recordingPodList{deployments: []string{"dep-a", "dep-b"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(picked).NotTo(BeNil())
		Expect(picked.RequestID).To(Equal("req-b"))
		Expect(recorder.calls).To(Equal([]string{"profile:pod-dep-a"}))
	})
})
