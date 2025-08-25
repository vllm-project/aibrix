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
	"strconv"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

// PodSetRoleSyncer implements RoleRollingSyncer using PodSets
// This follows the exact same patterns as StatefulRoleSyncer but operates on PodSets instead of Pods
type PodSetRoleSyncer struct {
	cli client.Client
	// To allow injection for testing.
	computeHashFunc func(template *v1.PodTemplateSpec, collisionCount *int32) string
}

func (p *PodSetRoleSyncer) Scale(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	var podSetsToCreate, podSetsToDelete []*orchestrationv1alpha1.PodSet
	allPodSets, err := getRolePodSets(ctx, p.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	// delete podsets that are in terminated state
	activePodSets, inactivePodSets := filterActivePodSets(allPodSets)
	terminatingPodSets, terminatedPodSets := filterTerminatingPodSets(inactivePodSets)
	podSetsToDelete = append(podSetsToDelete, terminatedPodSets...)

	// delete podsets that cannot find the corresponding slot
	slots, toDelete := p.podSetSlotForRole(role, activePodSets)
	podSetsToDelete = append(podSetsToDelete, toDelete...)
	createBudget := int32(len(slots)) + MaxSurge(role) - int32(len(activePodSets)) - int32(len(terminatingPodSets))
	// check podsets for each slot
	for i := range slots {
		if len(slots[i]) == 0 {
			if createBudget <= 0 {
				continue
			}
			podSet := p.createPodSetForRole(roleSet, role, &i)
			podSetsToCreate = append(podSetsToCreate, podSet)
			createBudget--
		} else if len(slots[i]) > 1 {
			readyPodSets, notReadyPodSets := filterReadyPodSets(slots[i])
			roleTemplateHash := p.computeHashFunc(&role.Template, nil)
			updatedReadyPodSets, outdatedReadyPodSets := filterUpdatedPodSets(readyPodSets, roleTemplateHash)
			updatedNotReadyPodSets, outdatedNotReadyPodSets := filterUpdatedPodSets(notReadyPodSets, roleTemplateHash)
			podSetsToDelete = append(podSetsToDelete, outdatedNotReadyPodSets...)
			if len(updatedReadyPodSets) > 0 {
				// only keep 1 updated ready podset for each slot
				podSetsToDelete = append(podSetsToDelete, outdatedReadyPodSets...)
				podSetsToDelete = append(podSetsToDelete, updatedNotReadyPodSets...)
				if len(updatedReadyPodSets) > 1 {
					podSetsToDelete = append(podSetsToDelete, updatedReadyPodSets[1:]...)
				}
			} else {
				// keep 1 updated not ready podset & 1 outdated ready podset for each slot
				if len(outdatedReadyPodSets) > 1 {
					podSetsToDelete = append(podSetsToDelete, outdatedReadyPodSets[1:]...)
				}
				if len(updatedNotReadyPodSets) > 1 {
					podSetsToDelete = append(podSetsToDelete, updatedNotReadyPodSets[1:]...)
				}
			}
		}
	}
	if _, err = createPodSetsInBatch(ctx, p.cli, podSetsToCreate); err != nil {
		return false, err
	}
	if _, err = deletePodSetsInBatch(ctx, p.cli, podSetsToDelete); err != nil {
		return false, err
	}
	p.printLog(roleSet, role, podSetsToCreate, podSetsToDelete)
	return len(podSetsToCreate) > 0 || len(podSetsToDelete) > 0, nil
}

func (p *PodSetRoleSyncer) readySlotNum(role *orchestrationv1alpha1.RoleSpec, allPodSets []*orchestrationv1alpha1.PodSet) int {
	activePodSets, _ := filterActivePodSets(allPodSets)
	slots, _ := p.podSetSlotForRole(role, activePodSets)
	var result int
	for i := range slots {
		ready, _ := filterReadyPodSets(slots[i])
		if len(ready) >= 1 {
			result++
		}
	}
	return result
}

func (p *PodSetRoleSyncer) updatedSlotNum(role *orchestrationv1alpha1.RoleSpec, allPodSets []*orchestrationv1alpha1.PodSet) (int32, int32, int32) {
	activePodSets, _ := filterActivePodSets(allPodSets)
	slots, _ := p.podSetSlotForRole(role, activePodSets)
	currentHash := p.computeHashFunc(&role.Template, nil)

	updatedTotal := 0
	updatedReadyTotal := 0
	outdatedTotal := 0
	for i := range slots {
		// Consider the slot updated if it contains any PodSet with the new version
		hasNewVersion := false
		for _, podSet := range slots[i] {
			if podSet.Labels[constants.RoleTemplateHashLabelKey] == currentHash {
				hasNewVersion = true
				break
			}
		}
		if hasNewVersion {
			updatedTotal++
			if len(slots[i]) == 1 && isPodSetReady(slots[i][0]) {
				updatedReadyTotal++
			}
		} else if len(slots[i]) > 0 {
			outdatedTotal++
		}
	}
	return int32(updatedTotal), int32(updatedReadyTotal), int32(outdatedTotal)
}

func (p *PodSetRoleSyncer) Rollout(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) error {
	var toCreate, toDelete []*orchestrationv1alpha1.PodSet
	allPodSets, err := getRolePodSets(ctx, p.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}
	expectedReplicas := getRoleReplicas(role)
	activePodSets, _ := filterActivePodSets(allPodSets)
	readySlotNum := p.readySlotNum(role, allPodSets)
	deleteBudget := int32(readySlotNum) - expectedReplicas + MaxUnavailable(role)
	createBudget := expectedReplicas + MaxSurge(role) - int32(len(allPodSets))
	roleTemplateHash := p.computeHashFunc(&role.Template, nil)
	klog.Infof("[PodSetRoleSyncer.Rollout] roleset %s/%s role %s expectedReplicas %d, deleteBudget %d, createBudget %d, template hash %s", roleSet.Namespace, roleSet.Name, role.Name, expectedReplicas, deleteBudget, createBudget, roleTemplateHash)

	slots, _ := p.podSetSlotForRole(role, activePodSets)
	for i := range slots {
		if len(slots[i]) != 1 {
			// wait for scale to handle this slot
			continue
		}
		if slots[i][0].Labels[constants.RoleTemplateHashLabelKey] == roleTemplateHash {
			continue
		}
		if !isPodSetReady(slots[i][0]) {
			toDelete = append(toDelete, slots[i][0])
			continue
		}
		if deleteBudget > 0 {
			toDelete = append(toDelete, slots[i][0])
			deleteBudget--
		} else if createBudget > 0 {
			podSet := p.createPodSetForRole(roleSet, role, &i)
			toCreate = append(toCreate, podSet)
			createBudget--
		}
	}
	if _, err = createPodSetsInBatch(ctx, p.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodSetsInBatch(ctx, p.cli, toDelete); err != nil {
		return err
	}
	p.printLog(roleSet, role, toCreate, toDelete)
	return nil
}

func (p *PodSetRoleSyncer) RolloutByStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, currentStep int32) error {
	var toCreate, toDelete []*orchestrationv1alpha1.PodSet
	allPodSets, err := getRolePodSets(ctx, p.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}

	expectedReplicas := getRoleReplicas(role)
	expectedUpdatedReplicas := utils.MinInt32((MaxSurge(role)+MaxUnavailable(role))*currentStep, expectedReplicas)
	activePodSets, _ := filterActivePodSets(allPodSets)
	readySlotNum := p.readySlotNum(role, allPodSets)
	deleteBudget := int32(readySlotNum) - expectedReplicas + MaxUnavailable(role)
	createBudget := expectedReplicas + MaxSurge(role) - int32(len(allPodSets))
	roleTemplateHash := p.computeHashFunc(&role.Template, nil)
	klog.Infof("[PodSetRoleSyncer.RolloutByStep] Step %d: roleset %s/%s role %s expectedReplicas %d, deleteBudget %d, createBudget %d, template hash %s", currentStep, roleSet.Namespace, roleSet.Name, role.Name, expectedReplicas, deleteBudget, createBudget, roleTemplateHash)

	updatedTotal, _, outdatedTotal := p.updatedSlotNum(role, allPodSets)

	// Constraints for this step:
	// By the end of the current step, we aim to have (expectedReplicas - expectedUpdatedReplicas) outdated PodSets
	// and expectedUpdatedReplicas updated PodSets.
	// Therefore, the create and delete budgets must not exceed the difference between the current and expected states.
	createBudget = utils.MinInt32(createBudget, expectedUpdatedReplicas-updatedTotal)
	deleteBudget = utils.MinInt32(deleteBudget, outdatedTotal-expectedReplicas+expectedUpdatedReplicas)

	klog.Infof("[PodSetRoleSyncer.RolloutByStep] Step %d: roleset %s/%s role %s expectedUpdatedReplicas %d, updatedTotal %d, outdatedTotal %d, deleteBudget %d, createBudget %d",
		currentStep, roleSet.Namespace, roleSet.Name, role.Name, expectedUpdatedReplicas, updatedTotal, outdatedTotal, deleteBudget, createBudget)
	slots, _ := p.podSetSlotForRole(role, activePodSets)
	for i := range slots {
		if len(slots[i]) != 1 {
			// wait for scale to handle this slot
			continue
		}
		if slots[i][0].Labels[constants.RoleTemplateHashLabelKey] == roleTemplateHash {
			continue
		}
		if !isPodSetReady(slots[i][0]) {
			toDelete = append(toDelete, slots[i][0])
			continue
		}
		if deleteBudget > 0 {
			toDelete = append(toDelete, slots[i][0])
			deleteBudget--
		} else if createBudget > 0 {
			podSet := p.createPodSetForRole(roleSet, role, &i)
			toCreate = append(toCreate, podSet)
			createBudget--
		}
	}
	if _, err = createPodSetsInBatch(ctx, p.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodSetsInBatch(ctx, p.cli, toDelete); err != nil {
		return err
	}
	p.printLog(roleSet, role, toCreate, toDelete)
	return nil
}

func (p *PodSetRoleSyncer) AllReady(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	allPodSets, err := getRolePodSets(ctx, p.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	activePodSets, inactivePodSets := filterActivePodSets(allPodSets)
	if len(inactivePodSets) != 0 {
		return false, nil
	}
	ready, notReady := filterReadyPodSets(activePodSets)
	if len(notReady) != 0 {
		return false, nil
	}
	updated, outdated := filterUpdatedPodSets(ready, p.computeHashFunc(&role.Template, nil))
	if len(outdated) != 0 {
		return false, nil
	}
	slots, toDelete := p.podSetSlotForRole(role, updated)
	if len(toDelete) != 0 {
		return false, nil
	}
	for i := range slots {
		if len(slots[i]) != 1 {
			return false, nil
		}
	}
	return true, nil
}

func (p *PodSetRoleSyncer) CheckCurrentStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, int32, error) {
	allPodSets, err := getRolePodSets(ctx, p.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, 1, err
	}

	stepLength := MaxSurge(role) + MaxUnavailable(role)
	if stepLength < 1 {
		stepLength = 1
	}

	activePodSets, inactivePodSets := filterActivePodSets(allPodSets)
	ready, notReady := filterReadyPodSets(activePodSets)
	_, outdated := filterUpdatedPodSets(ready, p.computeHashFunc(&role.Template, nil))

	slotsActive, toDelete := p.podSetSlotForRole(role, activePodSets)
	updatedSlots, readySlots, outdatedSlots := p.updatedSlotNum(role, allPodSets)
	klog.Infof("[PodSetRoleSyncer.CheckCurrentStep] roleset %s/%s role %s updatedSlots: %d, updatedReadySlots: %d, outdatedSlots: %d",
		roleSet.Namespace, roleSet.Name, role.Name, updatedSlots, readySlots, outdatedSlots)

	// First, determine the current step based on the number of ready slots already updated.
	currentStep := (readySlots-1)/stepLength + int32(1)

	// Then, check whether we've entered the next step. This handles the following cases:
	// 1. Critical boundary where the current step has just finished:
	//    (len(toDelete) == 0 && int32(readySlots) == currentStep*stepLength && len(inactivePodSets) == 0 && len(notReady) == 0)
	// 2. The current step has already finished earlier, but no new slot has become ready yet.
	//    2.1 maxSurge is not allowed, so we terminate old slots first, leading to:
	//        int32(outdatedSlots) < int32(len(slotsActive)) - stepLength * currentStep
	//    2.2 maxUnavailable is not allowed, so we update new slots first, leading to:
	//        int32(updatedSlots) > currentStep * stepLength

	// Q: Why not use updatedSlots and outdatedSlots directly to calculate the current step?
	// A: To detect the critical boundary state precisely. The counts of updated and outdated slots
	//    are not sufficient to determine this — we must rely on the number of *ready* slots.
	if (len(toDelete) == 0 && readySlots == currentStep*stepLength && len(inactivePodSets) == 0 && len(notReady) == 0) ||
		updatedSlots > currentStep*stepLength ||
		outdatedSlots < int32(len(slotsActive))-stepLength*currentStep {
		currentStep++
	}

	allReady := len(toDelete) == 0 &&
		len(inactivePodSets) == 0 &&
		len(notReady) == 0 &&
		len(outdated) == 0 &&
		readySlots == int32(len(slotsActive))

	return allReady, currentStep, nil
}

func (p *PodSetRoleSyncer) printLog(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, toCreate, toDelete []*orchestrationv1alpha1.PodSet) {
	var creationNames, deletionNames []string
	for _, podSet := range toCreate {
		creationNames = append(creationNames, podSet.Name)
	}
	for _, podSet := range toDelete {
		deletionNames = append(deletionNames, podSet.Name)
	}
	klog.Infof("roleset %s/%s role %s, toCreate %v, toDelete %v", roleSet.Namespace, roleSet.Name, role.Name, creationNames, deletionNames)
}

func (p *PodSetRoleSyncer) podSetSlotForRole(role *orchestrationv1alpha1.RoleSpec, activePodSets []*orchestrationv1alpha1.PodSet) (slots [][]*orchestrationv1alpha1.PodSet, toDelete []*orchestrationv1alpha1.PodSet) {
	expectedReplicas := getRoleReplicas(role)
	slots = make([][]*orchestrationv1alpha1.PodSet, expectedReplicas)
	for i := range activePodSets {
		indexStr, ok := activePodSets[i].Labels[constants.RoleReplicaIndexLabelKey]
		if !ok {
			toDelete = append(toDelete, activePodSets[i])
			continue
		}
		index, err := strconv.Atoi(indexStr)
		if err != nil || index < 0 || index >= len(slots) {
			toDelete = append(toDelete, activePodSets[i])
			continue
		}
		slots[index] = append(slots[index], activePodSets[i])
	}
	return
}

func (p *PodSetRoleSyncer) createPodSetForRole(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, roleIndex *int) *orchestrationv1alpha1.PodSet {
	templateHash := p.computeHashFunc(&role.Template, nil)
	podSet := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: roleSet.Namespace,
			Labels: map[string]string{
				// inject pod labels
				constants.RoleSetNameLabelKey:      roleSet.Name,
				constants.RoleNameLabelKey:         role.Name,
				constants.RoleTemplateHashLabelKey: templateHash,
				constants.StormServiceNameLabelKey: roleSet.Labels[constants.StormServiceNameLabelKey],
			},
			Annotations: map[string]string{
				constants.RoleSetIndexAnnotationKey: roleSet.Annotations[constants.RoleSetIndexAnnotationKey],
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)),
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: *role.PodGroupSize,
			Template:     role.Template,
			Stateful:     role.Stateful,
		},
	}

	if roleIndex != nil {
		// add role template hash to pod name, to avoid pod name duplication during rollout
		podSet.Name = fmt.Sprintf("%s-%s-%s-%d", roleSet.Name, role.Name, templateHash, *roleIndex)
	} else {
		podSet.GenerateName = fmt.Sprintf("%s-%s-", roleSet.Name, role.Name)
	}

	// inject pod annotations
	podSet.Annotations[constants.RoleSetIndexAnnotationKey] = roleSet.Annotations[constants.RoleSetIndexAnnotationKey]
	if roleIndex != nil {
		podSet.Annotations[constants.RoleReplicaIndexAnnotationKey] = fmt.Sprintf("%d", *roleIndex)
		// inject to label as well for routing service discovery (some engines use label selector to find pods only)
		podSet.Labels[constants.RoleReplicaIndexLabelKey] = fmt.Sprintf("%d", *roleIndex)
	}

	if roleSet.Spec.SchedulingStrategy.PodGroup != nil {
		podSet.Annotations[constants.GodelPodGroupNameAnnotationKey] = roleSet.Name
	}

	// inject container env
	for i := range podSet.Spec.Template.Spec.Containers {
		injectContainerEnvVars(
			&podSet.Spec.Template.Spec.Containers[i],
			roleSet,
			role,
			roleIndex,
			templateHash,
		)
	}

	return podSet
}

