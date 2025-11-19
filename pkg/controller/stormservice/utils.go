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

package stormservice

import (
	"sort"

	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

const (
	ScalingEventType         = "Scaling"
	RolloutEventType         = "Rollout"
	HeadlessServiceEventType = "HeadlessServiceSynced"
)

// SetStormServiceCondition updates the stormService to include the provided condition. If the condition that
// we are about to add already exists and has the same status and reason then we are not going to update.
func SetStormServiceCondition(status *orchestrationv1alpha1.StormServiceStatus, condition orchestrationv1alpha1.Condition) {
	currentCond := utils.GetCondition(status.Conditions, condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}
	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}
	newConditions := utils.FilterOutCondition(status.Conditions, condition.Type)
	status.Conditions = append(newConditions, condition)
}

// RemoveStormServiceCondition removes the stormService condition with the provided type.
func RemoveStormServiceCondition(status *orchestrationv1alpha1.StormServiceStatus, condType orchestrationv1alpha1.ConditionType) {
	status.Conditions = utils.FilterOutCondition(status.Conditions, condType)
}

// MaxUnavailable returns the maximum unavailable roleSets a rolling stormService can take.
func MaxUnavailable(stormService orchestrationv1alpha1.StormService) int32 {
	if !IsRollingUpdate(&stormService) || *(stormService.Spec.Replicas) == 0 {
		return int32(0)
	}
	// Error caught by validation
	_, maxUnavailable, _ := ResolveFenceposts(stormService.Spec.UpdateStrategy.MaxSurge, stormService.Spec.UpdateStrategy.MaxUnavailable, *(stormService.Spec.Replicas))
	if maxUnavailable > *stormService.Spec.Replicas {
		return *stormService.Spec.Replicas
	}
	return maxUnavailable
}

// MinAvailable returns the minimum available roleSets of a given stormService
func MinAvailable(stormService *orchestrationv1alpha1.StormService) int32 {
	if !IsRollingUpdate(stormService) {
		return int32(0)
	}
	return *(stormService.Spec.Replicas) - MaxUnavailable(*stormService)
}

// MaxSurge returns the maximum surge roleSets a rolling stormService can take.
func MaxSurge(stormService *orchestrationv1alpha1.StormService) int32 {
	if !IsRollingUpdate(stormService) {
		return int32(0)
	}
	// Error caught by validation
	maxSurge, _, _ := ResolveFenceposts(stormService.Spec.UpdateStrategy.MaxSurge, stormService.Spec.UpdateStrategy.MaxUnavailable, *(stormService.Spec.Replicas))
	return maxSurge
}

// IsRollingUpdate returns true if the strategy type is a rolling update.
func IsRollingUpdate(stormService *orchestrationv1alpha1.StormService) bool {
	return stormService.Spec.UpdateStrategy.Type == "" || stormService.Spec.UpdateStrategy.Type == orchestrationv1alpha1.RollingUpdateStormServiceStrategyType
}

// ResolveFenceposts resolves both maxSurge and maxUnavailable. This needs to happen in one
// step. For example:
//
// 2 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1), then old(-1), then new(+1)
// 1 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1)
// 2 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1)
// 2 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1)
func ResolveFenceposts(maxSurge, maxUnavailable *intstrutil.IntOrString, desired int32) (int32, int32, error) {
	surge, err := intstrutil.GetScaledValueFromIntOrPercent(intstrutil.ValueOrDefault(maxSurge, intstrutil.FromInt(0)), int(desired), true)
	if err != nil {
		return 0, 0, err
	}
	unavailable, err := intstrutil.GetScaledValueFromIntOrPercent(intstrutil.ValueOrDefault(maxUnavailable, intstrutil.FromInt(0)), int(desired), false)
	if err != nil {
		return 0, 0, err
	}

	if surge == 0 && unavailable == 0 {
		// Validation should never allow the user to explicitly use zero values for both maxSurge
		// maxUnavailable. Due to rounding down maxUnavailable though, it may resolve to zero.
		// If both fenceposts resolve to zero, then we should set maxUnavailable to 1 on the
		// theory that surge might not work due to quota.
		unavailable = 1
	}

	return int32(surge), int32(unavailable), nil
}

func getRoleSetRevision(roleSet *orchestrationv1alpha1.RoleSet) string {
	return roleSet.Labels[constants.StormServiceRevisionLabelKey]
}

func isRoleSetMatchRevision(roleSet *orchestrationv1alpha1.RoleSet, revision string) bool {
	return getRoleSetRevision(roleSet) == revision
}

