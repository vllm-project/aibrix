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
	"strconv"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/rayclusterfleet/util"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/klog/v2"
)

// rollback the deployment to the specified revision. In any case cleanup the rollback spec.
func (r *RayClusterFleetReconciler) rollback(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, rsList []*orchestrationv1alpha1.RayClusterReplicaSet) error {
	logger := klog.FromContext(ctx)
	newRS, allOldRSs, err := r.getAllReplicaSetsAndSyncRevision(ctx, d, rsList, true)
	if err != nil {
		return err
	}

	allRSs := append(allOldRSs, newRS)
	rollbackTo := getRollbackTo(d)
	// If rollback revision is 0, rollback to the last revision
	if rollbackTo.Revision == 0 {
		if rollbackTo.Revision = util.LastRevision(logger, allRSs); rollbackTo.Revision == 0 {
			// If we still can't find the last revision, gives up rollback
			r.emitRollbackWarningEvent(d, util.RollbackRevisionNotFound, "Unable to find last revision.")
			// Gives up rollback
			return r.updateDeploymentAndClearRollbackTo(ctx, d)
		}
	}
	for _, rs := range allRSs {
		v, err := util.Revision(rs)
		if err != nil {
			logger.V(4).Info("Unable to extract revision from deployment's replica set", "replicaSet", klog.KObj(rs), "err", err)
			continue
		}
		if v == rollbackTo.Revision {
			logger.V(4).Info("Found replica set with desired revision", "replicaSet", klog.KObj(rs), "revision", v)
			// rollback by copying podTemplate.Spec from the replica set
			// revision number will be incremented during the next getAllReplicaSetsAndSyncRevision call
			// no-op if the spec matches current deployment's podTemplate.Spec
			performedRollback, err := r.rollbackToTemplate(ctx, d, rs)
			if performedRollback && err == nil {
				r.emitRollbackNormalEvent(d, fmt.Sprintf("Rolled back deployment %q to revision %d", d.Name, rollbackTo.Revision))
			}
			return err
		}
	}
	r.emitRollbackWarningEvent(d, util.RollbackRevisionNotFound, "Unable to find the revision to rollback to.")
	// Gives up rollback
	return r.updateDeploymentAndClearRollbackTo(ctx, d)
}

// rollbackToTemplate compares the templates of the provided deployment and replica set and
// updates the deployment with the replica set template in case they are different. It also
// cleans up the rollback spec so subsequent requeues of the deployment won't end up in here.
func (r *RayClusterFleetReconciler) rollbackToTemplate(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet, rs *orchestrationv1alpha1.RayClusterReplicaSet) (bool, error) {
	logger := klog.FromContext(ctx)
	performedRollback := false
	if !util.EqualIgnoreHash(&d.Spec.Template, &rs.Spec.Template) {
		logger.V(4).Info("Rolling back deployment to old template spec", "deployment", klog.KObj(d), "templateSpec", rs.Spec.Template)
		util.SetFromReplicaSetTemplate(d, rs.Spec.Template)
		// set RS (the old RS we'll rolling back to) annotations back to the deployment;
		// otherwise, the deployment's current annotations (should be the same as current new RS) will be copied to the RS after the rollback.
		//
		// For example,
		// A Deployment has old RS1 with annotation {change-cause:create}, and new RS2 {change-cause:edit}.
		// Note that both annotations are copied from Deployment, and the Deployment should be annotated {change-cause:edit} as well.
		// Now, rollback Deployment to RS1, we should update Deployment's pod-template and also copy annotation from RS1.
		// Deployment is now annotated {change-cause:create}, and we have new RS1 {change-cause:create}, old RS2 {change-cause:edit}.
		//
		// If we don't copy the annotations back from RS to deployment on rollback, the Deployment will stay as {change-cause:edit},
		// and new RS1 becomes {change-cause:edit} (copied from deployment after rollback), old RS2 {change-cause:edit}, which is not correct.
		util.SetDeploymentAnnotationsTo(d, rs)
		performedRollback = true
	} else {
		logger.V(4).Info("Rolling back to a revision that contains the same template as current deployment, skipping rollback...", "deployment", klog.KObj(d))
		eventMsg := fmt.Sprintf("The rollback revision contains the same template as current deployment %q", d.Name)
		r.emitRollbackWarningEvent(d, util.RollbackTemplateUnchanged, eventMsg)
	}

	return performedRollback, r.updateDeploymentAndClearRollbackTo(ctx, d)
}

func (r *RayClusterFleetReconciler) emitRollbackWarningEvent(d *orchestrationv1alpha1.RayClusterFleet, reason, message string) {
	r.Recorder.Eventf(d, v1.EventTypeWarning, reason, message)
}

func (r *RayClusterFleetReconciler) emitRollbackNormalEvent(d *orchestrationv1alpha1.RayClusterFleet, message string) {
	r.Recorder.Eventf(d, v1.EventTypeNormal, util.RollbackDone, message)
}

// updateDeploymentAndClearRollbackTo sets .spec.rollbackTo to nil and update the input deployment
// It is assumed that the caller will have updated the deployment template appropriately (in case
// we want to rollback).
func (r *RayClusterFleetReconciler) updateDeploymentAndClearRollbackTo(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet) error {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Cleans up rollbackTo of deployment", "deployment", klog.KObj(d))
	setRollbackTo(d, nil)
	return r.Update(ctx, d)
}

// TODO: Remove this when extensions/v1beta1 and apps/v1beta1 Deployment are dropped.
func getRollbackTo(d *orchestrationv1alpha1.RayClusterFleet) *extensions.RollbackConfig {
	// Extract the annotation used for round-tripping the deprecated RollbackTo field.
	revision := d.Annotations[appsv1.DeprecatedRollbackTo]
	if revision == "" {
		return nil
	}
	revision64, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		// If it's invalid, ignore it.
		return nil
	}
	return &extensions.RollbackConfig{
		Revision: revision64,
	}
}

// TODO: remove this as we do not have extensiton api for ray clusters
// TODO: Remove this when extensions/v1beta1 and apps/v1beta1 Deployment are dropped.
func setRollbackTo(d *orchestrationv1alpha1.RayClusterFleet, rollbackTo *extensions.RollbackConfig) {
	if rollbackTo == nil {
		delete(d.Annotations, appsv1.DeprecatedRollbackTo)
		return
	}
	if d.Annotations == nil {
		d.Annotations = make(map[string]string)
	}
	d.Annotations[appsv1.DeprecatedRollbackTo] = strconv.FormatInt(rollbackTo.Revision, 10)
}
