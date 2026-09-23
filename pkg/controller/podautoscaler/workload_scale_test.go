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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStormServiceScalingMode(t *testing.T) {
	tests := map[string]struct {
		specMode   orchestrationv1alpha1.StormServiceMode
		replicas   *int32
		annotation string
		want       orchestrationv1alpha1.StormServiceMode
	}{
		"declared replica mode wins":                     {specMode: orchestrationv1alpha1.StormServiceReplicaMode, want: orchestrationv1alpha1.StormServiceReplicaMode},
		"declared pooled mode wins over annotation":      {specMode: orchestrationv1alpha1.StormServicePooledMode, annotation: "replica", want: orchestrationv1alpha1.StormServicePooledMode},
		"declared replica mode wins over annotation":     {specMode: orchestrationv1alpha1.StormServiceReplicaMode, annotation: "pool", want: orchestrationv1alpha1.StormServiceReplicaMode},
		"no mode falls back to replica annotation":       {annotation: "replica", want: orchestrationv1alpha1.StormServiceReplicaMode},
		"no mode with pool annotation defaults to pool":  {annotation: "pool", want: orchestrationv1alpha1.StormServicePooledMode},
		"no mode and no annotation defaults to pool":     {want: orchestrationv1alpha1.StormServicePooledMode},
		"no mode with unknown annotation stays pooled":   {annotation: "bogus", want: orchestrationv1alpha1.StormServicePooledMode},
		"declared pooled mode with no annotation pooled": {specMode: orchestrationv1alpha1.StormServicePooledMode, want: orchestrationv1alpha1.StormServicePooledMode},

		// spec.mode is optional, so an undeclared mode falls back to the same
		// inference the stormservice controller applies (StormServiceSpec.ResolvedMode):
		// replicas > 1 can only be replica mode, while replicas <= 1 stays pooled
		// because a single RoleSet cannot tell the two modes apart.
		"no mode with replicas above one infers replica mode": {replicas: ptr.To(int32(3)), want: orchestrationv1alpha1.StormServiceReplicaMode},
		"no mode with single replica stays pooled":            {replicas: ptr.To(int32(1)), want: orchestrationv1alpha1.StormServicePooledMode},
		"no mode with zero replicas stays pooled":             {replicas: ptr.To(int32(0)), want: orchestrationv1alpha1.StormServicePooledMode},
		// Inference must not outrank the two explicit signals.
		"declared pooled mode wins over replicas above one":    {specMode: orchestrationv1alpha1.StormServicePooledMode, replicas: ptr.To(int32(3)), want: orchestrationv1alpha1.StormServicePooledMode},
		"replica annotation wins over single replica":          {replicas: ptr.To(int32(1)), annotation: "replica", want: orchestrationv1alpha1.StormServiceReplicaMode},
		"pool annotation does not suppress replicas inference": {replicas: ptr.To(int32(3)), annotation: "pool", want: orchestrationv1alpha1.StormServiceReplicaMode},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			pa := &autoscalingv1alpha1.PodAutoscaler{}
			if tc.annotation != "" {
				pa.Annotations = map[string]string{AutoscalingStormServiceModeAnnotationKey: tc.annotation}
			}
			ss := &orchestrationv1alpha1.StormService{
				Spec: orchestrationv1alpha1.StormServiceSpec{Mode: tc.specMode, Replicas: tc.replicas},
			}
			assert.Equal(t, tc.want, stormServiceScalingMode(pa, ss))
		})
	}
}

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
		{
			// spec.mode replaces the annotation as the mode signal.
			name: "storm_service_with_declared_replica_mode",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
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
					Mode:     orchestrationv1alpha1.StormServiceReplicaMode,
					Replicas: &expectedReplicas,
				},
			},
			scale: scaleStormService,
		},
		{
			// A declared pooled mode wins over a stale "replica" annotation:
			// the role status is reported, not spec.replicas.
			name: "storm_service_declared_pooled_mode_wins_over_annotation",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
					Annotations: map[string]string{
						AutoscalingStormServiceModeAnnotationKey: "replica",
					},
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
					Mode:     orchestrationv1alpha1.StormServicePooledMode,
					Replicas: ptr.To(int32(5)), // must not be reported in pooled mode
				},
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

