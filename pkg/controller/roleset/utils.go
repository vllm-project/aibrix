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

package roleset

import (
	"context"
	"fmt"
	"sort"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ssctrl "github.com/vllm-project/aibrix/pkg/controller/stormservice"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// Reasons for roleSet conditions
	//
	// Ready:

	ReadyConditionType       = "Ready"
	ProgressingConditionType = "Progressing"
	FailureConditionType     = "Failure"

	PodGroupSyncedEventType = "PodGroupSynced"
	PodSyncedEventType      = "PodSynced"
	FailureEventType        = "Failure"
)

// GetReadyReplicaCountForRole returns the number of ready roleSets corresponding to the given replica sets.
func GetReadyReplicaCountForRole(pods []*v1.Pod) int32 {
	totalReadyReplicas := int32(0)
	for _, pod := range pods {
		if pod != nil {
			if podutil.IsPodReady(pod) {
				totalReadyReplicas++
			}
		}
	}
	return totalReadyReplicas
}

// SetRoleSetCondition updates the roleSet to include the provided condition. If the condition that
// we are about to add already exists and has the same status and reason then we are not going to update.
func SetRoleSetCondition(status *orchestrationv1alpha1.RoleSetStatus, condition orchestrationv1alpha1.Condition) {
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

// RemoveRoleSetCondition removes the roleSet condition with the provided type.
func RemoveRoleSetCondition(status *orchestrationv1alpha1.RoleSetStatus, condType orchestrationv1alpha1.ConditionType) {
	status.Conditions = utils.FilterOutCondition(status.Conditions, condType)
}

var (
	ContainerInjectEnv   = sets.NewString(constants.RoleTemplateHashEnvKey, constants.StormServiceNameEnvKey, constants.RoleSetNameEnvKey, constants.RoleSetIndexEnvKey, constants.RoleNameEnvKey, constants.RoleReplicaIndexEnvKey)
	roleSetInheritLabels = map[string]bool{
		// TODO: move to const
		"name":           true,
		"previous-owner": true,
	}
)

func renderStormServicePod(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, pod *v1.Pod, roleIndex *int) {
	templateHash := ctrlutil.ComputeHash(&role.Template, nil)
	if roleIndex != nil {
		// add role template hash to pod name, to avoid pod name duplication during rollout
		pod.Name = fmt.Sprintf("%s-%s-%s-%d", roleSet.Name, role.Name, templateHash, *roleIndex)
	} else {
		pod.GenerateName = fmt.Sprintf("%s-%s-", roleSet.Name, role.Name)
	}
	pod.Namespace = roleSet.Namespace
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	// inject pod labels
	pod.Labels[constants.RoleSetNameLabelKey] = roleSet.Name
	pod.Labels[constants.RoleNameLabelKey] = role.Name
	pod.Labels[constants.RoleTemplateHashLabelKey] = templateHash
	pod.Labels[constants.StormServiceNameLabelKey] = roleSet.Labels[constants.StormServiceNameLabelKey]
	for k, v := range roleSet.Labels {
		if _, ok := roleSetInheritLabels[k]; ok {
			pod.Labels[k] = v
		}
	}
	if roleSet.Spec.SchedulingStrategy != nil {
		if roleSet.Spec.SchedulingStrategy.CoschedulingSchedulingStrategy != nil {
			pod.Labels[constants.CoschedulingPodGroupNameLabelKey] = roleSet.Name
		}
		if roleSet.Spec.SchedulingStrategy.GodelSchedulingStrategy != nil {
			pod.Labels[constants.GodelPodGroupNameAnnotationKey] = roleSet.Name
		}
		if roleSet.Spec.SchedulingStrategy.VolcanoSchedulingStrategy != nil {
			pod.Labels[constants.VolcanoPodGroupNameAnnotationKey] = roleSet.Name
		}
	}

	// inject pod annotations
	pod.Annotations[constants.RoleSetIndexAnnotationKey] = roleSet.Annotations[constants.RoleSetIndexAnnotationKey]
	if roleIndex != nil {
		pod.Annotations[constants.RoleReplicaIndexAnnotationKey] = fmt.Sprintf("%d", *roleIndex)
		// inject to label as well for routing service discovery (some engines use label selector to find pods only)
		pod.Labels[constants.RoleReplicaIndexLabelKey] = fmt.Sprintf("%d", *roleIndex)
	}
	if roleSet.Spec.SchedulingStrategy != nil {
		if roleSet.Spec.SchedulingStrategy.VolcanoSchedulingStrategy != nil {
			pod.Annotations[constants.VolcanoPodGroupNameAnnotationKey] = roleSet.Name
		}
		if roleSet.Spec.SchedulingStrategy.GodelSchedulingStrategy != nil {
			pod.Annotations[constants.GodelPodGroupNameAnnotationKey] = roleSet.Name
		}
	}

	// inject per-role revision labels from RoleSet annotations
	// These are computed by StormService controller based on role template hash comparison
	roleRevKey := fmt.Sprintf("%s.%s", constants.RoleRevisionAnnotationPrefix, role.Name)
	if roleRev, ok := roleSet.Annotations[roleRevKey]; ok {
		pod.Labels[constants.RoleRevisionLabelKey] = roleRev
	}
	roleRevNameKey := fmt.Sprintf("%s.%s", constants.RoleRevisionNameAnnotationPrefix, role.Name)
	if roleRevName, ok := roleSet.Annotations[roleRevNameKey]; ok {
		pod.Labels[constants.RoleRevisionNameLabelKey] = roleRevName
	}

	// manually set the hostname and subdomain for FQDN
	pod.Spec.Hostname = pod.Name
	pod.Spec.Subdomain = roleSet.Labels[constants.StormServiceNameLabelKey]

	// inject container env
	for i := range pod.Spec.Containers {
		injectContainerEnvVars(
			&pod.Spec.Containers[i],
			roleSet,
			role,
			roleIndex,
			templateHash,
		)
	}
}

// injectContainerEnvVars injects env variables into container.
func injectContainerEnvVars(
	container *v1.Container,
	roleSet *orchestrationv1alpha1.RoleSet,
	role *orchestrationv1alpha1.RoleSpec,
	roleIndex *int,
	templateHash string,
) {
	envMap := make(map[string]v1.EnvVar, len(container.Env)+6)

	// copy existing env
	for _, e := range container.Env {
		if !ContainerInjectEnv.Has(e.Name) {
			envMap[e.Name] = e
		}
	}

	envMap[constants.StormServiceNameEnvKey] = v1.EnvVar{
		Name:  constants.StormServiceNameEnvKey,
		Value: roleSet.Labels[constants.StormServiceNameLabelKey],
	}

	envMap[constants.RoleSetNameEnvKey] = v1.EnvVar{
		Name:  constants.RoleSetNameEnvKey,
		Value: roleSet.Name,
	}

	envMap[constants.RoleSetIndexEnvKey] = v1.EnvVar{
		Name:  constants.RoleSetIndexEnvKey,
		Value: roleSet.Annotations[constants.RoleSetIndexAnnotationKey],
	}

	envMap[constants.RoleNameEnvKey] = v1.EnvVar{
		Name:  constants.RoleNameEnvKey,
		Value: role.Name,
	}

	envMap[constants.RoleTemplateHashEnvKey] = v1.EnvVar{
		Name:  constants.RoleTemplateHashEnvKey,
		Value: templateHash,
	}

	if roleIndex != nil {
		envMap[constants.RoleReplicaIndexEnvKey] = v1.EnvVar{
			Name:  constants.RoleReplicaIndexEnvKey,
			Value: fmt.Sprintf("%d", *roleIndex),
		}
	}
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		keys = append(keys, k)
	}
	// sort the env by name before adding them to the container spec
	// to ensure deterministic output and prevent unnecessary pod updates
	sort.Strings(keys)

	container.Env = make([]v1.EnvVar, 0, len(envMap))
	for _, k := range keys {
		container.Env = append(container.Env, envMap[k])
	}
}

