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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
