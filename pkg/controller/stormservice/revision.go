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
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/util/history"
)

func (r *StormServiceReconciler) getControllerRevision(ctx context.Context, obj metav1.Object) ([]*apps.ControllerRevision, error) {
	revisionList := &apps.ControllerRevisionList{}
	matchLabels := client.MatchingLabels(getRevisionLabels(obj))
	if err := r.Client.List(ctx, revisionList, matchLabels, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil, fmt.Errorf("list controller revision failed, %v", err)
	}

	revisions := make([]*apps.ControllerRevision, 0, len(revisionList.Items))
	for i := range revisionList.Items {
		revision := &revisionList.Items[i]
		for _, ownerRef := range revision.OwnerReferences {
			if ownerRef.UID != obj.GetUID() {
				continue
			}
			revisions = append(revisions, revision)
		}
	}
	return revisions, nil
}

func nextRevision(revisions []*apps.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

func getRevisionLabels(obj metav1.Object) map[string]string {
	return map[string]string{
		"name": obj.GetName(),
	}
}

func getPatch(stormService *orchestrationv1alpha1.StormService) ([]byte, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&stormService)
	if err != nil {
		return nil, fmt.Errorf("ToUnstructured error %+v", err)
	}
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})
	spec, ok := raw["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("spec type cast error")
	}
	template, ok := spec["template"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("template type cast error")
	}
	specCopy["template"] = template
	template["$patch"] = "replace"
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}

func newRevision(stormService *orchestrationv1alpha1.StormService, revision int64, collisionCount *int32) (*apps.ControllerRevision, error) {
	patch, err := getPatch(stormService)
	if err != nil {
		return nil, err
	}
	cr, err := history.NewControllerRevision(stormService,
		controllerKind,
		getRevisionLabels(stormService),
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	if cr.ObjectMeta.Annotations == nil {
		cr.ObjectMeta.Annotations = make(map[string]string)
	}
	return cr, nil
}

func (r *StormServiceReconciler) createControllerRevision(parent metav1.Object, revision *apps.ControllerRevision, collisionCount *int32) (*apps.ControllerRevision, error) {
	if collisionCount == nil {
		return nil, fmt.Errorf("collisionCount should not be nil")
	}

	// Clone the input
	clone := revision.DeepCopy()

	// Continue to attempt to create the revision updating the name with a new hash on each iteration
	for {
		hash := history.HashControllerRevision(revision, collisionCount)
		// Update the revisions name
		clone.Name = history.ControllerRevisionName(parent.GetName(), hash)
		clone.Namespace = parent.GetNamespace()

		createErr := r.Client.Create(context.TODO(), clone)
		if errors.IsAlreadyExists(createErr) {
			exists := &apps.ControllerRevision{}
			err := r.Client.Get(context.TODO(), client.ObjectKey{Namespace: clone.Namespace, Name: clone.Name}, clone)
			if err != nil {
				return nil, err
			}
			if bytes.Equal(exists.Data.Raw, clone.Data.Raw) {
				return exists, nil
			}
			*collisionCount++
			continue
		}
		return clone, createErr
	}
}

func (r *StormServiceReconciler) updateControllerRevision(revision *apps.ControllerRevision, newRevision int64) (*apps.ControllerRevision, error) {
	clone := revision.DeepCopy()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if clone.Revision == newRevision {
			return nil
		}
		clone.Revision = newRevision

		updateErr := r.Client.Update(context.TODO(), clone)
		if updateErr == nil {
			return nil
		}
		fresh := &apps.ControllerRevision{}
		if err := r.Client.Get(context.TODO(), client.ObjectKey{Namespace: clone.Namespace, Name: clone.Name}, fresh); err == nil {
			clone = fresh.DeepCopy()
		}
		return updateErr
	})
	return clone, err
}

func (r *StormServiceReconciler) syncRevision(stormService *orchestrationv1alpha1.StormService, revisions []*apps.ControllerRevision) (*apps.ControllerRevision, *apps.ControllerRevision, int32, error) {
	var currentRevision, updateRevision *apps.ControllerRevision
	revisionCount := len(revisions)

	var collisionCount int32
	if stormService.Status.CollisionCount != nil {
		collisionCount = *stormService.Status.CollisionCount
	}

	updateRevision, err := newRevision(stormService, nextRevision(revisions), &collisionCount)
	if err != nil {
		return nil, nil, collisionCount, err
	}

	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)

	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		updateRevision, err = r.updateControllerRevision(equalRevisions[equalCount-1], updateRevision.Revision)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = r.createControllerRevision(stormService, updateRevision, &collisionCount)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == stormService.Status.CurrentRevision {
			currentRevision = revisions[i]
			break
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, collisionCount, nil
}

// ApplyRevision returns a new stormService constructed by restoring the state in revision to stormService.
func applyRevision(stormService *orchestrationv1alpha1.StormService, revision *apps.ControllerRevision) (*orchestrationv1alpha1.StormService, error) {
	clone := stormService.DeepCopy()
	cloneBytes, err := json.Marshal(clone)
	if err != nil {
		return nil, err
	}
	patched, err := strategicpatch.StrategicMergePatch(cloneBytes, revision.Data.Raw, clone)
	if err != nil {
		return nil, err
	}
	restored := &orchestrationv1alpha1.StormService{}
	err = json.Unmarshal(patched, restored)
	if err != nil {
		return nil, err
	}
	return restored, nil
}

func (r *StormServiceReconciler) truncateHistory(
	stormService *orchestrationv1alpha1.StormService,
	revisions []*apps.ControllerRevision,
	current *apps.ControllerRevision,
	update *apps.ControllerRevision) error {
	roleSets, err := r.getRoleSetList(stormService.Spec.Selector)
	if err != nil {
		return err
	}

	revisionHistory := make([]*apps.ControllerRevision, 0, len(revisions))
	// mark all live revisions
	live := map[string]bool{}
	if current != nil {
		live[current.Name] = true
	}
	if update != nil {
		live[update.Name] = true
	}
	for i := range roleSets {
		live[getRoleSetRevision(roleSets[i])] = true
	}
	// collect live revisions and historic revisions
	for i := range revisions {
		if !live[revisions[i].Name] {
			revisionHistory = append(revisionHistory, revisions[i])
		}
	}
	historyLen := len(revisionHistory)
	historyLimit := DefaultRevisionHistoryLimit
	if stormService.Spec.RevisionHistoryLimit != nil {
		historyLimit = int(*stormService.Spec.RevisionHistoryLimit)
	}
	if historyLen <= historyLimit {
		return nil
	}
	// delete any non-live history to maintain the revision limit.
	revisionHistory = revisionHistory[:(historyLen - historyLimit)]
	for i := 0; i < len(revisionHistory); i++ {
		if err := r.Client.Delete(context.TODO(), revisionHistory[i]); err != nil {
			return err
		}
	}
	return nil
}
