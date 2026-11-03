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
	"strconv"

	apps "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
)

// getRoleSetList returns the RoleSets owned by stormService.
//
// Membership is controller identity rather than the user selector: the lookup is
// restricted to the StormService namespace, keyed on the storm-service-name label
// that renderRoleSet stamps on every RoleSet, and then narrowed to the RoleSets
// this StormService controls. That label is already the membership key for the
// headless Service and for status.scalingTargetSelector.
//
// spec.selector is a create-time contract that renderRoleSet enforces, not an
// ownership claim, and it is a poor membership index. Listing by it pulls in a
// peer's RoleSets whenever two StormServices in one namespace share a selector,
// and it hides an owned RoleSet whose labels drifted from scaling, status and
// finalize, which leaks the RoleSet on delete. It also makes every lookup depend
// on a field that may be nil, so a deleting StormService could never drop its
// finalizer.
func (r *StormServiceReconciler) getRoleSetList(ctx context.Context, stormService *orchestrationv1alpha1.StormService) ([]*orchestrationv1alpha1.RoleSet, error) {
	roleSetList := &orchestrationv1alpha1.RoleSetList{}
	if err := r.List(ctx, roleSetList,
		client.InNamespace(stormService.Namespace),
		client.MatchingLabels{constants.StormServiceNameLabelKey: stormService.Name},
	); err != nil {
		return nil, fmt.Errorf("failed to list roleSets for stormservice %s/%s: %w", stormService.Namespace, stormService.Name, err)
	}

	var result []*orchestrationv1alpha1.RoleSet
	for i := range roleSetList.Items {
		roleSet := &roleSetList.Items[i]
		if !metav1.IsControlledBy(roleSet, stormService) {
			// The name label points here but the RoleSet is controlled by another
			// object, or by nobody once an ownerReference is stripped. Owns() never
			// enqueues the StormService for an ownerless RoleSet, so this log is the
			// only place such a leak surfaces; keep it at info level.
			klog.InfoS("Skipping RoleSet that is not controlled by this StormService",
				"stormService", klog.KObj(stormService), "roleSet", klog.KObj(roleSet))
			continue
		}
		result = append(result, roleSet)
	}
	return result, nil
}

// renderRoleSet creates a RoleSet with per-role revision annotations
func (r *StormServiceReconciler) renderRoleSet(stormService *orchestrationv1alpha1.StormService, index *int, revisionName string, roleRevisions map[string]*apps.ControllerRevision) (*orchestrationv1alpha1.RoleSet, error) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: utils.Shorten(fmt.Sprintf("%s-roleset-", stormService.Name), true, true),
			Namespace:    stormService.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(stormService, orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.StormServiceKind)),
			},
			Labels:      utils.DeepCopyMap(stormService.Spec.Template.Labels),
			Annotations: utils.DeepCopyMap(stormService.Spec.Template.Annotations),
		},
		Spec: *stormService.Spec.Template.Spec.DeepCopy(),
	}
	if roleSet.Labels == nil {
		roleSet.Labels = make(map[string]string)
	}
	if roleSet.Annotations == nil {
		roleSet.Annotations = make(map[string]string)
	}
	// ensure roleset match stormservice's labelSelector
	selector, err := metav1.LabelSelectorAsSelector(stormService.Spec.Selector)
	if err != nil {
		return nil, err
	}
	if !selector.Matches(labels.Set(roleSet.Labels)) {
		return nil, fmt.Errorf("roleSet labels %v does not match stormService selector %v", roleSet.Labels, selector)
	}
	roleSet.Labels[constants.StormServiceNameLabelKey] = stormService.Name
	roleSet.Labels[constants.StormServiceRevisionLabelKey] = revisionName
	roleSet.Annotations[constants.RoleSetRevisionAnnotationKey] = revisionName

	// Add per-role revision annotations to RoleSet (will be read by RoleSet controller and injected into pods)
	for roleName, cr := range roleRevisions {
		if cr != nil {
			// Store revision number for this role in annotation
			roleRevKey := fmt.Sprintf("%s.%s", constants.RoleRevisionAnnotationPrefix, roleName)
			roleSet.Annotations[roleRevKey] = strconv.FormatInt(cr.Revision, 10)

			// Store revision name for this role (for debugging)
			roleRevNameKey := fmt.Sprintf("%s.%s", constants.RoleRevisionNameAnnotationPrefix, roleName)
			roleSet.Annotations[roleRevNameKey] = cr.Name
		}
	}

	if index != nil {
		roleSet.Annotations[constants.RoleSetIndexAnnotationKey] = strconv.Itoa(*index)
	}
	return roleSet, nil
}

// createRoleSet creates RoleSets with per-role revision annotations
func (r *StormServiceReconciler) createRoleSet(stormService *orchestrationv1alpha1.StormService, count int, revisionName string, roleRevisions map[string]*apps.ControllerRevision) (int, error) {
	if stormService.Spec.Template.Spec == nil {
		return 0, fmt.Errorf("bad stormService template: nil")
	}
	var toCreate []*orchestrationv1alpha1.RoleSet
	for i := 0; i < count; i++ {
		roleSet, err := r.renderRoleSet(stormService, &i, revisionName, roleRevisions)
		if err != nil {
			return 0, err
		}
		toCreate = append(toCreate, roleSet)
	}
	return utils.SlowStartBatch(len(toCreate), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] create roleset for stormservice %s/%s", stormService.Namespace, stormService.Name)
		return r.Create(context.TODO(), toCreate[i])
	})
}

func (r *StormServiceReconciler) deleteRoleSet(toDelete []*orchestrationv1alpha1.RoleSet) (int, error) {
	return utils.SlowStartBatch(len(toDelete), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] delete roleset %s", toDelete[i].Name)
		err := r.Delete(context.TODO(), toDelete[i])
		if err != nil && apierrors.IsNotFound(err) {
			// NotFound will be ignored
			return nil
		}
		return err
	})
}

// updateRoleSet updates RoleSets with per-role revision annotations
func (r *StormServiceReconciler) updateRoleSet(stormService *orchestrationv1alpha1.StormService, toUpdate []*orchestrationv1alpha1.RoleSet, revisionName string, roleRevisions map[string]*apps.ControllerRevision) (int, error) {
	target, err := r.renderRoleSet(stormService, nil, revisionName, roleRevisions)
	if err != nil {
		return 0, err
	}
	return utils.SlowStartBatch(len(toUpdate), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] update roleset %s", toUpdate[i].Name)
		// overwrite labels and annotations, to keep the revision updated
		toUpdate[i].Labels = target.Labels
		rsIdx := toUpdate[i].Annotations[constants.RoleSetIndexAnnotationKey]
		historicalNodeBindings := toUpdate[i].Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey]
		toUpdate[i].Annotations = utils.DeepCopyMap(target.Annotations)
		if rsIdx != "" {
			toUpdate[i].Annotations[constants.RoleSetIndexAnnotationKey] = rsIdx
		}
		if historicalNodeBindings != "" {
			toUpdate[i].Annotations[constants.RoleSetHistoricalNodeBindingsAnnotationKey] = historicalNodeBindings
		}
		// update roleset spec
		toUpdate[i].Spec = target.Spec
		return r.Update(context.TODO(), toUpdate[i])
	})
}
