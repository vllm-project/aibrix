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
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// division says why a card is being divided, which decides how small a move is
// still carried out and whether each moved engine's claim is told.
type division struct {
	// minimumChangeBytes is how far some engine's limit has to move for the
	// plan to be carried out at all. Zero carries out every plan.
	minimumChangeBytes int64
	// announce raises a KVLimitSet Event on each claim whose engine was moved.
	announce bool
}

// placementDivision makes room for a model being placed. Every byte of that
// room has to be made, and the neighbours did not ask to give it up, so each
// is told.
var placementDivision = division{announce: true}

// compositionDivision divides a card whose engines changed since it was last
// divided: an instance removed or failed, an engine asleep or awake, or a
// declaration changed. A model being placed divides its card itself. The plan
// the card was last divided by was made for other engines, so every move is
// carried out, and each moved engine's claim is told.
var compositionDivision = division{announce: true}

// stuckDivisionTries is how many divisions of a card may fail in a row before
// the claims on it are warned. A failure now and then is the race with an
// engine that is growing, which the next round plans around.
const stuckDivisionTries = 3

// loadDivision follows the load on a card. It runs every round, so a move too
// small to shift memory is skipped, and the moves are logged rather than
// raised on the claims.
func loadDivision(hbmUsableBytes int64) division {
	return division{minimumChangeBytes: minimumKVLimitChangeBytes(hbmUsableBytes)}
}

// cardDivisionState remembers, for each card, when a division of it was last
// tried, and which engines it was last divided for. It is controller-local:
// after a restart every card is seen for the first time.
type cardDivisionState struct {
	mu        sync.Mutex
	now       func() time.Time
	lastRound map[types.NamespacedName]time.Time
	// dividedFor is what the card was last divided for, and attemptedFor what
	// a division of it was last tried for. They differ while a change of
	// engines waits for a division that works.
	dividedFor   map[types.NamespacedName]string
	attemptedFor map[types.NamespacedName]string
	// failures counts the divisions of a card that have failed in a row.
	failures   map[types.NamespacedName]int
	lastPruned time.Time
}

func newCardDivisionState(now func() time.Time) *cardDivisionState {
	if now == nil {
		now = time.Now
	}
	return &cardDivisionState{
		now:          now,
		lastRound:    make(map[types.NamespacedName]time.Time),
		dividedFor:   make(map[types.NamespacedName]string),
		attemptedFor: make(map[types.NamespacedName]string),
		failures:     make(map[types.NamespacedName]int),
	}
}

// due reports whether a card is to be divided now, and whether that is because
// its engines changed since it was last divided.
//
// A card whose engines changed is due at once. Any other card is due once a
// round: every claim on a card reconciles on its own schedule and each of them
// sees the same card, so without the round the card would be divided once per
// claim.
//
// A change stays a change until a division for it succeeds. A card that
// cannot be divided yet, as while an engine that left is still exiting, is
// tried again by the round rather than on every pass, and each try is still
// made as for a change. A card seen for the first time, as every card is after
// a restart, is not taken as changed: the round divides it.
func (s *cardDivisionState) due(card types.NamespacedName, composition string) (divide, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	previous, known := s.dividedFor[card]
	changed = known && previous != composition
	if changed && s.attemptedFor[card] != composition {
		s.attemptedFor[card] = composition
		s.lastRound[card] = now
		return true, true
	}
	if last, found := s.lastRound[card]; found && now.Sub(last) < DefaultRequeueDuration {
		return false, false
	}
	s.attemptedFor[card] = composition
	s.lastRound[card] = now
	return true, changed
}

// divided records that a card was divided for these engines: by the round, by
// a division after its engines changed, or by placement. The next pass then
// does not take them for a change, and the card's round starts again.
func (s *cardDivisionState) divided(card types.NamespacedName, composition string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dividedFor[card] = composition
	s.attemptedFor[card] = composition
	s.lastRound[card] = s.now()
	delete(s.failures, card)
}

// failedAgain counts one more division of a card that did not work, and
// returns how many have failed in a row.
func (s *cardDivisionState) failedAgain(card types.NamespacedName) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[card]++
	return s.failures[card]
}

// pruneLocked forgets cards not divided for a long while, which is what a
// deleted pod leaves behind. Forgetting a card that is still there costs
// nothing: it is simply seen for the first time again.
func (s *cardDivisionState) pruneLocked(now time.Time) {
	const horizon = 30 * DefaultRequeueDuration
	if now.Sub(s.lastPruned) < horizon {
		return
	}
	s.lastPruned = now
	for card, last := range s.lastRound {
		if now.Sub(last) >= horizon {
			delete(s.lastRound, card)
			delete(s.dividedFor, card)
			delete(s.attemptedFor, card)
			delete(s.failures, card)
		}
	}
}

// cardOf is the key a pod's card is remembered by.
func cardOf(pod *corev1.Pod) types.NamespacedName {
	return types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
}

