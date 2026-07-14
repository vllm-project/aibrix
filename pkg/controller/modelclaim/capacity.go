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
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// vLLM's upstream default is 0.9. The ModelClaim controller treats this
	// existing engine argument as an envelope rather than adding a second,
	// user-facing GPU-resource field to the CRD.
	defaultVLLMHBMReservationFraction = 0.9
	capacityFractionEpsilon           = 1e-9
	defaultCapacityReservationTTL     = 2 * time.Minute
)

// CapacityRequirement is the per-visible-GPU HBM envelope requested by an
// engine. Known is false for engines without an established envelope, which
// intentionally preserves the Phase-2 placement fallback.
type CapacityRequirement struct {
	Known               bool
	ReservationFraction float64
}

// CapacityState summarizes whether a fresh runtime snapshot can admit one
// more engine. Fractions are per GPU because a fixed TP/PP pool presents the
// same whole GPU group to every engine it hosts.
type CapacityState struct {
	Known             bool
	Fits              bool
	RequestedFraction float64
	ReservedFraction  float64
	FreeFraction      float64
}

type capacityReservationPodKey struct {
	types.NamespacedName
	PodUID types.UID
}

type pendingCapacityReservation struct {
	fraction  float64
	expiresAt time.Time
}

// capacityReservationCache closes the short observation race between an
// accepted Activate request and the next runtime snapshot. It is deliberately
// process-local and expires conservatively; a sidecar remains authoritative
// once it reports the newly resident engine.
type capacityReservationCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[capacityReservationPodKey]map[string]pendingCapacityReservation
}

func newCapacityReservationCache(ttl time.Duration, now func() time.Time) *capacityReservationCache {
	if ttl <= 0 {
		ttl = defaultCapacityReservationTTL
	}
	if now == nil {
		now = time.Now
	}
	return &capacityReservationCache{
		ttl:     ttl,
		now:     now,
		entries: make(map[capacityReservationPodKey]map[string]pendingCapacityReservation),
	}
}

// Reserve atomically reserves an engine envelope against the snapshot state
// that selected the pod. It protects both the declared total envelope and the
// current physical free HBM from concurrent reconciles using the same snapshot.
func (c *capacityReservationCache) Reserve(
	pod types.NamespacedName,
	podUID types.UID,
	claimID string,
	fraction float64,
	state CapacityState,
) bool {
	if c == nil || !state.Known || claimID == "" || fraction <= 0 || fraction > 1 {
		return false
	}

	now := c.now()
	key := capacityReservationPodKey{NamespacedName: pod, PodUID: podUID}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(now)

	reservations := c.entries[key]
	pending := 0.0
	for id, reservation := range reservations {
		if id != claimID {
			pending += reservation.fraction
		}
	}
	if state.ReservedFraction+pending+fraction > 1+capacityFractionEpsilon ||
		pending+fraction > state.FreeFraction+capacityFractionEpsilon {
		return false
	}
	if reservations == nil {
		reservations = make(map[string]pendingCapacityReservation)
		c.entries[key] = reservations
	}
	reservations[claimID] = pendingCapacityReservation{
		fraction:  fraction,
		expiresAt: now.Add(c.ttl),
	}
	return true
}

// PendingFraction returns unobserved reservations for one concrete pod
// incarnation. A Pod replacement with the same name cannot inherit them.
func (c *capacityReservationCache) PendingFraction(pod types.NamespacedName, podUID types.UID) float64 {
	if c == nil {
		return 0
	}
	now := c.now()
	key := capacityReservationPodKey{NamespacedName: pod, PodUID: podUID}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(now)
	return reservationFraction(c.entries[key])
}

// Observe clears pending entries that the authoritative sidecar snapshot has
// incorporated. Engines from an older runtime lack ClaimRef and simply expire.
func (c *capacityReservationCache) Observe(
	pod types.NamespacedName,
	podUID types.UID,
	snapshot *RuntimeSnapshot,
) {
	if c == nil || snapshot == nil {
		return
	}
	key := capacityReservationPodKey{NamespacedName: pod, PodUID: podUID}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(c.now())
	reservations := c.entries[key]
	if len(reservations) == 0 {
		return
	}
	for _, model := range snapshot.Models {
		if model.ClaimRef != nil && model.ClaimRef.UID != "" {
			delete(reservations, model.ClaimRef.UID)
		}
	}
	if len(reservations) == 0 {
		delete(c.entries, key)
	}
}

// Release drops a pending reservation after the runtime rejects the activation.
func (c *capacityReservationCache) Release(pod types.NamespacedName, podUID types.UID, claimID string) {
	if c == nil || claimID == "" {
		return
	}
	key := capacityReservationPodKey{NamespacedName: pod, PodUID: podUID}
	c.mu.Lock()
	defer c.mu.Unlock()
	reservations := c.entries[key]
	delete(reservations, claimID)
	if len(reservations) == 0 {
		delete(c.entries, key)
	}
}

