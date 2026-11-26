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
	"strings"
	"time"

	schedv1alpha1 "github.com/kubewharf/godel-scheduler-api/pkg/apis/scheduling/v1alpha1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	schedulerpluginsv1aplha1 "sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
	volcanoschedv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	aibrixconst "github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	controllerdrain "github.com/vllm-project/aibrix/pkg/controller/drain"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	"github.com/vllm-project/aibrix/pkg/controller/util/patch"
)

func (r *RoleSetReconciler) syncPodGroup(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, spec *orchestrationv1alpha1.RoleSetSpec) error {
	if spec.SchedulingStrategy == nil {
		return nil
	}

	podGroupMeta := metav1.ObjectMeta{
		Name:      roleSet.Name,
		Namespace: roleSet.Namespace,
		Labels: map[string]string{
			constants.RoleSetNameLabelKey: roleSet.Name,
		},
		OwnerReferences: []metav1.OwnerReference{
			*metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)),
		},
	}

	if spec.SchedulingStrategy.GodelSchedulingStrategy != nil {
		expectedGroup := &schedv1alpha1.PodGroup{
			ObjectMeta: podGroupMeta,
			Spec:       schedv1alpha1.PodGroupSpec(*spec.SchedulingStrategy.GodelSchedulingStrategy),
		}
		expectedGroup.SetGroupVersionKind(schedv1alpha1.SchemeGroupVersion.WithKind("PodGroup"))
		if synced, err := utils.EnsurePodGroup(ctx, r.DynamicClient, expectedGroup, roleSet.Name, roleSet.Namespace); err != nil {
			return err
		} else if synced {
			r.EventRecorder.Eventf(roleSet, v1.EventTypeNormal, PodGroupSyncedEventType, "pod group %s synced", roleSet.Name)
		}
	}
	if spec.SchedulingStrategy.CoschedulingSchedulingStrategy != nil {
		expectedGroup := &schedulerpluginsv1aplha1.PodGroup{
			ObjectMeta: podGroupMeta,
			Spec:       schedulerpluginsv1aplha1.PodGroupSpec(*spec.SchedulingStrategy.CoschedulingSchedulingStrategy),
		}
		expectedGroup.SetGroupVersionKind(schedulerpluginsv1aplha1.SchemeGroupVersion.WithKind("PodGroup"))
		if synced, err := utils.EnsurePodGroup(ctx, r.DynamicClient, expectedGroup, roleSet.Name, roleSet.Namespace); err != nil {
			return err
		} else if synced {
			r.EventRecorder.Eventf(roleSet, v1.EventTypeNormal, PodGroupSyncedEventType, "pod group %s synced", roleSet.Name)
		}
	}
	if spec.SchedulingStrategy.VolcanoSchedulingStrategy != nil {
		expectedGroup := &volcanoschedv1beta1.PodGroup{
			ObjectMeta: podGroupMeta,
			Spec: volcanoschedv1beta1.PodGroupSpec{
				MinMember:         spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinMember,
				MinTaskMember:     spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinTaskMember,
				Queue:             spec.SchedulingStrategy.VolcanoSchedulingStrategy.Queue,
				PriorityClassName: spec.SchedulingStrategy.VolcanoSchedulingStrategy.PriorityClassName,
				MinResources:      &spec.SchedulingStrategy.VolcanoSchedulingStrategy.MinResources,
			},
		}
		expectedGroup.SetGroupVersionKind(volcanoschedv1beta1.SchemeGroupVersion.WithKind("PodGroup"))
		if synced, err := utils.EnsurePodGroup(ctx, r.DynamicClient, expectedGroup, roleSet.Name, roleSet.Namespace); err != nil {
			return err
		} else if synced {
			r.EventRecorder.Eventf(roleSet, v1.EventTypeNormal, PodGroupSyncedEventType, "pod group %s synced", roleSet.Name)
		}
	}
	return nil
}

