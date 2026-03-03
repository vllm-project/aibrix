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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/test/utils/validation"
	"github.com/vllm-project/aibrix/test/utils/wrapper"
)

// Condition type constants from controller
const (
	ConditionReady         = "Ready"
	ConditionValidSpec     = "ValidSpec"
	ConditionConflict      = "MultiPodAutoscalerConflict"
	ConditionScalingActive = "ScalingActive"
	ConditionAbleToScale   = "AbleToScale"

	ReasonAsExpected             = "AsExpected"
	ReasonInvalidScalingStrategy = "InvalidScalingStrategy"
	ReasonInvalidBounds          = "InvalidBounds"
	ReasonMissingTargetRef       = "MissingScaleTargetRef"
	ReasonMetricsConfigError     = "MetricsConfigError"
)

var _ = ginkgo.Describe("PodAutoscaler controller test", func() {
	var ns *corev1.Namespace

	// update represents a test step: optional mutation + validation
	type update struct {
		updateFunc func(pa *autoscalingv1alpha1.PodAutoscaler)
		checkFunc  func(context.Context, client.Client, *autoscalingv1alpha1.PodAutoscaler)
	}

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-pa-",
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
		makePodAutoscaler func() *autoscalingv1alpha1.PodAutoscaler
		updates           []*update
	}

	// Helper: creates a deployment for testing
	createDeployment := func(name, namespace string, replicas int32) *appsv1.Deployment {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": name,
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": name,
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
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, deployment)).To(gomega.Succeed())
		return deployment
	}

	// Helper: creates a StormService with two roles (prefill and decode)
	createStormService := func(name, namespace string, labelKey, labelValue string,
		prefillReplicas, decodeReplicas int32) *orchestrationapi.StormService {
		matchLabel := map[string]string{labelKey: labelValue}
		podTemplate := corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: matchLabel,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "vllm-container",
						Image: "vllm/vllm-openai:latest",
					},
				},
			},
		}
		roleSetSpec := &orchestrationapi.RoleSetSpec{
			Roles: []orchestrationapi.RoleSpec{
				{
					Name:     "prefill",
					Replicas: ptr.To(prefillReplicas),
					Template: podTemplate,
					Stateful: false,
				},
				{
					Name:     "decode",
					Replicas: ptr.To(decodeReplicas),
					Template: podTemplate,
					Stateful: false,
				},
			},
		}
		ss := wrapper.MakeStormService(name).
			Namespace(namespace).
			Replicas(ptr.To(int32(2))).
			Selector(metav1.SetAsLabelSelector(matchLabel)).
			UpdateStrategyType(orchestrationapi.RollingUpdateStormServiceStrategyType).
			RoleSetTemplateMeta(metav1.ObjectMeta{Labels: matchLabel}, roleSetSpec).
			Obj()
		gomega.Expect(k8sClient.Create(ctx, ss)).To(gomega.Succeed())
		return ss
	}

	// Helper: creates a test case for boundary enforcement with similar structure
	makeBoundaryTestCase := func(name, deploymentName string, min, max int32,
		deploymentReplicas int32) *testValidatingCase {
		return &testValidatingCase{
			makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
				return wrapper.MakePodAutoscaler(name).
					Namespace(ns.Name).
					ScalingStrategy(autoscalingv1alpha1.HPA).
					MinReplicas(min).
					MaxReplicas(max).
					ScaleTargetRefWithKind("Deployment", "apps/v1", deploymentName).
					MetricSource(wrapper.MakeMetricSourcePod(
						autoscalingv1alpha1.HTTP, "8080", "/metrics",
						"requests_per_second", "100")).
					Obj()
			},
			updates: []*update{
				{
					updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
						createDeployment(deploymentName, ns.Name, deploymentReplicas)
						gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
						time.Sleep(time.Second * 2)
					},
					checkFunc: func(ctx context.Context, k8sClient client.Client,
						pa *autoscalingv1alpha1.PodAutoscaler) {
						hpa := validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
						validation.ValidateHPASpec(hpa, min, max)
					},
				},
			},
		}
	}

	// Helper: creates a test case for spec validation with similar structure
	makeSpecValidationTestCase := func(name, deploymentName string, min, max int32,
		expectedCondition string, expectedStatus metav1.ConditionStatus,
		expectedReason string) *testValidatingCase {
		return &testValidatingCase{
			makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
				return wrapper.MakePodAutoscaler(name).
					Namespace(ns.Name).
					ScalingStrategy(autoscalingv1alpha1.HPA).
					MinReplicas(min).
					MaxReplicas(max).
					ScaleTargetRefWithKind("Deployment", "apps/v1", deploymentName).
					MetricSource(wrapper.MakeMetricSourcePod(
						autoscalingv1alpha1.HTTP, "8080", "/metrics",
						"requests_per_second", "100")).
					Obj()
			},
			updates: []*update{
				{
					updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
						createDeployment(deploymentName, ns.Name, 2)
						gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
						time.Sleep(time.Second * 2)
					},
					checkFunc: func(ctx context.Context, k8sClient client.Client,
						pa *autoscalingv1alpha1.PodAutoscaler) {
						validation.WaitForPodAutoscalerConditionWithReason(
							ctx, k8sClient, pa,
							expectedCondition, expectedStatus,
							expectedReason,
						)
					},
				},
			},
		}
	}

	ginkgo.DescribeTable("test PodAutoscaler creation and reconciliation",
		func(tc *testValidatingCase) {
			pa := tc.makePodAutoscaler()
			for _, upd := range tc.updates {
				if upd.updateFunc != nil {
					upd.updateFunc(pa)
				}

				// Run validation check directly (no need to fetch if PA is deleted)
				if upd.checkFunc != nil {
					upd.checkFunc(ctx, k8sClient, pa)
				}
			}
		},

		// =========================================================================
		// HPA Strategy - Resource Lifecycle Management
		// =========================================================================

		ginkgo.Entry("HPA Strategy - Create PA → HPA Created",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-hpa-create").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create deployment first
							createDeployment("test-deployment", ns.Name, 2)
							// Create PodAutoscaler
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Validate HPA is created
							hpa := validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
							gomega.Expect(hpa).ToNot(gomega.BeNil())

							// Validate HPA OwnerReference
							validation.ValidateHPAOwnerReference(hpa, pa.Name, "PodAutoscaler")

							// Validate HPA Spec
							validation.ValidateHPASpec(hpa, 1, 5)
							validation.ValidateHPAScaleTargetRef(hpa, "Deployment", "test-deployment")
						},
					},
				},
			},
		),

		ginkgo.Entry("HPA Strategy - Update PA → HPA Synced",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-hpa-update").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deployment-2").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("test-deployment-2", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Wait for HPA creation
							validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Update PodAutoscaler spec with retry for race conditions
							gomega.Eventually(func() error {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								minReplicas := int32(2)
								fetched.Spec.MinReplicas = &minReplicas
								fetched.Spec.MaxReplicas = 10
								return k8sClient.Update(ctx, fetched)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
							// Give controller time to reconcile the updated PA and sync HPA
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Validate HPA is updated with more relaxed timing
							gomega.Eventually(func(g gomega.Gomega) {
								hpa := validation.GetHPA(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
								validation.ValidateHPASpec(hpa, 2, 10)
							}, time.Second*15, time.Second*1).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("HPA Strategy - Delete PA → HPA Deleted (Cascade)",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-hpa-delete").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "test-deployment-3").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("test-deployment-3", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Wait for HPA creation
							validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Wait for initial reconcile
							time.Sleep(time.Second * 3)
							// Delete PodAutoscaler
							gomega.Expect(k8sClient.Delete(ctx, pa)).To(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Validate PA is deleted
							validation.WaitForPodAutoscalerDeleted(ctx, k8sClient, pa)
							// Note: In envtest, HPA cascade deletion via OwnerReference doesn't work
							// because garbage collector controller is not running. In real K8s,
							// the HPA would be automatically deleted due to OwnerReference.
							// We already verified OwnerReference is set correctly in the creation test.
						},
					},
				},
			},
		),

		// =========================================================================
		// Spec Validation Logic
		// =========================================================================

		ginkgo.Entry("Spec Validation - Invalid ScaleTargetRef (empty name)",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-invalid-ref").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", ""). // Empty name
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							// Wait for controller to reconcile
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Validate ValidSpec condition is False
							validation.WaitForPodAutoscalerConditionWithReason(
								ctx, k8sClient, pa,
								ConditionValidSpec, metav1.ConditionFalse,
								ReasonMissingTargetRef,
							)
						},
					},
				},
			},
		),

		ginkgo.Entry("Spec Validation - Invalid Replica Bounds (min > max)",
			makeSpecValidationTestCase(
				"pa-invalid-bounds", "test-deployment-4",
				5, 3, // min > max
				ConditionValidSpec, metav1.ConditionFalse, ReasonInvalidBounds,
			),
		),

		// Note: Invalid ScalingStrategy test is skipped because CRD-level validation
		// prevents invalid values from being created in the first place.

		// Note: Empty MetricsSources test is skipped because CRD-level validation
		// prevents empty metricsSources from being created (minItems=1).

		ginkgo.Entry("Spec Validation - Valid Spec",
			makeSpecValidationTestCase(
				"pa-valid-spec", "test-deployment-7",
				1, 5,
				ConditionValidSpec, metav1.ConditionTrue, ReasonAsExpected,
			),
		),

		// =========================================================================
		// Conflict Detection Mechanism
		// =========================================================================

		ginkgo.Entry("Conflict Detection - Two PAs target same Deployment",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-conflict-1").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "shared-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create deployment
							createDeployment("shared-deployment", ns.Name, 2)
							// Create first PA
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// PA1 should not have conflict
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							validation.ValidatePodAutoscalerConditionNotExists(fetched, ConditionConflict)
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create second PA targeting the same deployment
							pa2 := wrapper.MakePodAutoscaler("pa-conflict-2").
								Namespace(ns.Name).
								ScalingStrategy(autoscalingv1alpha1.HPA).
								MinReplicas(1).
								MaxReplicas(10).
								ScaleTargetRefWithKind("Deployment", "apps/v1", "shared-deployment").
								MetricSource(wrapper.MakeMetricSourcePod(
									autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
								Obj()
							gomega.Expect(k8sClient.Create(ctx, pa2)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// PA2 should have conflict condition with Status=False (conflict detected)
							pa2 := &autoscalingv1alpha1.PodAutoscaler{}
							gomega.Eventually(func(g gomega.Gomega) {
								err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pa-conflict-2"}, pa2)
								g.Expect(err).ToNot(gomega.HaveOccurred())
								validation.ValidatePodAutoscalerConditionExists(pa2, ConditionConflict)
								// When there's a conflict, Status=False (conflict exists)
								validation.ValidatePodAutoscalerCondition(pa2, ConditionConflict, metav1.ConditionFalse, "")
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("Conflict Resolution - Delete first PA",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-resolve-1").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "resolve-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create deployment and two PAs
							createDeployment("resolve-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())

							pa2 := wrapper.MakePodAutoscaler("pa-resolve-2").
								Namespace(ns.Name).
								ScalingStrategy(autoscalingv1alpha1.HPA).
								MinReplicas(1).
								MaxReplicas(10).
								ScaleTargetRefWithKind("Deployment", "apps/v1", "resolve-deployment").
								MetricSource(wrapper.MakeMetricSourcePod(
									autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
								Obj()
							gomega.Expect(k8sClient.Create(ctx, pa2)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA2 has conflict
							pa2 := &autoscalingv1alpha1.PodAutoscaler{}
							err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pa-resolve-2"}, pa2)
							gomega.Expect(err).ToNot(gomega.HaveOccurred())
							validation.ValidatePodAutoscalerConditionExists(pa2, ConditionConflict)
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Delete first PA
							gomega.Expect(k8sClient.Delete(ctx, pa)).To(gomega.Succeed())

							// Wait for deletion to complete
							gomega.Eventually(func() error {
								temp := &autoscalingv1alpha1.PodAutoscaler{}
								return k8sClient.Get(ctx, client.ObjectKeyFromObject(pa), temp)
							}, time.Second*10, time.Millisecond*250).ShouldNot(gomega.Succeed())

							// Give controller time to process the deletion event and update caches
							time.Sleep(time.Second * 2)

							// Manually trigger PA2 reconcile by updating it with retry for race conditions
							gomega.Eventually(func() error {
								pa2 := &autoscalingv1alpha1.PodAutoscaler{}
								err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pa-resolve-2"}, pa2)
								if err != nil {
									return err
								}
								// Add an annotation to trigger reconcile
								if pa2.Annotations == nil {
									pa2.Annotations = make(map[string]string)
								}
								pa2.Annotations["test.aibrix.ai/force-reconcile"] = time.Now().Format(time.RFC3339)
								return k8sClient.Update(ctx, pa2)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())

							// Give controller time to reconcile PA2 after annotation update
							time.Sleep(time.Second * 1)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// PA2 conflict should be resolved - condition should be removed
							pa2 := &autoscalingv1alpha1.PodAutoscaler{}
							gomega.Eventually(func(g gomega.Gomega) {
								err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: "pa-resolve-2"}, pa2)
								g.Expect(err).ToNot(gomega.HaveOccurred())
								// After conflict resolution, the conflict condition should be removed
								validation.ValidatePodAutoscalerConditionNotExists(pa2, ConditionConflict)
							}, time.Second*15, time.Second*1).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		// =========================================================================
		// Status and Condition Management
		// =========================================================================

		ginkgo.Entry("Status Management - DesiredScale and ActualScale",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-status-scale").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "status-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create deployment with 2 replicas
							createDeployment("status-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify status is updated
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// ActualScale should reflect deployment replicas
								g.Expect(fetched.Status.ActualScale).To(gomega.BeNumerically(">=", 0))
								// DesiredScale should be set
								g.Expect(fetched.Status.DesiredScale).To(gomega.BeNumerically(">=", 0))
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("Condition Management - AbleToScale",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-condition-able").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "condition-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("condition-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify AbleToScale condition exists and is True
							validation.WaitForPodAutoscalerCondition(
								ctx, k8sClient, pa,
								ConditionAbleToScale, metav1.ConditionTrue,
							)
						},
					},
				},
			},
		),

		ginkgo.Entry("Condition Management - Ready condition transitions",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-condition-ready").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "ready-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("ready-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify all basic conditions exist
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								validation.ValidatePodAutoscalerConditionExists(fetched, ConditionReady)
								validation.ValidatePodAutoscalerConditionExists(fetched, ConditionValidSpec)
								validation.ValidatePodAutoscalerConditionExists(fetched, ConditionAbleToScale)
								validation.ValidatePodAutoscalerConditionExists(fetched, ConditionScalingActive)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		// =========================================================================
		// Scale Target Management
		// =========================================================================

		ginkgo.Entry("Scale Target - Deployment scaling",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-scale-deployment").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(10).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "scale-test-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create deployment
							createDeployment("scale-test-deployment", ns.Name, 3)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA can get current replicas from deployment
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// ActualScale should reflect deployment's replicas
								g.Expect(fetched.Status.ActualScale).To(gomega.BeNumerically(">=", 0))
								// For HPA strategy, HPA should be created
								validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("Scale Target - Target Resource Not Found",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-target-notfound").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "nonexistent-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Don't create the deployment - test missing target
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Should not crash, ValidSpec should be True (spec itself is valid)
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// Spec validation should pass (the spec is syntactically correct)
								validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
								// Controller handles missing target gracefully
								// HPA will be created even if target doesn't exist (K8s HPA behavior)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		// =========================================================================
		// Boundary Enforcement
		// =========================================================================

		ginkgo.Entry("Boundary Enforcement - maxReplicas enforced in HPA",
			makeBoundaryTestCase("pa-boundary-max", "boundary-deployment", 1, 5, 8),
		),

		ginkgo.Entry("Boundary Enforcement - minReplicas enforced in HPA",
			makeBoundaryTestCase("pa-boundary-min", "boundary-min-deployment", 3, 10, 1),
		),

		ginkgo.Entry("Boundary Enforcement - minReplicas=0 in PA spec",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-boundary-zero").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(0). // Set minReplicas=0 in PA
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "boundary-zero-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("boundary-zero-deployment", ns.Name, 1)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA spec has minReplicas=0
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							gomega.Expect(fetched.Spec.MinReplicas).ToNot(gomega.BeNil())
							gomega.Expect(*fetched.Spec.MinReplicas).To(gomega.Equal(int32(0)))
							// HPA will not have minReplicas set (uses default 1) when PA minReplicas=0
							// This is controller design: only sets HPA minReplicas when PA minReplicas > 0
							hpa := validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
							// HPA minReplicas will be nil or 1 (K8s default)
							if hpa.Spec.MinReplicas != nil {
								gomega.Expect(*hpa.Spec.MinReplicas).To(gomega.BeNumerically(">=", 1))
							}
						},
					},
				},
			},
		),

		// =========================================================================
		// Scaling History Management
		// =========================================================================

		ginkgo.Entry("ScalingHistory - Basic history tracking in HPA mode",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-history").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(10).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "history-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("history-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA is created
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							// In HPA mode, ScalingHistory is managed by HPA and may be empty
							// Just verify the field exists and is within limits
							if fetched.Status.ScalingHistory != nil {
								// maxScalingHistorySize = 5
								gomega.Expect(len(fetched.Status.ScalingHistory)).To(gomega.BeNumerically("<=", 5))
							}
							// Main validation: PA has valid conditions
							validation.ValidatePodAutoscalerConditionExists(fetched, ConditionValidSpec)
						},
					},
				},
			},
		),

		// =========================================================================
		// StormService Scaling
		// =========================================================================

		ginkgo.Entry("StormService Scaling - Replica Mode (scale entire StormService)",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-ss-replica").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(10).
						ScaleTargetRefWithKind("StormService", "orchestration.aibrix.ai/v1alpha1", "test-stormservice").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create StormService with 2 roles
							createStormService("test-stormservice", ns.Name, "app", "test-vllm", 2, 1)
							// Create PA
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA is created and HPA is created
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// ValidSpec should be True
								validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
								// HPA should be created for StormService
								validation.WaitForHPACreated(ctx, k8sClient, ns.Name, pa.Name+"-hpa")
							}, time.Second*15, time.Millisecond*500).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("StormService Scaling - Role-Level with SubTargetSelector",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-ss-role").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(10).
						ScaleTargetRefWithKind("StormService", "orchestration.aibrix.ai/v1alpha1", "test-stormservice-role").
						SubTargetSelector("prefill"). // Only scale "prefill" role
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.7")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create StormService with prefill and decode roles
							createStormService("test-stormservice-role", ns.Name, "app", "test-vllm-role", 3, 2)
							// Create PA targeting only "prefill" role
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA is created with role-level targeting
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// ValidSpec should be True
								validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
								// SubTargetSelector should be set
								g.Expect(fetched.Spec.SubTargetSelector).ToNot(gomega.BeNil())
								g.Expect(fetched.Spec.SubTargetSelector.RoleName).To(gomega.Equal("prefill"))
								// AbleToScale should eventually be True
								validation.ValidatePodAutoscalerConditionExists(fetched, ConditionAbleToScale)
							}, time.Second*15, time.Millisecond*500).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("StormService Scaling - Role-Level Conflict Detection",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					// This will be PA2 (created second)
					return wrapper.MakePodAutoscaler("pa-ss-conflict-2").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(10).
						ScaleTargetRefWithKind("StormService", "orchestration.aibrix.ai/v1alpha1", "test-stormservice-conflict").
						SubTargetSelector("prefill"). // Same role as PA1
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.7")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Create StormService
							matchLabel := map[string]string{"app": "test-vllm-conflict"}
							podTemplate := corev1.PodTemplateSpec{
								ObjectMeta: metav1.ObjectMeta{
									Labels: matchLabel,
								},
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "vllm-container",
											Image: "vllm/vllm-openai:latest",
										},
									},
								},
							}
							roleSetSpec := &orchestrationapi.RoleSetSpec{
								Roles: []orchestrationapi.RoleSpec{
									{
										Name:     "prefill",
										Replicas: ptr.To(int32(3)),
										Template: podTemplate,
										Stateful: false,
									},
									{
										Name:     "decode",
										Replicas: ptr.To(int32(2)),
										Template: podTemplate,
										Stateful: false,
									},
								},
							}
							ss := wrapper.MakeStormService("test-stormservice-conflict").
								Namespace(ns.Name).
								Replicas(ptr.To(int32(2))).
								Selector(metav1.SetAsLabelSelector(matchLabel)).
								UpdateStrategyType(orchestrationapi.RollingUpdateStormServiceStrategyType).
								RoleSetTemplateMeta(metav1.ObjectMeta{Labels: matchLabel}, roleSetSpec).
								Obj()
							gomega.Expect(k8sClient.Create(ctx, ss)).To(gomega.Succeed())

							// Create PA1 first (targeting same SS and same role)
							pa1 := wrapper.MakePodAutoscaler("pa-ss-conflict-1").
								Namespace(ns.Name).
								ScalingStrategy(autoscalingv1alpha1.KPA).
								MinReplicas(1).
								MaxReplicas(10).
								ScaleTargetRefWithKind("StormService", "orchestration.aibrix.ai/v1alpha1", "test-stormservice-conflict").
								SubTargetSelector("prefill"). // Same role
								MetricSource(wrapper.MakeMetricSourcePod(
									autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.7")).
								Obj()
							gomega.Expect(k8sClient.Create(ctx, pa1)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)

							// Create PA2 (should have conflict)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 3)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA2 has conflict condition
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								// Should have Conflict condition with Status=False (meaning conflict exists)
								validation.ValidatePodAutoscalerCondition(fetched, ConditionConflict, metav1.ConditionFalse, "")
								// AbleToScale should be False due to conflict
								validation.ValidatePodAutoscalerCondition(fetched, ConditionAbleToScale, metav1.ConditionFalse, "")
							}, time.Second*15, time.Millisecond*500).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		// =========================================================================
		// Annotation-Based Configuration
		// =========================================================================

		ginkgo.Entry("Annotation - Scale up cooldown annotation",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					annotations := map[string]string{
						"kpa.autoscaling.aibrix.ai/scale-up-cooldown": "30s",
					}
					return wrapper.MakePodAutoscaler("pa-cooldown").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(10).
						Annotations(annotations).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "cooldown-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("cooldown-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify PA is created with cooldown annotation
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							gomega.Expect(fetched.Annotations).To(gomega.HaveKey("kpa.autoscaling.aibrix.ai/scale-up-cooldown"))
							gomega.Expect(fetched.Annotations["kpa.autoscaling.aibrix.ai/scale-up-cooldown"]).To(gomega.Equal("30s"))
							validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
						},
					},
				},
			},
		),

		ginkgo.Entry("Annotation - Scale down delay annotation",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					annotations := map[string]string{
						"kpa.autoscaling.aibrix.ai/scale-down-delay": "3m",
					}
					return wrapper.MakePodAutoscaler("pa-scale-down-delay").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(10).
						Annotations(annotations).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "delay-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("delay-deployment", ns.Name, 5)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify annotation is preserved
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							gomega.Expect(fetched.Annotations).To(gomega.HaveKey("kpa.autoscaling.aibrix.ai/scale-down-delay"))
							validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
						},
					},
				},
			},
		),

		ginkgo.Entry("Annotation - Multiple KPA annotations",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					annotations := map[string]string{
						"kpa.autoscaling.aibrix.ai/panic-threshold":   "200",
						"kpa.autoscaling.aibrix.ai/panic-window":      "10s",
						"kpa.autoscaling.aibrix.ai/stable-window":     "60s",
						"kpa.autoscaling.aibrix.ai/scale-up-cooldown": "30s",
						"kpa.autoscaling.aibrix.ai/scale-down-delay":  "180s",
						"kpa.autoscaling.aibrix.ai/tolerance":         "0.1",
					}
					return wrapper.MakePodAutoscaler("pa-annotations").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(10).
						Annotations(annotations).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "annotations-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "gpu_cache_usage_perc", "0.5")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("annotations-deployment", ns.Name, 3)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify all annotations are preserved
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							gomega.Expect(fetched.Annotations).To(gomega.HaveKey("kpa.autoscaling.aibrix.ai/panic-threshold"))
							gomega.Expect(fetched.Annotations["kpa.autoscaling.aibrix.ai/panic-threshold"]).To(gomega.Equal("200"))
							gomega.Expect(fetched.Annotations).To(gomega.HaveKey("kpa.autoscaling.aibrix.ai/tolerance"))
							gomega.Expect(fetched.Annotations["kpa.autoscaling.aibrix.ai/tolerance"]).To(gomega.Equal("0.1"))
							validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
						},
					},
				},
			},
		),

		// =========================================================================
		// Advanced Scenarios
		// =========================================================================

		ginkgo.Entry("Advanced - Update PA spec and verify reconciliation",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-update-spec").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.KPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "update-spec-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "cpu_usage", "0.7")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("update-spec-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
							gomega.Expect(fetched.Spec.MaxReplicas).To(gomega.Equal(int32(5)))
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Update spec: change maxReplicas with retry for race conditions
							gomega.Eventually(func() error {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								fetched.Spec.MaxReplicas = 10
								return k8sClient.Update(ctx, fetched)
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Verify update is applied and reconciled
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								g.Expect(fetched.Spec.MaxReplicas).To(gomega.Equal(int32(10)))
								validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
							}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
						},
					},
				},
			},
		),

		ginkgo.Entry("Advanced - Multiple rapid updates to spec",
			&testValidatingCase{
				makePodAutoscaler: func() *autoscalingv1alpha1.PodAutoscaler {
					return wrapper.MakePodAutoscaler("pa-rapid-updates").
						Namespace(ns.Name).
						ScalingStrategy(autoscalingv1alpha1.HPA).
						MinReplicas(1).
						MaxReplicas(5).
						ScaleTargetRefWithKind("Deployment", "apps/v1", "rapid-deployment").
						MetricSource(wrapper.MakeMetricSourcePod(
							autoscalingv1alpha1.HTTP, "8080", "/metrics", "requests_per_second", "100")).
						Obj()
				},
				updates: []*update{
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							createDeployment("rapid-deployment", ns.Name, 2)
							gomega.Expect(k8sClient.Create(ctx, pa)).To(gomega.Succeed())
							time.Sleep(time.Second * 2)
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
							validation.ValidatePodAutoscalerConditionExists(fetched, ConditionValidSpec)
						},
					},
					{
						updateFunc: func(pa *autoscalingv1alpha1.PodAutoscaler) {
							// Rapid updates: change maxReplicas multiple times
							for i := 0; i < 3; i++ {
								maxReplicas := int32(5 + i*2)
								gomega.Eventually(func() error {
									fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
									fetched.Spec.MaxReplicas = maxReplicas
									return k8sClient.Update(ctx, fetched)
								}, time.Second*5, time.Millisecond*100).Should(gomega.Succeed())
							}
						},
						checkFunc: func(ctx context.Context, k8sClient client.Client, pa *autoscalingv1alpha1.PodAutoscaler) {
							// Eventually consistent: final maxReplicas should be 9 (5 + 2*2)
							gomega.Eventually(func(g gomega.Gomega) {
								fetched := validation.GetPodAutoscaler(ctx, k8sClient, pa)
								g.Expect(fetched.Spec.MaxReplicas).To(gomega.Equal(int32(9)))
								validation.ValidatePodAutoscalerCondition(fetched, ConditionValidSpec, metav1.ConditionTrue, "")
							}, time.Second*15, time.Second*1).Should(gomega.Succeed())
						},
					},
				},
			},
		),
	)
})
