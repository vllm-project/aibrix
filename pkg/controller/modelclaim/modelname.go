/*
Copyright 2026 The Aibrix Team.

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

package modelclaim

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// waitsForModelName reports whether pm waits to be placed because another
// claim in its namespace uses its model name first. It then records why on the
// claim's Scheduled condition, and raises an Event when that changes. A claim
// that needs no new engine does not wait. Neither does one whose namespace
// could not be listed, since no card can take a claim then.
func (r *ModelClaimReconciler) waitsForModelName(
	pm *modelv1alpha1.ModelClaim,
	claims *modelv1alpha1.ModelClaimList,
	listErr error,
) bool {
	if listErr != nil || !needsEngine(pm) {
		return false
	}
	holder := modelNameHolder(pm, claims)
	if holder == nil {
		return false
	}
	r.waitForModelName(pm, fmt.Sprintf("model name %s is already used by ModelClaim %s", servedModelName(pm), holder.Name))
	return true
}

// waitsForEngineOfModelName reports whether pm waits to be placed because a
// candidate Pod still runs an engine under its model name for another claim.
// No claim in the namespace keeps the name by then, so that claim has gone,
// and its engine is most likely stopping. A Pod runs one engine per model name,
// so the runtime would refuse to start pm's engine there. The claim waits for
// the engine to go, as it waits for a claim that keeps the name.
func (r *ModelClaimReconciler) waitsForEngineOfModelName(
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	snapshots map[string]*RuntimeSnapshot,
) bool {
	if !needsEngine(pm) {
		return false
	}
	for i := range candidates {
		if owner := engineOfAnotherClaim(snapshots[candidates[i].Name], pm); owner != nil {
			r.waitForModelName(pm, fmt.Sprintf("model name %s is still used on pod %s by an engine of ModelClaim %s",
				servedModelName(pm), candidates[i].Name, owner.Name))
			return true
		}
	}
	return false
}

// waitForModelName records on pm's Scheduled condition why it waits for its
// model name, and raises an Event when that changes.
func (r *ModelClaimReconciler) waitForModelName(pm *modelv1alpha1.ModelClaim, message string) {
	if meta.SetStatusCondition(&pm.Status.Conditions, metav1.Condition{
		Type:    string(modelv1alpha1.ModelClaimConditionTypeScheduled),
		Status:  metav1.ConditionFalse,
		Reason:  constants.ModelClaimReasonModelNameInUse,
		Message: message,
	}) {
		r.Recorder.Event(pm, corev1.EventTypeWarning, constants.ModelClaimReasonModelNameInUse, message)
	}
}

// needsEngine reports whether pm has fewer engines than it asks for, counting
// a failed one as missing.
func needsEngine(pm *modelv1alpha1.ModelClaim) bool {
	return desiredReplicas(pm) > int32(len(pm.Status.Instances)-len(failedInstanceSlots(pm)))
}

// modelNameTaken reports whether pm waits because another claim keeps its model
// name, as its Scheduled condition says. The gauges of that name are then the
// other claim's.
func modelNameTaken(pm *modelv1alpha1.ModelClaim) bool {
	condition := meta.FindStatusCondition(pm.Status.Conditions, string(modelv1alpha1.ModelClaimConditionTypeScheduled))
	return condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.Reason == constants.ModelClaimReasonModelNameInUse
}

// modelNameHolder returns the claim that keeps pm from placing an engine,
// because it uses the same model name and comes first. It returns nil when pm
// comes first.
//
// The runtime agent keeps one engine per model name on a Pod, and the gateway
// routes by model name. So two claims with one name would share an engine on
// a Pod, or have their requests mixed across Pods. Only one claim in a
// namespace may place engines under a name. Claims are only placed on Pods in
// their own namespace, so claims in other namespaces cannot share an engine.
//
// A claim that is being deleted keeps its name until it is gone, since its
// engines still run while they are drained and stopped.
func modelNameHolder(pm *modelv1alpha1.ModelClaim, claims *modelv1alpha1.ModelClaimList) *modelv1alpha1.ModelClaim {
	if claims == nil {
		return nil
	}
	name := servedModelName(pm)
	var holder *modelv1alpha1.ModelClaim
	for i := range claims.Items {
		other := &claims.Items[i]
		if other.Namespace != pm.Namespace || other.Name == pm.Name || servedModelName(other) != name {
			continue
		}
		if comesFirstForModelName(other, pm) && (holder == nil || comesFirstForModelName(other, holder)) {
			holder = other
		}
	}
	return holder
}

// comesFirstForModelName orders two claims with the same model name. A claim
// with an engine that has not failed comes first, so a claim that runs keeps its
// name. A claim whose engines all failed comes next, so they are replaced, and a
// claim with no engine comes last. Otherwise the claim created first comes
// first, and the name breaks a tie, as for placement turns.
func comesFirstForModelName(a, b *modelv1alpha1.ModelClaim) bool {
	if aStanding, bStanding := modelNameStanding(a), modelNameStanding(b); aStanding != bStanding {
		return aStanding > bStanding
	}
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// modelNameStanding is 2 for a claim with an instance that has not failed, 1
// for a claim whose instances all failed, and 0 for a claim with none.
func modelNameStanding(pm *modelv1alpha1.ModelClaim) int {
	switch {
	case len(pm.Status.Instances) == 0:
		return 0
	case len(failedInstanceSlots(pm)) < len(pm.Status.Instances):
		return 2
	default:
		return 1
	}
}

// engineOfAnotherClaim returns the claim that a runtime reading shows pm's
// model name running for, by the claim UID the runtime records, when that is
// another claim. It returns nil otherwise. Two claims could share one engine
// before model names were kept apart, and the engine then belongs to the claim
// it was started for. The engine of a claim that has gone also shows here
// until it has stopped.
func engineOfAnotherClaim(snapshot *RuntimeSnapshot, pm *modelv1alpha1.ModelClaim) *ModelClaimRef {
	if snapshot == nil || pm.UID == "" {
		return nil
	}
	name := servedModelName(pm)
	for i := range snapshot.Models {
		model := &snapshot.Models[i]
		if model.ModelName == name && model.ClaimRef != nil && model.ClaimRef.UID != "" &&
			model.ClaimRef.UID != string(pm.UID) {
			return model.ClaimRef
		}
	}
	return nil
}
