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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	// The reasons an instance records, when its phase alone does not say why it
	// stands where it does.
	instanceReasonWaitingForRoom = "WaitingForRoom"
	instanceReasonNoRoomToWake   = "NoRoomToWake"
	instanceReasonWakeFailed     = "WakeFailed"

	// wakeRequestLifetime is how long a wake request waits for room before it
	// is taken back. A client still asking writes a new one.
	wakeRequestLifetime = 5 * time.Minute
)

const (
	// The reasons of the Ready condition, and of the route, that say more
	// than a claim's phase: a request waits for room, or the claim moves.
	readyReasonWaitingForRoom = "WaitingForRoom"
	readyReasonMoving         = "Moving"
)

// whyMoved words, for the Event of a move, why an instance left its pod.
func whyMoved(reason string) string {
	switch reason {
	case instanceReasonNoRoomToWake:
		return "because its card could not take it back from sleep,"
	case instanceReasonWakeFailed:
		return "because it could not be woken there,"
	}
	return "after terminal engine failure"
}

// movingReason reports whether an instance is marked to be moved, rather than
// failed for good.
func movingReason(reason string) bool {
	return reason == instanceReasonNoRoomToWake || reason == instanceReasonWakeFailed
}

// wakeRequested wakes the sleeping engines of a claim that a request has asked
// for, and takes back a request that has nothing left to do. It returns true
// when it started a wake in this pass.
//
// The gateway does not wake an engine itself. It writes a wake request on the
// pod whose engine sleeps, and this controller decides. An engine is woken only
// while its card is promised no more than it has. A sleeping engine keeps its
// seat, so that holds unless a declaration grew while it slept. An account that
// cannot be judged does not stop the wake: the seat is still there.
//
// An engine that cannot wake where it is is moved, when another pod can take
// its claim. That is an engine whose card is promised more than it has, and one
// whose runtime answers that it could not wake it. The instance is marked
// failed, with the reason, and the replacement that follows in the same pass
// stops the engine and places the claim anew. A card that cannot take the
// engine back, with no other pod to go to, keeps the request waiting. A wake
// that failed with no other pod to go to takes the request back, so the next
// request for the model asks again. A wake whose runtime was not reached, or
// did not answer in time, is asked again on a later pass, with the same
// operation: the engine may be waking already.
//
// A request stays while the engine boots, and goes once the engine serves, or
// when there is nothing to wake. A request not met within wakeRequestLifetime
// is taken back. A client that still asks writes a new one.
func (r *ModelClaimReconciler) wakeRequested(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) (woke bool) {
	key := constants.ModelClaimWakeAnnotationPrefix + pm.Name
	served := servedModelName(pm)
	for i := range pm.Status.Instances {
		inst := &pm.Status.Instances[i]
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pm.Namespace, Name: inst.Pod}, pod); err != nil {
			continue
		}
		requestedAt, asked := pod.Annotations[key]
		if !asked {
			r.setWaitingForRoom(ctx, pm, inst, pod, false)
			continue
		}
		switch inst.Phase {
		case modelv1alpha1.ModelClaimSleeping:
		case modelv1alpha1.ModelClaimActivating:
			// Booting after a wake, or after a restart. The request is done
			// once the engine serves.
			continue
		default:
			r.takeBackWakeRequest(ctx, pod, key)
			continue
		}
		if pod.Status.PodIP == "" {
			continue
		}
		if r.wakeRequestExpired(requestedAt) {
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WakeRequestExpired",
				"model %s stays asleep on pod %s: it could not be woken within %s of the request",
				served, pod.Name, wakeRequestLifetime)
			r.takeBackWakeRequest(ctx, pod, key)
			r.setWaitingForRoom(ctx, pm, inst, pod, false)
			continue
		}

		pods := []corev1.Pod{*pod}
		ledger := r.collectPodLedgers(ctx, pm.Namespace, pods, readings.ofPods(ctx, pods))[pod.Name]
		if ledger.judgeable && (ledger.maximumRoomBytes() < 0 || ledger.heldRoomBytes() < 0) {
			if r.canPlaceElsewhere(ctx, pm, candidates, readings) {
				r.markMoving(pm, inst, instanceReasonNoRoomToWake,
					fmt.Sprintf("its card on pod %s is promised more than it has", pod.Name))
				continue
			}
			// Raised once, when the wake starts to wait. The instance and the
			// claim's Ready condition say it for as long as it lasts. The claim
			// is looked at every few seconds, and client-go drops an object's
			// Events once it has raised 25 in a burst, so an Event on every
			// pass would crowd out the ones that follow, such as the wake's.
			if inst.Reason != instanceReasonWaitingForRoom {
				r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WaitingForRoom",
					"model %s stays asleep on pod %s: its card is promised more than it has", served, pod.Name)
			}
			r.setWaitingForRoom(ctx, pm, inst, pod, true)
			continue
		}
		r.setWaitingForRoom(ctx, pm, inst, pod, false)

		// One operation per request, so the runtime applies a request once
		// however many passes see it.
		operationID := fmt.Sprintf("controller-wake/%s/%s/%s", pod.UID, pm.UID, requestedAt)
		_, err := r.Runtime.Wake(ctx, pod.Status.PodIP, DefaultRuntimePort, &WakeRequest{
			ModelName:   served,
			OperationID: operationID,
		})
		readings.forget(pod.Name)
		if err != nil && !runtimeAnswered(err) {
			// Nothing is known to have failed. The request stays, and a later
			// pass asks again, or sees the engine wake.
			klog.InfoS("wake not answered; asking again on a later pass",
				"pod", klog.KObj(pod), "model", pm.Name, "err", err)
			continue
		}
		if err != nil {
			if r.canPlaceElsewhere(ctx, pm, candidates, readings) {
				r.markMoving(pm, inst, instanceReasonWakeFailed,
					fmt.Sprintf("the runtime on pod %s could not wake it: %v", pod.Name, err))
				continue
			}
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WakeFailed",
				"model %s could not be woken on pod %s, and no other pod can take it: %v", served, pod.Name, err)
			r.takeBackWakeRequest(ctx, pod, key)
			continue
		}
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Waking",
			"model %s is waking on pod %s, as asked at %s", served, pod.Name, requestedAt)
		woke = true
	}
	return woke
}