func getRoleByName(roleSet *orchestrationv1alpha1.RoleSet, name string) *orchestrationv1alpha1.RoleSpec {
	for i := range roleSet.Spec.Roles {
		if roleSet.Spec.Roles[i].Name == name {
			return &roleSet.Spec.Roles[i]
		}
	}
	return nil
}

func isAllRoleUpdated(roleSet *orchestrationv1alpha1.RoleSet) bool {
	var updatedAndReady = true
	for _, roleStatus := range roleSet.Status.Roles {
		roleSpec := getRoleByName(roleSet, roleStatus.Name)
		if roleSpec == nil {
			continue
		}
		var expectedReplicas int32
		if roleSpec.Replicas != nil {
			expectedReplicas = *roleSpec.Replicas
		}
		if expectedReplicas != roleStatus.UpdatedReplicas {
			updatedAndReady = false
			break
		}
	}
	return updatedAndReady
}

func isAllRoleUpdatedAndReady(roleSet *orchestrationv1alpha1.RoleSet) bool {
	var updatedAndReady = true
	for _, roleStatus := range roleSet.Status.Roles {
		roleSpec := getRoleByName(roleSet, roleStatus.Name)
		if roleSpec == nil {
			continue
		}
		var expectedReplicas int32
		if roleSpec.Replicas != nil {
			expectedReplicas = *roleSpec.Replicas
		}
		if expectedReplicas != roleStatus.UpdatedReadyReplicas {
			updatedAndReady = false
			break
		}
	}
	return updatedAndReady
}

func filterRoleSetByRevision(roleSets []*orchestrationv1alpha1.RoleSet, revision string) (match, notMatch []*orchestrationv1alpha1.RoleSet) {
	match = []*orchestrationv1alpha1.RoleSet{}
	notMatch = []*orchestrationv1alpha1.RoleSet{}
	for i := range roleSets {
		if isRoleSetMatchRevision(roleSets[i], revision) {
			match = append(match, roleSets[i])
		} else {
			notMatch = append(notMatch, roleSets[i])
		}
	}
	return
}

func filterReadyRoleSets(roleSets []*orchestrationv1alpha1.RoleSet) (ready []*orchestrationv1alpha1.RoleSet, notReady []*orchestrationv1alpha1.RoleSet) {
	ready = []*orchestrationv1alpha1.RoleSet{}
	notReady = []*orchestrationv1alpha1.RoleSet{}
	for i := range roleSets {
		if utils.IsRoleSetReady(roleSets[i]) {
			ready = append(ready, roleSets[i])
		} else {
			notReady = append(notReady, roleSets[i])
		}
	}
	return
}

func filterTerminatingRoleSets(roleSets []*orchestrationv1alpha1.RoleSet) (active, terminating []*orchestrationv1alpha1.RoleSet) {
	terminating = []*orchestrationv1alpha1.RoleSet{}
	active = []*orchestrationv1alpha1.RoleSet{}
	for i := range roleSets {
		if roleSets[i].DeletionTimestamp != nil {
			terminating = append(terminating, roleSets[i])
		} else {
			active = append(active, roleSets[i])
		}
	}
	return
}

func sortRoleSetByReadiness(roleSets []*orchestrationv1alpha1.RoleSet) {
	sort.Slice(roleSets, func(i, j int) bool {
		return !utils.IsRoleSetReady(roleSets[i])
	})
}

// Sorts role sets: old revisions before new, and within the same revision, not-ready before ready.
func sortRoleSetByRevision(roleSets []*orchestrationv1alpha1.RoleSet, updatedRevision string) {
	sort.Slice(roleSets, func(i, j int) bool {
		if isRoleSetMatchRevision(roleSets[i], updatedRevision) {
			return false
		} else if isRoleSetMatchRevision(roleSets[j], updatedRevision) {
			return true
		} else {
			return !utils.IsRoleSetReady(roleSets[i])
		}
	})
}

// isServiceEqual compares two Kubernetes Service objects for equality
func isServiceEqual(a, b *corev1.Service) bool {
	return a.Spec.Type == b.Spec.Type &&
		apiequality.Semantic.DeepEqual(a.Spec.Selector, b.Spec.Selector) &&
		a.Spec.ClusterIP == b.Spec.ClusterIP &&
		a.Spec.PublishNotReadyAddresses == b.Spec.PublishNotReadyAddresses
}

