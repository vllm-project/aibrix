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

	corev1 "k8s.io/api/core/v1"
)

// runtimeReadings is what each pod's runtime reported during one pass of the
// reconciler. Every step of a pass reads a runtime through it: the account,
// the ranking, the health check, the pool policy and the division of a card.
// So a runtime is read once a pass, and the steps agree on what they saw.
//
// A step that changes a runtime replaces its reading with the one it took to
// confirm the change, or forgets the reading, so the next step to ask reads the
// runtime again. No step acts on a reading it knows is out of date.
//
// A reading lives for one pass. Nothing carries over between passes, so an
// account is never built from what a runtime said before this pass began.
type runtimeReadings struct {
	runtime RuntimeClient
	byPod   map[string]runtimeReading
}

type runtimeReading struct {
	snapshot *RuntimeSnapshot
	err      error
}

func newRuntimeReadings(runtime RuntimeClient) *runtimeReadings {
	return &runtimeReadings{runtime: runtime, byPod: map[string]runtimeReading{}}
}

// of returns this pass's reading of a pod's runtime, and reads the runtime the
// first time it is asked for. A runtime that did not answer is not asked again
// in the same pass.
func (rr *runtimeReadings) of(ctx context.Context, pod *corev1.Pod) (*RuntimeSnapshot, error) {
	if reading, found := rr.byPod[pod.Name]; found {
		return reading.snapshot, reading.err
	}
	snapshot, err := rr.runtime.Snapshot(ctx, pod.Status.PodIP, DefaultRuntimePort)
	rr.byPod[pod.Name] = runtimeReading{snapshot: snapshot, err: err}
	return snapshot, err
}

// ofPods returns the readings of several pods by pod name. A pod whose runtime
// did not answer is simply absent.
func (rr *runtimeReadings) ofPods(ctx context.Context, pods []corev1.Pod) map[string]*RuntimeSnapshot {
	snapshots := make(map[string]*RuntimeSnapshot, len(pods))
	for i := range pods {
		snapshot, err := rr.of(ctx, &pods[i])
		if err != nil || snapshot == nil {
			continue
		}
		snapshots[pods[i].Name] = snapshot
	}
	return snapshots
}

// replace puts in the reading a step took after changing a runtime.
func (rr *runtimeReadings) replace(pod *corev1.Pod, snapshot *RuntimeSnapshot) {
	if rr == nil {
		return
	}
	rr.byPod[pod.Name] = runtimeReading{snapshot: snapshot}
}

// forget drops a pod's reading after a step changed its runtime without
// reading it back.
func (rr *runtimeReadings) forget(podName string) {
	if rr == nil {
		return
	}
	delete(rr.byPod, podName)
}

// PodPlacementState is the scheduling-relevant summary of one warm pod. It is
// not persisted in a CRD because runtime sidecars are the authoritative source.
type PodPlacementState struct {
	SnapshotKnown  bool
	ArtifactCached bool
	MemoryKnown    bool
	HBMFreeBytes   int64
	KVUsedBytes    int64
	ModelCount     int
	// HBMUsableBytes is how much of this pod's GPU memory can ever hold an
	// engine, and HBMUsableKnown separates a card with nothing left from a card
	// nobody could measure. A pod with several cards is described by its
	// smallest, since which card an engine lands on is the device plugin's
	// decision rather than ours.
	HBMUsableBytes int64
	HBMUsableKnown bool
	// MaximumRoomBytes is the most the account says this card could ever
	// offer, and MaximumRoomKnown says whether it could be worked out at all.
	// Ranking uses it in preference to free memory: free memory moves with
	// traffic, so ordering two admitted pods by it would contradict the gate
	// they just passed.
	MaximumRoomBytes int64
	MaximumRoomKnown bool
}

func placementStateFromSnapshot(snapshot *RuntimeSnapshot, artifactURL string, parallelism int64) PodPlacementState {
	state := PodPlacementState{SnapshotKnown: true, ModelCount: len(snapshot.Models)}
	for _, cached := range snapshot.CachedArtifacts {
		if cached == artifactURL {
			state.ArtifactCached = true
			break
		}
	}
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > 1 && int64(len(snapshot.Accelerators)) != parallelism {
		return state
	}
	for _, accelerator := range snapshot.Accelerators {
		// A single-GPU engine needs the largest available single-device slot. A
		// fixed TP/PP group uses every visible GPU, so its safe headroom is the
		// least-free rank rather than a misleading aggregate or maximum.
		if !state.MemoryKnown || (parallelism == 1 && accelerator.HBMFreeBytes > state.HBMFreeBytes) ||
			(parallelism > 1 && accelerator.HBMFreeBytes < state.HBMFreeBytes) {
			state.HBMFreeBytes = accelerator.HBMFreeBytes
			state.MemoryKnown = true
		}
	}
	state.HBMUsableBytes, state.HBMUsableKnown = snapshot.hbmUsableBytes()
	for _, model := range snapshot.Models {
		// An engine with no KV allocator to read reports a negative figure. It
		// has mapped nothing, so it adds nothing here.
		if model.KVUsedBytes < 0 {
			continue
		}
		state.KVUsedBytes += model.KVUsedBytes
	}
	return state
}

// hbmUsableBytes is how much of a pod's GPU memory can ever hold an engine,
// taken from what the runtime measured rather than derived here. A pod with
// several cards is described by its smallest one, because which card an engine
// lands on is decided by the device plugin and not by placement. In a
// topology-homogeneous pool every card is the same size and the choice costs
// nothing.
//
// One card the runtime could not measure leaves the whole pod unsized. Taking
// the cards it could read and ignoring the rest would describe a pod that does
// not exist.
func (s *RuntimeSnapshot) hbmUsableBytes() (int64, bool) {
	if s == nil || len(s.Accelerators) == 0 {
		return 0, false
	}
	smallest := int64(0)
	for i, accelerator := range s.Accelerators {
		if accelerator.HBMUsableBytes <= 0 {
			return 0, false
		}
		if i == 0 || accelerator.HBMUsableBytes < smallest {
			smallest = accelerator.HBMUsableBytes
		}
	}
	return smallest, true
}