func (r *RoleSetReconciler) syncPods(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (controllerdrain.Result, error) {
	var manager RollingManager
	switch roleSet.Spec.UpdateStrategy {
	case orchestrationv1alpha1.SequentialRoleSetStrategyType:
		manager = &RollingManagerSequential{
			cli:      r.Client,
			recorder: r.EventRecorder,
		}
	case orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType:
		manager = &RollingManagerParallel{
			cli:      r.Client,
			recorder: r.EventRecorder,
		}
	case orchestrationv1alpha1.InterleaveRoleSetStrategyType:
		manager = &RollingManagerInterleave{
			cli:      r.Client,
			recorder: r.EventRecorder,
		}
	default:
		manager = &RollingManagerSequential{
			cli:      r.Client,
			recorder: r.EventRecorder,
		}
	}
	return manager.Next(ctx, roleSet)
}

// cleanupStaleRoles deletes RoleSet-owned Pods and PodSets whose role label is not in the current
// Spec.Roles. When a role is removed from a StormService that updates in place -- Pooled mode
// (which forces InPlaceUpdate) or Replica + InPlaceUpdate -- updateRoleSet rewrites the RoleSet
// spec in place, but syncPods (the rolling managers) and status only iterate Spec.Roles, so the
// dropped role's Pods/PodSets are never reconciled away; and the RoleSet is not deleted, so
// owner-reference GC never reclaims them either. They would then consume capacity for the lifetime
// of the StormService. RollingUpdate replaces whole RoleSets and so is unaffected, but the sweep
// is safe there too: it only ever removes objects whose role is absent from the spec.
//
// Removed-role Pods are deleted through the drain path (controllerdrain.DeletePods) with a nil
// drain spec, so the deletion flows through the same path as scale-in and rollout but takes
// effect immediately: the role is gone from the spec and has no replica target to drain toward,
// so in-flight requests to its pods are cut off (no graceful drain). PodSets are removed with a
// plain Delete -- the drain path is pods-only -- and their child Pods are reclaimed by
// owner-reference GC, matching cleanupOrphanPodSets. Objects already being deleted are skipped so
// the sweep does not re-delete or re-log them every reconcile. When it deletes anything it records
// a single StaleRoleCleanup event on the RoleSet, since the immediate-delete and plain-Delete
// paths emit none of their own.
func (r *RoleSetReconciler) cleanupStaleRoles(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) error {
	activeRoles := make(map[string]struct{}, len(roleSet.Spec.Roles))
	for i := range roleSet.Spec.Roles {
		activeRoles[roleSet.Spec.Roles[i].Name] = struct{}{}
	}

	isStale := func(obj client.Object) bool {
		if obj.GetDeletionTimestamp() != nil {
			return false // already being deleted; don't re-delete or re-log it
		}
		role := obj.GetLabels()[constants.RoleNameLabelKey]
		if role == "" {
			return false // never sweep an object with no role label
		}
		if _, ok := activeRoles[role]; ok {
			return false // role is still in the spec
		}
		return isOwnedByRoleSet(obj, roleSet)
	}

	selectorOpt := client.MatchingLabels{constants.RoleSetNameLabelKey: roleSet.Name}
	var errs []error
	removedRoles := map[string]struct{}{}
	podSetsDeleted, podsDeleted := 0, 0

	// PodSets first (Pooled / podGroupSize > 1): deleting a PodSet garbage-collects its child Pods.
	// The drain path is pods-only, so PodSets use a plain Delete, as cleanupOrphanPodSets does.
	podSets := &orchestrationv1alpha1.PodSetList{}
	if err := r.List(ctx, podSets, client.InNamespace(roleSet.Namespace), selectorOpt); err != nil {
		return err
	}
	for i := range podSets.Items {
		ps := &podSets.Items[i]
		if !isStale(ps) {
			continue
		}
		role := ps.Labels[constants.RoleNameLabelKey]
		klog.Infof("[cleanupStaleRoles] deleting podset %s/%s of removed role %q (roleset %s)",
			ps.Namespace, ps.Name, role, roleSet.Name)
		if err := r.Delete(ctx, ps); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		podSetsDeleted++
		removedRoles[role] = struct{}{}
	}

	// Pods (Pod mode, and the direct Pods of a Replica-mode role): route through the drain path
	// with a nil spec (immediate delete), so removed-role pod deletions share the scale-in/rollout
	// path and already-terminating pods are skipped there too.
	pods := &v1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(roleSet.Namespace), selectorOpt); err != nil {
		errs = append(errs, err)
		return utilerrors.NewAggregate(errs)
	}
	var stalePods []*v1.Pod
	var stalePodRoles []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !isStale(pod) {
			continue
		}
		role := pod.Labels[constants.RoleNameLabelKey]
		klog.Infof("[cleanupStaleRoles] deleting pod %s/%s of removed role %q (roleset %s)",
			pod.Namespace, pod.Name, role, roleSet.Name)
		stalePods = append(stalePods, pod)
		stalePodRoles = append(stalePodRoles, role)
	}
	if len(stalePods) > 0 {
		if _, err := controllerdrain.DeletePods(ctx, r.Client, r.EventRecorder, roleSet, stalePods, nil, aibrixconst.PodDrainReasonRoleRemoved, time.Now()); err != nil {
			errs = append(errs, err)
		} else {
			podsDeleted = len(stalePods)
			for _, role := range stalePodRoles {
				removedRoles[role] = struct{}{}
			}
		}
	}

	// Record one event on the RoleSet so the removed-role cleanup is visible. The pods take the
	// immediate-delete drain path (nil spec), which deletes without emitting its own event, and
	// the PodSet deletes are plain Deletes, so without this the objects would vanish silently.
	if podsDeleted > 0 || podSetsDeleted > 0 {
		roles := make([]string, 0, len(removedRoles))
		for role := range removedRoles {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		r.EventRecorder.Eventf(roleSet, v1.EventTypeNormal, StaleRoleCleanupEventType,
			"deleted %d pod(s) and %d podset(s) of removed role(s) %s",
			podsDeleted, podSetsDeleted, strings.Join(roles, ", "))
	}

	return utilerrors.NewAggregate(errs)
}

