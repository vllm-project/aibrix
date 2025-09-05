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
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
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
							validation.WaitForStormServicePodsCreated(ctx, k8sClient, ns.Name, ss.Name, 25)
							// Mark all Pods as Ready
							validation.MarkStormServicePodsReady(ctx, k8sClient, ns.Name, ss.Name)
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
							patched := &orchestrationapi.StormService{}
							gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ss), patched)).To(gomega.Succeed())

							newReplicas := int32(3)
							patched.Spec.Replicas = &newReplicas
							gomega.Expect(k8sClient.Update(ctx, patched)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate scaling down
							validation.ValidateStormServiceReplicas(ctx, k8sClient, ss, 3)
						},
					},
					{
						updateFunc: func(ss *orchestrationapi.StormService) {
							// Step 5: Update replicas to test scaling (scale up)
							patched := &orchestrationapi.StormService{}
							gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(ss), patched)).To(gomega.Succeed())

							newReplicas := int32(6)
							patched.Spec.Replicas = &newReplicas
							gomega.Expect(k8sClient.Update(ctx, patched)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, ss *orchestrationapi.StormService) {
							// Validate scaling up
							validation.ValidateStormServiceReplicas(ctx, k8sClient, ss, 6)
						},
					},
				},
			},
		),
		// TODO: add more test cases for different update strategies, stateful services, etc.
	)
})
