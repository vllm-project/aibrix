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
	ingressRoleName       = "ingress"
	decodeRoleName        = "decode"
	prefillRoleName       = "prefill"
	postprocessRoleName   = "postprocess"
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
		ginkgo.Entry("respect role dependencies during scale-up",
			&testValidatingCase{
				makeRoleSet: func() *orchestrationapi.RoleSet {
					int32Ptr := func(i int32) *int32 { return &i }
					// ingress: no deps, starts first
					ingressRole := orchestrationapi.RoleSpec{
						Name:     ingressRoleName,
						Replicas: int32Ptr(1),
						Template: validation.MakePodTemplate("nginx:alpine"),
						// No Dependencies → starts immediately
					}

					// decode: depends on ingress
					decodeRole := orchestrationapi.RoleSpec{
						Name:         decodeRoleName,
						Replicas:     int32Ptr(2),
						Dependencies: []string{ingressRoleName},
						Template:     validation.MakePodTemplate("ghcr.io/llm-d/llm-d-inference-sim:latest"),
					}

					// prefill: depends on decode (intentionally reversed)
					prefillRole := orchestrationapi.RoleSpec{
						Name:         prefillRoleName,
						Replicas:     int32Ptr(2),
						Dependencies: []string{decodeRoleName},
						Template:     validation.MakePodTemplate("ghcr.io/llm-d/llm-d-inference-sim:latest"),
					}

					// postprocess: depends on both
					postprocessRole := orchestrationapi.RoleSpec{
						Name:         postprocessRoleName,
						Replicas:     int32Ptr(2),
						Dependencies: []string{prefillRoleName, decodeRoleName},
						Template:     validation.MakePodTemplate("alpine:latest"),
					}

					return wrapper.MakeRoleSet("dependency-test").
						Namespace(ns.Name).
						UpdateStrategy(orchestrationapi.SequentialRoleSetStrategyType).
						WithRoleAdvanced(ingressRole).
						WithRoleAdvanced(decodeRole).
						WithRoleAdvanced(prefillRole).
						WithRoleAdvanced(postprocessRole).
						Obj()
				},
				updates: []*update{
					// Stage 1: Create RoleSet → only ingress pods appear
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							gomega.Expect(k8sClient.Create(ctx, rs)).To(gomega.Succeed())

							// Only ingress (1 pod) should exist initially
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.RoleSetNameLabelKey, rs.Name, 1)

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Mark current pods (ingress) as ready
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleNameLabelKey, ingressRoleName)
							// Verify one roles
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, ingressRoleName, 1)).To(gomega.Succeed())
							}, time.Second*10).Should(gomega.Succeed())
						},
					},
					// Stage 2: decode pods should now be created (2 pods)
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							// Total pods: 1 (ingress) + 2 (decode) = 3
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.RoleSetNameLabelKey, rs.Name, 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Mark all decode pods as ready
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleNameLabelKey, decodeRoleName)
							// Verify two roles
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, ingressRoleName, 1)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, decodeRoleName, 2)).To(gomega.Succeed())
							}, time.Second*10).Should(gomega.Succeed())
						},
					},
					// Stage 3: prefill pods created (2 more → total 5)
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.RoleSetNameLabelKey, rs.Name, 5)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Mark prefill pods as ready
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleNameLabelKey, prefillRoleName)
							// Verify three roles
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.RoleSet{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, ingressRoleName, 1)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, decodeRoleName, 2)).To(gomega.Succeed())
								g.Expect(validation.ValidateRoleStatus(latest, prefillRoleName, 2)).To(gomega.Succeed())
							}, time.Second*10).Should(gomega.Succeed())
						},
					},
					// Stage 4: postprocess pods created (2 more → total 7)
					{
						updateFunc: func(rs *orchestrationapi.RoleSet) {
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.RoleSetNameLabelKey, rs.Name, 7)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, rs *orchestrationapi.RoleSet) {
							// Final mark
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.RoleNameLabelKey, postprocessRoleName)
							// Validate final status
							gomega.Eventually(func() error {
								latest := &orchestrationapi.RoleSet{}
								if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(rs), latest); err != nil {
									return err
								}
								if err := validation.ValidateRoleStatus(latest, ingressRoleName, 1); err != nil {
									return err
								}
								if err := validation.ValidateRoleStatus(latest, decodeRoleName, 2); err != nil {
									return err
								}
								if err := validation.ValidateRoleStatus(latest, prefillRoleName, 2); err != nil {
									return err
								}
								if err := validation.ValidateRoleStatus(latest, postprocessRoleName, 2); err != nil {
									return err
								}
								return nil
							}, time.Second*30).Should(gomega.Succeed())
						},
					},
				},
			},
		),
	)
})