func filterRolePods(role *orchestrationv1alpha1.RoleSpec, pods []*v1.Pod) []*v1.Pod {
	var filtered []*v1.Pod
	for i := range pods {
		if pods[i].Labels[constants.RoleNameLabelKey] == role.Name {
			filtered = append(filtered, pods[i])
		}
	}
	return filtered
}

func filterActivePods(pods []*v1.Pod) (active []*v1.Pod, inactive []*v1.Pod) {
	for i := range pods {
		if podutil.IsPodActive(pods[i]) {
			active = append(active, pods[i])
		} else {
			inactive = append(inactive, pods[i])
		}
	}
	return
}

func filterTerminatingPods(pods []*v1.Pod) (terminating []*v1.Pod, notTerminating []*v1.Pod) {
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			terminating = append(terminating, pods[i])
		} else {
			notTerminating = append(notTerminating, pods[i])
		}
	}
	return
}

func filterPodsByIndex(pods []*v1.Pod, index int) (result []*v1.Pod) {
	for i := range pods {
		if pods[i].Annotations[constants.RoleReplicaIndexAnnotationKey] == fmt.Sprintf("%d", index) {
			result = append(result, pods[i])
		}
	}
	return
}

func filterReadyPods(pods []*v1.Pod) (ready []*v1.Pod, notReady []*v1.Pod) {
	for i := range pods {
		if podutil.IsPodActive(pods[i]) && podutil.IsPodReady(pods[i]) {
			ready = append(ready, pods[i])
		} else {
			notReady = append(notReady, pods[i])
		}
	}
	return
}

