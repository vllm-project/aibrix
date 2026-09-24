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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// maximumPlacementBackoff is the longest a claim no card can hold waits
// between tries. A pool that gains room should take the waiting model within a
// minute, and a model that has waited all morning should not have every
// runtime in the pool read for it every ten seconds.
const maximumPlacementBackoff = time.Minute

// placementBackoff spaces out the tries of claims no card can hold.
//
// The work queue's own rate limiter does not fit. To use it, a refusal would
// have to return Requeue or an error. A limiter slow enough for placement
// would then slow the conflict retries too. It would also delay the engine
// checks of a claim that already has an instance. Besides, a reconcile that
// an event starts never passes through the limiter. So the wait is kept
// here, and a try checks it before any runtime is read.
//
// It is controller-local. A restart clears it, which costs one eager round of
// placement and nothing else, and that beats a status field every reader would
// have to understand.
type placementBackoff struct {
	mu       sync.Mutex
	now      func() time.Time
	attempts map[types.NamespacedName]placementAttempt
}

type placementAttempt struct {
	refusals int
	readyAt  time.Time
	// generation and room are the claim's spec and the pool as they stood at
	// the last refusal. A change in either can make room the wait would
	// otherwise sit through.
	generation int64
	room       roomSignature
}

// roomSignature is the pool as a waiting claim last saw it: for each candidate
// pod, what the instances recorded on it take. It is read from claim status,
// not from any runtime, so it costs nothing to compare on every pass.
type roomSignature map[string]podRoom

// podRoom is what the live instances on one pod take: how many there are, how
// many of them belong to claims that declare nothing, and what the rest are
// promised.
type podRoom struct {
	instances     int
	undeclared    int
	promisedBytes int64
}

func newPlacementBackoff(now func() time.Time) *placementBackoff {
	if now == nil {
		now = time.Now
	}
	return &placementBackoff{
		now:      now,
		attempts: make(map[types.NamespacedName]placementAttempt),
	}
}

// due reports whether a claim may try to find a card now, and how long is left
// when it may not.
//
// A waiting claim starts over at once when room may have appeared since its
// last refusal: its own spec changed, a pod joined the pool, or a pod now
// carries fewer instances, fewer claims that declare nothing, or less that is
// promised. It then waits from the shortest wait again if it is refused.
func (b *placementBackoff) due(claim types.NamespacedName, generation int64, room roomSignature) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt, waiting := b.attempts[claim]
	if !waiting {
		return true, 0
	}
	if generation != attempt.generation || roomMayHaveAppeared(attempt.room, room) {
		delete(b.attempts, claim)
		return true, 0
	}
	left := attempt.readyAt.Sub(b.now())
	if left <= 0 {
		return true, 0
	}
	return false, left
}

// refused records that no card could hold a claim, and returns how long it
// waits before its next try. The wait doubles with each refusal in a row, up to
// maximumPlacementBackoff.
func (b *placementBackoff) refused(claim types.NamespacedName, generation int64, room roomSignature) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt := b.attempts[claim]
	attempt.refusals++
	attempt.generation = generation
	attempt.room = room
	wait := DefaultRequeueDuration << min(attempt.refusals-1, 16)
	if wait > maximumPlacementBackoff || wait <= 0 {
		wait = maximumPlacementBackoff
	}
	attempt.readyAt = b.now().Add(wait)
	b.attempts[claim] = attempt
	return wait
}

// placed forgets a claim's refusals, so a claim that has to wait again starts
// from the shortest wait. A deleted claim is forgotten the same way.
func (b *placementBackoff) placed(claim types.NamespacedName) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.attempts, claim)
}

func (r *ModelClaimReconciler) backoff() *placementBackoff {
	if r.Backoff != nil {
		return r.Backoff
	}
	// Production and the reconciler tests set this. A narrow test that builds
	// the reconciler by hand gets a fresh one, and every claim is due.
	return newPlacementBackoff(time.Now)
}