func (r *RoleSetReconciler) calculateStatus(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, managedErrors []error, podGroupSyncErr error) (*orchestrationv1alpha1.RoleSetStatus, error) {
	newStatus := rs.Status.DeepCopy()
	newStatus.Roles = nil
	var notReadyRoles []string
	for _, role := range rs.Spec.Roles {
		if roleStatus, err := r.calculateStatusForRole(ctx, rs, &role); err != nil {
			// TODO: add into condition
			klog.Warningf("Failed to calculate status for role %s: %v", role.Name, err)
			continue
		} else {
			newStatus.Roles = append(newStatus.Roles, *roleStatus)
			if roleStatus.ReadyReplicas < getRoleReplicas(&role) {
				notReadyRoles = append(notReadyRoles, role.Name)
			}
		}
	}

	if len(notReadyRoles) > 0 {
		notReadyCondition := utils.NewCondition(orchestrationv1alpha1.RoleSetReady, v1.ConditionFalse, "roleset is not ready", fmt.Sprintf("role %s is not ready", strings.Join(notReadyRoles, ",")))
		SetRoleSetCondition(newStatus, *notReadyCondition)
	} else {
		readyCondition := utils.NewCondition(orchestrationv1alpha1.RoleSetReady, v1.ConditionTrue, "roleset is ready", "")
		SetRoleSetCondition(newStatus, *readyCondition)
	}

	failureCond := utils.GetCondition(rs.Status.Conditions, orchestrationv1alpha1.RoleSetReplicaFailure)
	if len(managedErrors) != 0 && failureCond == nil {
		cond := utils.NewCondition(orchestrationv1alpha1.RoleSetReplicaFailure, v1.ConditionTrue, "reconcile roleset error", fmt.Sprintf("%+v", managedErrors))
		SetRoleSetCondition(newStatus, *cond)
	} else if len(managedErrors) == 0 && failureCond != nil {
		RemoveRoleSetCondition(newStatus, orchestrationv1alpha1.RoleSetReplicaFailure)
	}
	// TODO: what if new errors added and failureCond is not nil. can it reflect the new errors?
	if err := r.setVolcanoGangConditions(ctx, rs, newStatus, podGroupSyncErr); err != nil {
		return nil, err
	}
	return newStatus, nil
}

