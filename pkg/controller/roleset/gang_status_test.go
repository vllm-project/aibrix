/*
Copyright 2026 The Aibrix Team.

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

package roleset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	volcanoschedv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	ctrlutils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func TestSetVolcanoGangConditions(t *testing.T) {
	ctx := context.Background()

	t.Run("reports missing PodGroup", func(t *testing.T) {
		reconciler := &RoleSetReconciler{DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, newVolcanoRoleSet(), status, nil)

		assert.NoError(t, err)
		assert.Equal(t, corev1.ConditionFalse, ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetPodGroupSynced).Status)
		assert.Equal(t, "PodGroupNotFound", ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError).Reason)
	})

	t.Run("reports unknown when PodGroup observation is disabled", func(t *testing.T) {
		reconciler := &RoleSetReconciler{}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, newVolcanoRoleSet(), status, nil)

		assert.NoError(t, err)
		assert.Equal(t, corev1.ConditionUnknown, ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetPodGroupSynced).Status)
		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
		assert.Equal(t, corev1.ConditionUnknown, errCond.Status)
		assert.Equal(t, "PodGroupObservationDisabled", errCond.Reason)
	})

	t.Run("reports synced and healthy PodGroup", func(t *testing.T) {
		reconciler := &RoleSetReconciler{DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), newVolcanoPodGroup("test-roleset", "default", string(volcanoschedv1beta1.PodGroupRunning), nil))}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, newVolcanoRoleSet(), status, nil)

		assert.NoError(t, err)
		assert.Equal(t, corev1.ConditionTrue, ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetPodGroupSynced).Status)
		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
		assert.Equal(t, corev1.ConditionFalse, errCond.Status)
		assert.Equal(t, "GangSchedulingHealthy", errCond.Reason)
	})

	t.Run("reports unschedulable PodGroup condition", func(t *testing.T) {
		conditions := []interface{}{
			map[string]interface{}{
				"type":    string(volcanoschedv1beta1.PodGroupUnschedulableType),
				"status":  string(corev1.ConditionTrue),
				"reason":  "NotEnoughPodsOfTask",
				"message": "not enough pods of task prefill",
			},
		}
		reconciler := &RoleSetReconciler{DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), newVolcanoPodGroup("test-roleset", "default", string(volcanoschedv1beta1.PodGroupInqueue), conditions))}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, newVolcanoRoleSet(), status, nil)

		assert.NoError(t, err)
		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
		assert.Equal(t, corev1.ConditionTrue, errCond.Status)
		assert.Equal(t, "NotEnoughPodsOfTask", errCond.Reason)
		assert.Equal(t, "not enough pods of task prefill", errCond.Message)
	})

	t.Run("reports malformed PodGroup status", func(t *testing.T) {
		podGroup := newVolcanoPodGroup("test-roleset", "default", string(volcanoschedv1beta1.PodGroupRunning), nil)
		podGroup.Object["status"] = "malformed"
		reconciler := &RoleSetReconciler{DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), podGroup)}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, newVolcanoRoleSet(), status, nil)

		assert.NoError(t, err)
		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
		assert.Equal(t, corev1.ConditionTrue, errCond.Status)
		assert.Equal(t, podGroupStatusMalformedReason, errCond.Reason)
	})

	t.Run("warns when minMember is lower than minTaskMember sum", func(t *testing.T) {
		roleSet := newVolcanoRoleSet()
		roleSet.Spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinMember = 1
		roleSet.Spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinTaskMember = map[string]int32{"prefill": 1, "decode": 1}
		reconciler := &RoleSetReconciler{DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), newVolcanoPodGroup("test-roleset", "default", string(volcanoschedv1beta1.PodGroupRunning), nil))}
		status := &orchestrationv1alpha1.RoleSetStatus{}

		err := reconciler.setVolcanoGangConditions(ctx, roleSet, status, nil)

		assert.NoError(t, err)
		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.RoleSetGangSchedulingError)
		assert.Equal(t, corev1.ConditionTrue, errCond.Status)
		assert.Equal(t, "MinTaskMemberNotEnforced", errCond.Reason)
	})
}

func newVolcanoRoleSet() *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-roleset",
			Namespace: "default",
		},
		Spec: orchestrationv1alpha1.RoleSetSpec{
			SchedulingStrategy: &orchestrationv1alpha1.SchedulingStrategy{
				VolcanoSchedulingStrategy: &orchestrationv1alpha1.VolcanoSchedulingStrategySpec{
					MinMember: 2,
				},
			},
		},
	}
}

func newVolcanoPodGroup(name, namespace, phase string, conditions []interface{}) *unstructured.Unstructured {
	podGroup := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "scheduling.volcano.sh/v1beta1",
			"kind":       "PodGroup",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"status": map[string]interface{}{
				"phase": phase,
			},
		},
	}
	if conditions != nil {
		_ = unstructured.SetNestedSlice(podGroup.Object, conditions, "status", "conditions")
	}
	return podGroup
}