// roomMayHaveAppeared compares the pool with how a waiting claim last saw it.
// A pod that left frees nothing for anyone, so it does not count. Without both
// descriptions there is nothing to compare.
func roomMayHaveAppeared(before, now roomSignature) bool {
	if before == nil || now == nil {
		return false
	}
	for key, taken := range now {
		was, seen := before[key]
		if !seen {
			return true
		}
		if taken.instances < was.instances || taken.undeclared < was.undeclared ||
			taken.promisedBytes < was.promisedBytes {
			return true
		}
	}
	return false
}

// podKey names a pod in a roomSignature. The UID tells a pod that was
// replaced from the one it replaced, which is a pod joining the pool.
func podKey(pod *corev1.Pod) string {
	return pod.Name + "/" + string(pod.UID)
}

// roomSignatureOf describes what the live instances on each candidate take,
// from a listing of the claims. It is nil when there is no listing.
func roomSignatureOf(candidates []corev1.Pod, claims *modelv1alpha1.ModelClaimList) roomSignature {
	if claims == nil {
		return nil
	}
	room := make(roomSignature, len(candidates))
	keys := make(map[string]string, len(candidates))
	for i := range candidates {
		key := podKey(&candidates[i])
		room[key] = podRoom{}
		keys[candidates[i].Name] = key
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		perGPU, perGPUErr := perGPUBytesOf(claim)
		for _, instance := range claim.Status.Instances {
			key, candidate := keys[instance.Pod]
			if !candidate || instance.Phase == modelv1alpha1.ModelClaimFailed {
				continue
			}
			taken := room[key]
			taken.instances++
			if perGPUErr != nil {
				taken.undeclared++
			} else {
				taken.promisedBytes += perGPU.minimumReserveBytes()
			}
			room[key] = taken
		}
	}
	return room
}

// liveInstances counts the instances of a claim that still take room.
func liveInstances(pm *modelv1alpha1.ModelClaim) int {
	live := 0
	for _, instance := range pm.Status.Instances {
		if instance.Phase != modelv1alpha1.ModelClaimFailed {
			live++
		}
	}
	return live
}

// freesRoom reports whether a change to a claim can free room on a card for a
// claim that is waiting: an instance gone or failed, a declaration that
// shrank, or one that became usable and so closes a hole in its card's
// account.
func freesRoom(before, after *modelv1alpha1.ModelClaim) bool {
	if liveInstances(after) < liveInstances(before) {
		return true
	}
	was, wasErr := perGPUBytesOf(before)
	now, nowErr := perGPUBytesOf(after)
	if nowErr != nil {
		return false
	}
	return wasErr != nil || now.minimumReserveBytes() < was.minimumReserveBytes()
}

// roomMayHaveFreed passes the claim events after which a waiting claim should
// look again: a claim deleted, or a change for which freesRoom says yes.
func roomMayHaveFreed() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, beforeOK := e.ObjectOld.(*modelv1alpha1.ModelClaim)
			after, afterOK := e.ObjectNew.(*modelv1alpha1.ModelClaim)
			return beforeOK && afterOK && freesRoom(before, after)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// enqueueWaitingClaims wakes the claims in the same namespace that wait for a
// card, when another claim may have freed one.
func enqueueWaitingClaims(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		claims := &modelv1alpha1.ModelClaimList{}
		if err := c.List(ctx, claims, client.InNamespace(obj.GetNamespace())); err != nil {
			klog.ErrorS(err, "unable to list model claims to wake", "namespace", obj.GetNamespace())
			return nil
		}
		var requests []reconcile.Request
		for i := range claims.Items {
			claim := &claims.Items[i]
			if claim.Name == obj.GetName() || !claim.DeletionTimestamp.IsZero() ||
				desiredReplicas(claim) <= int32(len(claim.Status.Instances)) {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name},
			})
		}
		return requests
	}
}