func filterUpdatedPods(pods []*v1.Pod, templateHash string) (updated []*v1.Pod, outdated []*v1.Pod) {
	for i := range pods {
		if pods[i].Labels[constants.RoleTemplateHashLabelKey] == templateHash {
			updated = append(updated, pods[i])
		} else {
			outdated = append(outdated, pods[i])
		}
	}
	return
}

func sortPodsByActive(pods []*v1.Pod) {
	sort.Slice(pods, func(i, j int) bool {
		if !podutil.IsPodActive(pods[i]) {
			return true
		} else if !podutil.IsPodActive(pods[j]) {
			return false
		}
		if !podutil.IsPodReady(pods[i]) {
			return true
		} else if !podutil.IsPodReady(pods[j]) {
			return false
		}
		return !pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
}

// outdated notReady -> outdated ready -> current notReady -> current ready
func sortPodsByTemplateHash(pods []*v1.Pod, targetHash string) {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Labels[constants.RoleTemplateHashLabelKey] != pods[j].Labels[constants.RoleTemplateHashLabelKey] {
			if pods[i].Labels[constants.RoleTemplateHashLabelKey] == targetHash {
				return false
			}
			if pods[j].Labels[constants.RoleTemplateHashLabelKey] == targetHash {
				return true
			}
		}
		if !podutil.IsPodReady(pods[i]) {
			return true
		} else if !podutil.IsPodReady(pods[j]) {
			return false
		}
		return !pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp)
	})
}

func MaxUnavailable(role *orchestrationv1alpha1.RoleSpec) int32 {
	expectedReplicas := getRoleReplicas(role)
	if expectedReplicas == 0 {
		return 0
	}
	// Error caught by validation
	_, maxUnavailable, _ := ssctrl.ResolveFenceposts(role.UpdateStrategy.MaxSurge, role.UpdateStrategy.MaxUnavailable, expectedReplicas)
	if maxUnavailable > expectedReplicas {
		return expectedReplicas
	}
	return maxUnavailable
}

func MaxSurge(role *orchestrationv1alpha1.RoleSpec) int32 {
	expectedReplicas := getRoleReplicas(role)
	if expectedReplicas == 0 {
		return 0
	}
	maxSurge, _, _ := ssctrl.ResolveFenceposts(role.UpdateStrategy.MaxSurge, role.UpdateStrategy.MaxUnavailable, expectedReplicas)
	return maxSurge
}