// getRolePodSets returns all PodSets for a specific role
func getRolePodSets(ctx context.Context, cli client.Client, namespace, roleSetName, roleName string) ([]*orchestrationv1alpha1.PodSet, error) {
	roleSetReq, _ := labels.NewRequirement(constants.RoleSetNameLabelKey, selection.Equals, []string{roleSetName})
	roleReq, _ := labels.NewRequirement(constants.RoleNameLabelKey, selection.Equals, []string{roleName})
	labelSelector := labels.NewSelector().Add(*roleSetReq).Add(*roleReq)

	podSetList := &orchestrationv1alpha1.PodSetList{}
	err := cli.List(ctx, podSetList,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labelSelector})
	if err != nil {
		return nil, err
	}

	result := make([]*orchestrationv1alpha1.PodSet, len(podSetList.Items))
	for i := range podSetList.Items {
		result[i] = &podSetList.Items[i]
	}
	return result, nil
}

// filterActivePodSets separates active from inactive PodSets based on deletion timestamp
func filterActivePodSets(allPodSets []*orchestrationv1alpha1.PodSet) ([]*orchestrationv1alpha1.PodSet, []*orchestrationv1alpha1.PodSet) {
	var active, inactive []*orchestrationv1alpha1.PodSet
	for _, podSet := range allPodSets {
		if podSet.DeletionTimestamp == nil && podSet.Status.Phase != orchestrationv1alpha1.PodSetPhaseFailed {
			active = append(active, podSet)
		} else {
			inactive = append(inactive, podSet)
		}
	}
	return active, inactive
}

