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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states, 0,
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
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states, 0,
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
		0,
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

// --- admission control -----------------------------------------------------

func TestPodFitsAdmitsWhenInformationIsMissing(t *testing.T) {
	// The whole point of the check is to stop placements that are known to
	// fail. Anything it cannot judge must behave exactly as it did before the
	// check existed, or the change is a regression for every model whose size
	// has never been observed.
	cases := map[string]struct {
		state     PodPlacementState
		footprint int64
	}{
		"no runtime snapshot":     {PodPlacementState{}, 8},
		"snapshot without memory": {PodPlacementState{SnapshotKnown: true, HBMFreeBytes: 1}, 8},
		"nothing to reserve":      {PodPlacementState{SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 1}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.True(t, podFits(tc.state, tc.footprint))
		})
	}
}

func TestPodFitsComparesFootprintAndFloorAgainstFreeMemory(t *testing.T) {
	state := func(free, floor int64) PodPlacementState {
		return PodPlacementState{
			SnapshotKnown: true, MemoryKnown: true,
			HBMFreeBytes: free, FloorBytes: floor,
		}
	}
	cases := map[string]struct {
		state     PodPlacementState
		footprint int64
		want      bool
	}{
		"room to spare":            {state(100, 10), 50, true},
		"exactly enough":           {state(100, 10), 90, true},
		"one byte short":           {state(100, 10), 91, false},
		"floor alone does not fit": {state(5, 10), 0, false},
		"no floor declared":        {state(100, 0), 100, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, podFits(tc.state, tc.footprint))
		})
	}
}

func TestSelectPodForActivationSkipsPodsWithoutRoom(t *testing.T) {
	// Ranking puts a cached artifact ahead of free memory, so without a filter
	// it would choose the full pod and the engine would die there. Admission has
	// to be a filter around the ranking rather than another ranking term.
	candidates := []corev1.Pod{namedPod("cached-but-full"), namedPod("empty")}
	states := map[string]PodPlacementState{
		"cached-but-full": {
			SnapshotKnown: true, ArtifactCached: true, MemoryKnown: true,
			HBMFreeBytes: 2_000,
		},
		"empty": {
			SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 60_000,
		},
	}

	got, err := selectPodForActivationWithState(
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states, 7_800,
	)
	require.NoError(t, err)
	assert.Equal(t, "empty", got.Name, "a pod that cannot hold the model must lose to one that can")
}

func TestSelectPodForActivationReportsInsufficientCapacity(t *testing.T) {
	candidates := []corev1.Pod{namedPod("small"), namedPod("smaller")}
	states := map[string]PodPlacementState{
		"small":   {SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 100},
		"smaller": {SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 10},
	}

	_, err := selectPodForActivationWithState(
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states, 5_000,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInsufficientCapacity,
		"a full pool must be distinguishable from a selector that matched nothing")
}

func TestSelectPodForActivationReportsNoMatchWhenEveryPodIsTaken(t *testing.T) {
	candidates := []corev1.Pod{namedPod("only")}
	_, err := selectPodForActivationWithState(
		candidates, map[string]bool{"only": true}, map[string]int{}, "m", uniformLocality{},
		map[string]PodPlacementState{}, 5_000,
	)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errInsufficientCapacity,
		"a pod already hosting the model is not a capacity problem")
}

func TestSelectPodForActivationKeepsRankingAmongFeasiblePods(t *testing.T) {
	// Regression guard: admission removes candidates, it never reorders the
	// survivors. Both pods fit here, so the pre-existing preference wins.
	candidates := []corev1.Pod{namedPod("cached"), namedPod("roomier")}
	states := map[string]PodPlacementState{
		"cached":  {SnapshotKnown: true, ArtifactCached: true, MemoryKnown: true, HBMFreeBytes: 10_000},
		"roomier": {SnapshotKnown: true, MemoryKnown: true, HBMFreeBytes: 90_000},
	}

	got, err := selectPodForActivationWithState(
		candidates, map[string]bool{}, map[string]int{}, "m", uniformLocality{}, states, 1_000,
	)
	require.NoError(t, err)
	assert.Equal(t, "cached", got.Name, "artifact locality must still outrank free memory when both fit")
}

func TestPlacementStateFromSnapshotMeasuresLargestFootprint(t *testing.T) {
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{{HBMTotalBytes: 1000, HBMFreeBytes: 400}},
		Models: []RuntimeSnapshotModel{
			{ModelName: "small", KVUsedBytes: 20, HBMPeakBytes: 100},
			{ModelName: "large", KVUsedBytes: 50, HBMPeakBytes: 300},
			{ModelName: "unreported", KVUsedBytes: 10, HBMPeakBytes: 0},
		},
	}

	state := placementStateFromSnapshot(snapshot, "artifact", 1)
	assert.Equal(t, int64(250), state.MaxFootprintBytes, "largest engine is 300 held minus 50 of KV")
	assert.Equal(t, int64(80), state.KVUsedBytes)
}

func TestPlacementStateFromSnapshotIgnoresEnginesWithoutMemoryReadings(t *testing.T) {
	// A runtime without NVML reports zero, which would otherwise subtract KV
	// and leave a negative footprint standing in for a real measurement.
	snapshot := &RuntimeSnapshot{
		Models: []RuntimeSnapshotModel{{ModelName: "m", KVUsedBytes: 700, HBMPeakBytes: 0}},
	}
	state := placementStateFromSnapshot(snapshot, "artifact", 1)
	assert.Zero(t, state.MaxFootprintBytes)
}

