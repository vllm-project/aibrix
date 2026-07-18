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
	"context"
	"fmt"
	"time"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	"github.com/vllm-project/aibrix/pkg/controller/util/patch"
)

func (r *StormServiceReconciler) sync(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision *apps.ControllerRevision, updateRevision *apps.ControllerRevision, collisionCount int32) (time.Duration, error) {
	current, err := applyRevision(stormService, currentRevision)
	if err != nil {
		return 0, err
	}

	// reconcile headless service
	if err := r.syncHeadlessService(ctx, stormService); err != nil {
		return 0, fmt.Errorf("sync headless service error %v", err)
	}

	var reconcileErr error
	// 1. reconcile the number of roleSets to meet the spec.Replicas, both currentRevision and updateRevision
	if scaling, err := r.scaling(ctx, stormService, current, currentRevision, updateRevision); err != nil {
		r.EventRecorder.Eventf(stormService, corev1.EventTypeWarning, ScalingEventType, "scaling error %s", err.Error())
		reconcileErr = err
	} else if !stormService.Spec.Paused && !scaling { // skip rollout when paused and in scaling
		// 2. check the rollout progress
		reconcileErr = r.rollout(ctx, stormService, current, currentRevision, updateRevision)
		if reconcileErr != nil {
			r.EventRecorder.Eventf(stormService, corev1.EventTypeWarning, RolloutEventType, "rollout error %s", reconcileErr.Error())
		}
	}
	// 3. update status
	if ready, err := r.updateStatus(ctx, stormService, reconcileErr, currentRevision, updateRevision, collisionCount); err != nil {
		klog.Errorf("failed to update status for stormservice %s/%s, err: %v", stormService.Namespace, stormService.Name, err)
		return 0, err
	} else if !ready {
		return DefaultRequeueAfter, nil
	}
	return 0, nil
}

func (r *StormServiceReconciler) syncHeadlessService(ctx context.Context, service *orchestrationv1alpha1.StormService) error {
	expectedService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			Labels:    service.Labels,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(service, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.StormServiceKind)),
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 map[string]string{constants.StormServiceNameLabelKey: service.Name},
			PublishNotReadyAddresses: true,
		},
	}

	headlessService := &corev1.Service{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: service.Name, Namespace: service.Namespace}, headlessService)
	if err != nil {
		if errors.IsNotFound(err) {
			// service doesn't exist, create it
			if createErr := r.Client.Create(ctx, expectedService); createErr != nil {
				return fmt.Errorf("failed to create headless service: %w", createErr)
			}
			r.EventRecorder.Eventf(service, corev1.EventTypeNormal, HeadlessServiceEventType, "Headless Service(discovery) %s created", service.Name)
			return nil
		}
		return err // Return other errors immediately
	}

	if !isServiceEqual(headlessService, expectedService) {
		headlessService.Spec = expectedService.Spec
		if err := r.Client.Update(ctx, headlessService); err != nil {
			return fmt.Errorf("failed to update headless service: %w", err)
		}
		r.EventRecorder.Eventf(service, corev1.EventTypeNormal, HeadlessServiceEventType, "Headless Service %s updated", service.Name)
	}
	return nil
}

func calculateReplicas(desiredReplica, current, updated int32) (desiredCurrent int32, desiredUpdated int32) {
	if desiredReplica == current+updated {
		return current, updated
	}
	currentTotal := current + updated
	if currentTotal != 0 {
		desiredCurrent = integer.RoundToInt32(float64(current*desiredReplica) / float64(currentTotal))
		desiredUpdated = integer.RoundToInt32(float64(updated*desiredReplica) / float64(currentTotal))
	}
	// 1. if current == updated == 0, scaling out updated
	// 2. overcome the rounding error
	if desiredCurrent+desiredUpdated != desiredReplica {
		desiredUpdated = desiredReplica - desiredCurrent
	}
	return
}

