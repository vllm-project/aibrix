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
	"strings"

	schedv1alpha1 "github.com/kubewharf/godel-scheduler-api/pkg/apis/scheduling/v1alpha1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	"github.com/vllm-project/aibrix/pkg/controller/util/patch"
)

func (r *RoleSetReconciler) syncPodGroup(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet, spec *orchestrationv1alpha1.RoleSetSpec) error {
	if spec.SchedulingStrategy.PodGroup == nil {
		return nil
	}
	expectedGroup := &schedv1alpha1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleSet.Name,
			Namespace: roleSet.Namespace,
			Labels: map[string]string{
				constants.RoleSetNameLabelKey: roleSet.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(roleSet, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)),
			},
		},
		Spec: *spec.SchedulingStrategy.PodGroup,
	}
	podGroup := &schedv1alpha1.PodGroup{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: roleSet.Name, Namespace: roleSet.Namespace}, podGroup); client.IgnoreNotFound(err) != nil {
		return err
	} else if err != nil {
		// not found pg, need to create
		if err = r.Client.Create(ctx, expectedGroup); err == nil {
			r.EventRecorder.Eventf(roleSet, v1.EventTypeNormal, PodGroupSyncedEventType, "pod group %s synced", roleSet.Name)
		}
		return err
	}
	return nil
}

func (r *RoleSetReconciler) syncPods(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) error {
	var manager RollingManager
	switch roleSet.Spec.UpdateStrategy {
	case orchestrationv1alpha1.SequentialRoleSetStrategyType:
		manager = &RollingManagerSequential{
			cli: r.Client,
		}
	case orchestrationv1alpha1.ParallelRoleSetUpdateStrategyType:
		manager = &RollingManagerParallel{
			cli: r.Client,
		}
	case orchestrationv1alpha1.InterleaveRoleSetStrategyType:
		manager = &RollingManagerInterleave{
			cli: r.Client,
		}
	default:
		manager = &RollingManagerSequential{
			cli: r.Client,
		}
	}
	return manager.Next(ctx, roleSet)
}

func (r *RoleSetReconciler) calculateStatus(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, managedErrors []error) (*orchestrationv1alpha1.RoleSetStatus, error) {
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
			if roleStatus.ReadyReplicas < *role.Replicas {
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
	return newStatus, nil
}

func (r *RoleSetReconciler) calculateStatusForRole(ctx context.Context, rs *orchestrationv1alpha1.RoleSet, role *orchestrationv1alpha1.RoleSpec) (*orchestrationv1alpha1.RoleStatus, error) {
	// collect pods of role
	roleSetRequirement, _ := labels.NewRequirement(constants.RoleSetNameLabelKey, selection.Equals, []string{rs.Name})
	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*roleSetRequirement)
	allPods := &v1.PodList{}
	if err := r.Client.List(context.Background(), allPods,
		client.InNamespace(rs.Namespace),
		client.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
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

func (r *RoleSetReconciler) finalize(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (bool, error) {
	// 1. check if all pods are deleted
	roleSetRequirement, _ := labels.NewRequirement(constants.RoleSetNameLabelKey, selection.Equals, []string{roleSet.Name})
	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*roleSetRequirement)
	allPods := &v1.PodList{}
	if err := r.Client.List(ctx, allPods,
		client.InNamespace(roleSet.Namespace),
		client.MatchingLabelsSelector{Selector: labelSelector}); err != nil {
		return false, err
	} else if len(allPods.Items) != 0 {
		// delete pods
		for i := range allPods.Items {
			if err = r.Client.Delete(ctx, &allPods.Items[i]); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return false, err
			}
		}
		// let's wait for next reconcile to move to next step, it helps make sure the pod resources are cleaned up.
		return false, nil
	}

	// TODO: temporarily disable pod group
	// 2. check if pg is deleted
	//podGroup := &schedv1alpha1.PodGroup{}
	//if err := r.Client.Get(ctx, client.ObjectKey{Name: roleSet.Name, Namespace: roleSet.Namespace}, podGroup); client.IgnoreNotFound(err) != nil {
	//	return false, err
	//} else if err == nil {
	//	// delete pg
	//	if err = r.Client.Delete(ctx, podGroup); err != nil {
	//		return false, err
	//	}
	//	return false, nil
	//}

	// 3. remove finalizer
	if controllerutil.ContainsFinalizer(roleSet, RoleSetFinalizer) {
		if err := utils.Patch(ctx, r.Client, roleSet, patch.RemoveFinalizerPatch(roleSet, RoleSetFinalizer)); err != nil {
			klog.Warningf("Failed to remove finalizer for roleSet %s/%s: %v", roleSet.Namespace, roleSet.Name, err)
			return false, err
		}
	}
	return true, nil
}
