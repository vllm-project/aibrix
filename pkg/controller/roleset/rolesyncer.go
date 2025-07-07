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
	"strconv"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

type RoleRollingSyncer interface {
	Scale(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error)
	Rollout(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) error
	RolloutByStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, currentStep int32) error
	AllReady(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error)
	CheckCurrentStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, int32, error)
}

type StatefulRoleSyncer struct {
	cli client.Client
}

func (s *StatefulRoleSyncer) Scale(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	var podsToCreate, podsToDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	// delete pods that are in terminated state
	activePods, inactivePods := filterActivePods(allPods)
	terminatingPods, terminatedPods := filterTerminatingPods(inactivePods)
	podsToDelete = append(podsToDelete, terminatedPods...)

	// delete pods that cannot find the corresponding slot
	slots, toDelete := s.podSlotForRole(role, activePods)
	podsToDelete = append(podsToDelete, toDelete...)
	createBudget := int32(len(slots)) + MaxSurge(role) - int32(len(activePods)) - int32(len(terminatingPods))
	// check pods for each slot
	for i := range slots {
		if len(slots[i]) == 0 {
			if createBudget <= 0 {
				continue
			}
			pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
			if err != nil {
				return false, err
			}
			renderStormServicePod(roleSet, role, pod, &i)
			podsToCreate = append(podsToCreate, pod)
			createBudget--
		} else if len(slots[i]) > 1 {
			readyPods, notReadyPods := filterReadyPods(slots[i])
			updatedReadyPods, outdatedReadyPods := filterUpdatedPods(readyPods, ctrlutil.ComputeHash(&role.Template, nil))
			updatedNotReadyPods, outdatedNotReadyPods := filterUpdatedPods(notReadyPods, ctrlutil.ComputeHash(&role.Template, nil))
			podsToDelete = append(podsToDelete, outdatedNotReadyPods...)
			if len(updatedReadyPods) > 0 {
				// only keep 1 updated ready pod for each slot
				podsToDelete = append(podsToDelete, outdatedReadyPods...)
				podsToDelete = append(podsToDelete, updatedNotReadyPods...)
				if len(updatedReadyPods) > 1 {
					podsToDelete = append(podsToDelete, updatedReadyPods[1:]...)
				}
			} else {
				// keep 1 updated not ready pod & 1 outdated ready pod for each slot
				if len(outdatedReadyPods) > 1 {
					podsToDelete = append(podsToDelete, outdatedReadyPods[1:]...)
				}
				if len(updatedNotReadyPods) > 1 {
					podsToDelete = append(podsToDelete, updatedNotReadyPods[1:]...)
				}
			}
		}
	}
	if _, err = createPodsInBatch(ctx, s.cli, podsToCreate); err != nil {
		return false, err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, podsToDelete); err != nil {
		return false, err
	}
	s.printLog(roleSet, role, podsToCreate, podsToDelete)
	return len(podsToCreate) > 0 || len(podsToDelete) > 0, nil
}

func (s *StatefulRoleSyncer) readySlotNum(role *orchestrationv1alpha1.RoleSpec, allPods []*v1.Pod) int {
	activePods, _ := filterActivePods(allPods)
	slots, _ := s.podSlotForRole(role, activePods)
	var result int
	for i := range slots {
		ready, _ := filterReadyPods(slots[i])
		if len(ready) >= 1 {
			result++
		}
	}
	return result
}

func (s *StatefulRoleSyncer) updatedSlotNum(role *orchestrationv1alpha1.RoleSpec, allPods []*v1.Pod) (int32, int32, int32) {
	activePods, _ := filterActivePods(allPods)
	slots, _ := s.podSlotForRole(role, activePods)
	currentHash := ctrlutil.ComputeHash(&role.Template, nil)

	updatedTotal := 0
	updatedReadyTotal := 0
	outdatedTotal := 0
	for i := range slots {
		// Consider the slot updated if it contains any Pod with the new version
		hasNewVersion := false
		for _, pod := range slots[i] {
			if pod.Labels[constants.RoleTemplateHashLabelKey] == currentHash {
				hasNewVersion = true
				break
			}
		}
		if hasNewVersion {
			updatedTotal++
			if len(slots[i]) == 1 && podutil.IsPodReady(slots[i][0]) {
				updatedReadyTotal++
			}
		} else if len(slots[i]) > 0 {
			outdatedTotal++
		}
	}
	return int32(updatedTotal), int32(updatedReadyTotal), int32(outdatedTotal)
}

