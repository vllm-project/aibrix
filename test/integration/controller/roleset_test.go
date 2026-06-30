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
	"reflect"
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
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name, 3)

							// Step 3: Patch all Pods to Running and Ready (simulate integration test environment)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name)

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Validate Spec
							validation.ValidateRoleSetSpec(rs, 2, orchestrationapi.SequentialRoleSetStrategyType)

							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								key := client.ObjectKeyFromObject(rs)
								if err := k8sClient.Get(ctx, key, latest); err != nil {
									return fmt.Errorf("failed to get latest RoleSet: %w", err)
								}
								// Validate master status
								if err := validation.ValidateRoleStatus(latest, "master", 1); err != nil {
									return err
								}
								// Validate worker status
								if err := validation.ValidateRoleStatus(latest, "worker", 2); err != nil {
									return err
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
						Template:     validation.MakePodTemplate(routerImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					prefillRole := orchestrationapi.RoleSpec{
						Name:         prefill,
						Replicas:     int32Ptr(2),
						UpgradeOrder: int32Ptr(2),
						Template:     validation.MakePodTemplate(prefillImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					decodeRole := orchestrationapi.RoleSpec{
						Name:         decode,
						Replicas:     int32Ptr(2),
						UpgradeOrder: int32Ptr(3),
						Template:     validation.MakePodTemplate(decodeImageVersionV1),
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

							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.RoleSetNameLabelKey, rs.Name, 5)

							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {

							validation.WaitForRolesReady(ctx, k8sClient, rs, []string{router, prefill, decode})

						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {

							validation.UpdateRolesTemplate(ctx, k8sClient, rs, map[string]string{
								router:  routerImageVersionV2,
								prefill: prefillImageVersionV2,
								decode:  decodeImageVersionV2,
							})

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							stopChan := validation.StartRoleSetPodReadyLoop(ctx, k8sClient, rs, ns.Name)
							defer close(stopChan)

							// Router should upgrade first
							validation.WaitForRoleImage(ctx, k8sClient, rs, ns.Name,
								router, routerImageVersionV2,
								"router role should start upgrading first")

							// Prefill should upgrade after router
							validation.WaitForRoleUpgradeInOrder(ctx, k8sClient, rs, ns.Name,
								router, routerImageVersionV2,
								prefill, prefillImageVersionV2)

							// Decode should upgrade after prefill
							validation.WaitForRoleUpgradeInOrder(ctx, k8sClient, rs, ns.Name,
								prefill, prefillImageVersionV2,
								decode, decodeImageVersionV2)

							// Verify final state - all roles should be upgraded
							validation.WaitForRoleSetFinalState(ctx, k8sClient, rs, ns.Name, map[string]string{
								router:  routerImageVersionV2,
								prefill: prefillImageVersionV2,
								decode:  decodeImageVersionV2,
							})

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
						Template:     validation.MakePodTemplate(prefillImageVersionV1),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					decodeRole := orchestrationapi.RoleSpec{
						Name:         decode,
						Replicas:     int32Ptr(1),
						UpgradeOrder: int32Ptr(1),
						Template:     validation.MakePodTemplate(decodeImageVersionV1),
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
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name, 2)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {

							validation.WaitForRolesReady(ctx, k8sClient, rs, []string{prefill, decode})

						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {

							validation.UpdateRolesTemplate(ctx, k8sClient, rs, map[string]string{
								prefill: prefillImageVersionV2,
								decode:  decodeImageVersionV2,
							})

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Wait for prefill to be upgraded first, as it's defined first in the RoleSet
							validation.WaitForRoleImageAndReady(ctx, k8sClient, rs, ns.Name,
								prefill, prefillImageVersionV2,
								"prefill role should upgrade first")

							// Verify decode role is not upgraded yet
							decodePods := validation.GetRoleSetPodForRole(ctx, k8sClient, rs, ns.Name, decode)
							for _, pod := range decodePods {
								gomega.Expect(pod.Spec.Containers[0].Image).To(gomega.Equal(decodeImageVersionV1),
									"decode role should not be upgraded yet")
							}

							// Now, wait for decode role to be upgraded
							validation.WaitForRoleImageAndReady(ctx, k8sClient, rs, ns.Name,
								decode, decodeImageVersionV2,
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
						Template:     validation.MakePodTemplate("low:v1"),
						UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
							MaxUnavailable: &maxUnavailable,
						},
					}

					defaultRole := orchestrationapi.RoleSpec{
						Name:         "default-order",
						Replicas:     int32Ptr(1),
						UpgradeOrder: nil, // Should upgrade last
						Template:     validation.MakePodTemplate("default:v1"),
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
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name, 2)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleSetNameLabelKey, rs.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {

							validation.WaitForRolesReady(ctx, k8sClient, rs, []string{"default-order", "low-order"})

						},
					},
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {

							validation.UpdateRolesTemplate(ctx, k8sClient, rs, map[string]string{
								"default-order": "default:v2",
								"low-order":     "low:v2",
							})

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Low order role (order 1) should upgrade first
							validation.WaitForRoleImageAndReady(ctx, k8sClient, rs, ns.Name,
								"low-order", "low:v2",
								"Low order role should upgrade first")

							// Then default order role (nil) should upgrade last
							validation.WaitForRoleImageAndReady(ctx, k8sClient, rs, ns.Name,
								"default-order", "default:v2",
								"Default order role (nil) should upgrade last")

						},
					},
				},
			},
		),
	)

	ginkgo.It("injects default preferred topology affinity for pod and podset roles", func() {
		int32Ptr := func(i int32) *int32 { return &i }
		podGroupSize := int32(2)

		directRole := orchestrationapi.RoleSpec{
			Name:     "direct",
			Replicas: int32Ptr(1),
			Template: validation.MakePodTemplate("direct:v1"),
		}
		podSetRole := orchestrationapi.RoleSpec{
			Name:         "group",
			Replicas:     int32Ptr(1),
			PodGroupSize: &podGroupSize,
			Template:     validation.MakePodTemplate("group:v1"),
		}

		rs := wrapper.MakeRoleSet("topology-policy-test").
			Namespace(ns.Name).
			Label(constants.StormServiceNameLabelKey, "test-stormservice").
			Annotation(constants.RoleSetIndexAnnotationKey, "0").
			UpdateStrategy(orchestrationapi.ParallelRoleSetUpdateStrategyType).
			WithRoleAdvanced(directRole).
			WithRoleAdvanced(podSetRole).
			Obj()
		rs.Spec.TopologyPolicy = &orchestrationapi.TopologyPolicy{
			Scope: orchestrationapi.TopologyRoleSetScope,
			Key:   "kubernetes.io/hostname",
		}

		gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

		var directPod corev1.Pod
		gomega.Eventually(func(g gomega.Gomega) {
			podList := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, podList,
				client.InNamespace(ns.Name),
				client.MatchingLabels{
					constants.RoleSetNameLabelKey: rs.Name,
					constants.RoleNameLabelKey:    directRole.Name,
				},
			)).To(gomega.Succeed())
			g.Expect(podList.Items).To(gomega.HaveLen(1))
			directPod = podList.Items[0]
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		var podSet orchestrationapi.PodSet
		gomega.Eventually(func(g gomega.Gomega) {
			podSetList := &orchestrationapi.PodSetList{}
			g.Expect(k8sClient.List(ctx, podSetList,
				client.InNamespace(ns.Name),
				client.MatchingLabels{
					constants.RoleSetNameLabelKey: rs.Name,
					constants.RoleNameLabelKey:    podSetRole.Name,
				},
			)).To(gomega.Succeed())
			g.Expect(podSetList.Items).To(gomega.HaveLen(1))
			podSet = podSetList.Items[0]
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		expectedLabels := map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		}
		assertPreferredTopologyAffinity(&directPod.Spec, "kubernetes.io/hostname", expectedLabels)
		assertPreferredTopologyAffinity(&podSet.Spec.Template.Spec, "kubernetes.io/hostname", expectedLabels)
	})

	ginkgo.It("injects required topology affinity when Required mode is selected", func() {
		int32Ptr := func(i int32) *int32 { return &i }

		role := orchestrationapi.RoleSpec{
			Name:     "direct",
			Replicas: int32Ptr(1),
			Template: validation.MakePodTemplate("direct:v1"),
		}

		rs := wrapper.MakeRoleSet("topology-required-test").
			Namespace(ns.Name).
			Label(constants.StormServiceNameLabelKey, "test-stormservice").
			Annotation(constants.RoleSetIndexAnnotationKey, "0").
			UpdateStrategy(orchestrationapi.ParallelRoleSetUpdateStrategyType).
			WithRoleAdvanced(role).
			Obj()
		rs.Spec.TopologyPolicy = &orchestrationapi.TopologyPolicy{
			Scope: orchestrationapi.TopologyRoleSetScope,
			Mode:  orchestrationapi.TopologyPolicyRequired,
			Key:   "topology.kubernetes.io/zone",
		}

		gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

		var pod corev1.Pod
		gomega.Eventually(func(g gomega.Gomega) {
			podList := &corev1.PodList{}
			g.Expect(k8sClient.List(ctx, podList,
				client.InNamespace(ns.Name),
				client.MatchingLabels{
					constants.RoleSetNameLabelKey: rs.Name,
					constants.RoleNameLabelKey:    role.Name,
				},
			)).To(gomega.Succeed())
			g.Expect(podList.Items).To(gomega.HaveLen(1))
			pod = podList.Items[0]
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		assertRequiredTopologyAffinity(&pod.Spec, "topology.kubernetes.io/zone", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
	})

	ginkgo.It("injects topology affinity for StormService and Role scopes", func() {
		int32Ptr := func(i int32) *int32 { return &i }

		for _, tc := range []struct {
			name           string
			scope          orchestrationapi.TopologyScope
			expectedLabels map[string]string
		}{
			{
				name:  "stormservice",
				scope: orchestrationapi.TopologyStormServiceScope,
				expectedLabels: map[string]string{
					constants.StormServiceNameLabelKey: "test-stormservice",
				},
			},
			{
				name:  "role",
				scope: orchestrationapi.TopologyRoleScope,
				expectedLabels: map[string]string{
					constants.StormServiceNameLabelKey: "test-stormservice",
					constants.RoleNameLabelKey:         "direct",
				},
			},
		} {
			role := orchestrationapi.RoleSpec{
				Name:     "direct",
				Replicas: int32Ptr(1),
				Template: validation.MakePodTemplate(tc.name + ":v1"),
			}

			rs := wrapper.MakeRoleSet("topology-scope-"+tc.name).
				Namespace(ns.Name).
				Label(constants.StormServiceNameLabelKey, "test-stormservice").
				Annotation(constants.RoleSetIndexAnnotationKey, "0").
				UpdateStrategy(orchestrationapi.ParallelRoleSetUpdateStrategyType).
				WithRoleAdvanced(role).
				Obj()
			rs.Spec.TopologyPolicy = &orchestrationapi.TopologyPolicy{
				Scope: tc.scope,
				Key:   "kubernetes.io/hostname",
			}

			gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

			var pod corev1.Pod
			gomega.Eventually(func(g gomega.Gomega) {
				podList := &corev1.PodList{}
				g.Expect(k8sClient.List(ctx, podList,
					client.InNamespace(ns.Name),
					client.MatchingLabels{
						constants.RoleSetNameLabelKey: rs.Name,
						constants.RoleNameLabelKey:    role.Name,
					},
				)).To(gomega.Succeed())
				g.Expect(podList.Items).To(gomega.HaveLen(1))
				pod = podList.Items[0]
			}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

			assertPreferredTopologyAffinity(&pod.Spec, "kubernetes.io/hostname", tc.expectedLabels)
		}
	})

	ginkgo.It("applies topology policy updates only to newly created pods", func() {
		int32Ptr := func(i int32) *int32 { return &i }

		role := orchestrationapi.RoleSpec{
			Name:     "direct",
			Replicas: int32Ptr(1),
			Template: validation.MakePodTemplate("direct:v1"),
		}

		rs := wrapper.MakeRoleSet("topology-update-test").
			Namespace(ns.Name).
			Label(constants.StormServiceNameLabelKey, "test-stormservice").
			Annotation(constants.RoleSetIndexAnnotationKey, "0").
			UpdateStrategy(orchestrationapi.ParallelRoleSetUpdateStrategyType).
			WithRoleAdvanced(role).
			Obj()

		gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

		pods := waitForRolePods(ns.Name, rs.Name, role.Name, 1)
		none, preferred, required := countPodsByTopologyAffinity(pods, "kubernetes.io/hostname", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
		gomega.Expect(none).To(gomega.Equal(1))
		gomega.Expect(preferred).To(gomega.Equal(0))
		gomega.Expect(required).To(gomega.Equal(0))

		latest := &orchestrationapi.RoleSet{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())
		latest.Spec.TopologyPolicy = &orchestrationapi.TopologyPolicy{
			Scope: orchestrationapi.TopologyRoleSetScope,
			Key:   "kubernetes.io/hostname",
		}
		gomega.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())

		pods = waitForRolePods(ns.Name, rs.Name, role.Name, 1)
		none, preferred, required = countPodsByTopologyAffinity(pods, "kubernetes.io/hostname", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
		gomega.Expect(none).To(gomega.Equal(1))
		gomega.Expect(preferred).To(gomega.Equal(0))
		gomega.Expect(required).To(gomega.Equal(0))

		pods = deletePodAndWaitForReplacement(ns.Name, rs.Name, role.Name, pods[0])
		none, preferred, required = countPodsByTopologyAffinity(pods, "kubernetes.io/hostname", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
		gomega.Expect(none).To(gomega.Equal(0))
		gomega.Expect(preferred).To(gomega.Equal(1))
		gomega.Expect(required).To(gomega.Equal(0))

		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())
		latest.Spec.TopologyPolicy.Mode = orchestrationapi.TopologyPolicyRequired
		gomega.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())

		pods = waitForRolePods(ns.Name, rs.Name, role.Name, 1)
		none, preferred, required = countPodsByTopologyAffinity(pods, "kubernetes.io/hostname", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
		gomega.Expect(none).To(gomega.Equal(0))
		gomega.Expect(preferred).To(gomega.Equal(1))
		gomega.Expect(required).To(gomega.Equal(0))

		pods = deletePodAndWaitForReplacement(ns.Name, rs.Name, role.Name, pods[0])
		none, preferred, required = countPodsByTopologyAffinity(pods, "kubernetes.io/hostname", map[string]string{
			constants.StormServiceNameLabelKey: "test-stormservice",
			constants.RoleSetNameLabelKey:      rs.Name,
		})
		gomega.Expect(none).To(gomega.Equal(0))
		gomega.Expect(preferred).To(gomega.Equal(0))
		gomega.Expect(required).To(gomega.Equal(1))
	})
})

func waitForRolePods(namespace, roleSetName, roleName string, expected int) []corev1.Pod {
	var pods []corev1.Pod
	gomega.Eventually(func(g gomega.Gomega) {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(namespace),
			client.MatchingLabels{
				constants.RoleSetNameLabelKey: roleSetName,
				constants.RoleNameLabelKey:    roleName,
			},
		)).To(gomega.Succeed())
		g.Expect(podList.Items).To(gomega.HaveLen(expected))
		pods = podList.Items
	}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())
	return pods
}

func deletePodAndWaitForReplacement(namespace, roleSetName, roleName string, oldPod corev1.Pod) []corev1.Pod {
	gomega.Expect(k8sClient.Delete(ctx, &oldPod, client.GracePeriodSeconds(0))).To(gomega.Succeed())

	var pods []corev1.Pod
	gomega.Eventually(func(g gomega.Gomega) {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(namespace),
			client.MatchingLabels{
				constants.RoleSetNameLabelKey: roleSetName,
				constants.RoleNameLabelKey:    roleName,
			},
		)).To(gomega.Succeed())
		g.Expect(podList.Items).To(gomega.HaveLen(1))
		g.Expect(podList.Items[0].UID).ToNot(gomega.Equal(oldPod.UID))
		pods = podList.Items
	}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())
	return pods
}

func assertPreferredTopologyAffinity(spec *corev1.PodSpec, topologyKey string, matchLabels map[string]string) {
	gomega.Expect(spec.Affinity).ToNot(gomega.BeNil())
	gomega.Expect(spec.Affinity.PodAffinity).ToNot(gomega.BeNil())
	gomega.Expect(spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(gomega.BeEmpty())

	preferredTerms := spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	gomega.Expect(preferredTerms).To(gomega.HaveLen(1))
	gomega.Expect(preferredTerms[0].Weight).To(gomega.Equal(int32(100)))
	gomega.Expect(preferredTerms[0].PodAffinityTerm.TopologyKey).To(gomega.Equal(topologyKey))
	gomega.Expect(preferredTerms[0].PodAffinityTerm.LabelSelector.MatchLabels).To(gomega.Equal(matchLabels))
}

func assertRequiredTopologyAffinity(spec *corev1.PodSpec, topologyKey string, matchLabels map[string]string) {
	gomega.Expect(spec.Affinity).ToNot(gomega.BeNil())
	gomega.Expect(spec.Affinity.PodAffinity).ToNot(gomega.BeNil())
	gomega.Expect(spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution).To(gomega.BeEmpty())

	requiredTerms := spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	gomega.Expect(requiredTerms).To(gomega.HaveLen(1))
	gomega.Expect(requiredTerms[0].TopologyKey).To(gomega.Equal(topologyKey))
	gomega.Expect(requiredTerms[0].LabelSelector.MatchLabels).To(gomega.Equal(matchLabels))
}

func countPodsByTopologyAffinity(
	pods []corev1.Pod,
	topologyKey string,
	matchLabels map[string]string,
) (none, preferred, required int) {
	for i := range pods {
		spec := &pods[i].Spec
		switch {
		case hasRequiredTopologyAffinity(spec, topologyKey, matchLabels):
			required++
		case hasPreferredTopologyAffinity(spec, topologyKey, matchLabels):
			preferred++
		default:
			none++
		}
	}
	return none, preferred, required
}

func hasRequiredTopologyAffinity(spec *corev1.PodSpec, topologyKey string, matchLabels map[string]string) bool {
	if spec.Affinity == nil || spec.Affinity.PodAffinity == nil {
		return false
	}
	for _, term := range spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == topologyKey &&
			term.LabelSelector != nil &&
			reflect.DeepEqual(term.LabelSelector.MatchLabels, matchLabels) {
			return true
		}
	}
	return false
}

func hasPreferredTopologyAffinity(spec *corev1.PodSpec, topologyKey string, matchLabels map[string]string) bool {
	if spec.Affinity == nil || spec.Affinity.PodAffinity == nil {
		return false
	}
	for _, term := range spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if term.Weight == int32(100) &&
			term.PodAffinityTerm.TopologyKey == topologyKey &&
			term.PodAffinityTerm.LabelSelector != nil &&
			reflect.DeepEqual(term.PodAffinityTerm.LabelSelector.MatchLabels, matchLabels) {
			return true
		}
	}
	return false
}
