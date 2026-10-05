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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// scheduledReasonMakingRoom says on the Scheduled condition that no pod has room
// for a claim yet, and that engines are being put to sleep to make it.
const scheduledReasonMakingRoom = constants.ModelClaimReasonMakingRoom

// reservationLifetime is how long a card is held for a claim that room is being
// made for. Each sleep takes seconds, so this leaves time for several. A claim
// still not placed then waits for room as any other.
const reservationLifetime = 2 * time.Minute

// cardReservations remembers the cards held for claims that room is being made
// for. A card held for a claim offers that room to no other claim, and a
// division of the card does not hand it out as KV. It is controller-local:
// after a restart, room is made again from the start.
type cardReservations struct {
	mu   sync.Mutex
	held map[types.NamespacedName]cardReservation
}

// cardReservation is one card held for one claim in the card's namespace.
type cardReservation struct {
	claim string
	bytes int64
	until time.Time
}

func newCardReservations() *cardReservations {
	return &cardReservations{held: map[types.NamespacedName]cardReservation{}}
}

func (r *ModelClaimReconciler) reservations() *cardReservations {
	if r.Reservations == nil {
		r.Reservations = newCardReservations()
	}
	return r.Reservations
}

// hold holds a card for a claim, for the room it needs, and reports whether it
// could. A card already held for another claim cannot be. A claim that holds a
// card keeps the time its hold began with.
func (c *cardReservations) hold(card types.NamespacedName, claim string, bytes int64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	if current, found := c.held[card]; found {
		return current.claim == claim
	}
	for other, current := range c.held {
		if other.Namespace == card.Namespace && current.claim == claim {
			delete(c.held, other)
		}
	}
	c.held[card] = cardReservation{claim: claim, bytes: bytes, until: now.Add(reservationLifetime)}
	return true
}

// heldFor is the card held for a claim, if any.
func (c *cardReservations) heldFor(namespace, claim string, now time.Time) (types.NamespacedName, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	for card, current := range c.held {
		if card.Namespace == namespace && current.claim == claim {
			return card, true
		}
	}
	return types.NamespacedName{}, false
}

// release lets go of the card held for a claim.
func (c *cardReservations) release(namespace, claim string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for card, current := range c.held {
		if card.Namespace == namespace && current.claim == claim {
			delete(c.held, card)
		}
	}
}

// takeFrom takes the room held for other claims than the one named off each
// card's account. An empty name takes off every hold, as a division does.
func (c *cardReservations) takeFrom(ledgers map[string]podLedger, namespace, claim string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	for card, current := range c.held {
		if card.Namespace != namespace || current.claim == claim {
			continue
		}
		if ledger, found := ledgers[card.Name]; found && ledger.judgeable {
			ledger.reservedBytes += current.bytes
			ledgers[card.Name] = ledger
		}
	}
}

func (c *cardReservations) pruneLocked(now time.Time) {
	for card, current := range c.held {
		if !now.Before(current.until) {
			delete(c.held, card)
		}
	}
}

// sleepingFootprints remembers what each claim's engine held the last time it
// was seen asleep. The room that putting it to sleep again gives back can then
// be told before the sleep. It is controller-local, and a claim's reading goes
// when the claim goes.
type sleepingFootprints struct {
	mu      sync.Mutex
	byClaim map[types.NamespacedName]claimFootprint
}

// claimFootprint is what one claim's engine held asleep. The UID tells the
// claim from a later one of the same name.
type claimFootprint struct {
	uid   types.UID
	bytes int64
}

func newSleepingFootprints() *sleepingFootprints {
	return &sleepingFootprints{byClaim: map[types.NamespacedName]claimFootprint{}}
}

func (r *ModelClaimReconciler) footprints() *sleepingFootprints {
	if r.Footprints == nil {
		r.Footprints = newSleepingFootprints()
	}
	return r.Footprints
}

// forgetClaim forgets what this controller keeps in memory for a claim that is
// gone: its wait to be placed, the card held for it, and what its engine held
// asleep. A card held for it is let go at once, so that its room is not kept
// from the other claims until the hold runs out.
func (r *ModelClaimReconciler) forgetClaim(claim types.NamespacedName) {
	r.backoff().placed(claim)
	r.reservations().release(claim.Namespace, claim.Name)
	r.footprints().forget(claim)
}

