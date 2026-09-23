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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// countingRuntime answers snapshots for one pod and counts how often it is
// asked.
type countingRuntime struct {
	fakeRuntime
	reads int
	err   error
}

func (c *countingRuntime) Snapshot(_ context.Context, _ string, _ int) (*RuntimeSnapshot, error) {
	c.reads++
	if c.err != nil {
		return nil, c.err
	}
	return &RuntimeSnapshot{Models: []RuntimeSnapshotModel{{ModelName: "m"}}}, nil
}

func TestRuntimeReadingsReadEachRuntimeOnce(t *testing.T) {
	runtime := &countingRuntime{}
	readings := newRuntimeReadings(runtime)
	pod := warmPod("warm-1", "pool", true, corev1.PodRunning)

	first, err := readings.of(context.Background(), pod)
	require.NoError(t, err)
	second, err := readings.of(context.Background(), pod)
	require.NoError(t, err)

	assert.Same(t, first, second)
	assert.Equal(t, 1, runtime.reads)
}

func TestRuntimeReadingsDoNotAskARuntimeThatFailedAgain(t *testing.T) {
	runtime := &countingRuntime{err: errors.New("connection refused")}
	readings := newRuntimeReadings(runtime)
	pod := warmPod("warm-1", "pool", true, corev1.PodRunning)

	_, first := readings.of(context.Background(), pod)
	_, second := readings.of(context.Background(), pod)

	require.Error(t, first)
	require.Error(t, second)
	assert.Equal(t, 1, runtime.reads)
	assert.Empty(t, readings.ofPods(context.Background(), []corev1.Pod{*pod}))
}

func TestRuntimeReadingsReadAgainOnlyWhatWasChanged(t *testing.T) {
	runtime := &countingRuntime{}
	readings := newRuntimeReadings(runtime)
	pod := warmPod("warm-1", "pool", true, corev1.PodRunning)
	_, err := readings.of(context.Background(), pod)
	require.NoError(t, err)

	confirmed := &RuntimeSnapshot{}
	readings.replace(pod, confirmed)
	got, err := readings.of(context.Background(), pod)
	require.NoError(t, err)
	assert.Same(t, confirmed, got)
	assert.Equal(t, 1, runtime.reads)

	readings.forget(pod.Name)
	_, err = readings.of(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 2, runtime.reads)
}

func TestPlacementStateFromSnapshot(t *testing.T) {
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 800},
			{ID: "GPU-1", HBMFreeBytes: 300},
		},
		Models: []RuntimeSnapshotModel{
			{ModelName: "m1", KVUsedBytes: 10},
			{ModelName: "m2", KVUsedBytes: 25},
		},
		CachedArtifacts: []string{"hf://Org/M1"},
	}

	singleGPUState := placementStateFromSnapshot(snapshot, "hf://Org/M1", 1)
	groupState := placementStateFromSnapshot(snapshot, "hf://Org/M1", 2)

	assert.True(t, singleGPUState.SnapshotKnown)
	assert.True(t, singleGPUState.ArtifactCached)
	assert.True(t, singleGPUState.MemoryKnown)
	assert.Equal(t, int64(800), singleGPUState.HBMFreeBytes)
	assert.Equal(t, int64(300), groupState.HBMFreeBytes)
	assert.Equal(t, int64(35), groupState.KVUsedBytes)
	assert.Equal(t, 2, groupState.ModelCount)
}

func TestPlacementStateSizesAPodByItsSmallestCard(t *testing.T) {
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 800, HBMUsableBytes: 1000},
			{ID: "GPU-1", HBMFreeBytes: 300, HBMUsableBytes: 900},
		},
	}

	state := placementStateFromSnapshot(snapshot, "hf://Org/M1", 2)

	assert.True(t, state.HBMUsableKnown)
	assert.Equal(t, int64(900), state.HBMUsableBytes)
}

func TestPlacementStateLeavesAPodUnsizedWhenACardCannotBeMeasured(t *testing.T) {
	unmeasured := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 800, HBMUsableBytes: 1000},
			{ID: "GPU-1", HBMFreeBytes: 300, HBMUsableBytes: -1},
		},
	}
	cardless := &RuntimeSnapshot{}

	unmeasuredState := placementStateFromSnapshot(unmeasured, "hf://Org/M1", 2)
	cardlessState := placementStateFromSnapshot(cardless, "hf://Org/M1", 1)

	assert.False(t, unmeasuredState.HBMUsableKnown)
	assert.Equal(t, int64(0), unmeasuredState.HBMUsableBytes)
	assert.False(t, cardlessState.HBMUsableKnown)
}

func TestPlacementStateLeavesAPodUnsizedWhenCardCountMissesParallelism(t *testing.T) {
	snapshot := &RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMFreeBytes: 800, HBMUsableBytes: 1000},
		},
	}

	state := placementStateFromSnapshot(snapshot, "hf://Org/M1", 2)

	assert.False(t, state.MemoryKnown)
	assert.False(t, state.HBMUsableKnown)
}