func (c *capacityReservationCache) expireLocked(now time.Time) {
	for key, reservations := range c.entries {
		for claimID, reservation := range reservations {
			if !reservation.expiresAt.After(now) {
				delete(reservations, claimID)
			}
		}
		if len(reservations) == 0 {
			delete(c.entries, key)
		}
	}
}

func reservationFraction(reservations map[string]pendingCapacityReservation) float64 {
	total := 0.0
	for _, reservation := range reservations {
		total += reservation.fraction
	}
	return total
}

// CapacityProvider derives an engine's HBM envelope and evaluates it against
// a runtime snapshot. It is deliberately controller-local and non-persistent:
// the sidecar snapshot remains the source of truth after a controller restart.
type CapacityProvider interface {
	Requirement(pm *modelv1alpha1.ModelClaim) (CapacityRequirement, error)
	State(snapshot *RuntimeSnapshot, requirement CapacityRequirement, parallelism int64) CapacityState
}

// engineArgCapacityProvider uses vLLM's existing --gpu-memory-utilization
// argument as the engine envelope. This keeps ModelClaim resource-free while
// still making co-resident vLLM engines admit only when their declared maxima
// fit the fixed topology pool.
type engineArgCapacityProvider struct{}

func (engineArgCapacityProvider) Requirement(pm *modelv1alpha1.ModelClaim) (CapacityRequirement, error) {
	if !isVLLMModel(pm) {
		return CapacityRequirement{}, nil
	}

	fraction := defaultVLLMHBMReservationFraction
	if pm.Spec.EngineConfig != nil {
		if raw, found := pm.Spec.EngineConfig.Args["--gpu-memory-utilization"]; found {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 || parsed > 1 {
				return CapacityRequirement{}, fmt.Errorf("--gpu-memory-utilization must be in (0, 1]")
			}
			fraction = parsed
		}
	}
	return CapacityRequirement{Known: true, ReservationFraction: fraction}, nil
}

func (engineArgCapacityProvider) State(
	snapshot *RuntimeSnapshot,
	requirement CapacityRequirement,
	parallelism int64,
) CapacityState {
	if !requirement.Known || snapshot == nil || len(snapshot.Accelerators) == 0 {
		return CapacityState{}
	}
	if parallelism < 1 {
		parallelism = 1
	}
	// A mismatched visible set cannot safely attribute a per-engine envelope to
	// a fixed GPU group, including the single-GPU case.
	if int64(len(snapshot.Accelerators)) != parallelism {
		return CapacityState{}
	}

	minTotalBytes := int64(0)
	state := CapacityState{Known: true, FreeFraction: 1}
	for _, accelerator := range snapshot.Accelerators {
		if accelerator.HBMTotalBytes <= 0 || accelerator.HBMFreeBytes < 0 {
			return CapacityState{}
		}
		if minTotalBytes == 0 || accelerator.HBMTotalBytes < minTotalBytes {
			minTotalBytes = accelerator.HBMTotalBytes
		}
		free := float64(accelerator.HBMFreeBytes) / float64(accelerator.HBMTotalBytes)
		state.FreeFraction = math.Min(state.FreeFraction, math.Min(free, 1))
	}

	for _, model := range snapshot.Models {
		reservation := model.HBMReservationFraction
		if reservation <= 0 || reservation > 1 || math.IsNaN(reservation) || math.IsInf(reservation, 0) {
			// A measured peak is only the resident engine's current footprint. It
			// cannot bound future KV growth without the launch envelope, so an old
			// runtime/model must retain the Phase-2 placement fallback.
			return CapacityState{}
		}
		if model.HBMPeakBytes > 0 {
			// A runtime can report a footprint larger than its original envelope
			// (for example after a version change). The live measurement becomes
			// the conservative floor rather than letting the ledger undercount it.
			reservation = math.Max(reservation, float64(model.HBMPeakBytes)/float64(minTotalBytes))
		}
		if reservation <= 0 || reservation > 1 || math.IsNaN(reservation) || math.IsInf(reservation, 0) {
			// A malformed observation cannot be safely converted into an
			// envelope. Preserve Phase-2 behavior instead of guessing.
			return CapacityState{}
		}
		state.ReservedFraction += reservation
	}

	state.RequestedFraction = requirement.ReservationFraction
	return applyPendingCapacity(state, 0)
}

// applyPendingCapacity folds requests accepted after the snapshot was taken
// into a state before it is ranked. The active reservation total is already
// represented by the snapshot; only pending activation envelopes are added.
func applyPendingCapacity(state CapacityState, pendingFraction float64) CapacityState {
	if !state.Known {
		return state
	}
	state.Fits = state.ReservedFraction+pendingFraction+state.RequestedFraction <= 1+capacityFractionEpsilon &&
		pendingFraction+state.RequestedFraction <= state.FreeFraction+capacityFractionEpsilon
	return state
}
