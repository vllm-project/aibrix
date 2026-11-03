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

package stormservice

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	ctrlutils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func TestSetGangSchedulingConditions(t *testing.T) {
	t.Run("does nothing when no RoleSet has gang conditions", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newRoleSetWithConditions("rs-0"),
		})

		assert.Empty(t, status.Conditions)
	})

	t.Run("removes stale conditions when no RoleSet uses volcano scheduling", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{
			Conditions: []orchestrationv1alpha1.Condition{
				condition(orchestrationv1alpha1.StormServiceGangSchedulingError, corev1.ConditionFalse, "GangSchedulingHealthy"),
				condition(orchestrationv1alpha1.StormServicePodGroupSynced, corev1.ConditionTrue, "PodGroupSynced"),
			},
		}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newRoleSetWithConditions("rs-0"),
		})

		assert.Empty(t, status.Conditions)
	})

	t.Run("aggregates healthy RoleSets", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newVolcanoRoleSetWithConditions("rs-0",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionFalse, "Healthy"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionTrue, "Synced"),
			),
			newVolcanoRoleSetWithConditions("rs-1",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionFalse, "Healthy"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionTrue, "Synced"),
			),
		})

		assert.Equal(t, corev1.ConditionFalse, ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServiceGangSchedulingError).Status)
		assert.Equal(t, corev1.ConditionTrue, ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServicePodGroupSynced).Status)
	})

	t.Run("aggregates error and unsynced RoleSets", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newVolcanoRoleSetWithConditions("rs-0",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionTrue, "PodGroupNotFound"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionFalse, "PodGroupNotFound"),
			),
			newVolcanoRoleSetWithConditions("rs-1",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionFalse, "Healthy"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionTrue, "Synced"),
			),
		})

		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServiceGangSchedulingError)
		assert.Equal(t, corev1.ConditionTrue, errCond.Status)
		assert.Contains(t, errCond.Message, "rs-0")

		syncedCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServicePodGroupSynced)
		assert.Equal(t, corev1.ConditionFalse, syncedCond.Status)
		assert.Contains(t, syncedCond.Message, "rs-0")
	})

	t.Run("reports incomplete when a volcano RoleSet has no synced condition yet", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newVolcanoRoleSetWithConditions("rs-0",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionFalse, "Healthy"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionTrue, "Synced"),
			),
			newVolcanoRoleSetWithConditions("rs-1"),
		})

		syncedCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServicePodGroupSynced)
		assert.Equal(t, corev1.ConditionFalse, syncedCond.Status)
		assert.Equal(t, "PodGroupSyncIncomplete", syncedCond.Reason)
		assert.Contains(t, syncedCond.Message, "rs-1")
	})

	t.Run("aggregates unknown gang scheduling error", func(t *testing.T) {
		status := &orchestrationv1alpha1.StormServiceStatus{}

		setGangSchedulingConditions(status, []*orchestrationv1alpha1.RoleSet{
			newVolcanoRoleSetWithConditions("rs-0",
				condition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionUnknown, "PodGroupObservationDisabled"),
				condition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionUnknown, "PodGroupObservationDisabled"),
			),
		})

		errCond := ctrlutils.GetCondition(status.Conditions, orchestrationv1alpha1.StormServiceGangSchedulingError)
		assert.Equal(t, corev1.ConditionUnknown, errCond.Status)
		assert.Equal(t, "GangSchedulingUnknown", errCond.Reason)
		assert.Contains(t, errCond.Message, "rs-0")
	})
}

func newRoleSetWithConditions(name string, conditions ...orchestrationv1alpha1.Condition) *orchestrationv1alpha1.RoleSet {
	return &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: orchestrationv1alpha1.RoleSetStatus{
			Conditions: conditions,
		},
	}
}

func newVolcanoRoleSetWithConditions(name string, conditions ...orchestrationv1alpha1.Condition) *orchestrationv1alpha1.RoleSet {
	roleSet := newRoleSetWithConditions(name, conditions...)
	roleSet.Spec.SchedulingStrategy = &orchestrationv1alpha1.SchedulingStrategy{
		VolcanoSchedulingStrategy: &orchestrationv1alpha1.VolcanoSchedulingStrategySpec{},
	}
	return roleSet
}

func condition(condType orchestrationv1alpha1.ConditionType, status corev1.ConditionStatus, reason string) orchestrationv1alpha1.Condition {
	return orchestrationv1alpha1.Condition{
		Type:   condType,
		Status: status,
		Reason: reason,
	}
}
