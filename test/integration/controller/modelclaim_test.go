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

package controller

import (
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	modelclaimcontroller "github.com/vllm-project/aibrix/pkg/controller/modelclaim"
	controllerutils "github.com/vllm-project/aibrix/test/utils/controller"
)

const (
	modelClaimTimeout  = 40 * time.Second
	modelClaimInterval = 200 * time.Millisecond
)

var _ = ginkgo.Describe("ModelClaim controller test", func() {
	var (
		ns      *corev1.Namespace
		fixture *controllerutils.ModelClaimFixture
	)

	ginkgo.BeforeEach(func() {
		fixture = nil
		ns = nil
		ns = controllerutils.CreateNamespace(ctx, k8sClient, "test-modelclaim-", 3*time.Second, modelClaimInterval)

		fixture = controllerutils.NewModelClaimFixture(ctx, k8sClient, modelClaimTimeout, modelClaimInterval)
	})

	ginkgo.AfterEach(func() {
		if ns != nil {
			claims := &modelapi.ModelClaimList{}
			gomega.Expect(k8sClient.List(ctx, claims, client.InNamespace(ns.Name))).To(gomega.Succeed())
			for i := range claims.Items {
				_ = k8sClient.Delete(ctx, &claims.Items[i])
			}
			gomega.Eventually(func() int {
				remaining := &modelapi.ModelClaimList{}
				if err := k8sClient.List(ctx, remaining, client.InNamespace(ns.Name)); err != nil {
					return -1
				}
				return len(remaining.Items)
			}, modelClaimTimeout, modelClaimInterval).Should(gomega.Equal(0))
		}

		controllerutils.DeleteNamespace(ctx, k8sClient, ns)
	})

	ginkgo.It("progresses from Pending through Activating to Active", func() {
		fixture.Runtime().SetDefaultState("activating", false)
		claim := fixture.CreateClaim(ns.Name, "claim-a", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Finalizers).To(gomega.ContainElement(modelclaimcontroller.ModelClaimFinalizer))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			initialized := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeInitialized),
			)
			g.Expect(initialized).NotTo(gomega.BeNil())
			g.Expect(initialized.Status).To(gomega.Equal(metav1.ConditionTrue))
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		pod := fixture.CreateWarmPod(ns.Name, "warm-1", "pool-a")

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Finalizers).To(gomega.ContainElement(modelclaimcontroller.ModelClaimFinalizer))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActivating))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			initialized := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeInitialized),
			)
			g.Expect(initialized).NotTo(gomega.BeNil())
			g.Expect(initialized.Status).To(gomega.Equal(metav1.ConditionTrue))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("EngineStarting"))
			fixture.ExpectRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateActivating)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		activationCallsWhileStarting := fixture.Runtime().ActivateCallCount()
		gomega.Expect(activationCallsWhileStarting).To(gomega.BeNumerically(">=", 1))

		fixture.Runtime().SetClaimState(string(claim.UID), "active", true, "")
		fixture.TriggerReconcile(claim)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionTrue))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				latest.Status.Instances[0].Port,
				constants.ModelClaimRoutingStateActive,
			)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Consistently(
			fixture.Runtime().ActivateCallCount,
			time.Second,
			modelClaimInterval,
		).Should(gomega.Equal(activationCallsWhileStarting))
	})

	ginkgo.It("reports NoMatchingPods and recovers when a warm pod appears", func() {
		fixture.Runtime().SetDefaultState("active", true)
		claim := fixture.CreateClaim(ns.Name, "claim-late-pod", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "NoMatchingPods")
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))

		_ = fixture.CreateWarmPod(ns.Name, "warm-late", "pool-a")

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal("warm-late"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("places a pending claim when its existing warm pod becomes runnable", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPodPending(ns.Name, "warm-becoming-ready", "pool-a", 0)
		claim := fixture.CreateClaim(ns.Name, "claim-becoming-ready", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeScheduled),
			)
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))

		fixture.MarkWarmPodRunning(pod)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(1))
	})

	ginkgo.It("keeps serving from a pod that leaves the pool", func() {
		fixture.Runtime().SetDefaultState("active", true)
		leaving := fixture.CreateWarmPod(ns.Name, "warm-leaving", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-leaving", "pool-a", nil, nil)
		var port int32
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(leaving.Name))
			port = latest.Status.Instances[0].Port
			fixture.ExpectRoute(g, ns.Name, leaving.Name, claim.Name, port, constants.ModelClaimRoutingStateActive)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.CreateWarmPod(ns.Name, "warm-staying", "pool-a")
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Candidates).To(gomega.Equal(int32(2)))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		pod := &corev1.Pod{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaving), pod)).To(gomega.Succeed())
		left := pod.DeepCopy()
		delete(left.Labels, constants.ModelPoolLabelEnabled)
		gomega.Expect(k8sClient.Patch(ctx, left, client.MergeFrom(pod))).To(gomega.Succeed())

		// The first pass without the label counts one candidate. The claim
		// stays on the pod it runs on, and no engine is started or stopped.
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Candidates).To(gomega.Equal(int32(1)))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		latest := &modelapi.ModelClaim{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
		gomega.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		gomega.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
		gomega.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(leaving.Name))
		fixture.ExpectRoute(gomega.Default, ns.Name, leaving.Name, claim.Name, port, constants.ModelClaimRoutingStateActive)
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(1))
		gomega.Expect(fixture.Runtime().DeactivateRequests()).To(gomega.BeEmpty())

		// Deleting the claim still stops its engine there.
		gomega.Expect(k8sClient.Delete(ctx, claim)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.Runtime().DeactivateRequests()).To(gomega.ContainElement(
				gomega.HaveField("ModelName", claim.Name),
			))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(leaving), latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations).NotTo(gomega.HaveKey(constants.ModelClaimPodAnnotationPrefix + claim.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("places a parallel model only on a pod with the required GPU topology", func() {
		fixture.Runtime().SetCards(8<<30, 8<<30)
		fixture.Runtime().SetDefaultState("active", true)
		oneGPU := fixture.CreateWarmPodPending(ns.Name, "warm-one-gpu", "pool-parallel", 1)
		fixture.MarkWarmPodRunning(oneGPU)
		claim := fixture.CreateClaim(ns.Name, "claim-parallel", "pool-parallel", nil, map[string]string{
			"--tensor-parallel-size": "2",
		})

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))

		twoGPU := fixture.CreateWarmPodPending(ns.Name, "warm-two-gpu", "pool-parallel", 2)
		fixture.MarkWarmPodRunning(twoGPU)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Candidates).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(twoGPU.Name))
			oldPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(oneGPU), oldPod)).To(gomega.Succeed())
			g.Expect(oldPod.Annotations).NotTo(gomega.HaveKey(
				constants.ModelClaimPodAnnotationPrefix + claim.Name,
			))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("distinguishes temporary lack of room from a model too large for every card", func() {
		fixture.Runtime().SetDefaultState("active", true)
		fixture.Runtime().SetCard(4 << 30)
		temporaryPod := fixture.CreateWarmPodPending(ns.Name, "warm-temporary-room", "pool-temporary-room", 1)
		fixture.MarkWarmPodRunning(temporaryPod)
		createClaim := func(name, pool, footprint, floor string) *modelapi.ModelClaim {
			claim := &modelapi.ModelClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec: modelapi.ModelClaimSpec{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						constants.ModelPoolLabelName: pool,
					}},
					ArtifactURL: "huggingface://integration/" + name,
					Engine:      "vllm",
					PerGPU: &modelapi.ModelClaimPerGPU{
						MaximumFootprint: resource.MustParse(footprint),
						KVFloor:          resource.MustParse(floor),
					},
				},
			}
			gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
			return claim
		}

		occupant := createClaim("claim-occupant", "pool-temporary-room", "3Gi", "1Gi")
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, occupant).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		waiting := createClaim("claim-waiting-room", "pool-temporary-room", "1Gi", "1Gi")
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, waiting)
			scheduled := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeScheduled),
			)
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, occupant)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, waiting).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, 2*modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(k8sClient.Delete(ctx, waiting)).To(gomega.Succeed())
		gomega.Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(waiting), &modelapi.ModelClaim{})
			return apierrors.IsNotFound(err)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.BeTrue())

		fixture.Runtime().SetCard(2 << 30)
		tooLargePod := fixture.CreateWarmPodPending(ns.Name, "warm-too-small", "pool-too-large", 1)
		fixture.MarkWarmPodRunning(tooLargePod)
		tooLarge := createClaim("claim-too-large", "pool-too-large", "2Gi", "1Gi")
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, tooLarge)
			scheduled := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeScheduled),
			)
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal(constants.ModelClaimReasonTooLargeForAnyCard))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetCard(8 << 30)
		largePod := fixture.CreateWarmPodPending(ns.Name, "warm-large-enough", "pool-too-large", 1)
		fixture.MarkWarmPodRunning(largePod)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, tooLarge)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
		}, 2*modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("surfaces invalid engine configuration without contacting the runtime", func() {
		_ = fixture.CreateWarmPod(ns.Name, "warm-invalid", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-invalid", "pool-a", nil, map[string]string{
			"--tensor-parallel-size": "invalid",
		})

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("InvalidEngineConfig"))
			g.Expect(ready.Message).To(gomega.ContainSubstring("must be a positive integer"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "InvalidEngineConfig")
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))
	})

	ginkgo.It("accepts a claim that declares no per-GPU cost and does not place it", func() {
		_ = fixture.CreateWarmPod(ns.Name, "warm-undeclared", "pool-a")
		claim := &modelapi.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claim-undeclared", Namespace: ns.Name},
			Spec: modelapi.ModelClaimSpec{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: "pool-a"}},
				ArtifactURL: "huggingface://integration/claim-undeclared",
				Engine:      "vllm",
			},
		}
		// The schema leaves perGPU optional, so the API server takes the claim,
		// and the controller is what keeps it off every card.
		gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(scheduled.Reason).To(gomega.Equal("InvalidPerGPU"))
			g.Expect(scheduled.Message).To(gomega.ContainSubstring("spec.perGPU is missing"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "InvalidPerGPU")
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))
	})

	ginkgo.It("enforces ModelClaim defaults and schema validation", func() {
		validClaim := func(name string) *modelapi.ModelClaim {
			return &modelapi.ModelClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name},
				Spec: modelapi.ModelClaimSpec{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						constants.ModelPoolLabelName: "pool-a",
					}},
					ArtifactURL: "huggingface://integration/" + name,
					PerGPU: &modelapi.ModelClaimPerGPU{
						MaximumFootprint: resource.MustParse("1Gi"),
						KVFloor:          resource.MustParse("1Gi"),
					},
				},
			}
		}

		defaults := validClaim("claim-defaults")
		gomega.Expect(k8sClient.Create(ctx, defaults)).To(gomega.Succeed())
		stored := &modelapi.ModelClaim{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(defaults), stored)).To(gomega.Succeed())
		gomega.Expect(stored.Spec.Engine).To(gomega.Equal("vllm"))
		gomega.Expect(stored.Spec.Replicas).NotTo(gomega.BeNil())
		gomega.Expect(*stored.Spec.Replicas).To(gomega.Equal(int32(1)))

		invalid := []struct {
			name   string
			change func(*modelapi.ModelClaim)
		}{
			{name: "engine", change: func(claim *modelapi.ModelClaim) { claim.Spec.Engine = "unknown" }},
			{name: "replicas-zero", change: func(claim *modelapi.ModelClaim) { claim.Spec.Replicas = ptr.To[int32](0) }},
			{name: "replicas-two", change: func(claim *modelapi.ModelClaim) { claim.Spec.Replicas = ptr.To[int32](2) }},
			{name: "pod-selector", change: func(claim *modelapi.ModelClaim) { claim.Spec.PodSelector = nil }},
			{name: "artifact-url", change: func(claim *modelapi.ModelClaim) { claim.Spec.ArtifactURL = "" }},
		}
		for _, tc := range invalid {
			claim := validClaim("claim-invalid-" + tc.name)
			tc.change(claim)
			err := k8sClient.Create(ctx, claim)
			gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%s: %v", tc.name, err)
		}

		for _, missing := range []string{"maximumFootprint", "kvFloor"} {
			perGPU := map[string]any{"maximumFootprint": "1Gi", "kvFloor": "1Gi"}
			delete(perGPU, missing)
			claim := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": modelapi.GroupVersion.String(),
				"kind":       "ModelClaim",
				"metadata": map[string]any{
					"name":      "claim-missing-" + strings.ToLower(missing),
					"namespace": ns.Name,
				},
				"spec": map[string]any{
					"podSelector": map[string]any{"matchLabels": map[string]any{
						constants.ModelPoolLabelName: "pool-a",
					}},
					"artifactURL": "huggingface://integration/missing-field",
					"perGPU":      perGPU,
				},
			}}
			err := k8sClient.Create(ctx, claim)
			gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "%s: %v", missing, err)
		}
	})

	ginkgo.It("does not activate claims with unusable per-GPU declarations", func() {
		_ = fixture.CreateWarmPod(ns.Name, "warm-invalid-per-gpu", "pool-a")
		cases := []struct {
			name, footprint, floor, field string
		}{
			{name: "zero-footprint", footprint: "0", floor: "1Gi", field: "maximumFootprint"},
			{name: "negative-floor", footprint: "1Gi", floor: "-1Gi", field: "kvFloor"},
			{name: "oversized-footprint", footprint: "2Pi", floor: "1Gi", field: "maximumFootprint"},
		}
		for _, tc := range cases {
			claim := &modelapi.ModelClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "claim-" + tc.name, Namespace: ns.Name},
				Spec: modelapi.ModelClaimSpec{
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						constants.ModelPoolLabelName: "pool-a",
					}},
					ArtifactURL: "huggingface://integration/" + tc.name,
					Engine:      "vllm",
					PerGPU: &modelapi.ModelClaimPerGPU{
						MaximumFootprint: resource.MustParse(tc.footprint),
						KVFloor:          resource.MustParse(tc.floor),
					},
				},
			}
			gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
			gomega.Eventually(func(g gomega.Gomega) {
				latest := fixture.GetClaim(g, claim)
				g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
				g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
				scheduled := meta.FindStatusCondition(
					latest.Status.Conditions,
					string(modelapi.ModelClaimConditionTypeScheduled),
				)
				g.Expect(scheduled).NotTo(gomega.BeNil())
				g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionFalse))
				g.Expect(scheduled.Reason).To(gomega.Equal("InvalidPerGPU"))
				g.Expect(scheduled.Message).To(gomega.ContainSubstring(tc.field))
			}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		}
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(0))
	})

	ginkgo.It("places a refused claim after its per-GPU declaration is corrected", func() {
		fixture.Runtime().SetDefaultState("active", true)
		_ = fixture.CreateWarmPod(ns.Name, "warm-corrected-per-gpu", "pool-a")
		claim := &modelapi.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claim-corrected-per-gpu", Namespace: ns.Name},
			Spec: modelapi.ModelClaimSpec{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					constants.ModelPoolLabelName: "pool-a",
				}},
				ArtifactURL: "huggingface://integration/corrected-per-gpu",
				Engine:      "vllm",
				PerGPU: &modelapi.ModelClaimPerGPU{
					MaximumFootprint: resource.MustParse("0"),
					KVFloor:          resource.MustParse("1Gi"),
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, claim)).To(gomega.Succeed())
		originalUID := claim.UID
		originalGeneration := claim.Generation
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			scheduled := meta.FindStatusCondition(
				latest.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeScheduled),
			)
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Reason).To(gomega.Equal("InvalidPerGPU"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		latest := &modelapi.ModelClaim{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.PerGPU.MaximumFootprint = resource.MustParse("1Gi")
		gomega.Expect(k8sClient.Patch(ctx, latest, patch)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			updated := fixture.GetClaim(g, claim)
			g.Expect(updated.UID).To(gomega.Equal(originalUID))
			g.Expect(updated.Generation).To(gomega.BeNumerically(">", originalGeneration))
			g.Expect(updated.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			scheduled := meta.FindStatusCondition(
				updated.Status.Conditions,
				string(modelapi.ModelClaimConditionTypeScheduled),
			)
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Status).To(gomega.Equal(metav1.ConditionTrue))
			g.Expect(scheduled.Reason).To(gomega.Equal("Placed"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(1))
	})

	ginkgo.It("takes a change of perGPU and refuses a change of anything else", func() {
		claim := fixture.CreateClaim(ns.Name, "claim-immutable", "pool-a", nil, map[string]string{"--max-model-len": "4096"})

		changes := map[string]func(*modelapi.ModelClaimSpec){
			"modelName": func(spec *modelapi.ModelClaimSpec) { spec.ModelName = ptr.To("another-model") },
			"podSelector": func(spec *modelapi.ModelClaimSpec) {
				spec.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{constants.ModelPoolLabelName: "pool-b"}}
			},
			"artifactURL": func(spec *modelapi.ModelClaimSpec) { spec.ArtifactURL = "huggingface://integration/another" },
			"engine":      func(spec *modelapi.ModelClaimSpec) { spec.Engine = "sglang" },
			"engineConfig": func(spec *modelapi.ModelClaimSpec) {
				spec.EngineConfig = &modelapi.ModelClaimEngineConfig{Args: map[string]string{"--max-model-len": "8192"}}
			},
		}
		for field, change := range changes {
			changed := claim.DeepCopy()
			change(&changed.Spec)
			err := k8sClient.Patch(ctx, changed, client.MergeFrom(claim))
			gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "a change of %s: %v", field, err)
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(field + " is immutable"))
		}
		withoutConfig := claim.DeepCopy()
		withoutConfig.Spec.EngineConfig = nil
		err := k8sClient.Patch(ctx, withoutConfig, client.MergeFrom(claim))
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "removing engineConfig: %v", err)

		resized := claim.DeepCopy()
		resized.Spec.PerGPU.MaximumFootprint = resource.MustParse("2Gi")
		gomega.Expect(k8sClient.Patch(ctx, resized, client.MergeFrom(claim))).To(gomega.Succeed())
		latest := &modelapi.ModelClaim{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
		gomega.Expect(latest.Spec.PerGPU.MaximumFootprint.String()).To(gomega.Equal("2Gi"))
		gomega.Expect(latest.Spec.ArtifactURL).To(gomega.Equal(claim.Spec.ArtifactURL))
	})

	ginkgo.It("routes a claim whose name has 63 characters and refuses a longer name", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-long-name", "pool-a")
		claim := fixture.CreateClaim(ns.Name, strings.Repeat("m", 63), "pool-a", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				latest.Status.Instances[0].Port,
				constants.ModelClaimRoutingStateActive,
			)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		longer := &modelapi.ModelClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claim.Name + "m", Namespace: ns.Name},
			Spec:       *claim.Spec.DeepCopy(),
		}
		err := k8sClient.Create(ctx, longer)
		gomega.Expect(apierrors.IsInvalid(err)).To(gomega.BeTrue(), "a 64-character name: %v", err)
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("at most 63 characters"))
	})

	ginkgo.It("records activation failure and retries to Active", func() {
		fixture.Runtime().SetDefaultState("active", true)
		fixture.Runtime().FailNextActivations(1)
		_ = fixture.CreateWarmPod(ns.Name, "warm-retry", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-retry", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Status).To(gomega.Equal(metav1.ConditionFalse))
			g.Expect(ready.Reason).To(gomega.Equal("ActivateFailed"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "ActivateFailed")

		// The controller's failure path returns RequeueAfter rather than relying on
		// a status update event. Wait for that policy-driven retry to succeed.
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.BeNumerically(">=", 2))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("tries two claims whose engines cannot be started by the round, and not in a loop", func() {
		fixture.Runtime().SetDefaultState("active", true)
		fixture.Runtime().FailNextActivations(1000)
		_ = fixture.CreateWarmPod(ns.Name, "warm-refusing", "pool-a")
		first := fixture.CreateClaim(ns.Name, "claim-first", "pool-a", nil, nil)
		second := fixture.CreateClaim(ns.Name, "claim-second", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			for _, claim := range []*modelapi.ModelClaim{first, second} {
				latest := fixture.GetClaim(g, claim)
				g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
				g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			}
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		// A start that failed takes its record back, and that write must not
		// wake the other claim. Each claim is tried again when its wait is up,
		// which is after 10 seconds and then after 20 more.
		gomega.Consistently(func() int {
			return fixture.Runtime().ActivateCallCount()
		}, 15*time.Second, time.Second).Should(gomega.BeNumerically("<=", 6))
	})

	ginkgo.It("reflects sleeping and terminal failed runtime states", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-state", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-state", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(claim.UID), "sleeping", false, "")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			g.Expect(latest.Status.Instances[0].Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Reason).To(gomega.Equal("EngineSleeping"))
			fixture.ExpectRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateSleeping)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeNormal, "Sleeping")

		fixture.Runtime().SetClaimState(string(claim.UID), "failed", false, "restart budget exhausted")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			g.Expect(latest.Status.Instances[0].Phase).To(gomega.Equal(modelapi.ModelClaimFailed))
			ready := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionReady))
			g.Expect(ready).NotTo(gomega.BeNil())
			g.Expect(ready.Reason).To(gomega.Equal("EngineFailed"))
			fixture.ExpectRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateFailed)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "EngineFailed")
	})

	ginkgo.It("wakes a sleeping engine when a request asks for it", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-wake", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-wake", "pool-a", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(claim.UID), "sleeping", false, "")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			fixture.ExpectRoute(g, ns.Name, pod.Name, claim.Name, 0, constants.ModelClaimRoutingStateSleeping)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().WakeRequests()).To(gomega.BeEmpty())

		// The request alone brings the claim back, well before its next round.
		fixture.RequestWake(ns.Name, pod.Name, claim.Name)
		gomega.Eventually(func() int {
			return len(fixture.Runtime().WakeRequests())
		}, 5*time.Second, modelClaimInterval).Should(gomega.BeNumerically(">=", 1))
		// The claim names no model, so it serves under its own name.
		gomega.Expect(fixture.Runtime().WakeRequests()[0].ModelName).To(gomega.Equal(claim.Name))

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			latest := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latest)).To(gomega.Succeed())
			g.Expect(latest.Annotations).NotTo(gomega.HaveKey(constants.ModelClaimWakeAnnotationPrefix + claim.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.ExpectEvent(claim, corev1.EventTypeNormal, "Waking")
	})

	ginkgo.It("puts the neighbour idle longest to sleep to make room for a wake", func() {
		fixture.Runtime().SetDefaultState("active", true)
		// One card of 5 GiB. Every claim declares 1 GiB and a 1 GiB floor, so two
		// fit awake, and a third fits only beside a sleeper without a wake reserve.
		fixture.Runtime().SetCard(5 << 30)
		fixture.Runtime().SetSleepingFootprint(256 << 20)
		pod := fixture.CreatePoolPod(ns.Name, "pool-room", "pool-room",
			`{"lifecycle":{"noWakeReserveWhileAsleep":true,"sleepToMakeRoomAfterSeconds":1}}`)
		waker := fixture.CreateClaim(ns.Name, "claim-waker", "pool-room", nil, nil)
		first := fixture.CreateClaim(ns.Name, "claim-first", "pool-room", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, waker).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(fixture.GetClaim(g, first).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(waker.UID), "sleeping", false, "")
		fixture.TriggerReconcile(waker)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, waker).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		// Asleep, the waker is charged only the 256 MiB it holds.
		second := fixture.CreateClaim(ns.Name, "claim-second", "pool-room", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, second).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.RequestWake(ns.Name, pod.Name, waker.Name)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, waker).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			asleep := 0
			for _, neighbour := range []*modelapi.ModelClaim{first, second} {
				if fixture.GetClaim(g, neighbour).Status.Phase == modelapi.ModelClaimSleeping {
					asleep++
				}
			}
			g.Expect(asleep).To(gomega.Equal(1), "one neighbour goes to sleep, and the waker fits")
		}, 2*modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().SleepRequests()).To(gomega.HaveLen(1))
		fixture.ExpectEvent(waker, corev1.EventTypeNormal, "Waking")
		// While the wake waited for room, the card was still divided.
		events := &corev1.EventList{}
		gomega.Expect(k8sClient.List(ctx, events, client.InNamespace(ns.Name))).To(gomega.Succeed())
		for _, event := range events.Items {
			gomega.Expect(event.Reason).NotTo(gomega.Equal("KVLimitFailed"), event.Message)
		}
	})

	ginkgo.It("puts the engine idle longest to sleep to make room for a new claim", func() {
		fixture.Runtime().SetDefaultState("active", true)
		// One card of 5 GiB. Every claim declares 1 GiB and a 1 GiB floor, so
		// two fit, and a third only once one of them sleeps.
		fixture.Runtime().SetCard(5 << 30)
		fixture.Runtime().SetSleepingFootprint(256 << 20)
		fixture.CreatePoolPod(ns.Name, "pool-new", "pool-new",
			`{"lifecycle":{"noWakeReserveWhileAsleep":true,"sleepToMakeRoomAfterSeconds":1}}`)
		first := fixture.CreateClaim(ns.Name, "claim-first", "pool-new", nil, nil)
		second := fixture.CreateClaim(ns.Name, "claim-second", "pool-new", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, first).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(fixture.GetClaim(g, second).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		third := fixture.CreateClaim(ns.Name, "claim-third", "pool-new", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, third).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			// claim-first was seen idle no later than claim-second, and its
			// name breaks a tie, so its engine is the one idle longest.
			g.Expect(fixture.GetClaim(g, first).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping),
				"the engine idle longest goes to sleep, and the new claim fits")
			g.Expect(fixture.GetClaim(g, second).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, 2*modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		sleeps := fixture.Runtime().SleepRequests()
		gomega.Expect(sleeps).To(gomega.HaveLen(1))
		gomega.Expect(sleeps[0].ModelName).To(gomega.Equal(first.Name))
		fixture.ExpectEvent(third, corev1.EventTypeNormal, "MakingRoom")
		events := &corev1.EventList{}
		gomega.Expect(k8sClient.List(ctx, events, client.InNamespace(ns.Name))).To(gomega.Succeed())
		for _, event := range events.Items {
			gomega.Expect(event.Reason).NotTo(gomega.Equal("KVLimitFailed"), event.Message)
		}
	})

	ginkgo.It("moves a claim whose engine cannot be woken", func() {
		fixture.Runtime().SetDefaultState("active", true)
		fixture.CreateWarmPod(ns.Name, "warm-move-a", "pool-a")
		fixture.CreateWarmPod(ns.Name, "warm-move-b", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-move", "pool-a", nil, nil)
		from := ""
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			from = latest.Status.Instances[0].Pod
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(claim.UID), "sleeping", false, "")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().FailNextWakes(1)
		fixture.RequestWake(ns.Name, from, claim.Name)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).NotTo(gomega.Equal(from))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			old := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: from}, old)).To(gomega.Succeed())
			g.Expect(old.Annotations).NotTo(gomega.HaveKey(constants.ModelClaimWakeAnnotationPrefix + claim.Name))
			g.Expect(old.Annotations).NotTo(gomega.HaveKey(constants.ModelClaimPodAnnotationPrefix + claim.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().WakeRequests()).To(gomega.HaveLen(1))
		fixture.ExpectEvent(claim, corev1.EventTypeWarning, "Moving")
		fixture.ExpectEvent(claim, corev1.EventTypeNormal, "Rescheduled")
	})

	ginkgo.It("drops a lost assignment and activates on a replacement pod", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-old", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-replace", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, pod)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimPending))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.BeEmpty())
			scheduled := meta.FindStatusCondition(latest.Status.Conditions, string(modelapi.ModelClaimConditionTypeScheduled))
			g.Expect(scheduled).NotTo(gomega.BeNil())
			g.Expect(scheduled.Reason).To(gomega.Equal("NoMatchingPods"))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		_ = fixture.CreateWarmPod(ns.Name, "warm-new", "pool-a")
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal("warm-new"))
			g.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.BeNumerically(">=", 2))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("deactivates the runtime and removes routing on deletion", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-delete", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-delete", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Finalizers).To(gomega.ContainElement(modelclaimcontroller.ModelClaimFinalizer))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				latest.Status.Instances[0].Port,
				constants.ModelClaimRoutingStateActive,
			)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		gomega.Expect(k8sClient.Delete(ctx, claim)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), &modelapi.ModelClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "expected claim deletion to finish, got %v", err)
			g.Expect(fixture.Runtime().DeactivateRequests()).To(gomega.ContainElement(modelclaimcontroller.DeactivateRequest{
				ModelName: claim.Name,
				Mode:      modelclaimcontroller.DeactivateStop,
			}))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations).NotTo(gomega.HaveKey(constants.ModelClaimPodAnnotationPrefix + claim.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("keeps a card-backed engine off the route until its KV limit reads back", func() {
		fixture.Runtime().SetCard(8 << 30)
		fixture.Runtime().SetDefaultState("active", true)
		fixture.Runtime().SetKVLimitReadBack(false)
		pod := fixture.CreateWarmPodPending(ns.Name, "warm-kv-gate", "pool-kv", 1)
		fixture.MarkWarmPodRunning(pod)
		claim := fixture.CreateClaim(ns.Name, "claim-kv-gate", "pool-kv", nil, nil)

		var recordedLimit int64
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			recordedLimit = latest.Status.Instances[0].KVLimitBytes
			g.Expect(recordedLimit).To(gomega.BeNumerically(">", 0))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActivating))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				0, constants.ModelClaimRoutingStateActivating,
			)
			requests := fixture.Runtime().KVLimitRequests()
			g.Expect(requests).NotTo(gomega.BeEmpty())
			g.Expect(requests[len(requests)-1].LimitBytes).To(gomega.Equal(recordedLimit))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetKVLimitReadBack(true)
		fixture.Runtime().SetClaimKV(string(claim.UID), 0, recordedLimit)
		fixture.TriggerReconcile(claim)

		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				latest.Status.Instances[0].Port, constants.ModelClaimRoutingStateActive,
			)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("de-routes an unhealthy active engine and restores the same instance", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-health", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-health", "pool-a", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		activationCalls := fixture.Runtime().ActivateCallCount()

		fixture.Runtime().SetClaimState(string(claim.UID), "active", false, "")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).NotTo(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(0)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			fixture.ExpectRoute(
				g, ns.Name, pod.Name, claim.Name,
				0, constants.ModelClaimRoutingStateActivating,
			)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(claim.UID), "active", true, "")
		fixture.TriggerReconcile(claim)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(1)))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(latest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		gomega.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(activationCalls))
	})

	ginkgo.It("removes routing before draining and stopping a deleted claim", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-drain", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-drain", "pool-a", nil, nil)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, claim).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		fixture.Runtime().SetClaimRequests(string(claim.UID), 1, 0)

		gomega.Expect(k8sClient.Delete(ctx, claim)).To(gomega.Succeed())
		gomega.Eventually(func(g gomega.Gomega) {
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latestPod)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations).NotTo(gomega.HaveKey(
				constants.ModelClaimPodAnnotationPrefix + claim.Name,
			))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		latest := &modelapi.ModelClaim{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), latest)).To(gomega.Succeed())
		gomega.Expect(latest.DeletionTimestamp.IsZero()).To(gomega.BeFalse())
		gomega.Expect(latest.Finalizers).To(gomega.ContainElement(modelclaimcontroller.ModelClaimFinalizer))
		gomega.Expect(fixture.Runtime().DeactivateRequests()).To(gomega.BeEmpty())

		fixture.Runtime().SetClaimRequests(string(claim.UID), 0, 0)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.Runtime().DeactivateRequests()).To(gomega.ContainElement(
				modelclaimcontroller.DeactivateRequest{
					ModelName: claim.Name,
					Mode:      modelclaimcontroller.DeactivateStop,
				},
			))
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), &modelapi.ModelClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue(), "claim deletion: %v", err)
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("is idempotent across repeated reconciliations", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-idempotent", "pool-a")
		claim := fixture.CreateClaim(ns.Name, "claim-idempotent", "pool-a", nil, nil)

		var originalAnnotation string
		gomega.Eventually(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(
				ctx,
				types.NamespacedName{Namespace: ns.Name, Name: pod.Name},
				latestPod,
			)).To(gomega.Succeed())
			originalAnnotation = latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name]
			g.Expect(originalAnnotation).NotTo(gomega.BeEmpty())
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
		originalActivationCalls := fixture.Runtime().ActivateCallCount()
		gomega.Expect(originalActivationCalls).To(gomega.BeNumerically(">=", 1))

		for range 3 {
			fixture.TriggerReconcile(claim)
		}
		gomega.Consistently(func(g gomega.Gomega) {
			latest := fixture.GetClaim(g, claim)
			g.Expect(latest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(latest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(fixture.Runtime().ActivateCallCount()).To(gomega.Equal(originalActivationCalls))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(
				ctx,
				types.NamespacedName{Namespace: ns.Name, Name: pod.Name},
				latestPod,
			)).To(gomega.Succeed())
			g.Expect(
				latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+claim.Name],
			).To(gomega.Equal(originalAnnotation))
		}, 2*time.Second, modelClaimInterval).Should(gomega.Succeed())
	})

	ginkgo.It("keeps multiple ModelClaims assignments and status isolated", func() {
		fixture.Runtime().SetDefaultState("active", true)
		pod := fixture.CreateWarmPod(ns.Name, "warm-shared", "pool-a")
		first := fixture.CreateClaim(ns.Name, "claim-one", "pool-a", nil, nil)
		second := fixture.CreateClaim(ns.Name, "claim-two", "pool-a", nil, nil)

		gomega.Eventually(func(g gomega.Gomega) {
			firstLatest := fixture.GetClaim(g, first)
			secondLatest := fixture.GetClaim(g, second)
			g.Expect(firstLatest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(secondLatest.Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			g.Expect(firstLatest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(secondLatest.Status.Instances).To(gomega.HaveLen(1))
			g.Expect(firstLatest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			g.Expect(secondLatest.Status.Instances[0].Pod).To(gomega.Equal(pod.Name))
			g.Expect(fixture.Runtime().ClaimUIDs()).To(gomega.ConsistOf(string(first.UID), string(second.UID)))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(
				ctx,
				types.NamespacedName{Namespace: ns.Name, Name: pod.Name},
				latestPod,
			)).To(gomega.Succeed())
			g.Expect(latestPod.Annotations).To(gomega.HaveKey(
				constants.ModelClaimPodAnnotationPrefix + first.Name,
			))
			g.Expect(latestPod.Annotations).To(gomega.HaveKey(
				constants.ModelClaimPodAnnotationPrefix + second.Name,
			))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())

		fixture.Runtime().SetClaimState(string(first.UID), "sleeping", false, "")
		fixture.TriggerReconcile(first)
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(fixture.GetClaim(g, first).Status.Phase).To(gomega.Equal(modelapi.ModelClaimSleeping))
			g.Expect(fixture.GetClaim(g, second).Status.Phase).To(gomega.Equal(modelapi.ModelClaimActive))
			latestPod := &corev1.Pod{}
			g.Expect(k8sClient.Get(
				ctx,
				types.NamespacedName{Namespace: ns.Name, Name: pod.Name},
				latestPod,
			)).To(gomega.Succeed())
			g.Expect(
				latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+first.Name],
			).To(gomega.ContainSubstring(`"state":"sleeping"`))
			g.Expect(
				latestPod.Annotations[constants.ModelClaimPodAnnotationPrefix+second.Name],
			).To(gomega.ContainSubstring(`"state":"active"`))
		}, modelClaimTimeout, modelClaimInterval).Should(gomega.Succeed())
	})
})
