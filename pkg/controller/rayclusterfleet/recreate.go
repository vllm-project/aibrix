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

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	rayclusterutil "github.com/aibrix/aibrix/pkg/utils"
	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"

	"github.com/aibrix/aibrix/pkg/controller/rayclusterfleet/util"
	"k8s.io/apimachinery/pkg/types"
)

// rolloutRecreate implements the logic for recreating a replica set.
func (r *RayClusterFleetReconciler) rolloutRecreate(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, rsList []*orchestrationv1alpha1.RayClusterReplicaSet, clusterMap map[types.UID][]*rayclusterv1.RayCluster) error {
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
	if oldPodsRunning(newRS, oldRSs, clusterMap) {
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
func oldPodsRunning(newRS *orchestrationv1alpha1.RayClusterReplicaSet, oldRSs []*orchestrationv1alpha1.RayClusterReplicaSet, podMap map[types.UID][]*rayclusterv1.RayCluster) bool {
	if oldPods := util.GetActualReplicaCountForReplicaSets(oldRSs); oldPods > 0 {
		return true
	}
	for rsUID, podList := range podMap {
		// If the pods belong to the new ReplicaSet, ignore.
		if newRS != nil && newRS.UID == rsUID {
			continue
		}
		for _, pod := range podList {
			if !pod.DeletionTimestamp.IsZero() && rayclusterutil.IsRayClusterReady(pod) {
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
