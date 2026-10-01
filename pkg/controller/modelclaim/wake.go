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

// wakeRequested wakes the sleeping engines of a claim that a request has asked
// for, and takes back a request that has nothing left to do. It returns true
// when it started a wake in this pass.
//
// The gateway does not wake an engine itself. It writes a wake request on the
// pod whose engine sleeps, and this controller decides. An engine is woken
// only while its card is promised no more than it has. A sleeping engine keeps
// its seat, so that holds unless a declaration grew while it slept. An account
// that cannot be judged does not stop the wake: the seat is still there.
//
// A request stays while the engine boots, and goes once the engine serves, or
// when there is nothing to wake. A wake that fails is reported, and its request
// is taken back, so the next request for the model asks again.
func (r *ModelClaimReconciler) wakeRequested(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
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

		pods := []corev1.Pod{*pod}
		ledger := r.collectPodLedgers(ctx, pm.Namespace, pods, readings.ofPods(ctx, pods))[pod.Name]
		if ledger.judgeable && (ledger.maximumRoomBytes() < 0 || ledger.heldRoomBytes() < 0) {
			// Logged rather than raised as an Event. The claim is looked at
			// every few seconds while it waits, and client-go drops an
			// object's Events once it has raised 25 in a burst. The Events
			// that follow, such as the wake's, would then be lost.
			klog.V(2).InfoS("wake waits: the card is promised more than it has",
				"model", served, "pod", klog.KObj(pod))
			continue
		}

		// One operation per request, so the runtime applies a request once
		// however many passes see it.
		operationID := fmt.Sprintf("controller-wake/%s/%s/%s", pod.UID, pm.UID, requestedAt)
		_, err := r.Runtime.Wake(ctx, pod.Status.PodIP, DefaultRuntimePort, &WakeRequest{
			ModelName:   served,
			OperationID: operationID,
		})
		readings.forget(pod.Name)
		if err != nil {
			r.Recorder.Eventf(pm, corev1.EventTypeWarning, "WakeFailed",
				"model %s could not be woken on pod %s: %v", served, pod.Name, err)
			r.takeBackWakeRequest(ctx, pod, key)
			continue
		}
		r.Recorder.Eventf(pm, corev1.EventTypeNormal, "Waking",
			"model %s is waking on pod %s, as asked at %s", served, pod.Name, requestedAt)
		woke = true
	}
	return woke
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