func (r *RoleSetReconciler) calculateStatusForRole(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (*orchestrationv1alpha1.RoleStatus, error) {
	// Check if this role uses PodSet (podGroupSize > 1)
	if role.PodGroupSize != nil && *role.PodGroupSize > 1 {
		// Use PodSet-based status calculation
		roleStatus, err := r.calculateStatusFromPodSets(ctx, rs, role)
		if err != nil {
			klog.Warningf("Failed to get PodSet status for role %s in RoleSet %s/%s: %v", role.Name, rs.Namespace, rs.Name, err)
			// Fall back to zero values
			return nil, err
		}
		return roleStatus, nil
	}

	// Use traditional pod-based status calculation for podGroupSize <= 1
	allPods := &v1.PodList{}
	if err := r.List(ctx, allPods, client.InNamespace(rs.Namespace), client.MatchingLabels{
		constants.RoleSetNameLabelKey: rs.Name,
	}); err != nil {
		return nil, err
	}
	var pods []*v1.Pod
	for i := range allPods.Items {
		pods = append(pods, &allPods.Items[i])
	}
	pods = filterRolePods(role, pods)
	pods, _ = filterActivePods(pods)
	readyReplicas := GetReadyReplicaCountForRole(pods)
	updated, _ := filterUpdatedPods(pods, ctrlutil.ComputeHash(&role.Template, nil))
	updatedReplicas := len(updated)
	updatedReadyReplicas := GetReadyReplicaCountForRole(updated)
	totalReplicas := len(pods)
	notReadyReplicas := totalReplicas - int(readyReplicas)
	return &orchestrationv1alpha1.RoleStatus{
		Name:                 role.Name,
		Replicas:             int32(totalReplicas),
		ReadyReplicas:        readyReplicas,
		NotReadyReplicas:     int32(notReadyReplicas),
		UpdatedReplicas:      int32(updatedReplicas),
		UpdatedReadyReplicas: updatedReadyReplicas,
	}, nil
}

func (r *RoleSetReconciler) calculateStatusFromPodSets(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (*orchestrationv1alpha1.RoleStatus, error) {
	// Get PodSets for this role
	podSetList := &orchestrationv1alpha1.PodSetList{}
	err := r.List(ctx, podSetList, client.InNamespace(rs.Namespace), client.MatchingLabels{
		constants.RoleSetNameLabelKey: rs.Name,
		constants.RoleNameLabelKey:    role.Name,
	})
	if err != nil {
		return nil, err
	}

	var totalReplicas, readyReplicas int32
	currentHash := ctrlutil.ComputeHash(&role.Template, nil)
	var updatedReplicas, updatedReadyReplicas int32

	for _, podSet := range podSetList.Items {
		totalReplicas++
		if isPodSetActive(&podSet) && isPodSetReady(&podSet) {
			readyReplicas++
		}

		// Check if PodSet is updated (has current template hash)
		if podSet.Labels[constants.RoleTemplateHashLabelKey] == currentHash {
			updatedReplicas++
			if isPodSetReady(&podSet) {
				updatedReadyReplicas++
			}
		}
	}

	klog.V(4).Infof("roleName: %s, totalReplicas: %d, readyReplicas: %d, updatedReplicas: %d, updatedReadyReplicas: %d",
		role.Name, totalReplicas, readyReplicas, updatedReplicas, updatedReadyReplicas)
	return &orchestrationv1alpha1.RoleStatus{
		Name:                 role.Name,
		Replicas:             totalReplicas,
		ReadyReplicas:        readyReplicas,
		NotReadyReplicas:     totalReplicas - readyReplicas,
		UpdatedReplicas:      updatedReplicas,
		UpdatedReadyReplicas: updatedReadyReplicas,
	}, nil
}

func (r *RoleSetReconciler) finalize(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (bool, error) {
	selectorOpt := client.MatchingLabels{constants.RoleSetNameLabelKey: roleSet.Name}
	// 1. check if all podsets are delete for podGroupSize > 1
	allPodSets := &orchestrationv1alpha1.PodSetList{}
	if err := r.List(ctx, allPodSets, client.InNamespace(roleSet.Namespace), selectorOpt); err != nil {
		return false, err
	} else if len(allPodSets.Items) != 0 {
		// delete pods
		for i := range allPodSets.Items {
			if err = r.Delete(ctx, &allPodSets.Items[i]); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
		}
		// let's wait for next reconcile to move to next step, it helps make sure the podsets resources are cleaned up.
		return false, nil
	}

	// 2. check if all pods are deleted.
	allPods := &v1.PodList{}
	if err := r.List(ctx, allPods, client.InNamespace(roleSet.Namespace), selectorOpt); err != nil {
		return false, err
	} else if len(allPods.Items) != 0 {
		// delete pods
		for i := range allPods.Items {
			if err = r.Delete(ctx, &allPods.Items[i]); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
		}
		// let's wait for next reconcile to move to next step, it helps make sure the pod resources are cleaned up.
		return false, nil
	}

	// 3. check if pg is deleted
	if err := utils.FinalizePodGroup(ctx, r.DynamicClient, r.Client, &schedv1alpha1.PodGroup{}, roleSet.Name, roleSet.Namespace); err != nil {
		return false, err
	}
	if err := utils.FinalizePodGroup(ctx, r.DynamicClient, r.Client, &schedulerpluginsv1aplha1.PodGroup{}, roleSet.Name, roleSet.Namespace); err != nil {
		return false, err
	}
	if err := utils.FinalizePodGroup(ctx, r.DynamicClient, r.Client, &volcanoschedv1beta1.PodGroup{}, roleSet.Name, roleSet.Namespace); err != nil {
		return false, err
	}

	// 3. remove finalizer
	if controllerutil.ContainsFinalizer(roleSet, RoleSetFinalizer) {
		if err := utils.Patch(ctx, r.Client, roleSet, patch.RemoveFinalizerPatch(roleSet, RoleSetFinalizer)); err != nil {
			klog.Warningf("Failed to remove finalizer for roleSet %s/%s: %v", roleSet.Namespace, roleSet.Name, err)
			return false, err
		}
	}
	return true, nil
}