// note remembers what a claim's engine was seen to hold asleep.
func (s *sleepingFootprints) note(claim *modelv1alpha1.ModelClaim, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byClaim[client.ObjectKeyFromObject(claim)] = claimFootprint{uid: claim.UID, bytes: bytes}
}

// seenAsleep is what a claim's engine held the last time it was seen asleep.
// Another engine's figure is no stand-in for it. Models differ, and a figure
// too large would rule out a pod where room could be made, while one too small
// would pick a pod where it cannot.
func (s *sleepingFootprints) seenAsleep(claim *modelv1alpha1.ModelClaim) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, found := s.byClaim[client.ObjectKeyFromObject(claim)]
	if !found || seen.uid != claim.UID {
		return 0, false
	}
	return seen.bytes, true
}

// forget forgets what the engine of a claim that is gone held asleep.
func (s *sleepingFootprints) forget(claim types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byClaim, claim)
}

// roomPlan is how room would be made for a claim on a pod: the engines to put
// to sleep there, the ones idle longest. A known plan is one in which each of
// those engines has been seen asleep before, so that the room its sleep gives
// back is known, and together they make enough room. Otherwise the first engine
// whose figure is not known ends the plan. It is put to sleep after the ones
// before it, and the next pass decides again from what it then holds.
type roomPlan struct {
	pod    *corev1.Pod
	sleeps []idleEngine
	known  bool
	// room is what the pod's card offers now, without anything given back.
	room int64
}

// betterPlan reports whether a plan makes room more surely than another. A
// known plan beats one that is not. Of two known plans, the one with the
// fewest sleeps wins. Of two that are not, the one whose card offers the most
// room now wins, as the nearest to fitting. A tie goes to the plan whose first
// engine has been idle longest.
func betterPlan(plan, than roomPlan) bool {
	switch {
	case plan.known != than.known:
		return plan.known
	case plan.known && len(plan.sleeps) != len(than.sleeps):
		return len(plan.sleeps) < len(than.sleeps)
	case !plan.known && plan.room != than.room:
		return plan.room > than.room
	}
	return plan.sleeps[0].idleSince.Before(than.sleeps[0].idleSince)
}

// makeRoomToPlace makes room for a claim that no pod has room for, and returns
// what it does, worded for the claim's Scheduled condition. It reports false
// when no room can be made for the claim now.
//
// Room is made for one claim in a pool at a time: the one that has waited
// longest. It is made in a pool that keeps no wake reserve, on the pod where
// the fewest engines would have to sleep, the ones idle longest, as told by
// what each held the last time it slept. Where that is not known, it is made
// first on the pod nearest to fitting. That card is
// held for the claim, so that no other claim takes the room, and no division
// lends it out. One engine goes to sleep a pass. Each pass first tries to
// place the claim, and the claim is placed once a pass finds the room. A card
// with nothing left to put to sleep is let go, and the claim waits for room as
// before.
func (r *ModelClaimReconciler) makeRoomToPlace(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	perGPU perGPUBytes,
	candidates []corev1.Pod,
	ledgers map[string]podLedger,
	readings *runtimeReadings,
) (string, bool) {
	now := r.now()
	if !r.waitedLongest(ctx, pm, candidates) {
		return "", false
	}
	seat := perGPU.minimumReserveBytes()
	held, holding := r.reservations().heldFor(pm.Namespace, pm.Name, now)
	var best *roomPlan
	for i := range candidates {
		pod := &candidates[i]
		if holding && pod.Name != held.Name {
			continue
		}
		plan, found := r.planRoom(ctx, pm, pod, ledgers[pod.Name], seat, readings)
		if !found {
			continue
		}
		if best == nil || betterPlan(plan, *best) {
			best = &plan
		}
	}
	if best == nil {
		r.reservations().release(pm.Namespace, pm.Name)
		return "", false
	}
	if !r.reservations().hold(cardOf(best.pod), pm.Name, seat, now) {
		return "", false
	}
	if !r.putIdleEngineToSleep(ctx, best.sleeps[0], best.pod, pm, readings) {
		// No room was made, so the card is let go at once. Held, it would be
		// kept from every other claim for the rest of the hold, while this
		// claim says that it waits for room as before.
		r.reservations().release(pm.Namespace, pm.Name)
		return "", false
	}
	more := ""
	if left := len(best.sleeps) - 1; left > 0 {
		more = fmt.Sprintf(", and %d more may follow", left)
	}
	return fmt.Sprintf("no pod has room for this model, which needs %s on a card; "+
		"model %s was put to sleep on pod %s to make room%s",
		gibibytes(seat), best.sleeps[0].model.ModelName, best.pod.Name, more), true
}

