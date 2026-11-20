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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// Cleanup requires two observations separated by this interval. A fresh
// controller starts the observation window again, and retries are paced per Pod
// even when many claims reconcile against the same pool.
const relocationCheckInterval = 30 * time.Second

type relocationEngine struct {
	claim ModelClaimRef
	model string
	port  int32
}

type relocationObservation struct {
	at      time.Time
	podUID  types.UID
	podIP   string
	engines map[relocationEngine]bool
}

type relocationChecks struct {
	pods      map[types.NamespacedName]relocationObservation
	nextPrune time.Time
}

func (r *ModelClaimReconciler) reconcileRelocationCleanup(
	ctx context.Context,
	namespace string,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) {
	now := r.now()
	checks := &r.relocationChecks
	if checks.pods == nil {
		checks.pods = make(map[types.NamespacedName]relocationObservation)
	}
	if !now.Before(checks.nextPrune) {
		for key, observation := range checks.pods {
			if now.Sub(observation.at) > 3*relocationCheckInterval {
				delete(checks.pods, key)
			}
		}
		checks.nextPrune = now.Add(relocationCheckInterval)
	}

	var due []corev1.Pod
	for i := range candidates {
		pod := &candidates[i]
		if pod.UID == "" {
			continue
		}
		previous := checks.pods[client.ObjectKeyFromObject(pod)]
		if previous.podUID != pod.UID || previous.podIP != pod.Status.PodIP || now.Sub(previous.at) >= relocationCheckInterval {
			due = append(due, *pod)
		}
	}
	if len(due) == 0 {
		return
	}

	snapshots := readings.ofPods(ctx, due)
	// Use the API server, including reservations written before an engine
	// starts that may not yet be visible in the controller's informer.
	claims, err := r.listClaimsForAccount(ctx, namespace)
	ledgers := podLedgersFrom(claims, err, due, snapshots, nil)
	for i := range due {
		pod := &due[i]
		key := client.ObjectKeyFromObject(pod)
		previous := checks.pods[key]
		current := relocationObservation{at: now, podUID: pod.UID, podIP: pod.Status.PodIP}
		for _, model := range ledgers[pod.Name].unclaimed {
			owner := claimForRuntimeSnapshot(claims, model)
			if owner == nil || !relocatedFrom(owner, pod.Name, model.ModelName, claims) {
				continue
			}
			engine := relocationEngine{claim: *model.ClaimRef, model: model.ModelName, port: model.Port}
			if current.engines == nil {
				current.engines = make(map[relocationEngine]bool)
			}
			current.engines[engine] = true
			if previous.podUID == pod.UID && previous.podIP == pod.Status.PodIP && previous.engines[engine] {
				r.stopRelocatedEngine(ctx, pod, owner, engine, readings)
			}
		}
		checks.pods[key] = current
	}
}

// Limit cleanup to claims that still have a placement elsewhere. Protect any
// same-name instance recorded here, even if the runtime attributes it to a
// different Claim UID.
func relocatedFrom(owner *modelv1alpha1.ModelClaim, podName, modelName string, claims *modelv1alpha1.ModelClaimList) bool {
	if !owner.DeletionTimestamp.IsZero() || servedModelName(owner) != modelName || len(owner.Status.Instances) == 0 {
		return false
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if servedModelName(claim) == modelName && slices.ContainsFunc(claim.Status.Instances, func(instance modelv1alpha1.ModelClaimInstance) bool {
			return instance.Pod == podName
		}) {
			return false
		}
	}
	return true
}

func (r *ModelClaimReconciler) stopRelocatedEngine(
	ctx context.Context,
	pod *corev1.Pod,
	owner *modelv1alpha1.ModelClaim,
	engine relocationEngine,
	readings *runtimeReadings,
) {
	snapshot, err := r.Runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
	if err != nil || !slices.ContainsFunc(snapshot.models(), func(model RuntimeSnapshotModel) bool {
		return model.ClaimRef != nil && *model.ClaimRef == engine.claim &&
			model.ModelName == engine.model && model.Port == engine.port &&
			model.Phase != runtimePhaseStopping && (model.Phase != runtimePhaseFailed || model.Alive)
	}) {
		return
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	current := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil ||
		current.UID != pod.UID || current.Status.PodIP != pod.Status.PodIP ||
		current.Status.Phase != corev1.PodRunning ||
		current.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue ||
		!current.DeletionTimestamp.IsZero() {
		return
	}

	routeKey := constants.ModelClaimPodAnnotationPrefix + owner.Name
	wakeKey := constants.ModelClaimWakeAnnotationPrefix + owner.Name
	if _, routed := current.Annotations[routeKey]; routed || wakeAsked(current, owner.Name) {
		patch := client.MergeFromWithOptions(current.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(current.Annotations, routeKey)
		delete(current.Annotations, wakeKey)
		if err := r.Patch(ctx, current, patch); err != nil {
			klog.ErrorS(err, "remove the relocated model's old route", "pod", klog.KObj(current), "model", engine.model)
			return
		}
	}

	// ModelClaim has one worker. Its placement and stop calls cannot run
	// concurrently, and every managed start reserves an instance before
	// calling the runtime. The latest ledger and Pod UID protect that path.
	stopCtx, cancel := context.WithTimeout(ctx, deletionStopTimeout)
	defer cancel()
	err = r.Runtime.Deactivate(stopCtx, current.Status.PodIP, DefaultRuntimePort, &DeactivateRequest{
		ModelName: engine.model,
		Mode:      DeactivateStop,
	})
	readings.forget(current.Name)
	if err != nil {
		klog.ErrorS(err, "failed to stop relocated model on its old pod", "pod", klog.KObj(current), "model", engine.model)
	}
}
