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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func namedPod(name string) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestSelectPodForActivation_LeastLoaded(t *testing.T) {
	cands := []corev1.Pod{namedPod("a"), namedPod("b"), namedPod("c")}
	load := map[string]int{"a": 2, "b": 0, "c": 1}
	got, err := selectPodForActivation(cands, map[string]bool{}, load, "m", uniformLocality{})
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name)
}

func TestSelectPodForActivation_SkipsAlreadyOn(t *testing.T) {
	cands := []corev1.Pod{namedPod("a"), namedPod("b")}
	load := map[string]int{"a": 0, "b": 5}
	got, err := selectPodForActivation(cands, map[string]bool{"a": true}, load, "m", uniformLocality{})
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name, "a is excluded even though least loaded")
}

func TestSelectPodForActivation_TieBreakByName(t *testing.T) {
	cands := []corev1.Pod{namedPod("z"), namedPod("a")}
	load := map[string]int{"z": 0, "a": 0}
	got, err := selectPodForActivation(cands, map[string]bool{}, load, "m", uniformLocality{})
	require.NoError(t, err)
	assert.Equal(t, "a", got.Name)
}

func TestSelectPodForActivation_NoCapacity(t *testing.T) {
	cands := []corev1.Pod{namedPod("a")}
	_, err := selectPodForActivation(cands, map[string]bool{"a": true}, map[string]int{}, "m", uniformLocality{})
	assert.Error(t, err)
}

func TestServedModelName(t *testing.T) {
	pm := &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	assert.Equal(t, "foo", servedModelName(pm))
	name := "bar"
	pm.Spec.ModelName = &name
	assert.Equal(t, "bar", servedModelName(pm))
}

func TestIpcNameFor(t *testing.T) {
	pm := &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	assert.Equal(t, "kvc_foo", ipcNameFor(pm))

	// Sanitized to match kvcached's normalization (verified on real hardware):
	// '.' and '/' become '-', existing '-' is kept.
	dotted := &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "qwen3-0.6b"}}
	assert.Equal(t, "kvc_qwen3-0-6b", ipcNameFor(dotted))
	slashed := &modelv1alpha1.ModelClaim{ObjectMeta: metav1.ObjectMeta{Name: "Qwen/Qwen2-7B"}}
	assert.Equal(t, "kvc_Qwen-Qwen2-7B", ipcNameFor(slashed))
}

func TestDesiredReplicas(t *testing.T) {
	pm := &modelv1alpha1.ModelClaim{}
	assert.Equal(t, int32(1), desiredReplicas(pm))
	one := int32(1)
	pm.Spec.Replicas = &one
	assert.Equal(t, int32(1), desiredReplicas(pm))
}

// fakeLocality maps nodeName -> load cost for tests (0 = weights already hot).
type fakeLocality map[string]float64

func (f fakeLocality) Cost(model, nodeName string) float64 { return f[nodeName] }

func podOnNode(name, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func TestSelectPodForActivation_LocalityDominatesLoad(t *testing.T) {
	// "hot" sits on a node whose store already has the weights (cost 0) but is
	// busier; "cold" is idle but on a node that must stage weights (cost 5).
	cands := []corev1.Pod{podOnNode("cold", "n-cold"), podOnNode("hot", "n-hot")}
	load := map[string]int{"cold": 0, "hot": 3}
	loc := fakeLocality{"n-hot": 0, "n-cold": 5}
	got, err := selectPodForActivation(cands, map[string]bool{}, load, "m", loc)
	require.NoError(t, err)
	assert.Equal(t, "hot", got.Name, "lower locality cost wins over lower load")
}

func TestSelectPodForActivation_LoadBreaksEqualLocality(t *testing.T) {
	// Two nodes equally hot (cost 0): fall back to least-loaded.
	cands := []corev1.Pod{podOnNode("a", "n1"), podOnNode("b", "n2")}
	load := map[string]int{"a": 2, "b": 1}
	loc := fakeLocality{"n1": 0, "n2": 0}
	got, err := selectPodForActivation(cands, map[string]bool{}, load, "m", loc)
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name)
}

func TestSelectPodForActivation_NilLocalityIsUniform(t *testing.T) {
	// A nil provider must not panic and must behave like load-only selection.
	cands := []corev1.Pod{podOnNode("a", "n1"), podOnNode("b", "n2")}
	load := map[string]int{"a": 5, "b": 0}
	got, err := selectPodForActivation(cands, map[string]bool{}, load, "m", nil)
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name)
}

