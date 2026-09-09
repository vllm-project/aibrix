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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	volcanoschedv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

var volcanoPodGroupGVR = schema.GroupVersionResource{
	Group:    volcanoschedv1beta1.SchemeGroupVersion.Group,
	Version:  volcanoschedv1beta1.SchemeGroupVersion.Version,
	Resource: "podgroups",
}

const podGroupStatusMalformedReason = "PodGroupStatusMalformed"

func (r *RoleSetReconciler) setVolcanoGangConditions(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, status *orchestrationv1alpha1.RoleSetStatus, podGroupSyncErr error) error {
	if rs.Spec.SchedulingStrategy == nil || rs.Spec.SchedulingStrategy.VolcanoSchedulingStrategy == nil {
		RemoveRoleSetCondition(status, orchestrationv1alpha1.RoleSetPodGroupSynced)
		RemoveRoleSetCondition(status, orchestrationv1alpha1.RoleSetGangSchedulingError)
		return nil
	}

	if podGroupSyncErr != nil {
		SetRoleSetCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.RoleSetPodGroupSynced,
			corev1.ConditionFalse,
			"PodGroupSyncFailed",
			podGroupSyncErr.Error(),
		))
		SetRoleSetCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.RoleSetGangSchedulingError,
			corev1.ConditionTrue,
			"PodGroupSyncFailed",
			podGroupSyncErr.Error(),
		))
		return nil
	}

	if r.DynamicClient == nil {
		msg := "volcano scheduling is configured but PodGroup observation is disabled because the dynamic client is not initialized"
		SetRoleSetCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.RoleSetPodGroupSynced,
			corev1.ConditionUnknown,
			"PodGroupObservationDisabled",
			msg,
		))
		SetRoleSetCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.RoleSetGangSchedulingError,
			corev1.ConditionUnknown,
			"PodGroupObservationDisabled",
			msg,
		))
		return nil
	}

	podGroup, err := r.getVolcanoPodGroup(ctx, rs)
	if err != nil {
		return err
	}
	if podGroup == nil {
		msg := fmt.Sprintf("volcano scheduling is configured but PodGroup %s/%s is not observed", rs.Namespace, rs.Name)
		SetRoleSetCondition(status, *utils.NewCondition(orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionFalse, "PodGroupNotFound", msg))
		SetRoleSetCondition(status, *utils.NewCondition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionTrue, "PodGroupNotFound", msg))
		return nil
	}

	SetRoleSetCondition(status, *utils.NewCondition(
		orchestrationv1alpha1.RoleSetPodGroupSynced,
		corev1.ConditionTrue,
		"PodGroupSynced",
		fmt.Sprintf("volcano PodGroup %s/%s is synced", rs.Namespace, rs.Name),
	))

	if reason, msg, hasError := volcanoPodGroupSchedulingError(podGroup); hasError {
		SetRoleSetCondition(status, *utils.NewCondition(orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionTrue, reason, msg))
		return nil
	}

	if minMemberWarning := minTaskMemberWarning(rs.Spec.SchedulingStrategy.VolcanoSchedulingStrategy); minMemberWarning != "" {
		SetRoleSetCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.RoleSetGangSchedulingError,
			corev1.ConditionTrue,
			"MinTaskMemberNotEnforced",
			minMemberWarning,
		))
		return nil
	}

	SetRoleSetCondition(status, *utils.NewCondition(
		orchestrationv1alpha1.RoleSetGangSchedulingError,
		corev1.ConditionFalse,
		"GangSchedulingHealthy",
		"",
	))
	return nil
}

func (r *RoleSetReconciler) getVolcanoPodGroup(ctx context.Context, rs *orchestrationv1alpha1.RoleSet) (*unstructured.Unstructured, error) {
	podGroup, err := r.DynamicClient.Resource(volcanoPodGroupGVR).Namespace(rs.Namespace).Get(ctx, rs.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return podGroup, nil
}

func volcanoPodGroupSchedulingError(podGroup *unstructured.Unstructured) (string, string, bool) {
	phase, _, err := unstructured.NestedString(podGroup.Object, "status", "phase")
	if err != nil {
		return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup status.phase: %v", err), true
	}
	if phase == string(volcanoschedv1beta1.PodGroupUnknown) {
		return "PodGroupUnknown", "volcano PodGroup is Unknown", true
	}

	conditions, _, err := unstructured.NestedSlice(podGroup.Object, "status", "conditions")
	if err != nil {
		return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup status.conditions: %v", err), true
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			return podGroupStatusMalformedReason, "failed to parse volcano PodGroup status.conditions: condition entry is not an object", true
		}
		condType, _, err := unstructured.NestedString(condition, "type")
		if err != nil {
			return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup condition type: %v", err), true
		}
		condStatus, _, err := unstructured.NestedString(condition, "status")
		if err != nil {
			return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup condition status: %v", err), true
		}
		reason, _, err := unstructured.NestedString(condition, "reason")
		if err != nil {
			return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup condition reason: %v", err), true
		}
		message, _, err := unstructured.NestedString(condition, "message")
		if err != nil {
			return podGroupStatusMalformedReason, fmt.Sprintf("failed to parse volcano PodGroup condition message: %v", err), true
		}
		if condType == string(volcanoschedv1beta1.PodGroupUnschedulableType) && condStatus == string(corev1.ConditionTrue) {
			if reason == "" {
				reason = "PodGroupUnschedulable"
			}
			if message == "" {
				message = "volcano PodGroup is unschedulable"
			}
			return reason, message, true
		}
	}
	return "", "", false
}

func minTaskMemberWarning(strategy *orchestrationv1alpha1.VolcanoSchedulingStrategySpec) string {
	if strategy == nil || len(strategy.MinTaskMember) == 0 {
		return ""
	}
	var total int32
	for _, minMember := range strategy.MinTaskMember {
		total += minMember
	}
	if strategy.MinMember < total {
		return fmt.Sprintf("minMember %d is less than sum(minTaskMember) %d; Volcano skips task-level readiness checks in this configuration", strategy.MinMember, total)
	}
	return ""
}