func (s *StatefulRoleSyncer) Rollout(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) error {
	var toCreate, toDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}
	expectedReplicas := getRoleReplicas(role)
	activePods, _ := filterActivePods(allPods)
	readySlotNum := s.readySlotNum(role, allPods)
	deleteBudget := int32(readySlotNum) - expectedReplicas + MaxUnavailable(role)
	createBudget := expectedReplicas + MaxSurge(role) - int32(len(allPods))
	klog.Infof("[StatefulRoleSyncer.Rollout] roleset %s/%s role %s expectedReplicas %d, deleteBudget %d, createBudget %d, template hash %s", roleSet.Namespace, roleSet.Name, role.Name, expectedReplicas, deleteBudget, createBudget, ctrlutil.ComputeHash(&role.Template, nil))

	slots, _ := s.podSlotForRole(role, activePods)
	for i := range slots {
		if len(slots[i]) != 1 {
			// wait for scale to handle this slot
			continue
		}
		if slots[i][0].Labels[constants.RoleTemplateHashLabelKey] == ctrlutil.ComputeHash(&role.Template, nil) {
			continue
		}
		if !podutil.IsPodReady(slots[i][0]) {
			toDelete = append(toDelete, slots[i][0])
			continue
		}
		if deleteBudget > 0 {
			toDelete = append(toDelete, slots[i][0])
			deleteBudget--
		} else if createBudget > 0 {
			pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
			if err != nil {
				return err
			}
			renderStormServicePod(roleSet, role, pod, &i)
			toCreate = append(toCreate, pod)
			createBudget--
		}
	}
	if _, err = createPodsInBatch(ctx, s.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, toDelete); err != nil {
		return err
	}
	s.printLog(roleSet, role, toCreate, toDelete)
	return nil
}

func (s *StatefulRoleSyncer) RolloutByStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, currentStep int32) error {
	var toCreate, toDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}

	expectedReplicas := getRoleReplicas(role)
	expectedUpdatedReplicas := utils.MinInt32((MaxSurge(role)+MaxUnavailable(role))*currentStep, expectedReplicas)
	activePods, _ := filterActivePods(allPods)
	readySlotNum := s.readySlotNum(role, allPods)
	deleteBudget := int32(readySlotNum) - expectedReplicas + MaxUnavailable(role)
	createBudget := expectedReplicas + MaxSurge(role) - int32(len(allPods))
	klog.Infof("[StatefulRoleSyncer.RolloutByStep] Step %d: roleset %s/%s role %s expectedReplicas %d, deleteBudget %d, createBudget %d, template hash %s", currentStep, roleSet.Namespace, roleSet.Name, role.Name, expectedReplicas, deleteBudget, createBudget, ctrlutil.ComputeHash(&role.Template, nil))

	updatedTotal, _, outdatedTotal := s.updatedSlotNum(role, allPods)

	// Constraints for this step:
	// By the end of the current step, we aim to have (expectedReplicas - expectedUpdatedReplicas) outdated Pods
	// and expectedUpdatedReplicas updated Pods.
	// Therefore, the create and delete budgets must not exceed the difference between the current and expected states.
	createBudget = utils.MinInt32(createBudget, expectedUpdatedReplicas-updatedTotal)
	deleteBudget = utils.MinInt32(deleteBudget, outdatedTotal-expectedReplicas+expectedUpdatedReplicas)

	klog.Infof("[StatefulRoleSyncer.RolloutByStep] Step %d: roleset %s/%s role %s expectedUpdatedReplicas %d, updatedTotal %d, outdatedTotal %d, deleteBudget %d, createBudget %d",
		currentStep, roleSet.Namespace, roleSet.Name, role.Name, expectedUpdatedReplicas, updatedTotal, outdatedTotal, deleteBudget, createBudget)
	slots, _ := s.podSlotForRole(role, activePods)
	for i := range slots {
		if len(slots[i]) != 1 {
			// wait for scale to handle this slot
			continue
		}
		if slots[i][0].Labels[constants.RoleTemplateHashLabelKey] == ctrlutil.ComputeHash(&role.Template, nil) {
			continue
		}
		if !podutil.IsPodReady(slots[i][0]) {
			toDelete = append(toDelete, slots[i][0])
			continue
		}
		if deleteBudget > 0 {
			toDelete = append(toDelete, slots[i][0])
			deleteBudget--
		} else if createBudget > 0 {
			pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
			if err != nil {
				return err
			}
			renderStormServicePod(roleSet, role, pod, &i)
			toCreate = append(toCreate, pod)
			createBudget--
		}
	}
	if _, err = createPodsInBatch(ctx, s.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, toDelete); err != nil {
		return err
	}
	s.printLog(roleSet, role, toCreate, toDelete)
	return nil
}

