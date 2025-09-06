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
	"errors"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	router                = "router"
	prefill               = "prefill"
	decode                = "decode"
	prefillImageVersionV1 = "prefill:v1"
	prefillImageVersionV2 = "prefill:v2"
	decodeImageVersionV1  = "decode:v1"
	decodeImageVersionV2  = "decode:v2"
	routerImageVersionV1  = "router:v1"
	routerImageVersionV2  = "router:v2"
)

var _ = ginkgo.Describe("RoleSet controller test", func() {
	var ns *corev1.Namespace

	// update represents a test step: optional mutation + validation
	type update struct {
		updateFunc func(*orchestrationapi.RoleSet)
		checkFunc  func(context.Context, client.Client, *orchestrationapi.RoleSet)
	}

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-roleset-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		// Ensure namespace is fully created
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	// testValidatingCase defines a test case with initial setup and a series of updates
	type testValidatingCase struct {
		makeRoleSet func() *orchestrationapi.RoleSet
		updates     []*update
	}

	// Helper function to create pod template
	makePodTemplate := func(image string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "test",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: image,
					},
				},
			},
		}
	}

	// Helper function to make pod ready
	makePodReady := func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
				Reason: "TestReady",
			},
		}
	}

	// Helper function to wait for pods and make them ready
	waitAndMakePodsReady := func(rs *orchestrationapi.RoleSet, expectedCount int) {
		gomega.Eventually(func(g gomega.Gomega) int {
			podList := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, podList,
				client.InNamespace(ns.Name),
				client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name})).To(gomega.Succeed())
			return len(podList.Items)
		}, time.Second*10, time.Millisecond*250).Should(gomega.Equal(expectedCount))

		gomega.Eventually(func(g gomega.Gomega) {
			podList := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, podList,
				client.InNamespace(ns.Name),
				client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name})).To(gomega.Succeed())

			for i := range podList.Items {
				pod := &podList.Items[i]
				if pod.DeletionTimestamp != nil {
					continue
				}
				makePodReady(pod)
				g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
			}
		}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
	}

	// Helper function to get pods for a specific role
	getPodsForRole := func(rs *orchestrationapi.RoleSet, roleName string) []*corev1.Pod {
		podList := &corev1.PodList{}
		gomega.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(ns.Name),
			client.MatchingLabels{
				constants.RoleSetNameLabelKey: rs.Name,
				constants.RoleNameLabelKey:    roleName,
			})).To(gomega.Succeed())

		pods := make([]*corev1.Pod, len(podList.Items))
		for i := range podList.Items {
			pods[i] = &podList.Items[i]
		}
		return pods
	}

	// Helper function to continuously make pods ready during upgrade
	startPodReadyHelper := func(rs *orchestrationapi.RoleSet) chan struct{} {
		stopChan := make(chan struct{})
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopChan:
					return
				case <-ticker.C:
					podList := &corev1.PodList{}
					if err := k8sClient.List(ctx, podList,
						client.InNamespace(ns.Name),
						client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name}); err != nil {
						ginkgo.GinkgoLogr.Error(err, "Failed to list pods in startPodReadyHelper")
						continue
					}
					for i := range podList.Items {
						pod := &podList.Items[i]
						if pod.DeletionTimestamp != nil {
							continue
						}
						if pod.Status.Phase != corev1.PodRunning {
							makePodReady(pod)
							if err := k8sClient.Status().Update(ctx, pod); err != nil {
								ginkgo.GinkgoLogr.Error(err, "Failed to update pod status in startPodReadyHelper", "pod", pod.Name)
							}
						}
					}
				}
			}
		}()
		return stopChan
	}

	ginkgo.DescribeTable("test RoleSet creation and reconciliation",
		func(tc *testValidatingCase) {
			roleset := tc.makeRoleSet()
			for _, update := range tc.updates {
				if update.updateFunc != nil {
					update.updateFunc(roleset)
				}

				// Fetch the latest RoleSet after update
				fetched := &orchestrationapi.RoleSet{}
				gomega.Eventually(func(g gomega.Gomega) {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(roleset), fetched)
					g.Expect(err).ToNot(gomega.HaveOccurred())
				}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())

				// Run validation check
				if update.checkFunc != nil {
					update.checkFunc(ctx, k8sClient, fetched)
				}
			}
		},

		ginkgo.Entry("normal RoleSet create and update replicas with sequential update strategy",
			&testValidatingCase{
				makeRoleSet: func() *orchestrationapi.RoleSet {
					podTemplate := corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": "nginx",
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "nginx",
									Image: "nginx:latest",
								},
							},
						},
					}

					return wrapper.MakeRoleSet("rs-normal").
						Namespace(ns.Name).
						UpdateStrategy(orchestrationapi.SequentialRoleSetStrategyType).
						WithRole("worker", 2, 0, podTemplate).
						WithRole("master", 1, 0, podTemplate).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							// Step 1: Create the RoleSet
							gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

							// Step 2: Wait for 3 Pods to be created (1 master + 2 workers)
							gomega.Eventually(func(g gomega.Gomega) int {
								podList := &corev1.PodList{}
								g.Expect(k8sClient.List(ctx, podList,
									client.InNamespace(ns.Name),
									client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name})).To(gomega.Succeed())
								return len(podList.Items)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Equal(3)) // 2 worker + 1 master

							// Step 3: Patch all Pods to Running and Ready (simulate integration test environment)
							gomega.Eventually(func(g gomega.Gomega) {
								podList := &corev1.PodList{}
								g.Expect(k8sClient.List(ctx, podList,
									client.InNamespace(ns.Name),
									client.MatchingLabels{constants.RoleSetNameLabelKey: rs.Name})).To(gomega.Succeed())

								for i := range podList.Items {
									pod := &podList.Items[i]
									if pod.DeletionTimestamp != nil {
										continue
									}
									pod.Status.Phase = corev1.PodRunning
									pod.Status.Conditions = []corev1.PodCondition{
										{
											Type:   corev1.PodReady,
											Status: corev1.ConditionTrue,
											Reason: "TestReady",
										},
									}
									g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
								}
							}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Validate Spec
							gomega.Expect(rs.Spec.UpdateStrategy).To(gomega.Equal(orchestrationapi.SequentialRoleSetStrategyType))
							gomega.Expect(rs.Spec.Roles).To(gomega.HaveLen(2))

							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								key := client.ObjectKeyFromObject(rs)
								if err := k8sClient.Get(ctx, key, latest); err != nil {
									return fmt.Errorf("failed to get latest RoleSet: %w", err)
								}

								masterRole := validation.FindRoleStatus(latest, "master")
								workerRole := validation.FindRoleStatus(latest, "worker")

								if masterRole == nil {
									return errors.New("master role status not found")
								}
								if workerRole == nil {
									return errors.New("worker role status not found")
								}

								// Validate master status
								if masterRole.Replicas != 1 {
									return fmt.Errorf("expected master.Replicas=1, got %d", masterRole.Replicas)
								}
								if masterRole.ReadyReplicas != 1 {
									return fmt.Errorf("expected master.ReadyReplicas=1, got %d", masterRole.ReadyReplicas)
								}
								if masterRole.NotReadyReplicas != 0 {
									return fmt.Errorf("expected master.NotReadyReplicas=0, got %d", masterRole.NotReadyReplicas)
								}
								if masterRole.UpdatedReplicas != 1 {
									return fmt.Errorf("expected master.UpdatedReplicas=1, got %d", masterRole.UpdatedReplicas)
								}
								if masterRole.UpdatedReadyReplicas != 1 {
									return fmt.Errorf("expected master.UpdatedReadyReplicas=1, got %d", masterRole.UpdatedReadyReplicas)
								}

								// Validate worker status
								if workerRole.Replicas != 2 {
									return fmt.Errorf("expected worker.Replicas=2, got %d", workerRole.Replicas)
								}
								if workerRole.ReadyReplicas != 2 {
									return fmt.Errorf("expected worker.ReadyReplicas=2, got %d", workerRole.ReadyReplicas)
								}
								if workerRole.NotReadyReplicas != 0 {
									return fmt.Errorf("expected worker.NotReadyReplicas=0, got %d", workerRole.NotReadyReplicas)
								}
								if workerRole.UpdatedReplicas != 2 {
									return fmt.Errorf("expected worker.UpdatedReplicas=2, got %d", workerRole.UpdatedReplicas)
								}
								if workerRole.UpdatedReadyReplicas != 2 {
									return fmt.Errorf("expected worker.UpdatedReadyReplicas=2, got %d", workerRole.UpdatedReadyReplicas)
								}

								// Validate Conditions
								cond := validation.FindCondition(string(orchestrationapi.RoleSetReady), latest.Status.Conditions)
								if cond == nil {
									return errors.New("RoleSetReady condition not found")
								}
								return nil
							}, time.Second*30, time.Millisecond*250).Should(gomega.Succeed(), "RoleSet status did not become ready in time")
						},
					},
				},
			},
		),

		ginkgo.Entry("upgrade roles in correct order based on UpgradeOrder field",
			&testValidatingCase{
				makeRoleSet: func() *orchestrationapi.RoleSet {
					int32Ptr := func(i int32) *int32 { return &i }
					maxUnavailable := intstr.FromInt32(1)

					routerRole := orchestrationapi.RoleSpec{
						Name:         router,
						Replicas:     int32Ptr(1),
						UpgradeOrder: int32Ptr(1),
						Template:     makePodTemplate(routerImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					prefillRole := orchestrationapi.RoleSpec{
						Name:         prefill,
						Replicas:     int32Ptr(2),
						UpgradeOrder: int32Ptr(2),
						Template:     makePodTemplate(prefillImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					decodeRole := orchestrationapi.RoleSpec{
						Name:         decode,
						Replicas:     int32Ptr(2),
						UpgradeOrder: int32Ptr(3),
						Template:     makePodTemplate(decodeImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					return wrapper.MakeRoleSet("upgrade-order-test").
						Namespace(ns.Name).
						UpdateStrategy(orchestrationapi.SequentialRoleSetStrategyType).
						WithRoleAdvanced(routerRole).
						WithRoleAdvanced(prefillRole).
						WithRoleAdvanced(decodeRole).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())
							waitAndMakePodsReady(rs, 5) // 1 router + 2 prefill + 2 decode
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
									return err
								}

								for _, role := range []string{router, prefill, decode} {
									roleStatus := validation.FindRoleStatus(latest, role)
									if roleStatus == nil {
										return fmt.Errorf("role status for %s not found", role)
									}
									if roleStatus.ReadyReplicas == 0 {
										return fmt.Errorf("role %s not ready yet", role)
									}
								}
								return nil
							}, time.Second*30, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())

								for i := range latest.Spec.Roles {
									switch latest.Spec.Roles[i].Name {
									case router:
										latest.Spec.Roles[i].Template = makePodTemplate(routerImageVersionV2)
									case prefill:
										latest.Spec.Roles[i].Template = makePodTemplate(prefillImageVersionV2)
									case decode:
										latest.Spec.Roles[i].Template = makePodTemplate(decodeImageVersionV2)
									}
								}

								g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
							}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							stopChan := startPodReadyHelper(rs)
							defer close(stopChan)

							// Router should upgrade first
							gomega.Eventually(func() bool {
								routerPods := getPodsForRole(rs, router)
								for _, pod := range routerPods {
									if pod.Spec.Containers[0].Image == "router:v2" {
										return true
									}
								}
								return false
							}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(), "router role should start upgrading first")

							// Prefill should upgrade after router
							gomega.Eventually(func() bool {
								routerPods := getPodsForRole(rs, router)
								routerHasNewImage := false
								for _, pod := range routerPods {
									if pod.Spec.Containers[0].Image == "router:v2" {
										routerHasNewImage = true
										break
									}
								}

								if routerHasNewImage {
									prefillPods := getPodsForRole(rs, prefill)
									for _, pod := range prefillPods {
										if pod.Spec.Containers[0].Image == prefillImageVersionV2 {
											return true
										}
									}
								}
								return false
							}, time.Second*45, time.Millisecond*500).Should(gomega.BeTrue(),
								"prefill should start upgrading after router starts")

							// Decode should upgrade after prefill
							gomega.Eventually(func() bool {
								prefillPods := getPodsForRole(rs, prefill)
								prefillHasNewImage := false
								for _, pod := range prefillPods {
									if pod.Spec.Containers[0].Image == prefillImageVersionV2 {
										prefillHasNewImage = true
										break
									}
								}

								if prefillHasNewImage {
									decodePods := getPodsForRole(rs, decode)
									for _, pod := range decodePods {
										if pod.Spec.Containers[0].Image == decodeImageVersionV2 {
											return true
										}
									}
								}
								return false
							}, time.Second*45, time.Millisecond*500).Should(gomega.BeTrue(),
								"decode should start upgrading after prefill starts")

							// Verify final state - all roles should be upgraded
							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
									return err
								}

								for _, roleName := range []string{router, prefill, decode} {
									roleStatus := validation.FindRoleStatus(latest, roleName)
									if roleStatus == nil {
										return fmt.Errorf("role status for %s not found", roleName)
									}
									if roleStatus.UpdatedReadyReplicas != roleStatus.Replicas {
										return fmt.Errorf("role %s not fully upgraded: %d/%d updated ready",
											roleName, roleStatus.UpdatedReadyReplicas, roleStatus.Replicas)
									}
								}

								// Verify all pods have the new images
								routerPods := getPodsForRole(rs, router)
								for _, pod := range routerPods {
									if pod.Spec.Containers[0].Image != routerImageVersionV2 {
										return fmt.Errorf("router pod still has old image: %s", pod.Spec.Containers[0].Image)
									}
								}

								prefillPods := getPodsForRole(rs, prefill)
								for _, pod := range prefillPods {
									if pod.Spec.Containers[0].Image != prefillImageVersionV2 {
										return fmt.Errorf("prefill pod still has old image: %s", pod.Spec.Containers[0].Image)
									}
								}

								decodePods := getPodsForRole(rs, decode)
								for _, pod := range decodePods {
									if pod.Spec.Containers[0].Image != decodeImageVersionV2 {
										return fmt.Errorf("decode pod still has old image: %s", pod.Spec.Containers[0].Image)
									}
								}

								return nil
							}, time.Second*45, time.Millisecond*500).Should(gomega.Succeed(),
								"All roles should be fully upgraded with new images")
						},
					},
				},
			},
		),

		ginkgo.Entry("handle roles with same upgrade order",
			&testValidatingCase{
				makeRoleSet: func() *orchestrationapi.RoleSet {
					int32Ptr := func(i int32) *int32 { return &i }
					maxUnavailable := intstr.FromInt32(1)

					prefillRole := orchestrationapi.RoleSpec{
						Name:         prefill,
						Replicas:     int32Ptr(1),
						UpgradeOrder: int32Ptr(1),
						Template:     makePodTemplate(prefillImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					decodeRole := orchestrationapi.RoleSpec{
						Name:         decode,
						Replicas:     int32Ptr(1),
						UpgradeOrder: int32Ptr(1),
						Template:     makePodTemplate(decodeImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					return wrapper.MakeRoleSet("same-order-test").
						Namespace(ns.Name).
						UpdateStrategy(orchestrationapi.SequentialRoleSetStrategyType).
						WithRoleAdvanced(prefillRole).
						WithRoleAdvanced(decodeRole).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())
							waitAndMakePodsReady(rs, 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
									return err
								}

								prefillStatus := validation.FindRoleStatus(latest, prefill)
								decodeStatus := validation.FindRoleStatus(latest, decode)

								if prefillStatus == nil {
									return fmt.Errorf("prefill role status not found")
								}
								if decodeStatus == nil {
									return fmt.Errorf("decode role status not found")
								}
								if prefillStatus.ReadyReplicas == 0 {
									return fmt.Errorf("prefill not ready yet")
								}
								if decodeStatus.ReadyReplicas == 0 {
									return fmt.Errorf("decode not ready yet")
								}
								return nil
							}, time.Second*30, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())

								for i := range latest.Spec.Roles {
									switch latest.Spec.Roles[i].Name {
									case prefill:
										latest.Spec.Roles[i].Template = makePodTemplate(prefillImageVersionV2)
									case decode:
										latest.Spec.Roles[i].Template = makePodTemplate(decodeImageVersionV2)
									}
								}
								g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
							}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Wait for prefill to be upgraded first, as it's defined first in the RoleSet
							gomega.Eventually(func(g gomega.Gomega) bool {
								prefillPods := getPodsForRole(rs, prefill)
								for _, pod := range prefillPods {
									if pod.Spec.Containers[0].Image == prefillImageVersionV2 {
										makePodReady(pod)
										g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
										return true
									}
								}
								return false
							}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(),
								"prefill role should upgrade first")

							// Verify decode role is not upgraded yet
							decodePods := getPodsForRole(rs, decode)
							for _, pod := range decodePods {
								gomega.Expect(pod.Spec.Containers[0].Image).To(gomega.Equal(decodeImageVersionV1),
									"decode role should not be upgraded yet")
							}

							// Now, wait for decode role to be upgraded
							gomega.Eventually(func(g gomega.Gomega) bool {
								decodePods := getPodsForRole(rs, decode)
								for _, pod := range decodePods {
									if pod.Spec.Containers[0].Image == decodeImageVersionV2 {
										makePodReady(pod)
										g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
										return true
									}
								}
								return false
							}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(),
								"decode role should upgrade after prefill")
						},
					},
				},
			},
		),

		ginkgo.Entry("handle roles with nil upgrade order (upgrade last)",
			&testValidatingCase{
				makeRoleSet: func() *orchestrationapi.RoleSet {
					int32Ptr := func(i int32) *int32 { return &i }
					maxUnavailable := intstr.FromInt32(1)

					lowOrderRole := orchestrationapi.RoleSpec{
						Name:         "low-order",
						Replicas:     int32Ptr(1),
						UpgradeOrder: int32Ptr(1),
						Template:     makePodTemplate("low:v1"),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					defaultRole := orchestrationapi.RoleSpec{
						Name:         "default-order",
						Replicas:     int32Ptr(1),
						UpgradeOrder: nil, // Should upgrade last
						Template:     makePodTemplate("default:v1"),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					return wrapper.MakeRoleSet("nil-order-test").
						Namespace(ns.Name).
						UpdateStrategy(orchestrationapi.SequentialRoleSetStrategyType).
						WithRoleAdvanced(lowOrderRole).
						WithRoleAdvanced(defaultRole).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())
							waitAndMakePodsReady(rs, 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
									return err
								}

								defaultStatus := validation.FindRoleStatus(latest, "default-order")
								lowStatus := validation.FindRoleStatus(latest, "low-order")

								if defaultStatus == nil {
									return fmt.Errorf("default-order role status not found")
								}
								if lowStatus == nil {
									return fmt.Errorf("low-order role status not found")
								}
								if defaultStatus.ReadyReplicas == 0 {
									return fmt.Errorf("default-order not ready yet")
								}
								if lowStatus.ReadyReplicas == 0 {
									return fmt.Errorf("low-order not ready yet")
								}
								return nil
							}, time.Second*30, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())

								for i := range latest.Spec.Roles {
									switch latest.Spec.Roles[i].Name {
									case "default-order":
										latest.Spec.Roles[i].Template = makePodTemplate("default:v2")
									case "low-order":
										latest.Spec.Roles[i].Template = makePodTemplate("low:v2")
									}
								}
								g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
							}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Low order role (order 1) should upgrade first
							gomega.Eventually(func() bool {
								lowPods := getPodsForRole(rs, "low-order")
								for _, pod := range lowPods {
									if pod.Spec.Containers[0].Image == "low:v2" {
										makePodReady(pod)
										_ = k8sClient.Status().Update(ctx, pod)
										return true
									}
								}
								return false
							}, time.Second*15, time.Millisecond*500).Should(gomega.BeTrue(), "Low order role should upgrade first")

							// Then default order role (nil) should upgrade last
							gomega.Eventually(func() bool {
								defaultPods := getPodsForRole(rs, "default-order")
								for _, pod := range defaultPods {
									if pod.Spec.Containers[0].Image == "default:v2" {
										makePodReady(pod)
										_ = k8sClient.Status().Update(ctx, pod)
										return true
									}
								}
								return false
							}, time.Second*20, time.Millisecond*500).Should(gomega.BeTrue(), "Default order role (nil) should upgrade last")
						},
					},
				},
			},
		),
	)
})
