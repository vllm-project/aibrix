/*
Copyright 2024 The Aibrix Team.

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

package rayclusterfleet

import (
	"context"
	"fmt"
	"reflect"
	"time"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/rayclusterfleet/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

// syncRolloutStatus updates the status of a deployment during a rollout. There are
// cases this helper will run that cannot be prevented from the scaling detection,
// for example a resync of the deployment after it was scaled up. In those cases,
// we shouldn't try to estimate any progress.
func (r *RayClusterFleetReconciler) syncRolloutStatus(ctx context.Context, allRSs []*orchestrationv1alpha1.RayClusterReplicaSet, newRS *orchestrationv1alpha1.RayClusterReplicaSet, d *orchestrationv1alpha1.RayClusterFleet) (ctrl.Result, error) {
	newStatus := calculateStatus(allRSs, newRS, d)

	// If there is no progressDeadlineSeconds set, remove any Progressing condition.
	if !util.HasProgressDeadline(d) {
		util.RemoveDeploymentCondition(&newStatus, orchestrationv1alpha1.RayClusterFleetProgressing)
	}

	// If there is only one replica set that is active then that means we are not running
	// a new rollout and this is a resync where we don't need to estimate any progress.
	// In such a case, we should simply not estimate any progress for this deployment.
	currentCond := util.GetDeploymentCondition(d.Status, orchestrationv1alpha1.RayClusterFleetProgressing)
	isCompleteDeployment := newStatus.Replicas == newStatus.UpdatedReplicas && currentCond != nil && currentCond.Reason == util.NewRSAvailableReason
	// Check for progress only if there is a progress deadline set and the latest rollout
	// hasn't completed yet.
	if util.HasProgressDeadline(d) && !isCompleteDeployment {
		switch {
		case util.DeploymentComplete(d, &newStatus):
			// Update the deployment conditions with a message for the new replica set that
			// was successfully deployed. If the condition already exists, we ignore this update.
			msg := fmt.Sprintf("Deployment %q has successfully progressed.", d.Name)
			if newRS != nil {
				msg = fmt.Sprintf("ReplicaSet %q has successfully progressed.", newRS.Name)
			}
			condition := util.NewDeploymentCondition(orchestrationv1alpha1.RayClusterFleetProgressing, corev1.ConditionTrue, util.NewRSAvailableReason, msg)
			util.SetDeploymentCondition(&newStatus, *condition)

		case util.DeploymentProgressing(d, &newStatus):
			// If there is any progress made, continue by not checking if the deployment failed. This
			// behavior emulates the rolling updater progressDeadline check.
			msg := fmt.Sprintf("Deployment %q is progressing.", d.Name)
			if newRS != nil {
				msg = fmt.Sprintf("ReplicaSet %q is progressing.", newRS.Name)
			}
			condition := util.NewDeploymentCondition(orchestrationv1alpha1.RayClusterFleetProgressing, corev1.ConditionTrue, util.ReplicaSetUpdatedReason, msg)
			// Update the current Progressing condition or add a new one if it doesn't exist.
			// If a Progressing condition with status=true already exists, we should update
			// everything but lastTransitionTime. SetDeploymentCondition already does that but
			// it also is not updating conditions when the reason of the new condition is the
			// same as the old. The Progressing condition is a special case because we want to
			// update with the same reason and change just lastUpdateTime iff we notice any
			// progress. That's why we handle it here.
			if currentCond != nil {
				if currentCond.Status == corev1.ConditionTrue {
					condition.LastTransitionTime = currentCond.LastTransitionTime
				}
				util.RemoveDeploymentCondition(&newStatus, orchestrationv1alpha1.RayClusterFleetProgressing)
			}
			util.SetDeploymentCondition(&newStatus, *condition)

		case util.DeploymentTimedOut(ctx, d, &newStatus):
			// Update the deployment with a timeout condition. If the condition already exists,
			// we ignore this update.
			msg := fmt.Sprintf("Deployment %q has timed out progressing.", d.Name)
			if newRS != nil {
				msg = fmt.Sprintf("ReplicaSet %q has timed out progressing.", newRS.Name)
			}
			condition := util.NewDeploymentCondition(orchestrationv1alpha1.RayClusterFleetProgressing, corev1.ConditionFalse, util.TimedOutReason, msg)
			util.SetDeploymentCondition(&newStatus, *condition)
		}
	}

	// Move failure conditions of all replica sets in deployment conditions. For now,
	// only one failure condition is returned from getReplicaFailures.
	if replicaFailureCond := r.getReplicaFailures(allRSs, newRS); len(replicaFailureCond) > 0 {
		// There will be only one ReplicaFailure condition on the replica set.
		util.SetDeploymentCondition(&newStatus, replicaFailureCond[0])
	} else {
		util.RemoveDeploymentCondition(&newStatus, orchestrationv1alpha1.RayClusterFleetReplicaFailure)
	}

	if !reflect.DeepEqual(d.Status, newStatus) {
		d.Status = newStatus
		if err := r.Status().Update(ctx, d); err != nil {
			return ctrl.Result{}, err
		}
	}
	if after := r.requeueStuckDeployment(ctx, d, newStatus); after > 0 {
		return ctrl.Result{RequeueAfter: after}, nil
	}
	return ctrl.Result{}, nil
}

// getReplicaFailures will convert replica failure conditions from replica sets
// to deployment conditions.
func (r *RayClusterFleetReconciler) getReplicaFailures(allRSs []*orchestrationv1alpha1.RayClusterReplicaSet, newRS *orchestrationv1alpha1.RayClusterReplicaSet) []orchestrationv1alpha1.RayClusterFleetCondition {
	var conditions []orchestrationv1alpha1.RayClusterFleetCondition
	if newRS != nil {
		for _, c := range newRS.Status.Conditions {
			if c.Type != orchestrationv1alpha1.RayClusterReplicaSetReplicaFailure {
				continue
			}
			conditions = append(conditions, util.ReplicaSetToDeploymentCondition(c))
		}
	}

	// Return failures for the new replica set over failures from old replica sets.
	if len(conditions) > 0 {
		return conditions
	}

	for i := range allRSs {
		rs := allRSs[i]
		if rs == nil {
			continue
		}

		for _, c := range rs.Status.Conditions {
			if c.Type != orchestrationv1alpha1.RayClusterReplicaSetReplicaFailure {
				continue
			}
			conditions = append(conditions, util.ReplicaSetToDeploymentCondition(c))
		}
	}
	return conditions
}

// requeueStuckDeployment checks whether the provided deployment needs to be synced for a progress
// check. It returns a positive delay (at least one second), or -1 if no check is needed.
func (r *RayClusterFleetReconciler) requeueStuckDeployment(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, newStatus orchestrationv1alpha1.RayClusterFleetStatus) time.Duration {
	logger := klog.FromContext(ctx)
	currentCond := util.GetDeploymentCondition(newStatus, orchestrationv1alpha1.RayClusterFleetProgressing)
	// Can't estimate progress if there is no deadline in the spec or progressing condition in the current status.
	if d.Spec.Paused || d.DeletionTimestamp != nil || !util.HasProgressDeadline(d) || currentCond == nil {
		return time.Duration(-1)
	}
	// No need to estimate progress if the rollout is complete or already timed out. Keep the
	// NewRSAvailableReason exclusion: util.DeploymentTimedOut never reports a timeout for that reason, so a
	// stale condition would give a negative delay and a one-second requeue loop that cannot change the status.
	if util.DeploymentComplete(d, &newStatus) || currentCond.Reason == util.NewRSAvailableReason || currentCond.Reason == util.TimedOutReason {
		return time.Duration(-1)
	}
	// If there is no sign of progress at this point then there is a high chance that the
	// deployment is stuck. We should resync this deployment at some point in the future[1]
	// and check whether it has timed out. We definitely need this, otherwise we depend on the
	// controller resync interval. See https://github.com/kubernetes/kubernetes/issues/34458.
	//
	// [1] ProgressingCondition.LastUpdatedTime + progressDeadlineSeconds - time.Now()
	//
	// For example, if a Deployment updated its Progressing condition 3 minutes ago and has a
	// deadline of 10 minutes, it would need to be resynced for a progress check after 7 minutes.
	//
	// lastUpdated: 			00:00:00
	// now: 					00:03:00
	// progressDeadlineSeconds: 600 (10 minutes)
	//
	// lastUpdated + progressDeadlineSeconds - now => 00:00:00 + 00:10:00 - 00:03:00 => 07:00
	after := currentCond.LastUpdateTime.Time.Add(time.Duration(*d.Spec.ProgressDeadlineSeconds) * time.Second).Sub(util.NowFn())
	// Equality is not timed out; keep a positive delay so the due check is not lost.
	if after < time.Second {
		after = time.Second
	}
	logger.V(4).Info("Queueing up deployment for a progress check", "deployment", klog.KObj(d), "queueAfter", int(after.Seconds()))
	return after
}
