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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

func setGangSchedulingConditions(status *orchestrationv1alpha1.StormServiceStatus, roleSets []*orchestrationv1alpha1.RoleSet) {
	volcanoRoleSets := volcanoRoleSetNames(roleSets)
	if len(volcanoRoleSets) == 0 {
		RemoveStormServiceCondition(status, orchestrationv1alpha1.StormServicePodGroupSynced)
		RemoveStormServiceCondition(status, orchestrationv1alpha1.StormServiceGangSchedulingError)
		return
	}

	errorRoleSets := conditionRoleSetNames(roleSets, orchestrationv1alpha1.RoleSetGangSchedulingError, corev1.ConditionTrue)
	if len(errorRoleSets) > 0 {
		SetStormServiceCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.StormServiceGangSchedulingError,
			corev1.ConditionTrue,
			"GangSchedulingError",
			fmt.Sprintf("gang scheduling errors reported by RoleSets: %s", strings.Join(errorRoleSets, ",")),
		))
	} else {
		SetStormServiceCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.StormServiceGangSchedulingError,
			corev1.ConditionFalse,
			"GangSchedulingHealthy",
			"",
		))
	}

	syncedRoleSets := conditionRoleSetNames(roleSets, orchestrationv1alpha1.RoleSetPodGroupSynced, corev1.ConditionTrue)
	if len(syncedRoleSets) == len(volcanoRoleSets) {
		SetStormServiceCondition(status, *utils.NewCondition(
			orchestrationv1alpha1.StormServicePodGroupSynced,
			corev1.ConditionTrue,
			"PodGroupSynced",
			fmt.Sprintf("PodGroup is synced for RoleSets: %s", strings.Join(syncedRoleSets, ",")),
		))
		return
	}

	incomplete := roleSetNamesWithout(volcanoRoleSets, syncedRoleSets)
	SetStormServiceCondition(status, *utils.NewCondition(
		orchestrationv1alpha1.StormServicePodGroupSynced,
		corev1.ConditionFalse,
		"PodGroupSyncIncomplete",
		fmt.Sprintf("PodGroup sync is incomplete for RoleSets: %s", strings.Join(incomplete, ",")),
	))
}

func volcanoRoleSetNames(roleSets []*orchestrationv1alpha1.RoleSet) []string {
	names := map[string]struct{}{}
	for _, rs := range roleSets {
		if rs == nil {
			continue
		}
		if rs.Spec.SchedulingStrategy != nil && rs.Spec.SchedulingStrategy.VolcanoSchedulingStrategy != nil {
			names[rs.Name] = struct{}{}
		}
	}
	return sortedRoleSetNames(names)
}

func conditionRoleSetNames(roleSets []*orchestrationv1alpha1.RoleSet, condType orchestrationv1alpha1.ConditionType, condStatus corev1.ConditionStatus) []string {
	names := map[string]struct{}{}
	for _, rs := range roleSets {
		if rs == nil {
			continue
		}
		cond := utils.GetCondition(rs.Status.Conditions, condType)
		if cond != nil && cond.Status == condStatus {
			names[rs.Name] = struct{}{}
		}
	}
	return sortedRoleSetNames(names)
}

func roleSetNamesWithout(names, excluded []string) []string {
	excludedSet := map[string]struct{}{}
	for _, name := range excluded {
		excludedSet[name] = struct{}{}
	}
	remaining := map[string]struct{}{}
	for _, name := range names {
		if _, ok := excludedSet[name]; !ok {
			remaining[name] = struct{}{}
		}
	}
	return sortedRoleSetNames(remaining)
}

func sortedRoleSetNames(nameSet map[string]struct{}) []string {
	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
