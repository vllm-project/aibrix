package rayclusterfleet

import (
	"context"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/rayclusterfleet/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// rolloutRecreate implements the logic for recreating a replica set.
func (r *RayClusterFleetReconciler) rolloutRecreate(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, rsList []*orchestrationv1alpha1.RayClusterReplicaSet, podMap map[types.UID][]*v1.Pod) error {
	// Don't create a new RS if not already existed, so that we avoid scaling up before scaling down.
	newRS, oldRSs, err := r.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, false)
	if err != nil {
		return err
	}
	allRSs := append(oldRSs, newRS)
	activeOldRSs := util.FilterActiveReplicaSets(oldRSs)

	// scale down old replica sets.
	scaledDown, err := r.scaleDownOldReplicaSetsForRecreate(ctx, activeOldRSs, d)
	if err != nil {
		return err
	}
	if scaledDown {
		// Update DeploymentStatus.
		return r.syncRolloutStatus(ctx, allRSs, newRS, d)
	}

	// Do not process a deployment when it has old pods running.
	if oldPodsRunning(newRS, oldRSs, podMap) {
		return r.syncRolloutStatus(ctx, allRSs, newRS, d)
	}

	// If we need to create a new RS, create it now.
	if newRS == nil {
		newRS, oldRSs, err = r.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, true)
		if err != nil {
			return err
		}
		allRSs = append(oldRSs, newRS)
	}

	// scale up new replica set.
	if _, err := r.scaleUpNewReplicaSetForRecreate(ctx, newRS, d); err != nil {
		return err
	}

	if util.DeploymentComplete(d, &d.Status) {
		if err := r.cleanupDeployment(ctx, oldRSs, d); err != nil {
			return err
		}
	}

	// Sync deployment status.
	return r.syncRolloutStatus(ctx, allRSs, newRS, d)
}

// scaleDownOldReplicaSetsForRecreate scales down old replica sets when deployment strategy is "Recreate".
func (r *RayClusterFleetReconciler) scaleDownOldReplicaSetsForRecreate(ctx context.Context, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet) (bool, error) {
	scaled := false
	for i := range oldRSs {
		rs := oldRSs[i]
		// Scaling not required.
		if *(rs.Spec.Replicas) == 0 {
			continue
		}
		scaledRS, updatedRS, err := r.scaleReplicaSetAndRecordEvent(ctx, rs, 0, deployment)
		if err != nil {
			return false, err
		}
		if scaledRS {
			oldRSs[i] = updatedRS
			scaled = true
		}
	}
	return scaled, nil
}

// oldPodsRunning returns whether there are old pods running or any of the old ReplicaSets thinks that it runs pods.
func oldPodsRunning(newRS *orchestrationv1alpha1.RayClusterReplicaSet, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, podMap map[types.UID][]*v1.Pod) bool {
	if oldPods := util.GetActualReplicaCountForReplicaSets(oldRSs); oldPods > 0 {
		return true
	}
	for rsUID, podList := range podMap {
		// If the pods belong to the new ReplicaSet, ignore.
		if newRS != nil && newRS.UID == rsUID {
			continue
		}
		for _, pod := range podList {
			switch pod.Status.Phase {
			case v1.PodFailed, v1.PodSucceeded:
				// Don't count pods in terminal state.
				continue
			case v1.PodUnknown:
				// v1.PodUnknown is a deprecated status.
				// This logic is kept for backward compatibility.
				// This used to happen in situation like when the node is temporarily disconnected from the cluster.
				// If we can't be sure that the pod is not running, we have to count it.
				return true
			default:
				// Pod is not in terminal phase.
				return true
			}
		}
	}
	return false
}

// scaleUpNewReplicaSetForRecreate scales up new replica set when deployment strategy is "Recreate".
func (r *RayClusterFleetReconciler) scaleUpNewReplicaSetForRecreate(ctx context.Context, newRS *orchestrationv1alpha1.RayClusterReplicaSet, deployment *orchestrationv1alpha1.RayClusterFleet) (bool, error) {
	scaled, _, err := r.scaleReplicaSetAndRecordEvent(ctx, newRS, *(deployment.Spec.Replicas), deployment)
	return scaled, err
}
