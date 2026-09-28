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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// The docs quote both figures.
func TestTheBootingPaceIsTwoSecondsForFiveMinutes(t *testing.T) {
	assert.Equal(t, 2*time.Second, ActivatingRequeueDuration)
	assert.Equal(t, 5*time.Minute, ActivatingRequeueWindow)
	assert.Equal(t, 10*time.Second, DefaultRequeueDuration)
}

// routeOf is the routing annotation a claim has on a pod.
func routeOf(t *testing.T, r *ModelClaimReconciler, podName, claimName string) string {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: podName}, pod))
	return pod.Annotations[constants.ModelClaimPodAnnotationPrefix+claimName]
}

// anEngineComingUp is a claim with one Activating instance recorded at 300 on
// a card of 1000. Its engine is ready, and still under its allocator's own
// limit, so the pass writes its limit and reads it back.
func anEngineComingUp(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *corev1.Pod, *RuntimeSnapshot) {
	t.Helper()
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod:          "warm-1",
		Port:         9001,
		Phase:        modelv1alpha1.ModelClaimActivating,
		KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.ObservedAt = time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, pod, snapshot
}

// eventsNamed counts the recorded Events that carry a reason.
func eventsNamed(events []string, reason string) int {
	count := 0
	for _, event := range events {
		if strings.Contains(event, reason) {
			count++
		}
	}
	return count
}

// An engine is routed on what the read-back shows, and on all of it: ready,
// with a port, and held to its limit. A read-back that shows anything else
// keeps the engine off the route.
func TestReconcileRoutesAnEngineOnlyOnWhatItsReadBackShows(t *testing.T) {
	cases := map[string]struct {
		// between is what happens to the engine between the write and the
		// read-back.
		between func(snapshot *RuntimeSnapshot)
		pace    time.Duration
		// said is the Event the read-back leads to, if any.
		said string
	}{
		"the engine restarts, and keeps the segment": {
			between: func(snapshot *RuntimeSnapshot) {
				snapshot.Models[0].Phase = "restarting"
				snapshot.Models[0].Alive = false
				snapshot.Models[0].Ready = false
			},
			pace: DefaultRequeueDuration,
			said: "KVLimitSet",
		},
		"the engine boots again under its allocator's limit": {
			between: func(snapshot *RuntimeSnapshot) {
				started := snapshot.ObservedAt.Add(-time.Second)
				snapshot.Models[0].Phase = "booting"
				snapshot.Models[0].Ready = false
				snapshot.Models[0].LastTransition = &started
				snapshot.Models[0].KVCapacityBytes = 5000
			},
			// The pace follows the read-back as well.
			pace: ActivatingRequeueDuration,
			said: "KVLimitFailed",
		},
		"the segment is gone": {
			between: func(snapshot *RuntimeSnapshot) { snapshot.Models[0].KVCapacityBytes = kvLimitUnknown },
			pace:    DefaultRequeueDuration,
		},
		"the engine is gone": {
			between: func(snapshot *RuntimeSnapshot) { snapshot.Models = nil },
			pace:    DefaultRequeueDuration,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, runtime, pm, pod, snapshot := anEngineComingUp(t)
			runtime.onKVLimit = func() { tc.between(snapshot) }

			pace := reconcileFor(t, r, pm.Name)

			require.Len(t, runtime.kvLimitCalls, 1)
			got := getModel(t, r, pm.Name)
			require.Len(t, got.Status.Instances, 1)
			assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
			assert.Equal(t, int32(9001), got.Status.Instances[0].Port)
			assert.Contains(t, routeOf(t, r, pod.Name, pm.Name), `"port":0`)
			assert.Equal(t, tc.pace, pace)
			events := drainEvents(t, r)
			for _, reason := range []string{"KVLimitSet", "KVLimitFailed"} {
				want := 0
				if reason == tc.said {
					want = 1
				}
				assert.Equal(t, want, eventsNamed(events, reason), reason)
			}
		})
	}
}

// failingSnapshots fails the snapshot reads whose number is listed, counted
// from one.
type failingSnapshots struct {
	*fakeRuntime
	failing map[int]bool
}

func (f *failingSnapshots) Snapshot(ctx context.Context, podIP string, port int) (*RuntimeSnapshot, error) {
	if f.failing[f.snapshotCalls+1] {
		f.snapshotCalls++
		return nil, errors.New("runtime did not answer")
	}
	return f.fakeRuntime.Snapshot(ctx, podIP, port)
}

func TestReconcileDoesNotRouteAnEngineWhoseReadBackFails(t *testing.T) {
	r, runtime, pm, pod, _ := anEngineComingUp(t)
	r.Runtime = &failingSnapshots{fakeRuntime: runtime, failing: map[int]bool{2: true}}
	// The card's round is taken, so every read below is the health check's.
	r.Divisions.due(cardOf(pod), "taken")

	pace := reconcileFor(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, 2, runtime.snapshotCalls)
	got := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, pod.Name, pm.Name), `"port":0`)
	assert.Equal(t, DefaultRequeueDuration, pace)
	events := drainEvents(t, r)
	assert.Zero(t, eventsNamed(events, "KVLimitSet"), "nothing showed the limit in force")
	assert.Zero(t, eventsNamed(events, "KVLimitFailed"), "and nothing showed that it is not")

	// The write took. The next pass finds the limit in force and routes the
	// engine, without another write.
	reconcileOnce(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Len(t, runtime.kvLimitCalls, 1)
}