// computeRoleRevisions compares roles between current and update StormService versions
// and returns a map of role names to their effective ControllerRevision info.
// This is the key function that links role-template-hash (detection) with ControllerRevision (ordering).
func computeRoleRevisions(current, update *orchestrationv1alpha1.StormService, currentCR, updateCR *apps.ControllerRevision) map[string]*apps.ControllerRevision {
	roleRevisions := make(map[string]*apps.ControllerRevision)

	// Get roles from both versions
	currentRoles := make(map[string]*orchestrationv1alpha1.RoleSpec)
	if current != nil && current.Spec.Template.Spec != nil {
		for i := range current.Spec.Template.Spec.Roles {
			role := &current.Spec.Template.Spec.Roles[i]
			currentRoles[role.Name] = role
		}
	}

	updateRoles := make(map[string]*orchestrationv1alpha1.RoleSpec)
	if update != nil && update.Spec.Template.Spec != nil {
		for i := range update.Spec.Template.Spec.Roles {
			role := &update.Spec.Template.Spec.Roles[i]
			updateRoles[role.Name] = role
		}
	}

	// For each role in the update version, determine which CR to use
	for roleName, updateRole := range updateRoles {
		currentRole, exists := currentRoles[roleName]
		if !exists {
			// New role, use updateCR
			roleRevisions[roleName] = updateCR
			klog.Infof("Role %s is new, using update revision %d (%s)", roleName, updateCR.Revision, updateCR.Name)
			continue
		}

		// Compare template hashes (same hash algorithm as role-template-hash label)
		currentHash := ctrlutil.ComputeHash(&currentRole.Template, nil)
		updateHash := ctrlutil.ComputeHash(&updateRole.Template, nil)

		if currentHash != updateHash {
			// Role template changed, use updateCR
			roleRevisions[roleName] = updateCR
			klog.Infof("Role %s template changed (hash %s -> %s), using update revision %d (%s)",
				roleName, currentHash, updateHash, updateCR.Revision, updateCR.Name)
		} else {
			// Role template unchanged, use currentCR
			roleRevisions[roleName] = currentCR
			klog.Infof("Role %s template unchanged (hash %s), keeping current revision %d (%s)",
				roleName, currentHash, currentCR.Revision, currentCR.Name)
		}
	}

	return roleRevisions
}

// aggregateRoleStatuses aggregates role statuses from all RoleSets by role name.
// This provides pod-level aggregation across all RoleSets, which is useful in both:
// - Pool mode: Multiple roles per RoleSet (e.g., prefill, decode)
// - Replica mode: Single role per RoleSet, aggregated across multiple RoleSets
//
// The aggregation behavior:
// - Replicas, ReadyReplicas, NotReadyReplicas: Aggregated from ALL RoleSets regardless of revision
// - UpdatedReplicas, UpdatedReadyReplicas: Only aggregated from RoleSets matching updateRevision
//
// This ensures that during a rollout, Updated* fields reflect pods at the target revision,
// while other fields show total capacity across all revisions.
//
// Returns aggregated role statuses sorted by role name for consistent output.
func aggregateRoleStatuses(roleSets []*orchestrationv1alpha1.RoleSet, updateRevision string) []orchestrationv1alpha1.RoleStatus {
	roleMap := make(map[string]orchestrationv1alpha1.RoleStatus)

	// Aggregate statuses from all RoleSets
	for _, rs := range roleSets {
		isUpdateRevision := isRoleSetMatchRevision(rs, updateRevision)
		for _, roleStatus := range rs.Status.Roles {
			aggStatus := roleMap[roleStatus.Name]
			aggStatus.Name = roleStatus.Name
			// Always aggregate total capacity metrics from all RoleSets
			aggStatus.Replicas += roleStatus.Replicas
			aggStatus.ReadyReplicas += roleStatus.ReadyReplicas
			aggStatus.NotReadyReplicas += roleStatus.NotReadyReplicas
			// Only aggregate Updated* metrics from RoleSets matching the target revision
			if isUpdateRevision {
				aggStatus.UpdatedReplicas += roleStatus.Replicas
				aggStatus.UpdatedReadyReplicas += roleStatus.ReadyReplicas
			}
			roleMap[roleStatus.Name] = aggStatus
		}
	}

	// Convert map to slice and sort by role name for consistent output
	roleStatuses := make([]orchestrationv1alpha1.RoleStatus, 0, len(roleMap))
	for _, status := range roleMap {
		roleStatuses = append(roleStatuses, status)
	}

	sort.Slice(roleStatuses, func(i, j int) bool {
		return roleStatuses[i].Name < roleStatuses[j].Name
	})

	return roleStatuses
}