// markMoving marks an instance whose engine cannot wake where it is, so that
// the replacement of failed instances moves its claim to another pod.
func (r *ModelClaimReconciler) markMoving(
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	reason, why string,
) {
	inst.Phase = modelv1alpha1.ModelClaimFailed
	inst.Reason = reason
	r.Recorder.Eventf(pm, corev1.EventTypeWarning, "Moving",
		"model %s cannot wake on pod %s, and moves to another pod: %s", servedModelName(pm), inst.Pod, why)
}

// setWaitingForRoom records on a sleeping instance whether a request waits
// for room on its card, and tells the gateway on the route at once.
func (r *ModelClaimReconciler) setWaitingForRoom(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	inst *modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
	waiting bool,
) {
	if inst.Phase != modelv1alpha1.ModelClaimSleeping || (inst.Reason == instanceReasonWaitingForRoom) == waiting {
		return
	}
	inst.Reason = ""
	if waiting {
		inst.Reason = instanceReasonWaitingForRoom
	}
	if err := r.annotateWarmPodWithState(ctx, pm, pod, 0, constants.ModelClaimRoutingStateSleeping,
		bindingReason(inst)); err != nil {
		klog.ErrorS(err, "could not say on the route that a wake waits for room", "pod", klog.KObj(pod), "model", pm.Name)
	}
}

// wakeRequestExpired reports whether a request has waited longer than
// wakeRequestLifetime. A request whose time cannot be read is taken as old.
func (r *ModelClaimReconciler) wakeRequestExpired(requestedAt string) bool {
	asked, err := time.Parse(time.RFC3339, requestedAt)
	return err != nil || r.now().Sub(asked) > wakeRequestLifetime
}