func claimWithFootprint(artifact string, footprintArtifact string, bytes int64) *modelv1alpha1.ModelClaim {
	claim := &modelv1alpha1.ModelClaim{}
	claim.Spec.ArtifactURL = artifact
	if footprintArtifact != "" {
		claim.Status.ObservedFootprint = &modelv1alpha1.ModelClaimObservedFootprint{
			ArtifactURL: footprintArtifact,
			Bytes:       bytes,
		}
	}
	return claim
}

func TestPlacementFootprintPrefersMeasurementOverStandIn(t *testing.T) {
	states := map[string]PodPlacementState{
		"a": {MaxFootprintBytes: 900},
		"b": {MaxFootprintBytes: 300},
	}

	measured := claimWithFootprint("hf://small", "hf://small", 500)
	assert.Equal(t, int64(500), placementFootprintBytes(measured, states),
		"a measurement of this model beats the size of its neighbours")

	unmeasured := claimWithFootprint("hf://small", "", 0)
	assert.Equal(t, int64(900), placementFootprintBytes(unmeasured, states),
		"without a measurement, assume the model is as large as the largest one running")

	assert.Zero(t, placementFootprintBytes(unmeasured, map[string]PodPlacementState{}),
		"with nothing observed anywhere, reserve nothing and admit every pod")
}

func TestPlacementFootprintIgnoresAMeasurementOfDifferentWeights(t *testing.T) {
	// spec.artifactURL is mutable and changing it does not restart a running
	// engine, so the recorded footprint can belong to weights the claim no
	// longer asks for. Sizing a larger model from the smaller one it replaced
	// would place it somewhere it cannot fit, which is the failure this check
	// exists to prevent.
	states := map[string]PodPlacementState{"a": {MaxFootprintBytes: 40_000}}

	repointed := claimWithFootprint("hf://large", "hf://small", 7_200)
	assert.Equal(t, int64(40_000), placementFootprintBytes(repointed, states),
		"a measurement of the previous artifact must not size the new one")

	assert.Zero(t, placementFootprintBytes(repointed, map[string]PodPlacementState{}),
		"with no stand-in either, admit rather than size from the wrong artifact")
}

func TestRecordObservedFootprintOnlyTrustsReadyEngines(t *testing.T) {
	ready := &RuntimeSnapshotModel{ArtifactURL: "hf://small", KVUsedBytes: 700, HBMPeakBytes: 7900}

	claim := &modelv1alpha1.ModelClaim{}
	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActive, ready)
	require.NotNil(t, claim.Status.ObservedFootprint)
	assert.Equal(t, int64(7200), claim.Status.ObservedFootprint.Bytes)
	assert.Equal(t, "hf://small", claim.Status.ObservedFootprint.ArtifactURL)

	unchanged := func(t *testing.T, reason string) {
		t.Helper()
		require.NotNil(t, claim.Status.ObservedFootprint, reason)
		assert.Equal(t, int64(7200), claim.Status.ObservedFootprint.Bytes, reason)
		assert.Equal(t, "hf://small", claim.Status.ObservedFootprint.ArtifactURL, reason)
	}

	// An engine that is still booting has not captured its CUDA graphs, so its
	// reading understates the model and must not overwrite a good measurement.
	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActivating,
		&RuntimeSnapshotModel{ArtifactURL: "hf://small", KVUsedBytes: 100, HBMPeakBytes: 3500})
	unchanged(t, "a booting engine must not overwrite a ready measurement")

	// A runtime with no NVML reports zero, which would otherwise record a
	// negative footprint once KV is subtracted.
	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActive,
		&RuntimeSnapshotModel{ArtifactURL: "hf://small", KVUsedBytes: 700, HBMPeakBytes: 0})
	unchanged(t, "a runtime without an HBM reading must not record")

	// An older runtime that reports no artifact leaves the measurement
	// unattributed, and placement could not tell whether it still applies.
	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActive,
		&RuntimeSnapshotModel{KVUsedBytes: 700, HBMPeakBytes: 9000})
	unchanged(t, "an unattributed measurement must not be recorded")

	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActive, nil)
	unchanged(t, "a missing snapshot must not record")
}

func TestRecordObservedFootprintAttributesToTheEngineNotTheSpec(t *testing.T) {
	// The claim has been repointed at new weights, but the engine is still
	// serving the old ones because a spec change does not restart it. Recording
	// the spec's artifact here would label the old model's footprint as the new
	// model's and defeat the staleness check entirely.
	claim := &modelv1alpha1.ModelClaim{}
	claim.Spec.ArtifactURL = "hf://large"

	recordObservedFootprint(claim, modelv1alpha1.ModelClaimActive,
		&RuntimeSnapshotModel{ArtifactURL: "hf://small", KVUsedBytes: 700, HBMPeakBytes: 7900})

	require.NotNil(t, claim.Status.ObservedFootprint)
	assert.Equal(t, "hf://small", claim.Status.ObservedFootprint.ArtifactURL,
		"the footprint belongs to the weights the engine is running, not the ones spec asks for")
}
