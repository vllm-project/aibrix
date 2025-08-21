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
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
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
		// TODO: add more test case
	)
})