// canPlaceElsewhere reports whether placement could take the claim to another
// pod now. It judges as placement does, so a claim marked to move is moved in
// the same pass. Placement never picks a pod the claim is on, so neither does
// this.
func (r *ModelClaimReconciler) canPlaceElsewhere(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) bool {
	perGPU, err := perGPUBytesOf(pm)
	if err != nil {
		return false
	}
	on := instancePods(pm)
	others := make([]corev1.Pod, 0, len(candidates))
	for _, candidate := range candidates {
		if !on[candidate.Name] {
			others = append(others, candidate)
		}
	}
	if len(others) == 0 {
		return false
	}
	ledgers := r.collectPodLedgers(ctx, pm.Namespace, others, readings.ofPods(ctx, others))
	admissible, _ := admissibleCandidates(others, ledgers, perGPU.minimumReserveBytes(), instanceGPUCount(pm))
	return len(admissible) > 0
}

// bindingReason is what the route says, beside its state, about why an
// instance is not served: the reason of its Ready condition, when it is more
// than the state.
func bindingReason(inst *modelv1alpha1.ModelClaimInstance) string {
	switch {
	case inst.Phase == modelv1alpha1.ModelClaimSleeping && inst.Reason == instanceReasonWaitingForRoom:
		return readyReasonWaitingForRoom
	case inst.Phase == modelv1alpha1.ModelClaimFailed && movingReason(inst.Reason):
		return readyReasonMoving
	}
	return ""
}

// takeBackWakeRequest removes a wake request from its pod.
func (r *ModelClaimReconciler) takeBackWakeRequest(ctx context.Context, pod *corev1.Pod, key string) {
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, key)
	if err := r.Patch(ctx, pod, patch); err != nil {
		klog.ErrorS(err, "could not take back a wake request", "pod", klog.KObj(pod), "annotation", key)
	}
}

// wakeRequestsOf returns the wake requests on a pod, by annotation key.
func wakeRequestsOf(pod *corev1.Pod) map[string]string {
	var requests map[string]string
	for key, value := range pod.GetAnnotations() {
		if !strings.HasPrefix(key, constants.ModelClaimWakeAnnotationPrefix) {
			continue
		}
		if requests == nil {
			requests = map[string]string{}
		}
		requests[key] = value
	}
	return requests
}

// withoutWakeRequests returns a copy of a pod without its wake requests, and
// without the fields every write changes.
func withoutWakeRequests(pod *corev1.Pod) *corev1.Pod {
	stripped := pod.DeepCopy()
	for key := range wakeRequestsOf(pod) {
		delete(stripped.Annotations, key)
	}
	stripped.ResourceVersion = ""
	stripped.ManagedFields = nil
	return stripped
}

// notOnlyWakeRequests keeps the events of a pod that changed in more than its
// wake requests. A wake request concerns one claim, and wakeRequestsChanged
// enqueues that claim alone. Every claim in the namespace need not be looked at
// again for it.
func notOnlyWakeRequests() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return true
			}
			if equality.Semantic.DeepEqual(wakeRequestsOf(oldPod), wakeRequestsOf(newPod)) {
				return true
			}
			return !equality.Semantic.DeepEqual(withoutWakeRequests(oldPod), withoutWakeRequests(newPod))
		},
	}
}

// wakeRequestsChanged keeps the events of a pod whose wake requests were
// written or changed.
func wakeRequestsChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			pod, ok := e.Object.(*corev1.Pod)
			return ok && len(wakeRequestsOf(pod)) > 0
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			if !okOld || !okNew {
				return false
			}
			now := wakeRequestsOf(newPod)
			if len(now) == 0 {
				return false
			}
			before := wakeRequestsOf(oldPod)
			for key, value := range now {
				if before[key] != value {
					return true
				}
			}
			return false
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// enqueueRequestedWakes enqueues the claims a pod's wake requests name.
func enqueueRequestedWakes(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(wakeRequestsOf(pod)))
	for key := range wakeRequestsOf(pod) {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: pod.Namespace,
			Name:      strings.TrimPrefix(key, constants.ModelClaimWakeAnnotationPrefix),
		}})
	}
	return requests
}

var _ handler.MapFunc = enqueueRequestedWakes