// TestGetCurrentReplicasReplicaModeNilReplicas covers the review scenario for an
// omitted spec.replicas in replica mode: the field is optional with a documented
// default of 1 that neither the CRD nor a webhook materializes, so the autoscaler
// must resolve nil to 1 rather than report a scaled-to-zero workload.
func TestGetCurrentReplicasReplicaModeNilReplicas(t *testing.T) {
	scale := &unstructured.Unstructured{}
	scale.SetAPIVersion("orchestration.aibrix.ai/v1alpha1")
	scale.SetKind("StormService")

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{
			Namespace: "default",
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{},
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
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Mode: orchestrationv1alpha1.StormServiceReplicaMode,
			// Replicas intentionally omitted.
		},
	}

	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pa, ss).
		Build()

	workloadScale := NewWorkloadScale(fakeClient, nil)

	currentReplicas, err := workloadScale.GetCurrentReplicasFromScale(context.TODO(), pa, scale)

	assert.NoError(t, err)
	assert.Equal(t, int32(1), currentReplicas)
}

// inferredReplicaModeStormService builds a legacy StormService that omits
// spec.mode and declares replicas > 1: three RoleSets of two decode replicas
// each. status.roleStatuses carries the aggregate (6) so the assertions can tell
// which field the autoscaler actually read.
func inferredReplicaModeStormService() *orchestrationv1alpha1.StormService {
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: v1.ObjectMeta{Namespace: "default", Name: "test-storm"},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(3)),
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{Name: "prefill", Replicas: ptr.To(int32(1))},
						{Name: "decode", Replicas: ptr.To(int32(2))},
					},
				},
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			RoleStatuses: []orchestrationv1alpha1.RoleStatus{
				{Name: "prefill", Replicas: 3},
				{Name: "decode", Replicas: 6},
			},
		},
	}
}

// inferredReplicaModePodAutoscaler targets a single role and deliberately omits
// the deprecated storm-service-mode annotation, which is how the samples and the
// docs tell users to configure role-level autoscaling since the annotation was
// deprecated in favour of spec.mode.
func inferredReplicaModePodAutoscaler() *autoscalingv1alpha1.PodAutoscaler {
	return &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: v1.ObjectMeta{Namespace: "default", Name: "decode-pa"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "orchestration.aibrix.ai/v1alpha1",
				Kind:       "StormService",
				Name:       "test-storm",
			},
			SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{RoleName: "decode"},
		},
	}
}

// TestGetCurrentReplicasInferredReplicaMode covers a StormService that omits
// spec.mode while declaring replicas > 1. The autoscaler must resolve replica
// mode and report spec.replicas. Reporting the role status instead returns the
// count aggregated over every RoleSet, which is a different quantity from the
// one the autoscaler writes back.
func TestGetCurrentReplicasInferredReplicaMode(t *testing.T) {
	scale := &unstructured.Unstructured{}
	scale.SetAPIVersion("orchestration.aibrix.ai/v1alpha1")
	scale.SetKind("StormService")

	pa := inferredReplicaModePodAutoscaler()
	ss := inferredReplicaModeStormService()

	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pa, ss).Build()

	currentReplicas, err := NewWorkloadScale(fakeClient, nil).GetCurrentReplicasFromScale(context.TODO(), pa, scale)

	assert.NoError(t, err)
	assert.Equal(t, int32(3), currentReplicas, "expected spec.replicas, not the role status aggregated across RoleSets")
}

// TestSetDesiredReplicasInferredReplicaMode is the write-side counterpart: the
// autoscaler must scale spec.replicas and leave the role replicas in the
// template untouched. The template is copied into every RoleSet, so writing the
// desired count there multiplies it by the number of RoleSets.
func TestSetDesiredReplicasInferredReplicaMode(t *testing.T) {
	pa := inferredReplicaModePodAutoscaler()
	ss := inferredReplicaModeStormService()

	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pa, ss).Build()

	err := NewWorkloadScale(fakeClient, nil).SetDesiredReplicas(context.TODO(), pa, 5)
	assert.NoError(t, err)

	updated := &orchestrationv1alpha1.StormService{}
	assert.NoError(t, fakeClient.Get(context.TODO(),
		client.ObjectKey{Namespace: "default", Name: "test-storm"}, updated))

	assert.NotNil(t, updated.Spec.Replicas)
	assert.Equal(t, int32(5), *updated.Spec.Replicas, "replica mode scales the StormService through spec.replicas")

	decode := roleByName(t, updated, "decode")
	assert.NotNil(t, decode.Replicas)
	assert.Equal(t, int32(2), *decode.Replicas, "role replicas in the template must stay untouched")
}

