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

package podautoscaler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetCurrentReplicasFromScale(t *testing.T) {
	expectedReplicas := int32(2)

	scaleStormService := &unstructured.Unstructured{}
	scaleStormService.SetAPIVersion("orchestration.aibrix.ai/v1alpha1")
	scaleStormService.SetKind("StormService")

	table := []struct {
		name  string
		pa    *autoscalingv1alpha1.PodAutoscaler
		ss    *orchestrationv1alpha1.StormService
		scale *unstructured.Unstructured
	}{
		{
			name: "llm_model_with_spec_replicas",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "Deployment",
						Name: "test-llm",
					},
				},
			},
			ss: &orchestrationv1alpha1.StormService{},
			scale: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"replicas": int64(expectedReplicas),
					},
				},
			},
		},
		{
			name: "storm_service_with_spec_replicas",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
					Annotations: map[string]string{
						AutoscalingStormServiceModeAnnotationKey: "replica",
					},
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{},
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "StormService",
						Name: "test-storm",
					},
				},
			},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: &expectedReplicas,
				},
			},
			scale: scaleStormService,
		},
		{
			name: "storm_service_with_status_replicas",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
						RoleName: "prefill",
					},
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "StormService",
						Name: "test-storm",
					},
				},
			},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{},
				Status: orchestrationv1alpha1.StormServiceStatus{
					RoleStatuses: []orchestrationv1alpha1.RoleStatus{
						{
							Name:     "prefill",
							Replicas: expectedReplicas,
						},
					},
				},
			},
			scale: scaleStormService,
		},
		{
			name: "storm_service_with_template_spec_replicas",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
						RoleName: "prefill",
					},
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "StormService",
						Name: "test-storm",
					},
				},
			},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{
								{
									Name:     "prefill",
									Replicas: &expectedReplicas,
								},
							},
						},
					},
				},
			},
			scale: scaleStormService,
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = autoscalingv1alpha1.AddToScheme(scheme)
			_ = orchestrationv1alpha1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.pa, tt.ss).
				Build()

			workloadScale := NewWorkloadScale(fakeClient, nil)

			currentReplicas, err := workloadScale.GetCurrentReplicasFromScale(context.TODO(), tt.pa, tt.scale)

			assert.NoError(t, err)
			assert.Equal(t, expectedReplicas, currentReplicas)
		})
	}
}

func TestGetPodSelectorFromScale(t *testing.T) {
	t.Run("llm_model", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = autoscalingv1alpha1.AddToScheme(scheme)
		_ = orchestrationv1alpha1.AddToScheme(scheme)

		pa := &autoscalingv1alpha1.PodAutoscaler{
			ObjectMeta: v1.ObjectMeta{
				Namespace: "default",
			},
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				ScaleTargetRef: corev1.ObjectReference{
					Kind: "Deployment",
					Name: "test-llm",
				},
			},
		}
		scale := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"spec": map[string]interface{}{
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"model.aibrix.ai/name": "deepseek-llm-7b-chat",
						},
					},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pa).
			Build()

		workloadScale := NewWorkloadScale(fakeClient, nil)

		labelsSelector, err := workloadScale.GetPodSelectorFromScale(context.TODO(), pa, scale)

		assert.NoError(t, err)
		assert.NotNil(t, labelsSelector)
		requirements, _ := labelsSelector.Requirements()
		assert.Len(t, requirements, 1)
		assert.Equal(t, "model.aibrix.ai/name", requirements[0].Key())
		assert.Len(t, requirements[0].ValuesUnsorted(), 1)
		assert.Equal(t, "deepseek-llm-7b-chat", requirements[0].ValuesUnsorted()[0])
	})

	t.Run("storm_service", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = autoscalingv1alpha1.AddToScheme(scheme)
		_ = orchestrationv1alpha1.AddToScheme(scheme)

		pa := &autoscalingv1alpha1.PodAutoscaler{
			ObjectMeta: v1.ObjectMeta{
				Namespace: "default",
			},
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				ScaleTargetRef: corev1.ObjectReference{
					Kind: "StormService",
					Name: "test-storm",
				},
			},
		}
		ss := &orchestrationv1alpha1.StormService{
			ObjectMeta: v1.ObjectMeta{
				Name:      "test-storm",
				Namespace: "default",
			},
			Spec: orchestrationv1alpha1.StormServiceSpec{},
		}

		scale := &unstructured.Unstructured{}
		scale.SetAPIVersion("orchestration.aibrix.ai/v1alpha1")
		scale.SetKind("StormService")

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pa, ss).
			Build()

		workloadScale := NewWorkloadScale(fakeClient, nil)

		labelsSelector, err := workloadScale.GetPodSelectorFromScale(context.TODO(), pa, scale)

		assert.NoError(t, err)
		assert.NotNil(t, labelsSelector)
		requirements, _ := labelsSelector.Requirements()
		assert.Len(t, requirements, 1)
		assert.Equal(t, "storm-service-name", requirements[0].Key())
		assert.Len(t, requirements[0].ValuesUnsorted(), 1)
		assert.Equal(t, "test-storm", requirements[0].ValuesUnsorted()[0])
	})

	t.Run("storm_service_with_role", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = autoscalingv1alpha1.AddToScheme(scheme)
		_ = orchestrationv1alpha1.AddToScheme(scheme)

		pa := &autoscalingv1alpha1.PodAutoscaler{
			ObjectMeta: v1.ObjectMeta{
				Namespace: "default",
			},
			Spec: autoscalingv1alpha1.PodAutoscalerSpec{
				SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
					RoleName: "prefill",
				},
				ScaleTargetRef: corev1.ObjectReference{
					Kind: "StormService",
					Name: "test-storm",
				},
			},
		}
		ss := &orchestrationv1alpha1.StormService{
			ObjectMeta: v1.ObjectMeta{
				Name:      "test-storm",
				Namespace: "default",
			},
			Spec: orchestrationv1alpha1.StormServiceSpec{},
		}

		scale := &unstructured.Unstructured{}
		scale.SetAPIVersion("orchestration.aibrix.ai/v1alpha1")
		scale.SetKind("StormService")

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(pa, ss).
			Build()

		workloadScale := NewWorkloadScale(fakeClient, nil)

		labelsSelector, err := workloadScale.GetPodSelectorFromScale(context.TODO(), pa, scale)

		assert.NoError(t, err)
		assert.NotNil(t, labelsSelector)
		requirements, _ := labelsSelector.Requirements()
		assert.Len(t, requirements, 2)
		assert.Equal(t, "role-name", requirements[0].Key())
		assert.Len(t, requirements[0].ValuesUnsorted(), 1)
		assert.Equal(t, "prefill", requirements[0].ValuesUnsorted()[0])
		assert.Equal(t, "storm-service-name", requirements[1].Key())
		assert.Len(t, requirements[1].ValuesUnsorted(), 1)
		assert.Equal(t, "test-storm", requirements[1].ValuesUnsorted()[0])
	})
}