func TestSelectPodForActivationWithStatePrefersLiveRuntimeState(t *testing.T) {
	candidates := []corev1.Pod{namedPod("cold"), namedPod("hot")}
	states := map[string]PodPlacementState{
		"cold": {
			SnapshotKnown: true,
			MemoryKnown:   true,
			HBMFreeBytes:  900,
			KVUsedBytes:   10,
			ModelCount:    1,
		},
		"hot": {
			SnapshotKnown:  true,
			ArtifactCached: true,
			MemoryKnown:    true,
			HBMFreeBytes:   100,
			KVUsedBytes:    100,
			ModelCount:     3,
		},
	}

	got, err := selectPodForActivationWithState(
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states,
	)
	require.NoError(t, err)
	assert.Equal(t, "hot", got.Name, "cached artifact wins before live memory tie-breakers")
}

func TestSelectPodForActivationWithStateRanksMemoryAndKV(t *testing.T) {
	candidates := []corev1.Pod{namedPod("busy"), namedPod("free")}
	states := map[string]PodPlacementState{
		"busy": {
			SnapshotKnown: true,
			MemoryKnown:   true,
			HBMFreeBytes:  500,
			KVUsedBytes:   10,
			ModelCount:    1,
		},
		"free": {
			SnapshotKnown: true,
			MemoryKnown:   true,
			HBMFreeBytes:  600,
			KVUsedBytes:   100,
			ModelCount:    3,
		},
	}

	got, err := selectPodForActivationWithState(
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states,
	)
	require.NoError(t, err)
	assert.Equal(t, "free", got.Name, "higher free HBM wins before KV/model-count tie-breakers")
}

func TestSelectPodForActivationWithStateFallsBackForUnknownSnapshots(t *testing.T) {
	candidates := []corev1.Pod{namedPod("busy"), namedPod("idle")}
	got, err := selectPodForActivationWithState(
		candidates,
		map[string]bool{},
		map[string]int{"busy": 2, "idle": 0},
		"m",
		uniformLocality{},
		map[string]PodPlacementState{},
	)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Name)
}

func TestUniformLocality_AlwaysZero(t *testing.T) {
	assert.Zero(t, uniformLocality{}.Cost("m", "any-node"))
}

func TestPruneDeadInstances(t *testing.T) {
	pm := &modelv1alpha1.ModelClaim{}
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "alive", Port: 20000},
		{Pod: "gone", Port: 20001},
	}
	pruneDeadInstances(pm, []corev1.Pod{namedPod("alive")})
	require.Len(t, pm.Status.Instances, 1)
	assert.Equal(t, "alive", pm.Status.Instances[0].Pod,
		"instance on a vanished warm pod must be dropped so re-activation can run")

	// No candidates at all: every instance is stale.
	pruneDeadInstances(pm, nil)
	assert.Empty(t, pm.Status.Instances)
}

func gpuPod(name string) corev1.Pod {
	pod := namedPod(name)
	pod.Spec.Containers = []corev1.Container{{
		Name: "aibrix-runtime",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(1, resource.DecimalSI),
		}},
	}}
	return pod
}

func TestAdmissibleCandidatesKeepsOnlyPodsThatCanShowRoom(t *testing.T) {
	candidates := []corev1.Pod{
		gpuPod("roomy"), gpuPod("full"), gpuPod("unreadable"), namedPod("cpu-only"),
	}
	ledgers := map[string]podLedger{
		"roomy": {judgeable: true, hbmUsableBytes: 95 << 30, totalMinimumReserveBytes: 40 << 30,
			totalHeldBytes: 40 << 30, engines: make([]engineOnPod, 1)},
		"full": {judgeable: true, hbmUsableBytes: 95 << 30, totalMinimumReserveBytes: 80 << 30,
			totalHeldBytes: 80 << 30, engines: make([]engineOnPod, 2)},
		"unreadable": {blocked: "its runtime did not answer"},
	}

	admissible, refusals := admissibleCandidates(candidates, ledgers, 40<<30)

	require.Len(t, admissible, 2)
	assert.Equal(t, "roomy", admissible[0].Name)
	assert.Equal(t, "cpu-only", admissible[1].Name)
	require.Len(t, refusals, 2)
	assert.Equal(t,
		"full can offer at most 15.0 GiB, even with every engine on it at its floor "+
			"(the card holds 95.0 GiB, and 80.0 GiB of it is promised to 2 instance(s))",
		refusals[0].reason)
	assert.True(t, refusals[0].known)
	assert.Equal(t, "unreadable could not be judged: its runtime did not answer", refusals[1].reason)
	assert.False(t, refusals[1].known)
}