func (s *StatefulRoleSyncer) AllReady(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	activePods, inactivePods := filterActivePods(allPods)
	if len(inactivePods) != 0 {
		return false, nil
	}
	ready, notReady := filterReadyPods(activePods)
	if len(notReady) != 0 {
		return false, nil
	}
	updated, outdated := filterUpdatedPods(ready, ctrlutil.ComputeHash(&role.Template, nil))
	if len(outdated) != 0 {
		return false, nil
	}
	slots, toDelete := s.podSlotForRole(role, updated)
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

func (s *StatefulRoleSyncer) CheckCurrentStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, int32, error) {
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, 1, err
	}

	stepLength := MaxSurge(role) + MaxUnavailable(role)
	if stepLength < 1 {
		stepLength = 1
	}

	activePods, inactivePods := filterActivePods(allPods)
	ready, notReady := filterReadyPods(activePods)
	_, outdated := filterUpdatedPods(ready, ctrlutil.ComputeHash(&role.Template, nil))

	slotsActive, toDelete := s.podSlotForRole(role, activePods)
	updatedSlots, readySlots, outdatedSlots := s.updatedSlotNum(role, activePods)
	klog.Infof("[StatefulRoleSyncer.CheckCurrentStep] roleset %s/%s role %s updatedSlots: %d, updatedReadySlots: %d, outdatedSlots: %d",
		roleSet.Namespace, roleSet.Name, role.Name, updatedSlots, readySlots, outdatedSlots)

	// First, determine the current step based on the number of ready slots already updated.
	currentStep := (readySlots-1)/stepLength + int32(1)

	// Then, check whether we've entered the next step. This handles the following cases:
	// 1. Critical boundary where the current step has just finished:
	//    (len(toDelete) == 0 && int32(readySlots) == currentStep*stepLength && len(inactivePods) == 0 && len(notReady) == 0)
	// 2. The current step has already finished earlier, but no new slot has become ready yet.
	//    2.1 maxSurge is not allowed, so we terminate old slots first, leading to:
	//        int32(outdatedSlots) < int32(len(slotsActive)) - stepLength * currentStep
	//    2.2 maxUnavailable is not allowed, so we update new slots first, leading to:
	//        int32(updatedSlots) > currentStep * stepLength

	// Q: Why not use updatedSlots and outdatedSlots directly to calculate the current step?
	// A: To detect the critical boundary state precisely. The counts of updated and outdated slots
	//    are not sufficient to determine this — we must rely on the number of *ready* slots.
	if (len(toDelete) == 0 && readySlots == currentStep*stepLength && len(inactivePods) == 0 && len(notReady) == 0) ||
		updatedSlots > currentStep*stepLength ||
		outdatedSlots < int32(len(slotsActive))-stepLength*currentStep {
		currentStep++
	}

	allReady := len(toDelete) == 0 &&
		len(inactivePods) == 0 &&
		len(notReady) == 0 &&
		len(outdated) == 0 &&
		readySlots == int32(len(slotsActive))

	return allReady, currentStep, nil
}

func (s *StatefulRoleSyncer) printLog(roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, toCreate, toDelete []*v1.Pod) {
	var creationNames, deletionNames []string
	for _, pod := range toCreate {
		creationNames = append(creationNames, pod.Name)
	}
	for _, pod := range toDelete {
		deletionNames = append(deletionNames, pod.Name)
	}
	klog.Infof("roleset %s/%s role %s, toCreate %v, toDelete %v", roleSet.Namespace, roleSet.Name, role.Name, creationNames, deletionNames)
}