// planRoom finds the fewest engines on a pod that would have to sleep for a
// claim to fit there, the ones idle longest. Each would give back what it is
// charged, less what it held the last time it slept. The claim has to pass
// both checks that placement makes: the floors, and what the engines hold now.
// An engine that has not been seen asleep ends the plan, as roomPlan says.
func (r *ModelClaimReconciler) planRoom(
	ctx context.Context,
	pm *modelv1alpha1.ModelClaim,
	pod *corev1.Pod,
	ledger podLedger,
	seat int64,
	readings *runtimeReadings,
) (roomPlan, bool) {
	// A pod whose cards do not suit the claim's topology cannot take it, however
	// much room is made there.
	if !ledger.judgeable || !podHasGPUs(*pod, ledger.accelerators) || !fitsTopology(*pod, ledger, instanceGPUCount(pm)) {
		return roomPlan{}, false
	}
	lifecycle, can := r.roomCanBeMadeOn(ctx, pm, pod, ledger)
	if !can {
		return roomPlan{}, false
	}
	charged := make(map[string]engineOnPod, len(ledger.engines))
	for _, engine := range ledger.engines {
		charged[engine.claimName] = engine
	}
	floors, holding := ledger.maximumRoomBytes(), ledger.heldRoomBytes()
	plan := roomPlan{pod: pod, room: holding}
	for _, sleeper := range r.idleEngines(ctx, pm, pod, lifecycle.sleepToMakeRoomAfter(), readings) {
		engine, found := charged[sleeper.claim.Name]
		if !found {
			// An engine the account does not know of is left out. The ones
			// beside it are still counted.
			continue
		}
		plan.sleeps = append(plan.sleeps, sleeper)
		asleep, known := r.footprints().seenAsleep(sleeper.claim)
		if !known {
			return plan, true
		}
		floors += engine.minimumReserveBytes() - asleep
		holding += engine.heldBytes() - asleep
		if floors >= seat && holding >= seat {
			plan.known = true
			return plan, true
		}
	}
	return roomPlan{}, false
}

// waitedLongest reports whether a claim has waited longest of the claims that
// wait for a card in its pool: the claims in its namespace that select one of
// its candidate pods, and still miss an instance. A claim whose last start
// failed is not among them, this one included, as room does not help it. The
// claims are listed as the account lists them: the cache can miss an instance
// recorded a moment ago, and a claim just placed would then seem to wait.
func (r *ModelClaimReconciler) waitedLongest(ctx context.Context, pm *modelv1alpha1.ModelClaim, candidates []corev1.Pod) bool {
	if pm.Status.Phase == modelv1alpha1.ModelClaimFailed {
		return false
	}
	claims, err := r.listClaimsForAccount(ctx, pm.Namespace)
	if err != nil {
		return false
	}
	var waiting []modelv1alpha1.ModelClaim
	for i := range claims.Items {
		claim := claims.Items[i]
		if claim.Name == pm.Name {
			waiting = append(waiting, *pm)
			continue
		}
		if !claim.DeletionTimestamp.IsZero() || claim.Status.Phase == modelv1alpha1.ModelClaimFailed ||
			desiredReplicas(&claim) <= int32(len(claim.Status.Instances)) || !selectsAny(&claim, candidates) {
			continue
		}
		waiting = append(waiting, claim)
	}
	oldestFirst(waiting)
	return len(waiting) > 0 && waiting[0].Name == pm.Name
}

// selectsAny reports whether a claim selects any of the pods.
func selectsAny(claim *modelv1alpha1.ModelClaim, pods []corev1.Pod) bool {
	if claim.Spec.PodSelector == nil {
		return false
	}
	selector, err := metav1.LabelSelectorAsSelector(claim.Spec.PodSelector)
	if err != nil {
		return false
	}
	for i := range pods {
		if selector.Matches(labels.Set(pods[i].Labels)) {
			return true
		}
	}
	return false
}
