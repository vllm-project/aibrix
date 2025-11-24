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
	"sigs.k8s.io/controller-runtime/pkg/client"
	volcanoschedv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

var _ = ginkgo.Describe("PodSet controller test", func() {
	var ns *corev1.Namespace
	// using for podgroup type

	// update represents a test step: optional mutation + validation
	type update struct {
		updateFunc func(podset *orchestrationapi.PodSet)
		checkFunc  func(context.Context, client.Client, *orchestrationapi.PodSet)
	}

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-podset-",
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
		makePodSet func() *orchestrationapi.PodSet
		updates    []*update
	}

	ginkgo.DescribeTable("test PodSet creation and reconciliation",
		func(tc *testValidatingCase) {
			podset := tc.makePodSet()
			for _, update := range tc.updates {
				if update.updateFunc != nil {
					update.updateFunc(podset)
				}

				// Fetch the latest PodSet after update
				fetched := &orchestrationapi.PodSet{}
				gomega.Eventually(func(g gomega.Gomega) {
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(podset), fetched)
					g.Expect(err).ToNot(gomega.HaveOccurred())
				}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())

				// Run validation check
				if update.checkFunc != nil {
					update.checkFunc(ctx, k8sClient, fetched)
				}
			}
		},

		ginkgo.Entry("normal PodSet create and update replicas",
			&testValidatingCase{
				makePodSet: func() *orchestrationapi.PodSet {
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

					return wrapper.MakePodSet("podset-normal").
						Namespace(ns.Name).
						PodGroupSize(3).
						PodTemplate(podTemplate).
						Obj()
				},
				updates: []*update{
					{
						// create PodSet but all pod is not ready
						updateFunc: func(podset *orchestrationapi.PodSet) {
							// Step 1: Create the PodSet
							gomega.Expect(k8sClient.Create(ctx, podset)).To(gomega.Succeed())
							// Step 2: Wait for all Pods to be created
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.PodSetNameLabelKey, podset.Name, 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, podset *orchestrationapi.PodSet) {
							// Validate Spec
							validation.ValidatePodSetSpec(podset, 3, false)
							// Validate Status
							validation.ValidatePodSetStatus(ctx, k8sClient,
								podset, orchestrationapi.PodSetPhasePending, 3, 0)
						},
					},
					{
						// trigger PodSet all pods to ready
						updateFunc: func(podset *orchestrationapi.PodSet) {
							// Step 1: List all Pods
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.PodSetNameLabelKey, podset.Name, 3)
							// Step 2: Patch all Pods to Running and Ready (simulate integration test environment)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.PodSetNameLabelKey, podset.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, podset *orchestrationapi.PodSet) {
							// Validate Spec
							validation.ValidatePodSetSpec(podset, 3, false)
							// Validate Status
							validation.ValidatePodSetStatus(ctx, k8sClient,
								podset, orchestrationapi.PodSetPhaseReady, 3, 3)
						},
					},
				},
			},
		),
		ginkgo.Entry("normal PodSet create and create its volcano podgroup",
			&testValidatingCase{
				makePodSet: func() *orchestrationapi.PodSet {
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
					schedulingStrategy := &orchestrationapi.SchedulingStrategy{
						VolcanoSchedulingStrategy: &orchestrationapi.VolcanoSchedulingStrategySpec{
							MinMember: int32(1), Queue: "default",
						},
					}

					return wrapper.MakePodSet("podset-normal").
						Namespace(ns.Name).
						AddPodSetSchedulingStrategy(schedulingStrategy).
						PodTemplate(podTemplate).
						Obj()
				},
				updates: []*update{
					{
						// trigger PodSet all pods to ready
						updateFunc: func(podset *orchestrationapi.PodSet) {
							// Step 0: Create the PodSet
							gomega.Expect(k8sClient.Create(ctx, podset)).To(gomega.Succeed())
							// Step 1: List all Pods
							validation.WaitForPodsCreated(ctx, k8sClient, ns.Name, constants.PodSetNameLabelKey,
								podset.Name, 2)
							// Step 2: Patch all Pods to Running and Ready (simulate integration test environment)
							validation.MarkPodsReady(ctx, k8sClient, ns.Name, constants.PodSetNameLabelKey, podset.Name)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, podset *orchestrationapi.PodSet) {
							expectedLabels := map[string]string{
								constants.PodSetNameLabelKey: podset.Name,
							}
							podGroup := &volcanoschedv1beta1.PodGroup{}
							podGroup.SetGroupVersionKind(volcanoschedv1beta1.SchemeGroupVersion.WithKind("PodGroup"))
							minMember := podset.Spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinMember
							// Validate pg CRD exists
							validation.ValidatePodGroupCRDExist(ctx, dynamicClient, podGroup)
							// Validate pg Spec
							validation.ValidatePodGroupSpec(ctx, dynamicClient, podGroup, minMember,
								podset.Namespace, podset.Name)
							// Validate pg labels which is labelled by podSet controller
							validation.ValidatePodGroupLabels(ctx, dynamicClient, podGroup, expectedLabels,
								podset.Namespace, podset.Name)
						},
					},
				},
			},
		),
		// TODO: add more test case
	)
})