// A read-back that failed is no reading of the pod. What follows in the pass
// reads the runtime again, as it did before there was a read-back.
func TestReconcileForgetsAReadBackThatFailed(t *testing.T) {
	r, runtime, pm, pod, _ := anEngineComingUp(t)
	r.Runtime = &failingSnapshots{fakeRuntime: runtime, failing: map[int]bool{2: true}}
	readings := newRuntimeReadings(r.Runtime)
	claim := getModel(t, r, pm.Name)

	r.reconcileInstanceHealth(context.Background(), claim, readings)
	require.Equal(t, 2, runtime.snapshotCalls)

	snapshot, err := readings.of(context.Background(), pod)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Equal(t, 3, runtime.snapshotCalls, "the runtime is read again")
}

// KVLimitSet says that a limit is in force. For an engine coming up, that is
// known from the read-back, so an engine that does not take its limit is not
// told "set" on every pass.
func TestReconcileSaysALimitIsSetOnceItReadsBack(t *testing.T) {
	r, runtime, pm, _, _ := anEngineComingUp(t)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.kvLimitCalls, 1)
	events := drainEvents(t, r)
	assert.Equal(t, 1, eventsNamed(events, "KVLimitSet"))
	assert.Zero(t, eventsNamed(events, "KVLimitFailed"))

	deaf, deafRuntime, deafClaim, _, _ := anEngineComingUp(t)
	deafRuntime.deafToKVLimits = true
	for pass := 1; pass <= 3; pass++ {
		reconcileOnce(t, deaf, deafClaim.Name)
		require.Len(t, deafRuntime.kvLimitCalls, pass)
	}
	events = drainEvents(t, deaf)
	assert.Zero(t, eventsNamed(events, "KVLimitSet"))
	assert.Equal(t, 3, eventsNamed(events, "KVLimitFailed"))
	for _, event := range events {
		if strings.Contains(event, "KVLimitFailed") {
			assert.Contains(t, event, "was written, and the engine still reports")
		}
	}
}

// Any one instance booting sets the pace, wherever it stands among them.
func TestReconcileLooksAgainSoonWhenAnyInstanceBoots(t *testing.T) {
	for _, bootingFirst := range []bool{true, false} {
		pm := claimWithCost(300, 100)
		two := int32(2)
		pm.Spec.Replicas = &two
		bootingPod, bootingSnapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
		routedPod, routedSnapshot := sizedWarmPod("warm-2", "10.0.0.2", 1000)
		observedAt := time.Unix(1_700_000_000, 0)
		bootingSnapshot.ObservedAt, routedSnapshot.ObservedAt = observedAt, observedAt
		bootingSnapshot.Models = []RuntimeSnapshotModel{engineBootingFor(observedAt, time.Minute)}
		routedSnapshot.Models = []RuntimeSnapshotModel{readyEngine(600)}
		booting := modelv1alpha1.ModelClaimInstance{
			Pod: bootingPod.Name, Port: 9001, Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 600,
		}
		routed := modelv1alpha1.ModelClaimInstance{
			Pod: routedPod.Name, Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 600,
		}
		pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{routed, booting}
		if bootingFirst {
			pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{booting, routed}
		}
		r, runtime := newReconciler(t, pm, bootingPod, routedPod)
		runtime.snapshots = map[string]*RuntimeSnapshot{
			bootingPod.Status.PodIP: bootingSnapshot,
			routedPod.Status.PodIP:  routedSnapshot,
		}

		assert.Equal(t, ActivatingRequeueDuration, reconcileFor(t, r, pm.Name), "booting first: %v", bootingFirst)
	}
}

// An engine that was routed and lost its limit leaves the route first, so that
// the loss is seen. The write that pulls it back is said as well.
func TestReconcileSaysALimitIsSetWhenARoutedEngineIsPulledBack(t *testing.T) {
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.ObservedAt = time.Unix(1_700_000_000, 0)
	// The engine restarted, and its allocator put its own limit back.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The card's round is taken, so every read below is the health check's.
	r.Divisions.due(cardOf(pod), "taken")

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, 1, runtime.snapshotCalls, "it is not read back in this pass")
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, pod.Name, pm.Name), `"port":0`)
	events := drainEvents(t, r)
	assert.Equal(t, 1, eventsNamed(events, "KVLimitNotHeld"))
	assert.Equal(t, 1, eventsNamed(events, "KVLimitSet"))

	// The change to the pod's annotation starts the next pass, which finds the
	// limit in force and routes the engine again.
	reconcileOnce(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Len(t, runtime.kvLimitCalls, 1)
}