// filterTerminatingPodSets separates terminating from terminated PodSets
func filterTerminatingPodSets(inactivePodSets []*orchestrationv1alpha1.PodSet) ([]*orchestrationv1alpha1.PodSet, []*orchestrationv1alpha1.PodSet) {
	var terminating, terminated []*orchestrationv1alpha1.PodSet
	for _, podSet := range inactivePodSets {
		// PodSet is terminated if it has deletion timestamp and no finalizers
		if podSet.DeletionTimestamp != nil && len(podSet.Finalizers) == 0 {
			terminated = append(terminated, podSet)
		} else if podSet.DeletionTimestamp != nil {
			terminating = append(terminating, podSet)
		}
	}
	return terminating, terminated
}

// filterReadyPodSets separates ready from not ready PodSets
func filterReadyPodSets(podSets []*orchestrationv1alpha1.PodSet) ([]*orchestrationv1alpha1.PodSet, []*orchestrationv1alpha1.PodSet) {
	var ready, notReady []*orchestrationv1alpha1.PodSet
	for _, podSet := range podSets {
		if isPodSetReady(podSet) {
			ready = append(ready, podSet)
		} else {
			notReady = append(notReady, podSet)
		}
	}
	return ready, notReady
}

// filterUpdatedPodSets separates updated from outdated PodSets based on template hash
func filterUpdatedPodSets(podSets []*orchestrationv1alpha1.PodSet, templateHash string) ([]*orchestrationv1alpha1.PodSet, []*orchestrationv1alpha1.PodSet) {
	var updated, outdated []*orchestrationv1alpha1.PodSet
	for _, podSet := range podSets {
		if podSet.Labels[constants.RoleTemplateHashLabelKey] == templateHash {
			updated = append(updated, podSet)
		} else {
			outdated = append(outdated, podSet)
		}
	}
	return updated, outdated
}

// isPodSetReady checks if a PodSet is ready
func isPodSetReady(podSet *orchestrationv1alpha1.PodSet) bool {
	return podSet.Status.Phase == orchestrationv1alpha1.PodSetPhaseReady
}

// createPodSetsInBatch creates multiple PodSets
func createPodSetsInBatch(ctx context.Context, cli client.Client, podSets []*orchestrationv1alpha1.PodSet) (int, error) {
	for _, podSet := range podSets {
		if err := cli.Create(ctx, podSet); err != nil {
			return 0, err
		}
	}
	return len(podSets), nil
}

// deletePodSetsInBatch deletes multiple PodSets
func deletePodSetsInBatch(ctx context.Context, cli client.Client, podSets []*orchestrationv1alpha1.PodSet) (int, error) {
	for _, podSet := range podSets {
		if err := cli.Delete(ctx, podSet); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
	}
	return len(podSets), nil
}
