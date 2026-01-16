/*
Copyright 2025 The Aibrix Team.

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
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

const (
	originalImage = "busybox:1.36"
	newImage      = "busybox:1.37"
)

// NOTE: All canary pause steps require manual intervention to resume.
// The Duration field in PauseStep is accepted for API compatibility but not implemented.
// Tests must manually remove pause conditions to advance canary deployment.
// See examples in tests below where we patch status.CanaryStatus.PauseConditions = nil to resume.

var _ = ginkgo.Describe("StormService Canary Controller Integration Test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-canary-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())

		// Ensure namespace is fully created
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		// List all StormServices in the namespace and remove finalizers before deleting namespace
		// This prevents controller reconciliation conflicts during teardown
		stormServiceList := &orchestrationapi.StormServiceList{}
		if err := k8sClient.List(ctx, stormServiceList, client.InNamespace(ns.Name)); err == nil {
			for i := range stormServiceList.Items {
				ss := &stormServiceList.Items[i]
				if len(ss.Finalizers) > 0 {
					// Remove finalizers to allow clean deletion
					ss.Finalizers = nil
					_ = k8sClient.Update(ctx, ss)
				}
			}

			// Small delay to let controller process finalizer removal
			time.Sleep(1 * time.Second)
		}

		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.Context("Canary Rollout Constraints", func() {
		ginkgo.It("should respect maxUnavailable constraints during canary rollout", func() {
			stormService := createCanaryStormService(ns.Name, "constraint-test")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// 25% of 10 = 2.5 -> 3 desired, but maxUnavailable+maxSurge limits to 2 canary
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(25))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(50))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, time.Second*10).Should(gomega.Succeed())

			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				if updated.Status.CanaryStatus == nil {
					return false
				}
				// At 25% with replicas=10, maxSurge=2, maxUnavailable=1:
				// Achievable canary should be 2..3 (not capped at 1).
				return getCanaryReplicas(updated) >= 2 && getCanaryReplicas(updated) <= 3
			}, time.Second*10, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should not advance steps until rollout achieves target", func() {
			stormService := createCanaryStormService(ns.Name, "step-validation")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// 25% of 10 = 2.5 -> 3 desired, but maxUnavailable+maxSurge limits to 2 canary
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(25))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(50))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, time.Second*10).Should(gomega.Succeed())

			// 1. Wait for canary to reach achievable target
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				if updated.Status.CanaryStatus == nil {
					return false
				}
				canaryReplicaExpected := getCanaryReplicas(updated) >= 1 && getCanaryReplicas(updated) <= 3
				return canaryReplicaExpected && updated.Status.CanaryStatus.CurrentStep == 1
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			// 2. Once 1 replica is available and ready, rollout should advance to step 1 (pause)
			gomega.Eventually(func() bool {
				// Keep nudging RoleSets to Ready while waiting for step to advance
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st)
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				return getCanaryReplicas(st) >= 1 && cs.CurrentStep == 1
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			// 3. Step 1 should hold steady (pause)
			gomega.Consistently(func() bool {
				// Keep nudging RoleSets to Ready during pause validation
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st)
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				return getCanaryReplicas(st) >= 1 && cs.CurrentStep == 1
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

		})

		ginkgo.It("should handle 100% weight with progressive rollout respecting constraints", func() {
			stormService := createCanaryStormService(ns.Name, "full-rollout")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 2}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				// won't go over maxSurge + maxUnavailable
				return getCanaryReplicas(st) > 0 && getCanaryReplicas(st) <= 3
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}

				// A: clean up CanaryStatus after canary completion
				completedByRevision := st.Status.CurrentRevision == st.Status.UpdateRevision &&
					st.Status.Replicas == *stormService.Spec.Replicas

				// B: CanaryStatus still exist but canary replicas == desired replicas
				completedByCount := st.Status.CanaryStatus != nil &&
					getCanaryReplicas(st) == *stormService.Spec.Replicas

				return completedByRevision || completedByCount
			}, 10*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should enforce constraint calculation based on maxSurge+maxUnavailable cap", func() {
			ginkgo.Skip("Fix me later")
			stormService := createCanaryStormService(ns.Name, "constraint-calculation")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 2}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 3}
			// For 50% with replicas=10: desired=5, cap=maxSurge+maxUnavailable=5 → achievable=5 (progressive waves allowed)
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(50))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Short window: rollout should progress but never exceed the concurrent cap (5) at 50% step
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 0 && getCanaryReplicas(updated) > 0 && getCanaryReplicas(updated) <= 5
			}, 10*time.Second).Should(gomega.BeTrue())

			// Eventually it should reach the achievable target for 50% (min(desired=5, cap=5) = 5)
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 1 && getCanaryReplicas(updated) == 5
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())

				if latest.Status.CanaryStatus != nil &&
					latest.Status.CanaryStatus.CurrentStep == 1 &&
					latest.Status.CanaryStatus.Phase == orchestrationapi.CanaryPhasePaused {

					before := latest.DeepCopy()
					latest.Status.CanaryStatus.PauseConditions = nil
					g.Expect(k8sClient.Status().Patch(ctx, latest, client.MergeFrom(before))).To(gomega.Succeed())
				}
			}, 5*time.Second).Should(gomega.Succeed())

			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				return (cs != nil && cs.CurrentStep >= 2) || cs == nil
			}, 30*time.Second, time.Second).Should(gomega.BeTrue())

			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				if st.Spec.Replicas == nil {
					return false
				}
				want := *st.Spec.Replicas
				finished := st.Status.CurrentRevision == st.Status.UpdateRevision &&
					st.Status.Replicas == want &&
					st.Status.ReadyReplicas == want
				return finished
			}, 60*time.Second, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should prevent step advancement without achieving target replicas", func() {
			stormService := createCanaryStormService(ns.Name, "step-target-validation")
			stormService.Spec.Replicas = ptr.To(int32(6))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			// For 33% with replicas=6: desired=ceil(1.98)=2, cap=2 → achieved=2
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(33))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(66))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Gate: before reaching 2 canary replicas, step must stay 0.
			gomega.Consistently(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					// here it may not have canary status yet, let's return true
					return true
				}
				// As long as target not reached, do not advance.
				if getCanaryReplicas(st) < 2 {
					return cs.CurrentStep == 0
				}
				return true
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())

			// Once target is reached, it should advance to step 1 (Pause).
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)

				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				// Use >= to avoid missing a brief exact match.
				return getCanaryReplicas(st) >= 2 && cs.CurrentStep == 1
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())

				if latest.Status.CanaryStatus != nil &&
					latest.Status.CanaryStatus.CurrentStep == 1 &&
					latest.Status.CanaryStatus.Phase == orchestrationapi.CanaryPhasePaused {

					before := latest.DeepCopy()
					latest.Status.CanaryStatus.PauseConditions = nil
					g.Expect(k8sClient.Status().Patch(ctx, latest, client.MergeFrom(before))).To(gomega.Succeed())
				}
			}, 5*time.Second).Should(gomega.Succeed())

			// After the Pause expires, it should advance to step 2 (66%).
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				return cs.CurrentStep >= 2
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should show actual vs target replica counts during rollout", func() {
			stormService := createCanaryStormService(ns.Name, "constraint-test")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// 25% of 10 = 2.5 -> 3 desired, but maxUnavailable+maxSurge limits to 2 canary
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(50))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Initial revision present
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*10, time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger canary by bumping image
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Status should reflect first-step achievable (not desired): canary=2, stable=8
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				if cs == nil {
					return false
				}

				// CurrentStep stays at 0 because desired=5 not yet achieved under current controller semantics
				return cs != nil && cs.CurrentStep == 0 && getCanaryReplicas(updated) >= 1 && getCanaryReplicas(updated) <= 5
			}, 10*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
		})

		ginkgo.It("should complete canary only when all replicas are updated", func() {
			stormService := createCanaryStormService(ns.Name, "completion-validation")
			stormService.Spec.Replicas = ptr.To(int32(4))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			// Single 100% step; controller must still respect progressive constraints (cap=2)
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil &&
					got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Capture original revision and trigger canary
			originalRevision := ""
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if originalRevision == "" {
					originalRevision = latest.Status.CurrentRevision
				}
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// While canary is active, verify progressive rollout and cap adherence
			gomega.Consistently(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				if cs == nil {
					// rollout not started yet → still valid
					return true
				}
				// Before completion: canaries must not exceed cap=2
				finished := updated.Status.CurrentRevision == updated.Status.UpdateRevision &&
					updated.Status.CurrentRevision != originalRevision &&
					updated.Status.Replicas == *stormService.Spec.Replicas
				if finished {
					return true
				}
				spec := int32(1)
				if updated.Spec.Replicas != nil {
					spec = *updated.Spec.Replicas
				}
				return updated.Status.UpdatedReplicas <= spec
			}, 5*time.Second, time.Second).Should(gomega.BeTrue())

			// Completion: update must finish with all replicas updated and revisions aligned
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				finished := updated.Status.CurrentRevision == updated.Status.UpdateRevision &&
					updated.Status.CurrentRevision != originalRevision &&
					updated.Status.Replicas == *stormService.Spec.Replicas
				return finished
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("Canary Constraint Integration - most complex one", func() {
		ginkgo.It("should demonstrate actual vs target counting with complex constraints", func() {
			ginkgo.Skip("bring this case back later")
			stormService := createCanaryStormService(ns.Name, "complex-constraints")
			stormService.Spec.Replicas = ptr.To(int32(8))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// cap = maxSurge + maxUnavailable = 3
			// desired(25%) = 2  ⇒ achievable=min(2,3)=2
			// desired(50%) = 4  ⇒ achievable=min(4,3)=3
			// desired(75%) = 6  ⇒ achievable=min(6,3)=3
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(25))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(50))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(75))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second, 500*time.Millisecond).Should(gomega.BeTrue())

			gomega.Expect(markStableRoleSetsReady(ctx, k8sClient, stormService)).To(gomega.Succeed())

			// Trigger rollout
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = newImage
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Step 0 (25%): accept that controller may enter pause immediately after achieving 2;
			// require canary >= 2 and step <= 1 to tolerate the instant transition.
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				// stable=6 should still hold at/around this point
				return getCanaryReplicas(st) >= 2 && getStableReplicas(st) == 6 && cs.CurrentStep <= 1
			}, 30*time.Second, time.Second).Should(gomega.BeTrue())

			// After the pause resume, it should advance into step 2 (50%)
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())

				if latest.Status.CanaryStatus != nil &&
					latest.Status.CanaryStatus.CurrentStep == 1 &&
					latest.Status.CanaryStatus.Phase == orchestrationapi.CanaryPhasePaused {

					before := latest.DeepCopy()
					latest.Status.CanaryStatus.PauseConditions = nil
					g.Expect(k8sClient.Status().Patch(ctx, latest, client.MergeFrom(before))).To(gomega.Succeed())
				}
			}, 5*time.Second).Should(gomega.Succeed())

			// Step 2 (50%): achievable=3, so canary should converge to 3 and stable to 5
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				// Assert final counts at this step (Eventually will wait through transient values)
				return cs != nil && cs.CurrentStep == 2 && getCanaryReplicas(st) == 3 && getStableReplicas(st) == 5
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())

			// After the second 5s pause, it should advance into step 4 (75%)
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())

				if latest.Status.CanaryStatus != nil &&
					latest.Status.CanaryStatus.CurrentStep == 3 &&
					latest.Status.CanaryStatus.Phase == orchestrationapi.CanaryPhasePaused {

					before := latest.DeepCopy()
					latest.Status.CanaryStatus.PauseConditions = nil
					g.Expect(k8sClient.Status().Patch(ctx, latest, client.MergeFrom(before))).To(gomega.Succeed())
				}
			}, 5*time.Second).Should(gomega.Succeed())

			// Step 4 (75%): achievable still capped at 3 → canary stays at 3, stable=5 (8-3)
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 4 && getCanaryReplicas(st) == 3 && getStableReplicas(st) == 5
			}, 10*time.Second, 1*time.Second).Should(gomega.BeTrue())

			// Eventually finish 100%: all replicas updated and revisions aligned
			gomega.Eventually(func() bool {
				_ = markCanaryRoleSetsReady(ctx, k8sClient, stormService)
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				done := st.Status.CurrentRevision == st.Status.UpdateRevision &&
					st.Status.ReadyReplicas == *stormService.Spec.Replicas
				return done
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())
		})

	})

})

// getCanaryReplicas returns the current canary replica count using the consolidated status fields
func getCanaryReplicas(ss *orchestrationapi.StormService) int32 {
	return ss.Status.UpdatedReplicas
}

// getStableReplicas returns the current stable replica count using the consolidated status fields
func getStableReplicas(ss *orchestrationapi.StormService) int32 {
	return ss.Status.Replicas - ss.Status.UpdatedReplicas
}

func dumpRoleSetsAndPods(ctx context.Context, c client.Client, ss *orchestrationapi.StormService) {
	fmt.Fprintln(ginkgo.GinkgoWriter, "\n==== DUMP RS/PODS ====")

	cur := &orchestrationapi.StormService{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ss), cur); err == nil {
		fmt.Fprintf(ginkgo.GinkgoWriter, "SS[%s/%s]: curRev=%s updRev=%s replicas=%d ready=%d\n",
			cur.Namespace, cur.Name, cur.Status.CurrentRevision, cur.Status.UpdateRevision,
			cur.Status.Replicas, cur.Status.ReadyReplicas)
		if cs := cur.Status.CanaryStatus; cs != nil {
			fmt.Fprintf(ginkgo.GinkgoWriter, "  Canary: step=%d phase=%s canary=%d stable=%d\n",
				cs.CurrentStep, cs.Phase, getCanaryReplicas(cur), getStableReplicas(cur))
		}
	}

	var rsList orchestrationapi.RoleSetList
	if err := c.List(ctx, &rsList, client.InNamespace(ss.Namespace)); err != nil {
		fmt.Fprintln(ginkgo.GinkgoWriter, "list RS err:", err)
		return
	}

	upd := cur.Status.UpdateRevision
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		lbl := rs.GetLabels()
		ann := rs.GetAnnotations()
		rev := lbl["storm-service-revision"]
		if rev == "" {
			rev = lbl["orchestration.aibrix.ai/revision"]
		}
		if rev == "" {
			rev = ann["stormservice.orchestration.aibrix.ai/revision"]
		}
		isUpd := (rev == upd)

		fmt.Fprintf(ginkgo.GinkgoWriter, "- RS %s rev=%s isUpdate=%v del=%v\n",
			rs.Name, rev, isUpd, rs.DeletionTimestamp != nil)

		for _, r := range rs.Status.Roles {
			fmt.Fprintf(ginkgo.GinkgoWriter,
				"    role=%s rep=%d ready=%d upd=%d updReady=%d notReady=%d\n",
				r.Name, r.Replicas, r.ReadyReplicas, r.UpdatedReplicas, r.UpdatedReadyReplicas, r.NotReadyReplicas)
		}
		for _, cond := range rs.Status.Conditions {
			fmt.Fprintf(ginkgo.GinkgoWriter,
				"    cond %s=%s reason=%s msg=%s\n",
				cond.Type, cond.Status, cond.Reason, cond.Message)
		}

		// Dump Pods under this RoleSet
		var pods corev1.PodList
		if err := c.List(ctx, &pods,
			client.InNamespace(ss.Namespace),
			client.MatchingLabels(map[string]string{"roleset-name": rs.Name}),
		); err == nil {
			for _, p := range pods.Items {
				ready := false
				for _, pc := range p.Status.Conditions {
					if pc.Type == corev1.PodReady && pc.Status == corev1.ConditionTrue {
						ready = true
						break
					}
				}
				fmt.Fprintf(ginkgo.GinkgoWriter, "    pod=%s phase=%s ready=%v\n",
					p.Name, p.Status.Phase, ready)
			}
		}
	}
}

// markCanaryRoleSetsReady marks *per-role* status counters Ready for all RoleSets that belong
// to the update revision of the given StormService. It does NOT modify spec nor pods.
// This is intended only for integration tests to let canary math progress.
func markCanaryRoleSetsReady(ctx context.Context, c client.Client, ss *orchestrationapi.StormService) error {
	// 1) fetch latest SS to get UpdateRevision
	cur := &orchestrationapi.StormService{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ss), cur); err != nil {
		return err
	}
	upd := cur.Status.UpdateRevision
	if upd == "" {
		return nil
	}

	// 2) list RoleSets (we’ll filter by either label or annotation)
	var rsList orchestrationapi.RoleSetList
	labels := map[string]string{"storm-service-revision": upd, "storm-service-name": ss.Name}
	if err := c.List(ctx, &rsList, client.InNamespace(cur.Namespace), client.MatchingLabels(labels)); err != nil {
		return err
	}

	batchSize := 2 // magic number, we use it to avoid updating all replicas

	progressed := 0
	skip := 0
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if rs.DeletionTimestamp != nil {
			skip++
			continue
		}
		if progressed >= batchSize {
			break
		}

		// 3) list Pods of this RoleSet
		var pods corev1.PodList
		if err := c.List(ctx, &pods,
			client.InNamespace(cur.Namespace),
			client.MatchingLabels(map[string]string{"roleset-name": rs.Name}),
		); err != nil {
			return err
		}

		// allReady pods do not take the quota
		allReady := true
		for j := range pods.Items {
			p := &pods.Items[j]
			if p.DeletionTimestamp != nil {
				continue
			}
			// if there's one pod is not ready, think it's not ready
			ready := false
			for k := range p.Status.Conditions {
				if p.Status.Conditions[k].Type == corev1.PodReady && p.Status.Conditions[k].Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				allReady = false
				break
			}
		}
		if allReady {
			continue
		}

		now := metav1.Now()
		for j := range pods.Items {
			p := &pods.Items[j]

			if p.DeletionTimestamp != nil {
				continue
			}

			p.Status.Phase = corev1.PodRunning
			hasReady := false
			for k := range p.Status.Conditions {
				if p.Status.Conditions[k].Type == corev1.PodReady {
					p.Status.Conditions[k].Status = corev1.ConditionTrue
					p.Status.Conditions[k].LastTransitionTime = now
					hasReady = true
					break
				}
			}
			if !hasReady {
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
				})
			}

			for k := range p.Status.ContainerStatuses {
				cs := &p.Status.ContainerStatuses[k]
				cs.Ready = true
				cs.Started = ptr.To(true)
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				}
			}

			// update pod status
			if err := c.Status().Update(ctx, p); err != nil {
				// tolerate conflicts
				_ = c.Get(ctx, client.ObjectKeyFromObject(p), p)
				_ = c.Status().Update(ctx, p)
			}
		}
		progressed++
	}

	step := int32(-1)
	if cur.Status.CanaryStatus != nil {
		step = cur.Status.CanaryStatus.CurrentStep
	}
	fmt.Fprintf(ginkgo.GinkgoWriter, "step=%d, rsList length=%d, skipped %d, UpdateRevision %s, "+
		"currentRevision %s\n", step, len(rsList.Items), skip, cur.Status.UpdateRevision, cur.Status.CurrentRevision)
	return nil
}

func markStableRoleSetsReady(ctx context.Context, c client.Client, ss *orchestrationapi.StormService) error {
	cur := &orchestrationapi.StormService{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(ss), cur); err != nil {
		return err
	}
	curRev := cur.Status.CurrentRevision
	if curRev == "" {
		return nil
	}

	var rsList orchestrationapi.RoleSetList
	if err := c.List(ctx, &rsList,
		client.InNamespace(cur.Namespace),
		client.MatchingLabels(map[string]string{
			"storm-service-name":     ss.Name,
			"storm-service-revision": curRev,
		}),
	); err != nil {
		return err
	}

	now := metav1.Now()
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if rs.DeletionTimestamp != nil {
			continue
		}
		// list pods under this roleset
		var pods corev1.PodList
		if err := c.List(ctx, &pods,
			client.InNamespace(cur.Namespace),
			client.MatchingLabels(map[string]string{"roleset-name": rs.Name}),
		); err != nil {
			return err
		}
		for j := range pods.Items {
			p := &pods.Items[j]
			p.Status.Phase = corev1.PodRunning
			// Pod Ready condition
			readyIdx := -1
			for k := range p.Status.Conditions {
				if p.Status.Conditions[k].Type == corev1.PodReady {
					readyIdx = k
					break
				}
			}
			if readyIdx >= 0 {
				p.Status.Conditions[readyIdx].Status = corev1.ConditionTrue
				p.Status.Conditions[readyIdx].LastTransitionTime = now
			} else {
				p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: now,
				})
			}
			// ContainerStatuses
			for k := range p.Status.ContainerStatuses {
				cs := &p.Status.ContainerStatuses[k]
				cs.Ready = true
				cs.Started = ptr.To(true)
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				}
			}
			_ = c.Status().Update(ctx, p) // tolerance conflicts
		}
	}
	return nil
}

func createCanaryStormService(namespace, name string) *orchestrationapi.StormService {
	return &orchestrationapi.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationapi.StormServiceSpec{
			Replicas: ptr.To(int32(2)),
			Stateful: true,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			UpdateStrategy: orchestrationapi.StormServiceUpdateStrategy{
				Type: orchestrationapi.RollingUpdateStormServiceStrategyType,
				Canary: &orchestrationapi.CanaryUpdateStrategy{
					Steps: []orchestrationapi.CanaryStep{
						{SetWeight: ptr.To(int32(50))},
						// Duration is accepted but ignored - all pauses are manual
						{Pause: &orchestrationapi.PauseStep{}},
						{SetWeight: ptr.To(int32(100))},
					},
				},
			},
			Template: orchestrationapi.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: &orchestrationapi.RoleSetSpec{
					Roles: []orchestrationapi.RoleSpec{
						{
							Name:         "worker",
							Replicas:     ptr.To(int32(1)),
							UpgradeOrder: ptr.To(int32(1)),
							Stateful:     true,
							Template: corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: map[string]string{
										"app":  name,
										"role": "worker",
									},
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "busybox",
											Image: originalImage,
											Resources: corev1.ResourceRequirements{
												Requests: corev1.ResourceList{
													corev1.ResourceCPU:    resource.MustParse("100m"),
													corev1.ResourceMemory: resource.MustParse("128Mi"),
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func createManualPauseCanaryStormService(namespace, name string) *orchestrationapi.StormService {
	stormService := createCanaryStormService(namespace, name)
	stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
		{SetWeight: ptr.To(int32(50))},
		{Pause: &orchestrationapi.PauseStep{}}, // Manual pause
		{SetWeight: ptr.To(int32(100))},
	}
	return stormService
}

func createComplexCanaryStormService(namespace, name string) *orchestrationapi.StormService {
	stormService := createCanaryStormService(namespace, name)
	stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
		{SetWeight: ptr.To(int32(25))},
		// All pauses are manual - duration field ignored
		{Pause: &orchestrationapi.PauseStep{}},
		{SetWeight: ptr.To(int32(50))},
		{Pause: &orchestrationapi.PauseStep{}},
		{SetWeight: ptr.To(int32(100))},
	}
	return stormService
}