func (s *StatefulRoleSyncer) podSlotForRole(role *orchestrationv1alpha1.RoleSpec, activePods []*v1.Pod) (slots [][]*v1.Pod, toDelete []*v1.Pod) {
	expectedReplicas := getRoleReplicas(role)
	slots = make([][]*v1.Pod, expectedReplicas)
	for i := range activePods {
		indexStr, ok := activePods[i].Annotations[constants.RoleReplicaIndexAnnotationKey]
		if !ok {
			toDelete = append(toDelete, activePods[i])
			continue
		}
		index, err := strconv.Atoi(indexStr)
		if err != nil || index < 0 || index >= len(slots) {
			toDelete = append(toDelete, activePods[i])
			continue
		}
		slots[index] = append(slots[index], activePods[i])
	}
	return
}

type StatelessRoleSyncer struct {
	cli client.Client
}

func (s *StatelessRoleSyncer) Scale(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	var toCreate, toDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	// delete pods that are in terminated state
	activePods, inactivePods := filterActivePods(allPods)
	_, terminatedPods := filterTerminatingPods(inactivePods)
	toDelete = append(toDelete, terminatedPods...)

	// reconcile active pods to meet expected replicas
	expectedReplicas := getRoleReplicas(role)
	diff := len(activePods) - int(expectedReplicas)
	if diff > 0 {
		sortPodsByTemplateHash(activePods, ctrlutil.ComputeHash(&role.Template, nil))
		readyPods, _ := filterReadyPods(activePods)
		readyCount := len(readyPods)
		minAvailable := expectedReplicas - MaxUnavailable(role)
		klog.Infof("[StatelessRoleSyncer.Scale] roleset %s/%s role %s readyPods %d, expectedReplicas %d, minAvailable %d, deleting pods...", roleSet.Namespace, roleSet.Name, role.Name, len(readyPods), expectedReplicas, minAvailable)
		for i := 0; i < len(activePods); i++ {
			if diff == 0 {
				break
			}
			if podutil.IsPodReady(activePods[i]) && readyCount <= int(minAvailable) {
				break
			}
			toDelete = append(toDelete, activePods[i])
			if podutil.IsPodReady(activePods[i]) {
				readyCount--
			}
			diff--
		}
		klog.Infof("[StatelessRoleSyncer.Scale] roleset %s/%s role %s toDelete %d pods", roleSet.Namespace, roleSet.Name, role.Name, len(toDelete))
	} else if diff < 0 {
		klog.Infof("[StatelessRoleSyncer.Scale] roleset %s/%s role %s activePods %d, expectedReplicas %d, creating pods...", roleSet.Namespace, roleSet.Name, role.Name, len(activePods), expectedReplicas)
		terminatingPods, _ := filterTerminatingPods(allPods)
		terminatingPodCount := len(terminatingPods)
		// take pods that are in terminating state into account
		createBudget := utils.MinInt32(int32(-diff), expectedReplicas+MaxSurge(role)-int32(len(activePods))-int32(terminatingPodCount))
		for i := int32(0); i < createBudget; i++ {
			pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
			if err != nil {
				return false, err
			}
			renderStormServicePod(roleSet, role, pod, nil)
			toCreate = append(toCreate, pod)
		}
		klog.Infof("[StatelessRoleSyncer.Scale] roleset %s/%s role %s toCreate %d pods", roleSet.Namespace, roleSet.Name, role.Name, len(toCreate))
	}
	if _, err = createPodsInBatch(ctx, s.cli, toCreate); err != nil {
		return false, err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, toDelete); err != nil {
		return false, err
	}
	return len(toCreate) > 0 || len(toDelete) > 0, nil
}