func TestAdmissibleCandidatesTurnsAwayACardWhoseRoomIsHeld(t *testing.T) {
	candidates := []corev1.Pod{gpuPod("held")}
	// The card could hold the model once its engines give their pages back,
	// and they have not.
	ledgers := map[string]podLedger{
		"held": {judgeable: true, hbmUsableBytes: 95 << 30, totalMinimumReserveBytes: 40 << 30,
			totalHeldBytes: 70 << 30, engines: make([]engineOnPod, 1)},
	}

	admissible, refusals := admissibleCandidates(candidates, ledgers, 40<<30)

	assert.Empty(t, admissible)
	require.Len(t, refusals, 1)
	assert.Equal(t,
		"held has 25.0 GiB free, with the rest held by the engines already on it "+
			"(the card holds 95.0 GiB, and 70.0 GiB of it is held by 1 instance(s))",
		refusals[0].reason)
	assert.True(t, refusals[0].known)
	assert.Equal(t, int64(25)<<30, refusals[0].roomBytes)
}

func TestRankingPrefersTheCardWithTheMostRoomNotTheMostFreeMemory(t *testing.T) {
	// The tight card has more free memory right now, because the roomy card's
	// engine has mapped KV it is entitled to. Free memory is not room.
	states := map[string]PodPlacementState{
		"roomy": {SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 100},
		"tight": {SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 900},
	}
	ledgers := map[string]podLedger{
		"roomy": {judgeable: true, hbmUsableBytes: 1000, totalMinimumReserveBytes: 100},
		"tight": {judgeable: true, hbmUsableBytes: 1000, totalMinimumReserveBytes: 800},
	}

	rankByRoom(states, ledgers)

	assert.True(t, placementStateLess(states["roomy"], states["tight"]),
		"the card with 900 of room should rank ahead of the one with 200")
	assert.False(t, placementStateLess(states["tight"], states["roomy"]))
}

func TestRankingPutsACardWithoutAnAccountLast(t *testing.T) {
	states := map[string]PodPlacementState{
		"judged":  {SnapshotKnown: true, MemoryKnown: true},
		"unknown": {SnapshotKnown: true, MemoryKnown: true},
	}
	ledgers := map[string]podLedger{
		"judged":  {judgeable: true, hbmUsableBytes: 1000, totalMinimumReserveBytes: 900},
		"unknown": {blocked: "its runtime did not answer"},
	}

	rankByRoom(states, ledgers)

	assert.True(t, placementStateLess(states["judged"], states["unknown"]))
	assert.False(t, placementStateLess(states["unknown"], states["judged"]))
}

func TestSummarizeRefusalsNamesTheRoomiestPodThatStillCannotHold(t *testing.T) {
	refusals := []podRefusal{
		{pod: "tight", roomBytes: 1 << 30, known: true, reason: "tight can offer at most 1.0 GiB"},
		{pod: "roomier", roomBytes: 3 << 30, known: true, reason: "roomier can offer at most 3.0 GiB"},
		{pod: "unreadable", reason: "unreadable could not be judged: its runtime did not answer"},
	}

	message := summarizeRefusals(refusals, 4<<30)

	assert.Equal(t,
		"no warm pod can hold this model, which needs 4.0 GiB on a card: "+
			"roomier can offer at most 3.0 GiB; 2 other pod(s) were turned away as well",
		message)
}

func TestSummarizeRefusalsFallsBackToAPodItCouldNotJudge(t *testing.T) {
	refusals := []podRefusal{
		{pod: "unreadable", reason: "unreadable could not be judged: its cards could not be measured"},
	}

	message := summarizeRefusals(refusals, 2<<30)

	assert.Equal(t,
		"no warm pod can hold this model, which needs 2.0 GiB on a card: "+
			"unreadable could not be judged: its cards could not be measured",
		message)
}

func TestNoPlacementMessageBlamesTheCardsOnlyWhenTheyAreTheReason(t *testing.T) {
	generic := errors.New("no available candidate warm pod for model")
	tooSmall := []podRefusal{{pod: "warm-1", roomBytes: 10 << 30, known: true,
		reason: "warm-1 can offer at most 10.0 GiB"}}
	onIt := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "warm-2"}}}

	// No candidate at all: the selector is the thing to look at.
	assert.Equal(t, generic.Error(), noPlacementMessage(generic, nil, nil, 40<<30))
	// A pod is still admissible, so the model is already on every pod it could
	// use, even though another pod was turned away for room.
	assert.Equal(t, generic.Error(), noPlacementMessage(generic, onIt, tooSmall, 40<<30))
	// Every candidate was turned away, so say which card came closest.
	message := noPlacementMessage(generic, nil, tooSmall, 40<<30)
	assert.Contains(t, message, "needs 40.0 GiB")
	assert.Contains(t, message, "warm-1 can offer at most 10.0 GiB")
}
