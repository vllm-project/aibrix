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
	"strings"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// servedModelName returns the model name clients address, defaulting to the
// object name when Spec.ModelName is unset.
func servedModelName(pm *modelv1alpha1.ModelClaim) string {
	if pm.Spec.ModelName != nil && *pm.Spec.ModelName != "" {
		return *pm.Spec.ModelName
	}
	return pm.Name
}

// desiredReplicas resolves the target number of active instances.
func desiredReplicas(pm *modelv1alpha1.ModelClaim) int32 {
	if pm.Spec.Replicas != nil {
		return *pm.Spec.Replicas
	}
	return 1
}

// ipcNameFor derives the kvcached shared-memory segment name for a model. It
// must be unique per GPU so co-tenant engines do not collide on /dev/shm, and
// it is sanitized to match kvcached's own normalization (it replaces characters
// like '.' and '/' with '-'); otherwise kvctl operations would target a
// different segment name than the engine actually created.
func ipcNameFor(pm *modelv1alpha1.ModelClaim) string {
	return "kvc_" + sanitizeIPCName(pm.Name)
}

// sanitizeIPCName maps any character outside [A-Za-z0-9_-] to '-', matching how
// kvcached normalizes the KVCACHED_IPC_NAME (verified on real hardware: a name
// like "kvc_qwen3-0.6b" becomes "kvc_qwen3-0-6b").
func sanitizeIPCName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// LocalityProvider is an optional advisory estimate for bringing a model's
// weights onto a node. Runtime snapshots supply the authoritative per-pod
// artifact signal; this interface remains for future node-level hints. A zero
// cost means "already hot", and uniformLocality preserves load-only fallback.
type LocalityProvider interface {
	Cost(model, nodeName string) float64
}

// uniformLocality is the default: every node looks equally cheap, so locality
// never tips the decision and placement falls back to load + name ordering.
type uniformLocality struct{}

func (uniformLocality) Cost(model, nodeName string) float64 { return 0 }

// selectPodForActivation picks a warm pod to attach the model to. Among pods not
// already hosting this model, it chooses the lowest-cost pod, ranked
// lexicographically by (locality cost, current model load, name): prefer a node
// where the weights are already hot, then the least-loaded pod for density
// spread, breaking remaining ties by name for determinism.
//
// A nil provider is treated as uniform (load-only), preserving the existing
// deterministic fallback when runtime observations are unavailable.
func selectPodForActivation(candidates []corev1.Pod, alreadyOn map[string]bool, load map[string]int, model string, locality LocalityProvider) (*corev1.Pod, error) {
	return selectPodForActivationWithState(candidates, alreadyOn, load, model, locality, nil)
}

// podRefusal is one candidate the account turned away, and the sentence an
// operator can act on.
type podRefusal struct {
	pod string
	// roomBytes is what the card can still offer, and known says whether that
	// could be worked out at all.
	roomBytes int64
	known     bool
	reason    string
}

// admissibleCandidates keeps the pods whose account can show room for one more
// instance of this claim, and says why each of the others was turned away.
//
// A pod Kubernetes gave no GPU is not judged on GPU memory: the warm-pool
// contract puts the cards in the pod spec, and a pod without them is what the
// mock runtimes in tests run on. Every other pod has to show its room, so a
// card nobody could account for is turned away rather than admitted: the
// memory such an account cannot see is memory it would hand out twice.
func admissibleCandidates(
	candidates []corev1.Pod,
	ledgers map[string]podLedger,
	minimumReserveBytes int64,
) ([]corev1.Pod, []podRefusal) {
	admissible := make([]corev1.Pod, 0, len(candidates))
	var refusals []podRefusal
	for i := range candidates {
		pod := candidates[i]
		if podGPUCount(pod) == 0 {
			admissible = append(admissible, pod)
			continue
		}
		ledger := ledgers[pod.Name]
		switch {
		case !ledger.judgeable:
			refusals = append(refusals, podRefusal{
				pod:    pod.Name,
				reason: fmt.Sprintf("%s could not be judged: %s", pod.Name, ledger.blocked),
			})
		case ledger.maximumRoomBytes() < minimumReserveBytes:
			room := ledger.maximumRoomBytes()
			refusals = append(refusals, podRefusal{
				pod:       pod.Name,
				roomBytes: room,
				known:     true,
				reason: fmt.Sprintf("%s can offer at most %s, even with every engine on it at its floor",
					pod.Name, gibibytes(room)),
			})
		case ledger.heldRoomBytes() < minimumReserveBytes:
			// The card could hold this model, and does not today. Lowering a KV
			// limit does not evict a page, so the engines there have to give
			// the memory back themselves before this pod can be tried again.
			room := ledger.heldRoomBytes()
			refusals = append(refusals, podRefusal{
				pod:       pod.Name,
				roomBytes: room,
				known:     true,
				reason: fmt.Sprintf("%s has %s free, with the rest held by the engines already on it",
					pod.Name, gibibytes(room)),
			})
		default:
			admissible = append(admissible, pod)
		}
	}
	return admissible, refusals
}