// Scaling: adjust the number of RoleSets to match spec.Replicas. There are several scenarios that may lead to a mismatch:
// 1. The user modifies spec.Replicas, causing too few or too many RoleSets.
// 2. During the rollout process, new RoleSets may be created or old ones deleted.
// 3. RoleSets may be unexpectedly created or deleted.
// Note: Due to the presence of maxUnavailable, this function does not guarantee that the number of RoleSets
// will exactly match spec.Replicas upon return — multiple scaling adjustments may be required.
func (r *StormServiceReconciler) scaling(ctx context.Context, stormService, current *orchestrationv1alpha1.StormService, currentCR, updateCR *apps.ControllerRevision) (bool, error) {
	currentRevision := currentCR.Name
	updatedRevision := updateCR.Name
	var scaling bool
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return false, err
	}
	// skip scaling when there are terminating roleSets
	activeRoleSets, _ := filterTerminatingRoleSets(allRoleSets)
	var expectReplica int32
	if stormService.Spec.Replicas != nil {
		expectReplica = *stormService.Spec.Replicas
	}
	minAvailable := MinAvailable(stormService)
	maxSurge := MaxSurge(stormService)
	diff := len(activeRoleSets) - int(expectReplica)
	if diff < 0 {
		// 1. scale out
		diff = -diff
		if currentRevision == updatedRevision || !stormService.Spec.Paused {
			// 1.1: Two cases fall into this branch:
			// 1.1.1 Only one revision exists, and the number of RoleSets is insufficient — scale out the difference directly.
			// 1.1.2 A rollout is in progress, and some old RoleSets have been deleted — prioritize scaling out the updated revision.
			// Do not exceed maxSurge when scaling out.
			createBudget := utils.MinInt(diff, int(expectReplica+maxSurge-int32(len(allRoleSets))))
			klog.Infof("scaling out stormservice %s/%s, diff: %d, minAvailable: %d, maxSurge: %d, using revision %s, createBudget %d", stormService.Namespace, stormService.Name, diff, minAvailable, maxSurge, updatedRevision, createBudget)
			// Compute per-role revisions for creating new RoleSets
			roleRevisions := computeRoleRevisions(current, stormService, currentCR, updateCR)
			count, err := r.createRoleSet(stormService, createBudget, updatedRevision, roleRevisions)
			if err != nil {
				return false, err
			}
			scaling = scaling || count > 0
			klog.Infof("%s/%s successfully create %d roleSets, revision %s", stormService.Namespace, stormService.Name, count, updatedRevision)
		} else {
			// 1.2: Two revisions exist, and the number of RoleSets is insufficient — scale out both revisions proportionally.
			// TODO: currentRevisionSets may actually contain multiple revisions. For now, we treat them all as currentRevision.
			//       Revisit this logic for finer handling if needed in the future.
			updatedRevisionSets, currentRevisionSets := filterRoleSetByRevision(activeRoleSets, updatedRevision)
			expectCurrentReplica, expectUpdatedReplica := calculateReplicas(expectReplica, stormService.Status.CurrentReplicas, stormService.Status.UpdatedReplicas)
			klog.Infof("scaling out stormservice %s/%s, current revision %s, updated revision %s, currentReplica %d, updatedReplica %d, expectCurrentReplica: %d, expectUpdatedReplica: %d", stormService.Namespace, stormService.Name, currentRevision, updatedRevision, len(currentRevisionSets), len(updatedRevisionSets), expectCurrentReplica, expectUpdatedReplica)
			// For current revision, all roles use currentCR
			currentRoleRevisions := computeRoleRevisions(current, current, currentCR, currentCR)
			currentCreated, err := r.createRoleSet(current, int(expectCurrentReplica)-len(currentRevisionSets), currentRevision, currentRoleRevisions)
			if err != nil {
				return false, err
			}
			// For updated revision, compute per-role revisions
			updatedRoleRevisions := computeRoleRevisions(current, stormService, currentCR, updateCR)
			updatedCreated, err := r.createRoleSet(stormService, int(expectUpdatedReplica)-len(updatedRevisionSets), updatedRevision, updatedRoleRevisions)
			if err != nil {
				return false, err
			}
			scaling = scaling || currentCreated > 0 || updatedCreated > 0
			klog.Infof("%s/%s successfully create %d roleSets in current revision, %d rolesets in updated revision", stormService.Namespace, stormService.Name, currentCreated, updatedCreated)
		}
	} else if diff > 0 {
		// 2. scale in
		if currentRevision == updatedRevision || !stormService.Spec.Paused {
			// 2.1 Two cases fall into this branch:
			// 2.1.1 Only one revision exists and the number of RoleSets exceeds spec.Replicas — scale in by 'diff' based on readiness.
			// 2.1.2 Rollout is in progress and new RoleSets are already created — scale in old RoleSets first based on maxUnavailable.
			ready, _ := filterReadyRoleSets(activeRoleSets)
			readyCount := len(ready)
			klog.Infof("scaling in stormservice %s/%s, diff: %d, minAvailable: %d, maxSurge: %d, currentReady: %d", stormService.Namespace, stormService.Name, diff, minAvailable, maxSurge, readyCount)
			sortRoleSetByRevision(activeRoleSets, updatedRevision)
			var toDelete []*orchestrationv1alpha1.RoleSet
			for i := 0; i < len(activeRoleSets); i++ {
				if diff == 0 {
					break
				}
				if utils.IsRoleSetReady(activeRoleSets[i]) && readyCount <= int(minAvailable) {
					break
				}
				toDelete = append(toDelete, activeRoleSets[i])
				if utils.IsRoleSetReady(activeRoleSets[i]) {
					readyCount--
				}
				diff--
			}
			count, err := r.deleteRoleSet(toDelete)
			if err != nil {
				return false, err
			}
			scaling = scaling || count > 0
			klog.Infof("%s/%s successfully delete %d roleSets", stormService.Namespace, stormService.Name, count)
		} else {
			// 2.2 Two revisions exist and the rollout is paused — scale in both revisions proportionally.
			// 2.2.1 Prioritize deleting notReady RoleSets
			ready, notReady := filterReadyRoleSets(activeRoleSets)
			klog.Infof("scaling in stormservice %s/%s in pause, diff: %d, minAvailable: %d, maxSurge: %d, ready roleSet: %d, not ready roleSets: %d", stormService.Namespace, stormService.Name, diff, minAvailable, maxSurge, len(ready), len(notReady))
			var toDelete []*orchestrationv1alpha1.RoleSet
			if diff <= len(notReady) {
				toDelete = notReady[:diff]
				count, err := r.deleteRoleSet(toDelete)
				if err != nil {
					return false, err
				}
				klog.Infof("%s/%s successfully delete %d roleSets", stormService.Namespace, stormService.Name, count)
				return false, nil
			} else {
				toDelete = append(toDelete, notReady...)
				diff -= len(notReady)
				// TODO: note: diff is not being used in following logic, log a comment for short term, correct me!
				klog.Infof("current diff is %d", diff)
			}
			// 2.2.2 Continue scaling in ready RoleSets proportionally
			updatedReady, currentReady := filterRoleSetByRevision(ready, updatedRevision)
			expectCurrentReplica, expectUpdatedReplica := calculateReplicas(expectReplica, int32(len(currentReady)), int32(len(updatedReady)))
			toDelete = append(toDelete, currentReady[:len(currentReady)-int(expectCurrentReplica)]...)
			toDelete = append(toDelete, updatedReady[:len(updatedReady)-int(expectUpdatedReplica)]...)
			count, err := r.deleteRoleSet(toDelete)
			if err != nil {
				return false, err
			}
			scaling = scaling || count > 0
			klog.Infof("%s/%s successfully delete %d roleSets", stormService.Namespace, stormService.Name, count)
		}
	}
	return scaling, nil
}

