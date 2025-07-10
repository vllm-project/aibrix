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

func (r *StormServiceReconciler) getRoleSetList(ctx context.Context, selector *metav1.LabelSelector) ([]*orchestrationv1alpha1.RoleSet, error) {
	if selector == nil {
		return nil, fmt.Errorf("selector can not be nil")
	}
	roleSetSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("bad selector format: %v", err)
	}
	roleSetList := &orchestrationv1alpha1.RoleSetList{}
	err = r.Client.List(ctx, roleSetList, client.MatchingLabelsSelector{Selector: roleSetSelector})
	if err != nil {
		klog.Errorf("failed to list roleSets")
		return nil, err
	}

	var result []*orchestrationv1alpha1.RoleSet
	for i := range roleSetList.Items {
		result = append(result, &roleSetList.Items[i])
	}
	return result, nil
}

func (r *StormServiceReconciler) renderRoleSet(stormService *orchestrationv1alpha1.StormService, index *int, revisionName string) (*orchestrationv1alpha1.RoleSet, error) {
	roleSet := &orchestrationv1alpha1.RoleSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: stormService.Name + "-roleset-",
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
	if index != nil {
		roleSet.Annotations[constants.RoleSetIndexAnnotationKey] = fmt.Sprintf("%d", *index)
	}
	return roleSet, nil
}

func (r *StormServiceReconciler) createRoleSet(stormService *orchestrationv1alpha1.StormService, count int, revisionName string) (int, error) {
	if stormService.Spec.Template.Spec == nil {
		return 0, fmt.Errorf("bad stormService template: nil")
	}
	var toCreate []*orchestrationv1alpha1.RoleSet
	for i := 0; i < count; i++ {
		roleSet, err := r.renderRoleSet(stormService, nil, revisionName)
		if err != nil {
			return 0, err
		}
		toCreate = append(toCreate, roleSet)
	}
	return utils.SlowStartBatch(len(toCreate), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] create roleset for stormservice %s/%s", stormService.Namespace, stormService.Name)
		return r.Client.Create(context.TODO(), toCreate[i])
	})
}

func (r *StormServiceReconciler) deleteRoleSet(toDelete []*orchestrationv1alpha1.RoleSet) (int, error) {
	return utils.SlowStartBatch(len(toDelete), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] delete roleset %s", toDelete[i].Name)
		err := r.Client.Delete(context.TODO(), toDelete[i])
		if err != nil && apierrors.IsNotFound(err) {
			// NotFound will be ignored
			return nil
		}
		return err
	})
}

func (r *StormServiceReconciler) updateRoleSet(stormService *orchestrationv1alpha1.StormService, toUpdate []*orchestrationv1alpha1.RoleSet, revisionName string) (int, error) {
	target, err := r.renderRoleSet(stormService, nil, revisionName)
	if err != nil {
		return 0, err
	}
	return utils.SlowStartBatch(len(toUpdate), ctrlutil.SlowStartInitialBatchSize, func(i int) error {
		klog.Infof("[rolesetoperation] update roleset %s", toUpdate[i].Name)
		// overwrite labels and annotations, to keep the revision updated
		toUpdate[i].Labels = target.Labels
		toUpdate[i].Annotations = target.Annotations
		// update roleset spec
		toUpdate[i].Spec = target.Spec
		return r.Client.Update(context.TODO(), toUpdate[i])
	})
}
