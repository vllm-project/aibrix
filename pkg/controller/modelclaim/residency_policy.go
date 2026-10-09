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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func neverSleeps(claim *modelv1alpha1.ModelClaim) bool {
	policy := claim.Spec.ResidencyPolicy
	return policy != nil && policy.SleepPolicy != nil && policy.SleepPolicy.Mode == modelv1alpha1.ModelClaimSleepNever
}

func ensuresAwake(claim *modelv1alpha1.ModelClaim) bool {
	policy := claim.Spec.ResidencyPolicy
	if policy != nil && policy.WakePolicy != nil {
		return policy.WakePolicy.Mode == modelv1alpha1.ModelClaimWakeEnsureAwake
	}
	return neverSleeps(claim)
}

type policyWakeRequest struct {
	RequestedAt string `json:"requestedAt"`
	OperationID string `json:"operationID"`
}

func policyWakeOnPod(claim *modelv1alpha1.ModelClaim, pod *corev1.Pod) (policyWakeRequest, bool) {
	var request policyWakeRequest
	err := json.Unmarshal([]byte(pod.Annotations[constants.ModelClaimPolicyWakeAnnotationPrefix+claim.Name]), &request)
	prefix := fmt.Sprintf("controller-policy-wake/%s/%s/", pod.UID, claim.UID)
	return request, err == nil && request.RequestedAt == pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claim.Name] && strings.HasPrefix(request.OperationID, prefix)
}

// Policy wakes enter the same durable request queue and memory ledger as
// gateway wakes. A separate operation ID permits a new attempt after refusal
// even when the controller clock has not advanced.
func (r *ModelClaimReconciler) reconcilePolicyWakeRequests(ctx context.Context, claim *modelv1alpha1.ModelClaim) error {
	wakeKey := constants.ModelClaimWakeAnnotationPrefix + claim.Name
	policyKey := constants.ModelClaimPolicyWakeAnnotationPrefix + claim.Name
	for i := range claim.Status.Instances {
		inst := &claim.Status.Instances[i]
		if !ensuresAwake(claim) && inst.Phase == modelv1alpha1.ModelClaimSleeping && inst.Reason == instanceReasonWakeFailed {
			inst.Reason = ""
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: inst.Pod}, pod); err != nil {
			continue
		}
		if !ensuresAwake(claim) {
			if _, found := pod.Annotations[policyKey]; !found {
				continue
			}
			patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if _, owned := policyWakeOnPod(claim, pod); owned {
				delete(pod.Annotations, wakeKey)
			}
			delete(pod.Annotations, policyKey)
			if err := r.Patch(ctx, pod, patch); err != nil {
				return err
			}
			continue
		}
		if inst.Phase != modelv1alpha1.ModelClaimSleeping {
			continue
		}
		if _, requested := pod.Annotations[wakeKey]; requested {
			continue
		}
		request := policyWakeRequest{
			RequestedAt: r.now().UTC().Format(time.RFC3339Nano),
			OperationID: fmt.Sprintf("controller-policy-wake/%s/%s/%s", pod.UID, claim.UID, uuid.NewUUID()),
		}
		value, err := json.Marshal(request)
		if err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[wakeKey] = request.RequestedAt
		pod.Annotations[policyKey] = string(value)
		if err := r.Patch(ctx, pod, patch); err != nil {
			return err
		}
	}
	return nil
}

func (r *ModelClaimReconciler) notePolicyWakeFailure(claim *modelv1alpha1.ModelClaim, inst *modelv1alpha1.ModelClaimInstance, pod *corev1.Pod, err error) {
	if !ensuresAwake(claim) {
		return
	}
	if inst.Reason != instanceReasonWakeFailed {
		r.Recorder.Eventf(claim, corev1.EventTypeWarning, "WakeFailed", "model %s could not wake on pod %s: %v", servedModelName(claim), pod.Name, err)
	}
	inst.Reason = instanceReasonWakeFailed
}