func (s *StatelessRoleSyncer) Rollout(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) error {
	var toCreate, toDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}
	activePods, _ := filterActivePods(allPods)
	expectedReplicas := getRoleReplicas(role)
	updated, outdated := filterUpdatedPods(activePods, ctrlutil.ComputeHash(&role.Template, nil))
	klog.Infof("[StatelessRoleSyncer.Rollout] roleset %s/%s role %s updated %d, outdated %d, expectedReplicas %d, hash %s", roleSet.Namespace, roleSet.Name, role.Name, len(updated), len(outdated), expectedReplicas, ctrlutil.ComputeHash(&role.Template, nil))
	ready, _ := filterReadyPods(activePods)
	deleteBudget := int32(len(ready)) - expectedReplicas + MaxUnavailable(role)
	sortPodsByActive(outdated)
	// 1. delete outdated pods
	for i := 0; i < len(outdated); i++ {
		if podutil.IsPodReady(outdated[i]) {
			if deleteBudget <= 0 {
				break
			}
			deleteBudget--
		}
		toDelete = append(toDelete, outdated[i])
	}
	// 2. created new pods
	terminatingPods, _ := filterTerminatingPods(allPods)
	terminatingPodCount := len(terminatingPods)
	// take terminating pods into account
	createBudget := utils.MinInt32(expectedReplicas+MaxSurge(role)-int32(len(activePods))-int32(terminatingPodCount), expectedReplicas-int32(len(updated)))
	for i := int32(0); i < createBudget; i++ {
		pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
		if err != nil {
			return err
		}
		renderStormServicePod(roleSet, role, pod, nil)
		toCreate = append(toCreate, pod)
	}
	klog.Infof("[StatelessRoleSyncer.Rollout] roleset %s/%s outdated %d, expectedReplicas %d, deleteBudget %d, createBudget %d, allPods %d, toDelete %d, toCreate %d", roleSet.Namespace, roleSet.Name, len(outdated), expectedReplicas, deleteBudget, createBudget, len(allPods), len(toDelete), len(toCreate))
	if _, err = createPodsInBatch(ctx, s.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, toDelete); err != nil {
		return err
	}
	return nil
}

// RolloutByStep performs rollout in steps based on the defined step size
func (s *StatelessRoleSyncer) RolloutByStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec, currentStep int32) error {
	var toCreate, toDelete []*v1.Pod
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return err
	}

	activePods, _ := filterActivePods(allPods)
	expectedReplicas := getRoleReplicas(role)
	// Calculate the expected number of updated Pods based on the step size
	expectedUpdatedReplicas := utils.MinInt32((MaxSurge(role)+MaxUnavailable(role))*currentStep, expectedReplicas)
	updated, outdated := filterUpdatedPods(activePods, ctrlutil.ComputeHash(&role.Template, nil))
	klog.Infof("[StatelessRoleSyncer.RolloutByStep] Step %d: roleset %s/%s role %s updated %d, outdated %d, expectedReplicas %d, expectedUpdatedReplicas %d, hash %s", currentStep, roleSet.Namespace, roleSet.Name, role.Name, len(updated), len(outdated), expectedReplicas, expectedUpdatedReplicas, ctrlutil.ComputeHash(&role.Template, nil))
	if int32(len(updated)) >= expectedUpdatedReplicas {
		return nil
	}

	ready, _ := filterReadyPods(activePods)
	// Calculate the number of Pods that can be safely deleted, considering both step constraints and maxUnavailable:
	// - Step constraint: By the end of this step, we expect (expectedReplicas - expectedUpdatedReplicas) outdated Pods,
	//   so we must avoid deleting too many.
	deleteBudget := utils.MinInt32(int32(len(outdated))-expectedReplicas+expectedUpdatedReplicas, int32(len(ready))-expectedReplicas+MaxUnavailable(role))

	sortPodsByActive(outdated)
	// 1. delete outdated pods
	for i := 0; i < len(outdated); i++ {
		if podutil.IsPodReady(outdated[i]) {
			if deleteBudget <= 0 {
				break
			}
			deleteBudget--
		}
		toDelete = append(toDelete, outdated[i])
	}
	// 2. created new pods
	terminatingPods, _ := filterTerminatingPods(allPods)
	terminatingPodCount := len(terminatingPods)
	// take terminating pods into account
	// Calculate how many Pods can be created, considering both step constraints and maxSurge:
	// - Step constraint: By the end of this step, we aim to have expectedUpdatedReplicas new Pods,
	//   so we must avoid creating more than necessary.
	createBudget := utils.MinInt32(expectedReplicas+MaxSurge(role)-int32(len(activePods))-int32(terminatingPodCount), expectedUpdatedReplicas-int32(len(updated)))
	for i := int32(0); i < createBudget; i++ {
		pod, err := ctrlutil.GetPodFromTemplate(&role.Template, roleSet, metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)))
		if err != nil {
			return err
		}
		renderStormServicePod(roleSet, role, pod, nil)
		toCreate = append(toCreate, pod)
	}
	klog.Infof("[StatelessRoleSyncer.RolloutByStep] Step %d: roleset %s/%s outdated %d, expectedReplicas %d, expectedUpdatedReplicas %d, deleteBudget %d, createBudget %d, allPods %d, toDelete %d, toCreate %d", currentStep, roleSet.Namespace, roleSet.Name, len(outdated), expectedReplicas, expectedUpdatedReplicas, deleteBudget, createBudget, len(allPods), len(toDelete), len(toCreate))
	if _, err = createPodsInBatch(ctx, s.cli, toCreate); err != nil {
		return err
	}
	if _, err = deletePodsInBatch(ctx, s.cli, toDelete); err != nil {
		return err
	}
	return nil
}