// Rollout: execute the deployment update logic with per-role revision tracking
func (r *StormServiceReconciler) rollout(ctx context.Context, stormService, current *orchestrationv1alpha1.StormService, currentCR, updateCR *apps.ControllerRevision) error {
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return err
	}
	var expectReplica int32
	if stormService.Spec.Replicas != nil {
		expectReplica = *stormService.Spec.Replicas
	}
	updated, _ := filterRoleSetByRevision(allRoleSets, updateCR.Name)
	if len(updated) == int(expectReplica) {
		return nil
	}
	switch stormService.Spec.UpdateStrategy.Type {
	case "":
		// By default use RollingUpdate strategy
		fallthrough
	case orchestrationv1alpha1.RollingUpdateStormServiceStrategyType:
		return r.rollingUpdate(allRoleSets, stormService, current, currentCR, updateCR)
	case orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType:
		return r.inPlaceUpdate(allRoleSets, stormService, current, currentCR, updateCR)
	default:
		return fmt.Errorf("unexpected stormService strategy type: %s", stormService.Spec.UpdateStrategy.Type)
	}
}

// rollingUpdate: rolling update logic for replica mode
// The update is triggered by intentionally breaking the condition roleSet count == spec.Replicas,
// introducing controlled disturbances such as:
// 1. Creating new RoleSets with the updated revision
// 2. Deleting old RoleSets with the previous revision
// The entire process adheres to MaxSurge and MaxUnavailable constraints
func (r *StormServiceReconciler) rollingUpdate(allRoleSets []*orchestrationv1alpha1.RoleSet, stormService, current *orchestrationv1alpha1.StormService, currentCR, updateCR *apps.ControllerRevision) error {
	updatedRevision := updateCR.Name
	minAvailable := MinAvailable(stormService)
	activeRoleSets, _ := filterTerminatingRoleSets(allRoleSets)
	ready, _ := filterReadyRoleSets(activeRoleSets)
	readyCount := len(ready)
	maxSurge := MaxSurge(stormService)
	klog.Infof("rolling update for stormservice %s/%s, updatedRevision %s, currReady %d, minAvailable %d, maxSurge %d", stormService.Namespace, stormService.Name, updatedRevision, len(ready), minAvailable, maxSurge)

	// 1. delete outdated roleset, follow the max unavailable rule
	updated, outdated := filterRoleSetByRevision(activeRoleSets, updatedRevision)
	sortRoleSetByRevision(outdated, updatedRevision)
	var toDelete []*orchestrationv1alpha1.RoleSet
	for i := 0; i < len(outdated); i++ {
		if utils.IsRoleSetReady(outdated[i]) && readyCount <= int(minAvailable) {
			break
		}
		toDelete = append(toDelete, outdated[i])
		if utils.IsRoleSetReady(outdated[i]) {
			readyCount--
		}
	}
	_, err := r.deleteRoleSet(toDelete)
	if err != nil {
		return err
	}

	// 2. create roleset, follow the max surge rule
	var expectedReplica int
	if stormService.Spec.Replicas != nil {
		expectedReplica = int(*stormService.Spec.Replicas)
	}
	surge := utils.MinInt(expectedReplica+int(maxSurge)-len(allRoleSets), expectedReplica-len(updated))
	if surge < 0 {
		surge = 0
	}
	// Compute per-role revisions for rolling update (for consistency with InPlaceUpdate)
	roleRevisions := computeRoleRevisions(current, stormService, currentCR, updateCR)
	_, err = r.createRoleSet(stormService, surge, updatedRevision, roleRevisions)
	if err != nil {
		return err
	}
	return nil
}

