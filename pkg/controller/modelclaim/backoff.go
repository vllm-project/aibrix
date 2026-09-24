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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
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
func (b *placementBackoff) due(claim types.NamespacedName) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt, waiting := b.attempts[claim]
	if !waiting {
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
func (b *placementBackoff) refused(claim types.NamespacedName) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	attempt := b.attempts[claim]
	attempt.refusals++
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