// noPlacementMessage says why no pod was chosen for a claim.
//
// The cards are blamed only when they are the reason. With no candidate at all
// the selector is what to look at, and with a pod still admissible the model is
// already on every pod it could use; an operator sent to look at GPU memory in
// either case would be looking in the wrong place.
func noPlacementMessage(selectErr error, admissible []corev1.Pod, refusals []podRefusal, minimumReserveBytes int64) string {
	if len(admissible) == 0 && len(refusals) > 0 {
		return summarizeRefusals(refusals, minimumReserveBytes)
	}
	return selectErr.Error()
}

// withoutPod returns pods less the one named, leaving the slice it was given
// untouched.
func withoutPod(pods []corev1.Pod, name string) []corev1.Pod {
	kept := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		if pods[i].Name != name {
			kept = append(kept, pods[i])
		}
	}
	return kept
}

// summarizeRefusals states in one line how far the pool is from holding this
// model. It names the roomiest pod that still could not hold it, because that
// is the smallest gap and the one worth acting on, and counts the rest rather
// than listing a line per pod.
func summarizeRefusals(refusals []podRefusal, minimumReserveBytes int64) string {
	message := fmt.Sprintf("no warm pod can hold this model, which needs %s on a card",
		gibibytes(minimumReserveBytes))
	if len(refusals) == 0 {
		return message
	}
	roomiest := 0
	for i, refusal := range refusals {
		if !refusals[roomiest].known && refusal.known {
			roomiest = i
			continue
		}
		if refusal.known && refusals[roomiest].known && refusal.roomBytes > refusals[roomiest].roomBytes {
			roomiest = i
		}
	}
	message += ": " + refusals[roomiest].reason
	if len(refusals) > 1 {
		message += fmt.Sprintf("; %d other pod(s) were turned away as well", len(refusals)-1)
	}
	return message
}

// gibibytes renders a byte count the way an operator reads a GPU: one decimal
// place, since a tenth of a gibibyte is about as fine as these decisions get.
func gibibytes(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(1<<30))
}

// rankByRoom carries the account's answer into the placement state, so two
// pods that both passed the gate are ordered by the figure the gate used. A
// card nobody could account for is left without a room, and ranks behind every
// card that has one.
//
// The direction is unchanged: the roomiest card still wins, as the freest card
// used to. Packing onto the tightest card that still fits is a different
// decision and is not taken here.
func rankByRoom(states map[string]PodPlacementState, ledgers map[string]podLedger) {
	for name, ledger := range ledgers {
		if !ledger.judgeable {
			continue
		}
		state := states[name]
		state.MaximumRoomBytes = ledger.maximumRoomBytes()
		state.MaximumRoomKnown = true
		states[name] = state
	}
}

// selectPodForActivationWithState first prefers a pod that already has the
// artifact locally, then live GPU/KV observations, and finally the Phase-1
// locality/load/name rank. Missing runtime state is safe: it simply falls back
// to the existing deterministic placement behavior.
func selectPodForActivationWithState(
	candidates []corev1.Pod,
	alreadyOn map[string]bool,
	load map[string]int,
	model string,
	locality LocalityProvider,
	states map[string]PodPlacementState,
) (*corev1.Pod, error) {
	if locality == nil {
		locality = uniformLocality{}
	}
	var best *corev1.Pod
	var bestState PodPlacementState
	var bestLoc float64
	var bestLoad int
	for i := range candidates {
		pod := &candidates[i]
		if alreadyOn[pod.Name] {
			continue
		}
		state := states[pod.Name]
		loc := locality.Cost(model, pod.Spec.NodeName)
		l := load[pod.Name]
		if best == nil || placementStateLess(state, bestState) ||
			(!placementStateLess(bestState, state) && rankLess(loc, l, pod.Name, bestLoc, bestLoad, best.Name)) {
			best, bestState, bestLoc, bestLoad = pod, state, loc, l
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no available candidate warm pod for model")
	}
	return best, nil
}

// placementStateLess returns whether a ranks ahead of b using live runtime
// state. `false` in both directions means the legacy locality/load tie-breaker
// decides the winner.
func placementStateLess(a, b PodPlacementState) bool {
	if a.ArtifactCached != b.ArtifactCached {
		return a.ArtifactCached
	}
	if a.SnapshotKnown != b.SnapshotKnown {
		return a.SnapshotKnown
	}
	if a.MemoryKnown != b.MemoryKnown {
		return a.MemoryKnown
	}
	if a.MaximumRoomKnown != b.MaximumRoomKnown {
		return a.MaximumRoomKnown
	}
	if a.MaximumRoomKnown && a.MaximumRoomBytes != b.MaximumRoomBytes {
		return a.MaximumRoomBytes > b.MaximumRoomBytes
	}
	if a.MemoryKnown && a.HBMFreeBytes != b.HBMFreeBytes {
		return a.HBMFreeBytes > b.HBMFreeBytes
	}
	if a.SnapshotKnown && a.KVUsedBytes != b.KVUsedBytes {
		return a.KVUsedBytes < b.KVUsedBytes
	}
	if a.SnapshotKnown && a.ModelCount != b.ModelCount {
		return a.ModelCount < b.ModelCount
	}
	return false
}

// rankLess reports whether candidate (loc,load,name) ranks before
// (bLoc,bLoad,bName): lower locality cost first, then lower load, then lower name.
func rankLess(loc float64, load int, name string, bLoc float64, bLoad int, bName string) bool {
	if loc != bLoc {
		return loc < bLoc
	}
	if load != bLoad {
		return load < bLoad
	}
	return name < bName
}