func getRoleReplicas(role *orchestrationv1alpha1.RoleSpec) int32 {
	if role.Replicas != nil && *role.Replicas > 0 {
		return *role.Replicas
	}
	return 0
}

func getRolePods(ctx context.Context, cli client.Client, namespace, roleSetName, roleName string) (pods []*v1.Pod, err error) {
	roleSetRequirement, _ := labels.NewRequirement(constants.RoleSetNameLabelKey, selection.Equals, []string{roleSetName})
	roleRequirement, _ := labels.NewRequirement(constants.RoleNameLabelKey, selection.Equals, []string{roleName})
	labelSelector := labels.NewSelector().Add(*roleSetRequirement, *roleRequirement)
	podList := &v1.PodList{}
	if err = cli.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return nil, err
	}
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}
	return
}

func createPodsInBatch(ctx context.Context, cli client.Client, podsToCreate []*v1.Pod) (creation int, err error) {
	if len(podsToCreate) > PodBurst {
		podsToCreate = podsToCreate[:PodBurst]
	}
	return utils.SlowStartBatch(len(podsToCreate), PodOperationInitBatchSize, func(index int) error {
		pod := podsToCreate[index]
		err := cli.Create(ctx, pod)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				klog.V(4).InfoS("Pod already exists, skipping", "pod", pod.Name)
				return nil
			}
			if errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
				// if the namespace is being terminated, we don't have to do
				// anything because any creation will fail
				return nil
			}
		}
		return err
	})
}

func deletePodsInBatch(ctx context.Context, cli client.Client, podsToDelete []*v1.Pod) (deletion int, err error) {
	if len(podsToDelete) > PodBurst {
		podsToDelete = podsToDelete[:PodBurst]
	}
	return utils.SlowStartBatch(len(podsToDelete), PodOperationInitBatchSize, func(index int) error {
		pod := podsToDelete[index]
		err := cli.Delete(ctx, pod)
		if err != nil {
			if errors.IsNotFound(err) {
				klog.V(4).InfoS("Pod already deleted, skipping", "pod", pod.Name)
				return nil
			}
		}
		return err
	})
}

func sortRolesByUpgradeOrder(roles []orchestrationv1alpha1.RoleSpec) []orchestrationv1alpha1.RoleSpec {
	sortedRoles := make([]orchestrationv1alpha1.RoleSpec, len(roles))
	copy(sortedRoles, roles)
	sort.SliceStable(sortedRoles, func(i, j int) bool {
		iOrder := sortedRoles[i].UpgradeOrder
		jOrder := sortedRoles[j].UpgradeOrder
		if iOrder == nil {
			// i is nil. If j is also nil, stable sort. If j is not nil, i comes after.
			// In both cases, i is not "less than" j.
			return false
		}
		if jOrder == nil {
			// i is not nil, but j is. i comes before.
			return true
		}
		// Both have explicit orders, sort by value.
		return *iOrder < *jOrder
	})
	return sortedRoles
}

func isRoleDependenciesReady(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) bool {
	if len(role.Dependencies) == 0 {
		return true
	}

	// Initialize all roles to 0
	roleStatusMap := make(map[string]int32)
	for _, r := range roleSet.Spec.Roles {
		roleStatusMap[r.Name] = 0
	}
	for _, rs := range roleSet.Status.Roles {
		roleStatusMap[rs.Name] = rs.ReadyReplicas
	}

	for _, depName := range role.Dependencies {
		depReady := roleStatusMap[depName]
		expected := int32(1)
		for _, r := range roleSet.Spec.Roles {
			if r.Name == depName && r.Replicas != nil {
				expected = *r.Replicas
				break
			}
		}
		if depReady < expected {
			klog.V(4).Infof("Role %s depends on %s, "+
				"but only %d/%d ready", role.Name, depName, depReady, expected)
			return false
		}
	}
	return true
}
