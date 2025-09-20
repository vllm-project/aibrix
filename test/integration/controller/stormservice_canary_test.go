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
			time.Sleep(100 * time.Millisecond)
		}

		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.Context("Canary API Validation", func() {
		ginkgo.It("should accept valid canary configuration", func() {
			stormService := createCanaryStormService(ns.Name, "valid-canary")

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Verify canary configuration is preserved
			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Validate canary configuration
				g.Expect(created.Spec.UpdateStrategy.Canary).ToNot(gomega.BeNil())
				g.Expect(created.Spec.UpdateStrategy.Canary.Steps).To(gomega.HaveLen(3))
				g.Expect(*created.Spec.UpdateStrategy.Canary.Steps[0].SetWeight).To(gomega.Equal(int32(50)))
				g.Expect(created.Spec.UpdateStrategy.Canary.Steps[1].Pause).ToNot(gomega.BeNil())
				g.Expect(*created.Spec.UpdateStrategy.Canary.Steps[2].SetWeight).To(gomega.Equal(int32(100)))
			}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
		})

		ginkgo.It("should reject invalid canary weight values", func() {
			stormService := createCanaryStormService(ns.Name, "invalid-weight")
			// Set invalid weight (> 100)
			stormService.Spec.UpdateStrategy.Canary.Steps[0].SetWeight = ptr.To(int32(150))

			err := k8sClient.Create(ctx, stormService)
			gomega.Expect(err).To(gomega.HaveOccurred())
		})

		ginkgo.It("should reject negative canary weight values", func() {
			stormService := createCanaryStormService(ns.Name, "negative-weight")
			// Set negative weight
			stormService.Spec.UpdateStrategy.Canary.Steps[0].SetWeight = ptr.To(int32(-10))

			err := k8sClient.Create(ctx, stormService)
			gomega.Expect(err).To(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Canary Status Management", func() {
		ginkgo.It("should accept canary status fields and maintain them", func() {
			stormService := createCanaryStormService(ns.Name, "status-init")

			// Create initial StormService
			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for controller to process
			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*10).Should(gomega.Succeed())

			// Manually set canary status to test API field validation (with retry for conflicts)
			gomega.Eventually(func(g gomega.Gomega) {
				// Get latest version to avoid conflicts
				latest := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Set canary status on latest version
				latest.Status.CanaryStatus = &orchestrationapi.CanaryStatus{
					CurrentStep:    0,
					Phase:          orchestrationapi.CanaryPhaseProgressing,
					StableRevision: "stable-revision",
					CanaryRevision: "canary-revision",
				}

				// Update with retry
				err = k8sClient.Status().Update(ctx, latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Verify canary status fields are persisted
			gomega.Eventually(func(g gomega.Gomega) {
				updated := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(updated.Status.CanaryStatus).ToNot(gomega.BeNil(), "CanaryStatus should be persisted")
				g.Expect(updated.Status.CanaryStatus.CurrentStep).To(gomega.Equal(int32(0)))
				// Weight is derived from steps, not stored directly
				g.Expect(updated.Status.CanaryStatus.Phase).To(gomega.Equal(orchestrationapi.CanaryPhaseProgressing))
				g.Expect(updated.Status.CanaryStatus.StableRevision).To(gomega.Equal("stable-revision"))
				g.Expect(updated.Status.CanaryStatus.CanaryRevision).To(gomega.Equal("canary-revision"))
			}, time.Second*10, time.Millisecond*500).Should(gomega.Succeed())
		})

		ginkgo.It("should maintain pause state correctly", func() {
			stormService := createManualPauseCanaryStormService(ns.Name, "pause-test")

			// Create StormService with manual pause configuration
			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*10).Should(gomega.Succeed())

			// Manually set canary status to paused state to test state persistence (with retry for conflicts)
			gomega.Eventually(func(g gomega.Gomega) {
				// Get latest version to avoid conflicts
				latest := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Set canary status on latest version
				now := metav1.Now()
				latest.Status.CanaryStatus = &orchestrationapi.CanaryStatus{
					CurrentStep:    1,
					Phase:          orchestrationapi.CanaryPhasePaused,
					StableRevision: "stable-revision",
					CanaryRevision: "canary-revision",
					PauseConditions: []orchestrationapi.PauseCondition{
						{
							Reason:    orchestrationapi.PauseReasonCanaryPauseStep,
							StartTime: now,
						},
					},
				}

				// Update with retry
				err = k8sClient.Status().Update(ctx, latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Verify pause state is maintained
			gomega.Eventually(func(g gomega.Gomega) {
				updated := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(updated.Status.CanaryStatus).ToNot(gomega.BeNil(), "CanaryStatus should be persisted")
				g.Expect(updated.Status.CanaryStatus.Phase).To(gomega.Equal(orchestrationapi.CanaryPhasePaused))
				g.Expect(updated.Status.CanaryStatus.PauseConditions).ToNot(gomega.BeEmpty())
				g.Expect(updated.Status.CanaryStatus.CurrentStep).To(gomega.Equal(int32(1)))
			}, time.Second*10, time.Millisecond*500).Should(gomega.Succeed())

			// Test pause field clearing by updating to non-paused state (with retry for conflicts)
			gomega.Eventually(func(g gomega.Gomega) {
				// Get latest version to avoid conflicts
				toUpdate := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), toUpdate)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Ensure CanaryStatus exists before modifying
				if toUpdate.Status.CanaryStatus == nil {
					return // Skip this iteration, wait for status to be set
				}

				// Update to non-paused state
				toUpdate.Status.CanaryStatus.Phase = orchestrationapi.CanaryPhaseProgressing
				toUpdate.Status.CanaryStatus.PauseConditions = nil

				// Update with retry
				err = k8sClient.Status().Update(ctx, toUpdate)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Verify pause state is cleared
			gomega.Eventually(func(g gomega.Gomega) {
				resumed := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), resumed)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(resumed.Status.CanaryStatus.Phase).To(gomega.Equal(orchestrationapi.CanaryPhaseProgressing))
				g.Expect(resumed.Status.CanaryStatus.PauseConditions).To(gomega.BeEmpty())
			}, time.Second*10, time.Millisecond*500).Should(gomega.Succeed())
		})

		ginkgo.It("should accept pause conditions and maintain them", func() {
			stormService := createManualPauseCanaryStormService(ns.Name, "pause-condition-test")

			// Create StormService
			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*10).Should(gomega.Succeed())

			// Test pause conditions API (with retry for conflicts)
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Set canary status with pause conditions
				now := metav1.Now()
				latest.Status.CanaryStatus = &orchestrationapi.CanaryStatus{
					CurrentStep: 1,
					Phase:       orchestrationapi.CanaryPhasePaused,
					PauseConditions: []orchestrationapi.PauseCondition{
						{
							Reason:    orchestrationapi.PauseReasonCanaryPauseStep,
							StartTime: now,
						},
					},
					StableRevision: "stable-revision",
					CanaryRevision: "canary-revision",
				}

				err = k8sClient.Status().Update(ctx, latest)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Verify pause conditions are maintained
			gomega.Eventually(func(g gomega.Gomega) {
				updated := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(updated.Status.CanaryStatus).ToNot(gomega.BeNil())
				g.Expect(updated.Status.CanaryStatus.PauseConditions).To(gomega.HaveLen(1))
				g.Expect(updated.Status.CanaryStatus.PauseConditions[0].Reason).To(
					gomega.Equal(orchestrationapi.PauseReasonCanaryPauseStep))
				g.Expect(updated.Status.CanaryStatus.PauseConditions[0].StartTime).ToNot(gomega.BeZero())
			}, time.Second*10, time.Millisecond*500).Should(gomega.Succeed())

			// Test clearing pause conditions
			gomega.Eventually(func(g gomega.Gomega) {
				toUpdate := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), toUpdate)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				if toUpdate.Status.CanaryStatus == nil {
					return // Skip if status not set
				}

				// Clear pause conditions
				toUpdate.Status.CanaryStatus.PauseConditions = []orchestrationapi.PauseCondition{}
				toUpdate.Status.CanaryStatus.Phase = orchestrationapi.CanaryPhaseProgressing

				err = k8sClient.Status().Update(ctx, toUpdate)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Verify pause conditions are cleared
			gomega.Eventually(func(g gomega.Gomega) {
				cleared := &orchestrationapi.StormService{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), cleared)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(cleared.Status.CanaryStatus.PauseConditions).To(gomega.HaveLen(0))
				g.Expect(cleared.Status.CanaryStatus.Phase).To(gomega.Equal(orchestrationapi.CanaryPhaseProgressing))
			}, time.Second*10, time.Millisecond*500).Should(gomega.Succeed())
		})

		ginkgo.It("should clear canary status on completion", func() {
			// TODO(jiaxin): kind of flaky, fix it later
			ginkgo.Skip("temporarily skipped: flaky")

			name := "clear-canary-completion"
			stormService := createCanaryStormService(ns.Name, name)

			// Create StormService
			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision to be set by the controller
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*30, time.Millisecond*300).Should(gomega.BeTrue())

			// Trigger a new revision by changing the image to start canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				// Change container image to trigger updateRevision != currentRevision
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.21"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, time.Second*10, time.Millisecond*200).Should(gomega.Succeed())

			// Wait until controller detects update (either revisions differ or canary status appears)
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				return updated.Status.UpdateRevision != updated.Status.CurrentRevision || updated.Status.CanaryStatus != nil
			}, time.Second*60, time.Millisecond*300).Should(gomega.BeTrue())

			// Simulate completion by setting CurrentStep >= len(steps) and 100% weight
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				steps := latest.Spec.UpdateStrategy.Canary.Steps
				// Ensure canaryStatus exists to edit
				if latest.Status.CanaryStatus == nil {
					latest.Status.CanaryStatus = &orchestrationapi.CanaryStatus{}
				}
				latest.Status.CanaryStatus.CurrentStep = int32(len(steps)) // mark as completed
				// Test status update (weight is derived from steps)
				// Use observed revisions for promotion fields
				latest.Status.CanaryStatus.StableRevision = latest.Status.CurrentRevision
				latest.Status.CanaryStatus.CanaryRevision = latest.Status.UpdateRevision
				// Set to Completed phase since we've finished all steps
				latest.Status.CanaryStatus.Phase = orchestrationapi.CanaryPhaseCompleted

				g.Expect(k8sClient.Status().Update(ctx, latest)).To(gomega.Succeed())
			}, time.Second*5, time.Millisecond*200).Should(gomega.Succeed())

			// Status update should be enough to trigger reconcile; no spec poke required

			// Wait for canary status to be cleared by controller completion logic
			gomega.Eventually(func() bool {
				final := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), final); err != nil {
					return false
				}

				// Return true only when canary status is cleared AND revisions align
				return final.Status.CanaryStatus == nil && final.Status.UpdateRevision == final.Status.CurrentRevision
			}, time.Second*10, time.Millisecond*500).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("Canary Mode Detection", func() {
		ginkgo.It("should detect replica mode correctly", func() {
			stormService := createCanaryStormService(ns.Name, "replica-mode")
			stormService.Spec.Replicas = ptr.To(int32(3)) // Replica mode

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// The controller should handle this as replica mode
			// We can't easily test the internal mode detection, but we can verify
			// that the canary configuration is accepted and processed
			// TODO(jiaxin): find better ways to confirm the canary mode (replica or pool)
			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(*created.Spec.Replicas).To(gomega.Equal(int32(3)))
			}, time.Second*10).Should(gomega.Succeed())
		})

		ginkgo.It("should detect pooled mode correctly", func() {
			stormService := createCanaryStormService(ns.Name, "pooled-mode")
			stormService.Spec.Replicas = ptr.To(int32(1)) // Pooled mode

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(*created.Spec.Replicas).To(gomega.Equal(int32(1)))
			}, time.Second*10).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("Canary Step Validation", func() {
		ginkgo.It("should accept valid step configurations", func() {
			stormService := createComplexCanaryStormService(ns.Name, "complex-steps")

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				canary := created.Spec.UpdateStrategy.Canary
				g.Expect(canary.Steps).To(gomega.HaveLen(5))

				// Validate step sequence
				g.Expect(*canary.Steps[0].SetWeight).To(gomega.Equal(int32(25)))
				g.Expect(canary.Steps[1].Pause.Duration.StrVal).To(gomega.Equal("30s"))
				g.Expect(*canary.Steps[2].SetWeight).To(gomega.Equal(int32(50)))
				g.Expect(canary.Steps[3].Pause.Duration).To(gomega.BeNil()) // Manual pause
				g.Expect(*canary.Steps[4].SetWeight).To(gomega.Equal(int32(100)))
			}, time.Second*10).Should(gomega.Succeed())
		})

		ginkgo.It("should handle empty canary steps gracefully", func() {
			stormService := createCanaryStormService(ns.Name, "empty-steps")
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Controller should still process the StormService, but canary should be disabled
			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				g.Expect(created.Spec.UpdateStrategy.Canary.Steps).To(gomega.HaveLen(0))
			}, time.Second*10).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("Integration with Existing UpdateStrategy", func() {
		ginkgo.It("should work with RollingUpdate strategy", func() {
			stormService := createCanaryStormService(ns.Name, "rolling-canary")
			stormService.Spec.UpdateStrategy.Type = orchestrationapi.RollingUpdateStormServiceStrategyType
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				// Should preserve existing rolling update settings
				g.Expect(created.Spec.UpdateStrategy.Type).To(gomega.Equal(orchestrationapi.RollingUpdateStormServiceStrategyType))
				g.Expect(created.Spec.UpdateStrategy.MaxUnavailable.IntVal).To(gomega.Equal(int32(1)))
				g.Expect(created.Spec.UpdateStrategy.MaxSurge.IntVal).To(gomega.Equal(int32(1)))

				// Should also have canary configuration
				g.Expect(created.Spec.UpdateStrategy.Canary).ToNot(gomega.BeNil())
			}, time.Second*10).Should(gomega.Succeed())
		})

		ginkgo.It("should work with InPlaceUpdate strategy", func() {
			stormService := createCanaryStormService(ns.Name, "inplace-canary")
			stormService.Spec.UpdateStrategy.Type = orchestrationapi.InPlaceUpdateStormServiceStrategyType

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			created := &orchestrationapi.StormService{}
			gomega.Eventually(func(g gomega.Gomega) {
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), created)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				g.Expect(created.Spec.UpdateStrategy.Type).To(gomega.Equal(orchestrationapi.InPlaceUpdateStormServiceStrategyType))
				g.Expect(created.Spec.UpdateStrategy.Canary).ToNot(gomega.BeNil())
			}, time.Second*10).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("Canary Rollout Constraints", func() {
		ginkgo.It("should respect maxUnavailable constraints during canary rollout", func() {
			stormService := createCanaryStormService(ns.Name, "constraint-test")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// 25% of 10 = 2.5 -> 3 desired canary
			// With maxUnavailable=1: achievable = min(3, 0+1) = 1
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(25))},
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(50))}, // 50% = 5 desired, achievable = min(5, 1+1) = 2
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
			}, time.Second*10).Should(gomega.BeTrue())

			// Trigger canary by updating image
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, time.Second*10).Should(gomega.Succeed())

			// Verify canary status shows achievable replicas (1) not desired (3)
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				if updated.Status.CanaryStatus == nil {
					return false
				}
				// At 25% with replicas=10, maxSurge=2, maxUnavailable=1:
				// Achievable canary should be 2..3 (not capped at 1).
				return updated.Status.CanaryStatus.CanaryReplicas >= 2 && updated.Status.CanaryStatus.CanaryReplicas <= 3
			}, time.Second*20).Should(gomega.BeTrue())
		})

		ginkgo.It("should not advance steps until rollout achieves target", func() {
			stormService := createCanaryStormService(ns.Name, "step-validation")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			// 25% of 10 = 2.5 -> 3 desired, but maxUnavailable limits to 1 canary
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(25))}, // Achievable: 1 replica
				{Pause: &orchestrationapi.PauseStep{}},
				{SetWeight: ptr.To(int32(50))}, // Should advance after achieving 1 replica
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial setup
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, time.Second*10).Should(gomega.BeTrue())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, time.Second*10).Should(gomega.Succeed())

			// Wait for canary to start
			gomega.Eventually(func() bool {
				status := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), status); err != nil {
					return false
				}
				return status.Status.CanaryStatus != nil
			}, time.Second*10, time.Second*1).Should(gomega.BeTrue())

			// 1. Before reaching 3 canary replicas, step must remain 0
			gomega.Consistently(func() bool {
				st := &orchestrationapi.StormService{}
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st)
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				if cs.CanaryReplicas < 3 {
					return cs.CurrentStep == 0
				}
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				return true
			}, time.Second*10, time.Second*1).Should(gomega.BeTrue())

			// 2. Once 3+ replicas are available, rollout should advance to step 1
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st)
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				return cs.CanaryReplicas >= 3 && cs.CurrentStep == 1
			}, time.Second*10, time.Second*1).Should(gomega.BeTrue())

			// 3. Step 1 should hold steady (pause)
			gomega.Consistently(func() bool {
				st := &orchestrationapi.StormService{}
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st)
				cs := st.Status.CanaryStatus
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				return cs != nil && cs.CurrentStep == 1
			}, time.Second*5, time.Second*1).Should(gomega.BeTrue())

		})

		ginkgo.It("should handle 100% weight with progressive rollout respecting constraints", func() {
			stormService := createCanaryStormService(ns.Name, "full-rollout")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 2}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(100))}, // 单步 100%，但仍受 MaxUnavailable/MaxSurge 渐进推进
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got); err != nil {
					return false
				}
				return got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 &&
					len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
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
				return cs.CanaryReplicas > 0 && cs.CanaryReplicas <= 3
			}, 15*time.Second).Should(gomega.BeTrue())

			// finally check
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}

				// A: clean up CanaryStatus after canary completion
				completedByRevision := st.Status.CurrentRevision == st.Status.UpdateRevision &&
					st.Status.Replicas == *stormService.Spec.Replicas

				// B: CanaryStatus still exist but canary replicas == desired replicas
				completedByCount := st.Status.CanaryStatus != nil &&
					st.Status.CanaryStatus.CanaryReplicas == *stormService.Spec.Replicas

				return completedByRevision || completedByCount
			}, 2*time.Minute).Should(gomega.BeTrue())
		})

		ginkgo.It("should enforce constraint calculation based on maxSurge+maxUnavailable cap", func() {
			stormService := createCanaryStormService(ns.Name, "constraint-calculation")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 2}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 3}
			// For 50% with replicas=10: desired=5, cap=maxSurge+maxUnavailable=5 → achievable=5 (progressive waves allowed)
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(50))},
				{SetWeight: ptr.To(int32(80))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 && len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.25"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Short window: rollout should progress but never exceed the concurrent cap (5) at 50% step
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 0 && cs.CanaryReplicas > 0 && cs.CanaryReplicas <= 5
			}, 20*time.Second).Should(gomega.BeTrue())

			// Eventually it should reach the achievable target for 50% (min(desired=5, cap=5) = 5)
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 0 && cs.CanaryReplicas >= 5
			}, 45*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should prevent step advancement without achieving target replicas", func() {
			stormService := createCanaryStormService(ns.Name, "step-target-validation")
			stormService.Spec.Replicas = ptr.To(int32(6))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 1}
			// For 33% with replicas=6: desired=ceil(1.98)=2, cap=2 → achieved=2
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(33))},
				{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "10s"}}},
				{SetWeight: ptr.To(int32(66))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 && len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Gate: before reaching 2 canary replicas, step must stay 0.
			gomega.Consistently(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					// here it may not have canary status yet, let's return true
					return true
				}
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				// As long as target not reached, do not advance.
				if cs.CanaryReplicas < 2 {
					return cs.CurrentStep == 0
				}
				return true
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())

			// Once target is reached, it should advance to step 1 (Pause).
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}

				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				// Use >= to avoid missing a brief exact match.
				return cs.CanaryReplicas >= 2 && cs.CurrentStep == 1
			}, 10*time.Second).Should(gomega.BeTrue())

			// During the 10s Pause, it should *stay* at step 1.
			gomega.Consistently(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				return cs != nil && cs.CurrentStep == 1
			}, 9*time.Second, 1*time.Second).Should(gomega.BeTrue()) // slightly <10s to avoid edge off-by-one

			// After the Pause (~10s) expires, it should advance to step 2 (66%).
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				fmt.Fprintf(ginkgo.GinkgoWriter, "replicas=%d step=%d phase=%s\n", cs.CanaryReplicas, cs.CurrentStep, cs.Phase)
				if cs == nil {
					return false
				}
				return cs.CurrentStep >= 2
			}, 40*time.Second, 1*time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("should show actual vs target replica counts during rollout", func() {
			stormService := createCanaryStormService(ns.Name, "actual-vs-target")
			stormService.Spec.Replicas = ptr.To(int32(10))
			stormService.Spec.UpdateStrategy.MaxUnavailable = &intstr.IntOrString{IntVal: 1}
			stormService.Spec.UpdateStrategy.MaxSurge = &intstr.IntOrString{IntVal: 2}
			// For 50% with replicas=10: desired=5, cap=3 → achievable=3
			stormService.Spec.UpdateStrategy.Canary.Steps = []orchestrationapi.CanaryStep{
				{SetWeight: ptr.To(int32(50))},
				{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "2s"}}},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second).Should(gomega.BeTrue())

			// Trigger canary
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 && len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.24"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// Status should reflect achievable (not desired): canary=3, stable=7
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				cs := updated.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 0 &&
					cs.CanaryReplicas == 3 &&
					cs.StableReplicas == 7
			}, 30*time.Second).Should(gomega.BeTrue())
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
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second).Should(gomega.Succeed())

			// While canary is active, verify progressive rollout and cap adherence
			gomega.Consistently(func() bool {
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
				return cs.CanaryReplicas > 0 && cs.CanaryReplicas <= 2
			}, 10*time.Second, time.Second).Should(gomega.BeTrue())

			// Completion: update must finish with all replicas updated and revisions aligned
			gomega.Eventually(func() bool {
				updated := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), updated); err != nil {
					return false
				}
				finished := updated.Status.CurrentRevision == updated.Status.UpdateRevision &&
					updated.Status.CurrentRevision != originalRevision &&
					updated.Status.Replicas == *stormService.Spec.Replicas
				return finished
			}, 10*time.Second, 1*time.Second).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("Canary Constraint Integration - most complex one", func() {
		ginkgo.It("should demonstrate actual vs target counting with complex constraints", func() {
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
				{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "5s"}}},
				{SetWeight: ptr.To(int32(50))},
				{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "5s"}}},
				{SetWeight: ptr.To(int32(75))},
				{SetWeight: ptr.To(int32(100))},
			}

			gomega.Expect(k8sClient.Create(ctx, stormService)).To(gomega.Succeed())

			// Wait for initial revision
			gomega.Eventually(func() bool {
				got := &orchestrationapi.StormService{}
				return k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), got) == nil && got.Status.CurrentRevision != ""
			}, 10*time.Second, 500*time.Millisecond).Should(gomega.BeTrue())

			// Trigger rollout
			gomega.Eventually(func(g gomega.Gomega) {
				latest := &orchestrationapi.StormService{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), latest)).To(gomega.Succeed())
				if len(latest.Spec.Template.Spec.Roles) > 0 && len(latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers) > 0 {
					latest.Spec.Template.Spec.Roles[0].Template.Spec.Containers[0].Image = "busybox:1.37"
					g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
				}
			}, 10*time.Second, 200*time.Millisecond).Should(gomega.Succeed())

			// Step 0 (25%): accept that controller may enter pause immediately after achieving 2;
			// require canary >= 2 and step <= 1 to tolerate the instant transition.
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				if cs == nil {
					return false
				}
				// stable=6 should still hold at/around this point
				return cs.CanaryReplicas >= 2 && cs.StableReplicas == 6 && cs.CurrentStep <= 1
			}, 30*time.Second, 500*time.Millisecond).Should(gomega.BeTrue())

			// After the 5s pause, it should advance into step 2 (50%)
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				return cs != nil && cs.CurrentStep >= 2
			}, 25*time.Second, 1*time.Second).Should(gomega.BeTrue())

			// Step 2 (50%): achievable=3, so canary should converge to 3 and stable to 5
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				// Assert final counts at this step (Eventually will wait through transient values)
				return cs != nil && cs.CurrentStep == 2 && cs.CanaryReplicas == 3 && cs.StableReplicas == 5
			}, 45*time.Second, 500*time.Millisecond).Should(gomega.BeTrue())

			// After the second 5s pause, it should advance into step 4 (75%)
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				return cs != nil && cs.CurrentStep >= 4
			}, 25*time.Second, 1*time.Second).Should(gomega.BeTrue())

			// Step 4 (75%): achievable still capped at 3 → canary stays at 3, stable=5 (8-3)
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				cs := st.Status.CanaryStatus
				return cs != nil && cs.CurrentStep == 4 && cs.CanaryReplicas == 3 && cs.StableReplicas == 5
			}, 45*time.Second, 500*time.Millisecond).Should(gomega.BeTrue())

			// Eventually finish 100%: all replicas updated and revisions aligned
			gomega.Eventually(func() bool {
				st := &orchestrationapi.StormService{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormService), st); err != nil {
					return false
				}
				done := st.Status.CurrentRevision == st.Status.UpdateRevision &&
					st.Status.ReadyReplicas == *stormService.Spec.Replicas
				return done
			}, 10*time.Second, 1*time.Second).Should(gomega.BeTrue())
		})

	})

})

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
						{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "10s"}}},
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
											Image: "busybox:1.36",
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
		{Pause: &orchestrationapi.PauseStep{Duration: &intstr.IntOrString{Type: intstr.String, StrVal: "30s"}}},
		{SetWeight: ptr.To(int32(50))},
		{Pause: &orchestrationapi.PauseStep{}}, // Manual pause
		{SetWeight: ptr.To(int32(100))},
	}
	return stormService
}
