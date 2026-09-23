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

// loadDivision follows the load on a card. It runs every round, so a move too
// small to shift memory is skipped, and the moves are logged rather than
// raised on the claims.
func loadDivision(hbmUsableBytes int64) division {
	return division{minimumChangeBytes: minimumKVLimitChangeBytes(hbmUsableBytes)}
}

// cardDivisionState remembers when each card was last divided to follow its
// load. It is controller-local: after a restart every card is simply due.
type cardDivisionState struct {
	mu         sync.Mutex
	now        func() time.Time
	lastRound  map[types.NamespacedName]time.Time
	lastPruned time.Time
}

func newCardDivisionState(now func() time.Time) *cardDivisionState {
	if now == nil {
		now = time.Now
	}
	return &cardDivisionState{
		now:       now,
		lastRound: make(map[types.NamespacedName]time.Time),
	}
}

// due reports whether a card may be divided to follow its load now, and if so
// counts this as the card's division for the round. Every claim on a card
// reconciles on its own schedule and each of them sees the same card, so
// without this the card would be divided once per claim per round.
func (s *cardDivisionState) due(card types.NamespacedName) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if last, found := s.lastRound[card]; found && now.Sub(last) < DefaultRequeueDuration {
		return false
	}
	s.lastRound[card] = now
	return true
}

// pruneLocked forgets cards not divided for a long while, which is what a
// deleted pod leaves behind. Forgetting a card that is still there costs
// nothing: a card with no entry is simply due.
func (s *cardDivisionState) pruneLocked(now time.Time) {
	const horizon = 30 * DefaultRequeueDuration
	if now.Sub(s.lastPruned) < horizon {
		return
	}
	s.lastPruned = now
	for card, last := range s.lastRound {
		if now.Sub(last) >= horizon {
			delete(s.lastRound, card)
		}
	}
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
// declare what they cost, so that each engine's share follows its load rather
// than staying what it was when the last model landed. A card nobody could
// account for is left alone, which includes a card running an engine whose
// claim declares nothing.
func (r *ModelClaimReconciler) divideCards(ctx context.Context, candidates []corev1.Pod) {
	divisions := r.divisions()
	due := make([]corev1.Pod, 0, len(candidates))
	for i := range candidates {
		card := types.NamespacedName{Namespace: candidates[i].Namespace, Name: candidates[i].Name}
		if divisions.due(card) {
			due = append(due, candidates[i])
		}
	}
	if len(due) == 0 {
		return
	}

	ledgers := r.collectPodLedgers(ctx, due[0].Namespace, due, r.freshSnapshots(ctx, due))
	for i := range due {
		pod := &due[i]
		ledger := ledgers[pod.Name]
		if !ledger.judgeable || len(ledger.engines) == 0 || !podHasGPUs(*pod, ledger.accelerators) {
			continue
		}
		if _, err := r.arrangeCard(ctx, pod, ledger, ledger.engines, loadDivision(ledger.hbmUsableBytes)); err != nil {
			klog.V(2).InfoS("could not divide a card to follow its load", "pod", klog.KObj(pod), "err", err)
		}
	}
}
