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
package routingalgorithms

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/cache"
	metrics "github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readExampleProfile(filePath string) (*cache.ModelGPUProfile, error) {
	jsonDataBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var profile cache.ModelGPUProfile
	err = profile.Unmarshal(jsonDataBytes)
	if err != nil {
		return nil, err
	}

	return &profile, nil
}

func newReqWithTimeout(model, message, requestID string, timeout time.Duration) (*types.RoutingContext, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req := RouterSLO.NewContext(ctx, model, message, requestID, "")
	return req, cancel
}

func route(cache cache.Cache, req *types.RoutingContext, pods types.PodList) (string, error) {
	router, err := cache.GetRouter(req)
	if err != nil {
		return "", err
	}
	return router.Route(req, pods)
}

func getBlockChannel(cb func()) chan bool {
	block := make(chan bool)
	go func() {
		cb()
		close(block)
	}()
	return block
}

func shouldBlock(cb func(), duration time.Duration) chan bool {
	block := getBlockChannel(cb)
	Consistently(block, duration).ShouldNot(BeClosed())
	return block
}

var _ = Describe("SLO", func() {
	var (
		store      *cache.Store
		model      = "llama2-7b"
		deployment = "simulator-llama2-7b-a100"
		profileKey = cache.ModelGPUProfileKey(model, deployment)
		profile    *cache.ModelGPUProfile
		err        error
	)

	BeforeEach(func() {
		profile, err = readExampleProfile("../../../../python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/simulator-llama2-7b-a100.json")
		Expect(err).To(BeNil())

		store = cache.NewForTest()
		Init() // Required to initialize the router registry after store was initialized and before pods are added.
		store = cache.InitWithPods(cache.InitWithModelRouterProvider(store, NewSLORouter), []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployment + "-replicaset-pod1",
					Namespace: "default",
					Labels: map[string]string{
						utils.DeploymentIdentifier: deployment,
					},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.1",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployment + "-replicaset-pod2",
					Namespace: "default",
					Labels: map[string]string{
						utils.DeploymentIdentifier: deployment,
					},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.2",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
		}, model)
		store.UpdateModelProfile(profileKey, profile, true)
	})

	Describe("Error handling", func() {
		It("Should use fallback router if no profile", func() {
			store.UpdateModelProfile(profileKey, nil, true)
			pods, _ := store.ListPodsByModel(model)

			// Use a context with a timeout to ensure the function doesn't block indefinitely
			req, cancel := newReqWithTimeout(model, "message", "request_id", 10*time.Millisecond)
			defer cancel()
			_, err = route(store, req, pods)
			Expect(err).To(BeNil())
			Expect(req.HasRouted()).To(BeTrue())
			Expect(req.Algorithm).To(Equal(RouterLeastRequest))
		})

		It("Should use fallback router if profile contains no SLO info", func() {
			profile, err = readExampleProfile("../../../../python/aibrix/aibrix/gpu_optimizer/optimizer/profiling/result/simulator-llama2-7b-a100_obsoleted_v1.json")
			Expect(err).To(BeNil())

			store.UpdateModelProfile(profileKey, profile, true)

			c, _ := cache.Get()
			pods, _ := c.ListPodsByModel(model)

			// Use a context with a timeout to ensure the function doesn't block indefinitely
			req, cancel := newReqWithTimeout(model, "message", "request_id", 10*time.Millisecond)
			defer cancel()
			_, err = route(store, req, pods)
			Expect(err).To(BeNil())
			Expect(req.HasRouted()).To(BeTrue())
			Expect(req.Algorithm).To(Equal(RouterLeastRequest))
		})

		It("Should use fallback router if profile contains SLO info but misses corresponding metrics (TPOT)", func() {
			profile.SLOs.TTFT = 1 // Overwrite SLO in term of TPOT (TPOT has higher priority than E2E)
			store.UpdateModelProfile(profileKey, profile, true)
			pods, _ := store.ListPodsByModel(model)

			// Use a context with a timeout to ensure the function doesn't block indefinitely
			req, cancel := newReqWithTimeout(model, "message", "request_id", 10*time.Millisecond)
			defer cancel()
			_, err = route(store, req, pods)
			Expect(err).To(BeNil())
			Expect(req.HasRouted()).To(BeTrue())
			Expect(req.Algorithm).To(Equal(RouterLeastRequest))
		})

		It("Should report cache.ErrorSLOFailureRequest if profile predicts SLO violation", func() {
			pods, _ := store.ListPodsByModel(model)

			// Provide prediction history
			predictor, _ := store.GetOutputPredictor(model)
			predictor.AddTrace(2048, 512, 1)

			// Build dummy input messages of 2048 tokens.
			builder := &strings.Builder{}
			for i := 0; i < 2048; i++ {
				builder.WriteString("ho ")
			}
			req, cancel := newReqWithTimeout(model, builder.String(), "request_slo_failure", 100*time.Millisecond)
			defer cancel()
			promptLen, _ := req.PromptLength()
			Expect(promptLen > 2000).To(BeTrue())

			_, err = route(store, req, pods)
			Expect(err).To(BeIdenticalTo(cache.ErrorSLOFailureRequest))
			Expect(req.HasRouted()).To(BeFalse())
			Expect(req.Algorithm).To(Equal(RouterSLO))
		})
	})

	It("Cold start should not throw error", func() {
		pods, _ := store.ListPodsByModel(model)

		// Use a context with a timeout to ensure the function doesn't block indefinitely
		req, cancel := newReqWithTimeout(model, "message", "request_id", 10*time.Millisecond)
		defer cancel()
		podAddr, err := route(store, req, pods)
		Expect(err).To(BeNil())
		Expect(req.HasRouted()).To(BeTrue())
		Expect(req.Algorithm).To(Equal(RouterSLO))
		Expect(req.TargetPod()).ToNot(BeNil())
		Expect([]string{"1.0.0.1", "1.0.0.2"}).To(ContainElement(req.TargetPod().Status.PodIP))
		Expect(podAddr).To(Equal(req.TargetAddress()))
	})

	It("Packing router should prefer the same pod", func() {
		pods, _ := store.ListPodsByModel(model)

		req1, cancel1 := newReqWithTimeout(model, "message1", "request_id_1", 100*time.Millisecond)
		req2, cancel2 := newReqWithTimeout(model, "message2", "request_id_2", 100*time.Millisecond)
		req2.Algorithm = RouterSLOPackLoad // Override routing algorithm
		defer cancel1()
		defer cancel2()

		podAddr1, err := route(store, req1, pods)
		Expect(err).To(BeNil())

		time.Sleep(1 * time.Millisecond) // There is a small gap between route and metric update due to operated by different goroutines.
		pendingLoad, err := store.GetMetricValueByPod(req1.TargetPod().Name, req1.TargetPod().Namespace, metrics.RealtimeNormalizedPendings)
		Expect(err).To(BeNil())
		Expect(pendingLoad.GetSimpleValue() <= 0.5).To(BeTrue())

		podAddr2, err := route(store, req2, pods)
		Expect(err).To(BeNil())
		Expect(podAddr1).To(Equal(podAddr2))
	})

	It("Queue router should be blocked and released successfully", func() {
		pods, _ := store.ListPodsByModel(model)

		makeOneRequest := func(id int, timeout time.Duration) (*types.RoutingContext, float64) {
			defer GinkgoRecover()

			req, cancel := newReqWithTimeout(model, "message", fmt.Sprintf("request_id_%d", id), timeout)
			req.Algorithm = RouterSLOPackLoad // Override routing algorithm
			defer cancel()
			_, err := route(store, req, pods)
			Expect(err).To(BeNil())

			time.Sleep(1 * time.Millisecond) // There is a small gap between route and metric update due to operated by different goroutines.
			pendingLoad, err := store.GetMetricValueByPod(req.TargetPod().Name, req.TargetPod().Namespace, metrics.RealtimeNormalizedPendings)
			Expect(err).To(BeNil())
			return req, pendingLoad.GetSimpleValue()
		}

		// Fill pods just enough.
		filledPods := make([]*v1.Pod, 0, pods.Len())
		firstReq, unitPendingLoad := makeOneRequest(1, 10*time.Millisecond)
		filledPods = append(filledPods, firstReq.TargetPod())
		lastPendingLoad := unitPendingLoad
		id := 2
		for len(filledPods) < pods.Len() || lastPendingLoad+unitPendingLoad < 1.0 {
			req, pendingLoad := makeOneRequest(id, 10*time.Millisecond)
			if req.TargetPod() != filledPods[len(filledPods)-1] {
				filledPods = append(filledPods, req.TargetPod())
			}
			lastPendingLoad = pendingLoad
			id++
		}

		// Make the request that should be blocked.
		var lastReq *types.RoutingContext
		blockage := shouldBlock(func() {
			lastReq, _ = makeOneRequest(id, 100*time.Millisecond)
		}, 10*time.Millisecond)

		// Release the request.
		store.DoneRequestCount(firstReq, "request_id_1", firstReq.Model, 1)

		// Check the blockage is released.
		Eventually(blockage, 1*time.Second).Should(BeClosed())
		Expect(lastReq).ToNot(BeNil())
		Expect(lastReq.TargetPod()).To(BeIdenticalTo(firstReq.TargetPod()))
	})

	It("Should cache.RequestTrace counts one and one only", func() {
		store = cache.InitWithRequestTrace(store)
		pods, _ := store.ListPodsByModel(model)

		// Use a context with a timeout to ensure the function doesn't block indefinitely
		req, cancel := newReqWithTimeout(model, "message", "request_id", 10*time.Millisecond)
		defer cancel()
		_, err := route(store, req, pods)
		Expect(err).To(BeNil())
		Expect(req.HasRouted()).To(BeTrue())
		Expect(req.Algorithm).To(Equal(RouterSLO))

		// Wait for metric to be updated
		time.Sleep(1 * time.Millisecond) // There is a small gap between route and metric update due to operated by different goroutines.
		_, err = store.GetMetricValueByPod(req.TargetPod().Name, req.TargetPod().Namespace, metrics.RealtimeNormalizedPendings)
		Expect(err).To(BeNil())

		trace := store.DumpRequestTrace(model)
		Expect(trace).ToNot(BeNil())
		Expect(trace[cache.MetaKeyTotalRequests.ToString()]).To(Equal(1))
	})
})
