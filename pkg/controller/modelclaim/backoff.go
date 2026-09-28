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
	"sort"
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
	"github.com/vllm-project/aibrix/pkg/utils"
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
	// tooLarge is whether the claim was refused because no card could ever
	// hold it. Only a pod joining the pool, or its own spec, can change that.
	tooLarge bool
	// failedToStart is whether the claim found a card, and its engine could
	// not be started there. Room appearing does not help it either.
	failedToStart bool
	// written is whether the claim's status was written after the refusal, so
	// that the claim says why it waits.
	written bool
}

// roomSignature is the pool as a waiting claim last saw it: for each candidate
// pod, what the instances recorded on it take. It is read from claim status,
// not from any runtime, so it costs nothing to compare on every pass.
type roomSignature map[string]podRoom

// podRoom is what the live instances on one pod take: how many there are, how
// many of them are awake, how many belong to claims that declare nothing, and
// what the rest are promised. It also says whether the pod is ready, which is
// when its runtime answers.
type podRoom struct {
	instances     int
	awake         int
	undeclared    int
	promisedBytes int64
	ready         bool
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
// last refusal: its own spec changed, a pod joined the pool or turned ready,
// or a pod now carries fewer instances, fewer that are awake, fewer claims
// that declare nothing, or less that is promised. It then waits from the
// shortest wait again if it is refused.
func (b *placementBackoff) due(claim types.NamespacedName, generation int64, room roomSignature) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt, waiting := b.attempts[claim]
	if !waiting {
		return true, 0
	}
	if generation != attempt.generation || roomMayHaveAppeared(attempt.room, room, attempt.tooLarge) {
		delete(b.attempts, claim)
		return true, 0
	}
	if !attempt.written {
		// The refusal did not reach the claim's status, so the claim would
		// wait without saying why. The try is made again, and counts once.
		attempt.refusals = max(attempt.refusals-1, 0)
		b.attempts[claim] = attempt
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
	return b.refuse(claim, generation, room, false)
}

// refusedAsTooLarge records that no candidate pod could ever hold a claim,
// even empty. It waits as a refused claim does, but room freed on a card is
// not a reason to try it again.
func (b *placementBackoff) refusedAsTooLarge(
	claim types.NamespacedName,
	generation int64,
	room roomSignature,
) time.Duration {
	return b.refuse(claim, generation, room, true)
}

// failedToStart records that a claim found a card and its engine could not be
// started there, and returns how long it waits before its next try. It waits
// as a refused claim does. Each try divides the card for the model and gives
// the room back, so a claim that tried every round would move its neighbours'
// limits every round.
func (b *placementBackoff) failedToStart(claim types.NamespacedName, generation int64) time.Duration {
	wait := b.refuse(claim, generation, nil, false)
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt := b.attempts[claim]
	attempt.failedToStart = true
	// The failure is written before the wait is recorded.
	attempt.written = true
	b.attempts[claim] = attempt
	return wait
}

// statusWritten records that a claim's status was written, and with it the
// refusal the claim waits on, if it waits.
func (b *placementBackoff) statusWritten(claim types.NamespacedName) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if attempt, waiting := b.attempts[claim]; waiting && !attempt.written {
		attempt.written = true
		b.attempts[claim] = attempt
	}
}

// waitsAfterAFailedStart reports whether a claim waits because its engine
// could not be started.
func (b *placementBackoff) waitsAfterAFailedStart(claim types.NamespacedName) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts[claim].failedToStart
}

func (b *placementBackoff) refuse(
	claim types.NamespacedName,
	generation int64,
	room roomSignature,
	tooLarge bool,
) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt := b.attempts[claim]
	attempt.refusals++
	attempt.generation = generation
	attempt.room = room
	attempt.tooLarge = tooLarge
	attempt.failedToStart = false
	attempt.written = false
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
// A pod that left frees nothing for anyone, so it does not count. An engine
// that went to sleep keeps its seat, and gives back the KV it had mapped. A
// pod that turned ready has a runtime that answers, which it may not have had
// when the claim was refused. For a claim no card could ever hold, only a pod
// that joined counts: every card was measured then, and room freed on a card
// too small for it changes nothing. Without both descriptions there is
// nothing to compare.
func roomMayHaveAppeared(before, now roomSignature, tooLarge bool) bool {
	if before == nil || now == nil {
		return false
	}
	for key, taken := range now {
		was, seen := before[key]
		if !seen {
			return true
		}
		if tooLarge {
			continue
		}
		if taken.instances < was.instances || taken.awake < was.awake ||
			taken.undeclared < was.undeclared || taken.promisedBytes < was.promisedBytes ||
			taken.ready && !was.ready {
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
		room[key] = podRoom{ready: utils.IsPodReady(&candidates[i])}
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
			if instance.Phase != modelv1alpha1.ModelClaimSleeping {
				taken.awake++
			}
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

// liveInstances counts the instances of a claim whose engines take room. An
// instance is recorded before its engine is started, and has no port until the
// engine is. When such a record is taken back, no engine has left a card.
func liveInstances(pm *modelv1alpha1.ModelClaim) int {
	live := 0
	for _, instance := range pm.Status.Instances {
		if instance.Phase != modelv1alpha1.ModelClaimFailed && instance.Port != 0 {
			live++
		}
	}
	return live
}

// awakeInstances counts the instances of a claim whose engines take room and
// are not asleep.
func awakeInstances(pm *modelv1alpha1.ModelClaim) int {
	awake := 0
	for _, instance := range pm.Status.Instances {
		if instance.Phase != modelv1alpha1.ModelClaimFailed &&
			instance.Phase != modelv1alpha1.ModelClaimSleeping && instance.Port != 0 {
			awake++
		}
	}
	return awake
}

// freesRoom reports whether a change to a claim can free room on a card for a
// claim that is waiting: an instance gone, failed or gone to sleep, a
// declaration that shrank, or one that became usable and so closes a hole in
// its card's account.
func freesRoom(before, after *modelv1alpha1.ModelClaim) bool {
	if liveInstances(after) < liveInstances(before) || awakeInstances(after) < awakeInstances(before) {
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

// oldestFirst orders claims by when they were created, and by name among the
// ones created together. The queue is first in, first out, and one worker
// takes from it. So the claims woken together are tried in this order, and
// the one that has waited longest gets the room first.
func oldestFirst(claims []modelv1alpha1.ModelClaim) {
	sort.SliceStable(claims, func(i, j int) bool {
		if !claims[i].CreationTimestamp.Equal(&claims[j].CreationTimestamp) {
			return claims[i].CreationTimestamp.Before(&claims[j].CreationTimestamp)
		}
		return claims[i].Name < claims[j].Name
	})
}

// enqueueWaitingClaims wakes the claims in the same namespace that wait for a
// card, when another claim may have freed one. A claim that failed is left
// out: its engine config is not valid, or its engine could not be started, and
// room helps neither.
func enqueueWaitingClaims(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		claims := &modelv1alpha1.ModelClaimList{}
		if err := c.List(ctx, claims, client.InNamespace(obj.GetNamespace())); err != nil {
			klog.ErrorS(err, "unable to list model claims to wake", "namespace", obj.GetNamespace())
			return nil
		}
		oldestFirst(claims.Items)
		var requests []reconcile.Request
		for i := range claims.Items {
			claim := &claims.Items[i]
			if claim.Name == obj.GetName() || !claim.DeletionTimestamp.IsZero() ||
				claim.Status.Phase == modelv1alpha1.ModelClaimFailed ||
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