func roleByName(t *testing.T, ss *orchestrationv1alpha1.StormService, name string) orchestrationv1alpha1.RoleSpec {
	t.Helper()
	for _, role := range ss.Spec.Template.Spec.Roles {
		if role.Name == name {
			return role
		}
	}
	t.Fatalf("role %q not found", name)
	return orchestrationv1alpha1.RoleSpec{}
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

func TestSetDesiredReplicas(t *testing.T) {
	currentReplicas := int32(1)
	expectedReplicas := int32(2)

	table := []struct {
		name           string
		pa             *autoscalingv1alpha1.PodAutoscaler
		deployment     *appsv1.Deployment
		ss             *orchestrationv1alpha1.StormService
		assertReplicas func(t *testing.T, fakeClient client.Client)
	}{
		{
			name: "llm_model_with_spec_replicas",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Kind:       "Deployment",
						Name:       "test-llm",
						APIVersion: "apps/v1",
					},
				},
			},
			deployment: &appsv1.Deployment{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-llm",
					Namespace: "default",
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &currentReplicas,
				},
			},
			ss: &orchestrationv1alpha1.StormService{},
			assertReplicas: func(t *testing.T, fakeClient client.Client) {
				deployment := &appsv1.Deployment{}
				err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "test-llm"}, deployment)
				assert.NoError(t, err)
				assert.Equal(t, expectedReplicas, *deployment.Spec.Replicas)
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
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
						RoleName: "",
					},
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "StormService",
						Name: "test-storm",
					},
				},
			},
			deployment: &appsv1.Deployment{},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Replicas: &currentReplicas,
				},
			},
			assertReplicas: func(t *testing.T, fakeClient client.Client) {
				ss := &orchestrationv1alpha1.StormService{}
				err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "test-storm"}, ss)
				assert.NoError(t, err)
				assert.Equal(t, expectedReplicas, *ss.Spec.Replicas)
			},
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
			deployment: &appsv1.Deployment{},
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
									Replicas: &currentReplicas,
								},
							},
						},
					},
				},
			},
			assertReplicas: func(t *testing.T, fakeClient client.Client) {
				ss := &orchestrationv1alpha1.StormService{}
				err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "test-storm"}, ss)
				assert.NoError(t, err)
				assert.Equal(t, expectedReplicas, *ss.Spec.Template.Spec.Roles[0].Replicas)
			},
		},
		{
			// spec.mode replaces the annotation as the mode signal: a declared
			// replica mode updates spec.replicas without any annotation set.
			name: "storm_service_with_declared_replica_mode",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
				},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
						RoleName: "",
					},
					ScaleTargetRef: corev1.ObjectReference{
						Kind: "StormService",
						Name: "test-storm",
					},
				},
			},
			deployment: &appsv1.Deployment{},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Mode:     orchestrationv1alpha1.StormServiceReplicaMode,
					Replicas: &currentReplicas,
				},
			},
			assertReplicas: func(t *testing.T, fakeClient client.Client) {
				ss := &orchestrationv1alpha1.StormService{}
				err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "test-storm"}, ss)
				assert.NoError(t, err)
				assert.Equal(t, expectedReplicas, *ss.Spec.Replicas)
			},
		},
		{
			// A declared pooled mode wins over a stale "replica" annotation: the
			// targeted role scales while spec.replicas stays untouched.
			name: "storm_service_declared_pooled_mode_wins_over_annotation",
			pa: &autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: v1.ObjectMeta{
					Namespace: "default",
					Annotations: map[string]string{
						AutoscalingStormServiceModeAnnotationKey: "replica",
					},
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
			deployment: &appsv1.Deployment{},
			ss: &orchestrationv1alpha1.StormService{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test-storm",
					Namespace: "default",
				},
				Spec: orchestrationv1alpha1.StormServiceSpec{
					Mode:     orchestrationv1alpha1.StormServicePooledMode,
					Replicas: ptr.To(int32(1)),
					Template: orchestrationv1alpha1.RoleSetTemplateSpec{
						Spec: &orchestrationv1alpha1.RoleSetSpec{
							Roles: []orchestrationv1alpha1.RoleSpec{
								{
									Name:     "prefill",
									Replicas: &currentReplicas,
								},
							},
						},
					},
				},
			},
			assertReplicas: func(t *testing.T, fakeClient client.Client) {
				ss := &orchestrationv1alpha1.StormService{}
				err := fakeClient.Get(context.TODO(), client.ObjectKey{Namespace: "default", Name: "test-storm"}, ss)
				assert.NoError(t, err)
				assert.Equal(t, expectedReplicas, *ss.Spec.Template.Spec.Roles[0].Replicas)
				assert.Equal(t, int32(1), *ss.Spec.Replicas)
			},
		},
	}

	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = autoscalingv1alpha1.AddToScheme(scheme)
			_ = orchestrationv1alpha1.AddToScheme(scheme)
			_ = appsv1.AddToScheme(scheme)

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.pa, tt.deployment, tt.ss).
				Build()

			workloadScale := NewWorkloadScale(fakeClient, nil)

			err := workloadScale.SetDesiredReplicas(context.TODO(), tt.pa, expectedReplicas)

			assert.NoError(t, err)
			tt.assertReplicas(t, fakeClient)
		})
	}
}
