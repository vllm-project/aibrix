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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

const (
	headRoleName   = "head"
	workerRoleName = "worker"
)

var _ = ginkgo.Describe("StormService controller test", func() {
	var ns *corev1.Namespace

	// update represents a test step: optional mutation + validation
	type update struct {
		updateFunc func(*orchestrationapi.StormService)
		checkFunc  func(context.Context, client.Client, *orchestrationapi.StormService)
	}

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-stormservice-",
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
		makeStormService func() *orchestrationapi.StormService
		updates          []*update
	}

	ginkgo.DescribeTable("test StormService creation and reconciliation",
		func(tc *testValidatingCase) {
			stormservice := tc.makeStormService()
			for _, update := range tc.updates {
				if update.updateFunc != nil {
					update.updateFunc(stormservice)
				}
				// Fetch the latest StormService after update
				fetched := &orchestrationapi.StormService{}
				gomega.Eventually(func(g gomega.Gomega) {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stormservice), fetched)
					g.Expect(err).ToNot(gomega.HaveOccurred())
				}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())

				// Run validation check
				if update.checkFunc != nil {
					update.checkFunc(ctx, k8sClient, fetched)
				}
			}
		},

		ginkgo.Entry("normal StormService create and update replicas with rolling update strategy",
			&testValidatingCase{
				makeStormService: func() *orchestrationapi.StormService {
					matchLabel := map[string]string{"app": "vllm-1p1d"}
					podTemplate := corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: matchLabel,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:    "vllm-pd-container",
									Image:   "vllm-openai:v0.10.0-cu128-nixl-v0.4.1-lmcache-0.3.2",
									Command: []string{"sh", "-c"},
									Args: []string{
										`python3 -m vllm.entrypoints.openai.api_server \
--host "0.0.0.0" \
--port "8000" \
--uvicorn-log-level warning \
--model /models/Qwen3-8B \
--served-model-name qwen3-8B \
--kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_both"}'`,
									},
								},
							},
						},
					}
					// Create a RoleSet spec for template
					roleSetSpec := &orchestrationapi.RoleSetSpec{
						Roles: []orchestrationapi.RoleSpec{
							{
								Name:     "prefill",
								Replicas: func() *int32 { i := int32(3); return &i }(),
								Template: podTemplate,
								Stateful: false,
							},
							{
								Name:     "decode",
								Replicas: func() *int32 { i := int32(2); return &i }(),
								Template: podTemplate,
								Stateful: true,
							},
						},
					}
					return wrapper.MakeStormService("stormservice-normal").
						Namespace(ns.Name).
						Replicas(ptr.To(int32(5))).
						Selector(metav1.SetAsLabelSelector(matchLabel)).
						UpdateStrategyType(orchestrationapi.RollingUpdateStormServiceStrategyType).
						RoleSetTemplateMeta(metav1.ObjectMeta{Labels: matchLabel}, roleSetSpec).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(ss *orchestrationapi.StormService) {
							gomega.Expect(k8sClient.Create(ctx, ss)).To(gomega.Succeed())
							// Wait for 5 RoleSets to be created (3 prefill + 2 decode roles)
							validation.WaitForRoleSetsCreated(ctx, k8sClient, ns.Name, ss.Name, 5)
							// Wait for 25 Pods (5 roles × 5 replicas each role)
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.StormServiceNameLabelKey, ss.Name, 25)
							// Mark all Pods as Ready
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.StormServiceNameLabelKey, ss.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate Spec
							validation.ValidateStormServiceSpec(ss, 5, orchestrationapi.RollingUpdateStormServiceStrategyType)
							// Validate Status
							validation.ValidateStormServiceStatus(
								ctx, k8sClient, ss,
								5, 5, 0,
								5, 5, 5,
								true, // Check revisions
							)
						},
					},
					{
						updateFunc: func(ss *orchestrationapi.StormService) {

							// Step 4: Update replicas to test scaling (scale down)
							validation.UpdateStormServiceReplicas(ctx, k8sClient, ss, 3)

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {

							// Validate scaling down
							validation.ValidateStormServiceReplicas(ctx, k8sClient, ss, 3)
						},
					},
					{
						updateFunc: func(ss *orchestrationapi.StormService) {

							// Step 5: Update replicas to test scaling (scale up)
							validation.UpdateStormServiceReplicas(ctx, k8sClient, ss, 6)

						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate scaling up
							validation.ValidateStormServiceReplicas(ctx, k8sClient, ss, 6)
						},
					},
				},
			},
		),
		ginkgo.Entry("respect role dependencies in StormService with multiple replicas",
			&testValidatingCase{
				makeStormService: func() *orchestrationapi.StormService {
					matchLabel := map[string]string{"app": "storm-deps-test"}
					podTemplate := corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: matchLabel,
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "dummy", Image: "alpine:latest", Command: []string{"sleep", "3600"}},
							},
						},
					}
					int32Ptr := func(i int32) *int32 { return &i }

					roleSetSpec := &orchestrationapi.RoleSetSpec{
						Roles: []orchestrationapi.RoleSpec{
							{
								Name:     headRoleName,
								Replicas: int32Ptr(1),
								Template: podTemplate,
								// No dependencies
							},
							{
								Name:         workerRoleName,
								Replicas:     int32Ptr(2),
								Dependencies: []string{headRoleName},
								Template:     podTemplate,
							},
						},
						UpdateStrategy: orchestrationapi.SequentialRoleSetStrategyType,
					}

					return wrapper.MakeStormService("stormservice-with-deps").
						Namespace(ns.Name).
						Replicas(ptr.To(int32(2))). // ← 2 RoleSets
						Selector(metav1.SetAsLabelSelector(matchLabel)).
						UpdateStrategyType(orchestrationapi.InPlaceUpdateStormServiceStrategyType).
						RoleSetTemplateMeta(metav1.ObjectMeta{Labels: matchLabel}, roleSetSpec).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(ss *orchestrationapi.StormService) {
							gomega.Expect(k8sClient.Create(ctx, ss)).To(gomega.Succeed())

							// Wait for 2 RoleSets to be created
							validation.WaitForRoleSetsCreated(ctx, k8sClient, ns.Name, ss.Name, 2)

							// Initially, only head pods exist: 2 RoleSets × 1 head = 2 pods
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.StormServiceNameLabelKey, ss.Name, 2)

							// Mark ALL head pods as ready → unblock worker creation in both RoleSets
							validation.MarkPodsReady(ctx, k8sClient, ns.Name,
								constants.RoleNameLabelKey, headRoleName)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate: only 'head' roles exist across all RoleSets
							gomega.Eventually(func(g gomega.Gomega) {
								roleSets := &orchestrationapi.RoleSetList{}
								g.Expect(k8sClient.List(ctx, roleSets,
									client.InNamespace(ns.Name),
									client.MatchingLabels{constants.StormServiceNameLabelKey: ss.Name})).
									To(gomega.Succeed())

								g.Expect(roleSets.Items).To(gomega.HaveLen(2))
								for _, rs := range roleSets.Items {
									// Each RoleSet should have head=1, worker=0 (not created yet)
									g.Expect(validation.ValidateRoleStatus(&rs, headRoleName, 1)).To(gomega.Succeed())
								}
							}, time.Second*10).Should(gomega.Succeed())

						},
					},
					{
						updateFunc: func(ss *orchestrationapi.StormService) {
							// After marking head ready, workers should be created
							// Total pods = 2 RoleSets × (1 head + 2 worker) = 6
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name,
								constants.StormServiceNameLabelKey, ss.Name, 6)

							// Mark worker pods ready (optional, but completes lifecycle)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name,
								constants.RoleNameLabelKey, workerRoleName)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate final RoleStatuses in StormService
							gomega.Eventually(func(g gomega.Gomega) {
								latest := &orchestrationapi.StormService{}
								g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ss), latest)).To(gomega.Succeed())

								// Aggregate role statuses: head=2, worker=4
								foundHead := false
								foundWorker := false
								for _, rs := range latest.Status.RoleStatuses {
									if rs.Name == headRoleName {
										g.Expect(rs.Replicas).To(gomega.Equal(int32(2)))
										g.Expect(rs.ReadyReplicas).To(gomega.Equal(int32(2)))
										foundHead = true
									}
									if rs.Name == workerRoleName {
										g.Expect(rs.Replicas).To(gomega.Equal(int32(4)))
										g.Expect(rs.ReadyReplicas).To(gomega.Equal(int32(4)))
										foundWorker = true
									}
								}
								g.Expect(foundHead).To(gomega.BeTrue())
								g.Expect(foundWorker).To(gomega.BeTrue())

								// Validate top-level status
								g.Expect(latest.Status.Replicas).To(gomega.Equal(int32(2)))      // 2 RoleSets
								g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(int32(2))) // both ready
							}, time.Second*15).Should(gomega.Succeed())

						},
					},
				},
			},
		),
		// TODO: add more test cases for different update strategies, stateful services, etc.
	)
})