func (s *StatelessRoleSyncer) AllReady(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, error) {
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, err
	}
	activePods, inactivePods := filterActivePods(allPods)
	if len(inactivePods) != 0 {
		return false, nil
	}
	ready, notReady := filterReadyPods(activePods)
	if len(notReady) != 0 {
		return false, nil
	}
	updated, outdated := filterUpdatedPods(ready, ctrlutil.ComputeHash(&role.Template, nil))
	if len(outdated) != 0 {
		return false, nil
	}
	expectedReplicas := getRoleReplicas(role)
	return len(updated) == int(expectedReplicas), nil
}

// CheckCurrentStep determines which step the current role is in
func (s *StatelessRoleSyncer) CheckCurrentStep(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (bool, int32, error) {
	allPods, err := getRolePods(ctx, s.cli, roleSet.Namespace, roleSet.Name, role.Name)
	if err != nil {
		return false, 1, err
	}

	// The step size defaults to MaxSurge(role) + MaxUnavailable(role)
	stepLength := MaxSurge(role) + MaxUnavailable(role)
	if stepLength < 1 {
		stepLength = 1
	}

	activePods, inactivePods := filterActivePods(allPods)
	ready, notReady := filterReadyPods(activePods)

	// 'updated' contains ready Pods with the new version, while 'updatedActive' includes all new Pods regardless of readiness
	updated, outdated := filterUpdatedPods(ready, ctrlutil.ComputeHash(&role.Template, nil))
	updatedActive, outdatedActive := filterUpdatedPods(activePods, ctrlutil.ComputeHash(&role.Template, nil))
	klog.Infof("[StatelessRoleSyncer.CheckCurrentStep] roleset %s/%s role %s updatedReadyTotal: %d, outdatedReadyTotal: %d, updatedTotal: %d, outdatedTotal: %d",
		roleSet.Namespace, roleSet.Name, role.Name, len(updated), len(outdated), len(updatedActive), len(outdatedActive))

	expectedReplicas := getRoleReplicas(role)

	// First, determine the minimum current step based on the number of ready Pods with the updated version
	currentStep := int32(len(updated)-1)/stepLength + int32(1)

	// Then, consider whether we have already entered the next step. Handle the following cases:
	// 1. Critical boundary where the current step is just completed:
	//    (len(inactivePods) == 0 && len(notReady) == 0 && int32(len(updated)) == stepLength * currentStep)
	// 2. The current step was already completed earlier, but no new Pod has become ready yet:
	//    2.1 maxSurge is not allowed, so old Pods must be deleted first, resulting in:
	//        int32(len(outdatedActive)) < expectedReplicas - stepLength * currentStep
	//    2.2 maxUnavailable is not allowed, so new Pods are updated first, resulting in:
	//        int32(len(updatedActive)) > stepLength * currentStep

	// Q: Why not use updatedActive and outdatedActive directly to determine the current step?
	// A: To detect the exact boundary where the step has just completed.
	//    The counts of updatedActive and outdatedActive alone are not sufficient to identify this state;
	//    we must rely on the number of ready updated Pods.
	if (len(inactivePods) == 0 && len(notReady) == 0 && int32(len(updated)) == stepLength*currentStep) ||
		int32(len(updatedActive)) > stepLength*currentStep ||
		int32(len(outdatedActive)) < expectedReplicas-stepLength*currentStep {
		currentStep++
	}

	allReady := len(inactivePods) == 0 &&
		len(notReady) == 0 &&
		len(outdated) == 0 &&
		len(updated) >= int(expectedReplicas)

	return allReady, currentStep, nil
}

func GetRoleSyncer(cli client.Client, role *orchestrationv1alpha1.RoleSpec) RoleRollingSyncer {
	if role.Stateful {
		return &StatefulRoleSyncer{
			cli: cli,
		}
	}
	return &StatelessRoleSyncer{
		cli: cli,
	}
}