// cardComposition describes the instances recorded on one card in the terms a
// division depends on: whose they are, whether each is awake, asleep or
// failed, and what its claim declared. Two passes that describe a card the same
// way would divide it for the same engines. Extra entries describe instances
// about to be recorded, which is how placement describes the card it divided.
func cardComposition(claims *modelv1alpha1.ModelClaimList, podName string, extra ...string) string {
	entries := append([]string(nil), extra...)
	if claims != nil {
		for i := range claims.Items {
			claim := &claims.Items[i]
			for _, instance := range claim.Status.Instances {
				if instance.Pod == podName {
					entries = append(entries, compositionEntry(claim, instance.Phase))
				}
			}
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

// compositionEntry describes one instance for cardComposition.
func compositionEntry(claim *modelv1alpha1.ModelClaim, phase modelv1alpha1.ModelClaimPhase) string {
	state := "awake"
	switch phase {
	case modelv1alpha1.ModelClaimSleeping:
		state = "asleep"
	case modelv1alpha1.ModelClaimFailed:
		state = "failed"
	}
	declared := "undeclared"
	if claim.Spec.PerGPU != nil {
		declared = fmt.Sprintf("%d+%d",
			claim.Spec.PerGPU.MaximumFootprint.Value(), claim.Spec.PerGPU.KVFloor.Value())
	}
	return claim.Name + "/" + state + "/" + declared
}

func (r *ModelClaimReconciler) divisions() *cardDivisionState {
	if r.Divisions != nil {
		return r.Divisions
	}
	// Production and the reconciler tests set this. A narrow test that builds
	// the reconciler by hand gets a fresh state, and every card is due.
	return newCardDivisionState(time.Now)
}

// divideCards divides again the cards in this claim's pool whose engines all
// declare what they cost. A card whose engines changed is divided at once, and
// any other card once a round, so that each engine's share follows its load
// rather than staying what it was when the last model landed. A card nobody
// could account for is left alone, which includes a card running an engine
// whose claim declares nothing.
func (r *ModelClaimReconciler) divideCards(
	ctx context.Context,
	candidates []corev1.Pod,
	readings *runtimeReadings,
) {
	if len(candidates) == 0 {
		return
	}
	// The same listing says what is on each card and what each card owes, so
	// the engines a card is divided for are the ones it is remembered by.
	claims, err := r.listClaimsForAccount(ctx, candidates[0].Namespace)
	if err != nil {
		return
	}
	divisions := r.divisions()
	due := make([]corev1.Pod, 0, len(candidates))
	changed := make(map[string]bool, len(candidates))
	compositions := make(map[string]string, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		composition := cardComposition(claims, pod.Name)
		if composition == "" {
			// Nothing is recorded on this card, so there is nothing to divide,
			// and no reason to read its runtime.
			continue
		}
		divide, engineChange := divisions.due(cardOf(pod), composition)
		if divide {
			due = append(due, *pod)
			changed[pod.Name] = engineChange
			compositions[pod.Name] = composition
		}
	}
	if len(due) == 0 {
		return
	}

	ledgers := podLedgersFrom(claims, nil, due, readings.ofPods(ctx, due))
	for i := range due {
		pod := &due[i]
		ledger := ledgers[pod.Name]
		if !ledger.judgeable || len(ledger.engines) == 0 || !podHasGPUs(*pod, ledger.accelerators) {
			continue
		}
		why := loadDivision(ledger.hbmUsableBytes)
		if changed[pod.Name] {
			why = compositionDivision
		}
		if _, err := r.arrangeCard(ctx, pod, ledger, ledger.engines, why, readings); err != nil {
			klog.V(2).InfoS("could not divide a card", "pod", klog.KObj(pod),
				"enginesChanged", changed[pod.Name], "err", err)
			if divisions.failedAgain(cardOf(pod)) == stuckDivisionTries {
				r.warnCardNotDivided(pod, claims, ledger, err)
			}
			continue
		}
		divisions.divided(cardOf(pod), compositions[pod.Name])
	}
}

// warnCardNotDivided tells each claim on a card that the card has kept its
// last division through several tries in a row. It is said once for each run
// of failures. A division that fails once is usually the race with an engine
// that is growing, and the next round plans around it. One that keeps failing
// points at an engine whose limit does not take, which only the log would show
// otherwise, since a round raises no events.
func (r *ModelClaimReconciler) warnCardNotDivided(
	pod *corev1.Pod,
	claims *modelv1alpha1.ModelClaimList,
	ledger podLedger,
	err error,
) {
	onCard := make(map[string]bool, len(ledger.engines))
	for _, engine := range ledger.engines {
		onCard[engine.claimName] = true
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if !onCard[claim.Name] {
			continue
		}
		r.Recorder.Eventf(claim, corev1.EventTypeWarning, "KVLimitFailed",
			"model %s on pod %s: its card could not be divided %d times in a row, and keeps its last division: %v",
			servedModelName(claim), pod.Name, stuckDivisionTries, err)
	}
}