// inPlaceUpdate: logic for in-place updates in pooled mode with per-role revision tracking
// Propagate changes from the StormService to all associated RoleSets
func (r *StormServiceReconciler) inPlaceUpdate(allRoleSets []*orchestrationv1alpha1.RoleSet, stormService, current *orchestrationv1alpha1.StormService, currentCR, updateCR *apps.ControllerRevision) error {
	// Compute per-role revisions
	roleRevisions := computeRoleRevisions(current, stormService, currentCR, updateCR)

	// Update all RoleSets with per-role revision info
	_, outdated := filterRoleSetByRevision(allRoleSets, updateCR.Name)
	if _, err := r.updateRoleSet(stormService, outdated, updateCR.Name, roleRevisions); err != nil {
		return err
	}
	return nil
}

func (r *StormServiceReconciler) updateStatus(ctx context.Context, stormService *orchestrationv1alpha1.StormService, reconcileErr error, currentRevision *apps.ControllerRevision, updateRevision *apps.ControllerRevision, collisionCount int32) (bool, error) {
	checkpoint := stormService.Status.DeepCopy()

	stormService.Status.ObservedGeneration = stormService.Generation
	stormService.Status.CurrentRevision = currentRevision.Name
	stormService.Status.UpdateRevision = updateRevision.Name
	stormService.Status.CollisionCount = &collisionCount
	if reconcileErr != nil {
		condition := []orchestrationv1alpha1.Condition{
			*utils.NewCondition(orchestrationv1alpha1.StormServiceReplicaFailure, corev1.ConditionTrue, "Failure", reconcileErr.Error()),
		}
		stormService.Status.Conditions = condition
		err := r.Client.Status().Update(ctx, stormService)
		return false, err
	}
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return false, err
	}
	stormService.Status.Replicas = int32(len(allRoleSets))
	stormService.Status.CurrentReplicas = 0
	stormService.Status.UpdatedReplicas = 0
	stormService.Status.UpdatedReadyReplicas = 0
	for _, rs := range allRoleSets {
		if isRoleSetMatchRevision(rs, currentRevision.Name) {
			stormService.Status.CurrentReplicas++
		}
		if isRoleSetMatchRevision(rs, updateRevision.Name) && isAllRoleUpdated(rs) {
			stormService.Status.UpdatedReplicas++
		}
		if isRoleSetMatchRevision(rs, updateRevision.Name) && utils.IsRoleSetReady(rs) && isAllRoleUpdatedAndReady(rs) {
			stormService.Status.UpdatedReadyReplicas++
		}
	}
	if stormService.Status.CurrentReplicas == 0 {
		stormService.Status.CurrentReplicas = stormService.Status.UpdatedReplicas
		stormService.Status.CurrentRevision = stormService.Status.UpdateRevision
	}
	ready, notReady := filterReadyRoleSets(allRoleSets)
	stormService.Status.ReadyReplicas = int32(len(ready))
	stormService.Status.NotReadyReplicas = int32(len(notReady))
	// set conditions
	var specReplica int32
	if stormService.Spec.Replicas != nil {
		specReplica = *stormService.Spec.Replicas
	}
	stormServiceReady := stormService.Status.ReadyReplicas >= specReplica &&
		stormService.Status.UpdatedReplicas == *stormService.Spec.Replicas &&
		stormService.Status.Replicas == *stormService.Spec.Replicas &&
		stormService.Status.CurrentRevision == stormService.Status.UpdateRevision
	if stormServiceReady {
		stormService.Status.Conditions = []orchestrationv1alpha1.Condition{
			*utils.NewCondition(orchestrationv1alpha1.StormServiceReady, corev1.ConditionTrue, "Ready", ""),
		}
	} else {
		stormService.Status.Conditions = []orchestrationv1alpha1.Condition{
			*utils.NewCondition(orchestrationv1alpha1.StormServiceProgressing, corev1.ConditionTrue, "Processing", ""),
		}
	}
	// support scale sub resources.
	// TODO: add pod template hash to avoid errors during upgrade.
	stormService.Status.ScalingTargetSelector = fmt.Sprintf("%s=%s", constants.StormServiceNameLabelKey, stormService.Name)

	// Aggregate role statuses from all RoleSets for both pool and replica modes
	// Updated* fields will only count pods from RoleSets matching the target revision
	stormService.Status.RoleStatuses = aggregateRoleStatuses(allRoleSets, updateRevision.Name)

	if !apiequality.Semantic.DeepEqual(checkpoint, &stormService.Status) {
		err = utils.UpdateStatus(ctx, r.Scheme, r.Client, stormService)
		if err != nil {
			return false, err
		}
	}
	return stormServiceReady, nil
}

func (r *StormServiceReconciler) finalize(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (bool, error) {
	// check if all rolesets are deleted
	allRoleSets, err := r.getRoleSetList(ctx, stormService.Spec.Selector)
	if err != nil {
		return false, err
	}
	if len(allRoleSets) > 0 {
		// delete rolesets
		if _, err := r.deleteRoleSet(allRoleSets); err != nil {
			return false, err
		}
		return false, nil
	}
	// remove finalizer
	if controllerutil.ContainsFinalizer(stormService, StormServiceFinalizer) {
		if err := utils.Patch(ctx, r.Client, stormService, patch.RemoveFinalizerPatch(stormService, StormServiceFinalizer)); err != nil {
			return false, err
		}
	}
	return true, nil
}
