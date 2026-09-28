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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
				// The boot is dated after the first reading of the pass, and
				// before the read-back.
				started := snapshot.ObservedAt.Add(9 * time.Second)
				snapshot.ObservedAt = snapshot.ObservedAt.Add(10 * time.Second)
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

// A runtime that did not answer the read-back is not asked again in the pass.
// A runtime that hangs holds the worker for a whole timeout each time.
func TestReconcileDoesNotAskARuntimeAgainWhoseReadBackFailed(t *testing.T) {
	r, runtime, pm, pod, _ := anEngineComingUp(t)
	r.Runtime = &failingSnapshots{fakeRuntime: runtime, failing: map[int]bool{2: true}}
	readings := newRuntimeReadings(r.Runtime)
	claim := getModel(t, r, pm.Name)

	r.reconcileInstanceHealth(context.Background(), claim, readings)
	require.Equal(t, 2, runtime.snapshotCalls)

	snapshot, err := readings.of(context.Background(), pod)
	require.Error(t, err)
	assert.Nil(t, snapshot, "what the runtime said before the write is not used either")
	assert.Equal(t, 2, runtime.snapshotCalls)
}

// A write that failed may have reached the engine all the same. What the pass
// had read of the runtime is dropped, and the next step reads it again.
func TestReconcileReadsARuntimeAgainAfterAWriteThatFailed(t *testing.T) {
	r, runtime, pm, pod, _ := anEngineComingUp(t)
	r.Runtime = &failingKVLimits{fakeRuntime: runtime, failing: map[int]bool{1: true}}
	readings := newRuntimeReadings(r.Runtime)
	claim := getModel(t, r, pm.Name)

	r.reconcileInstanceHealth(context.Background(), claim, readings)
	require.Len(t, runtime.kvLimitCalls, 1)
	require.Equal(t, 1, runtime.snapshotCalls, "a write that failed is not read back")

	_, err := readings.of(context.Background(), pod)
	require.NoError(t, err)
	assert.Equal(t, 2, runtime.snapshotCalls)
	assert.Equal(t, 1, eventsNamed(drainEvents(t, r), "KVLimitFailed"))
}

// For an engine coming up, KVLimitSet says that a limit is in force. That is
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
// the loss is seen. The write that pulls it back is said as well. It is not
// read back in this pass, so its Event says that the limit was written, and
// not that it is in force.
func TestReconcileSaysALimitIsWrittenWhenARoutedEngineIsPulledBack(t *testing.T) {
	pm := claimWithCost(7<<30, 1<<30)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 3 << 30,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 10<<30)
	snapshot.ObservedAt = time.Unix(1_700_000_000, 0)
	// The engine restarted, and its allocator put its own limit back.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5 << 30)}
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
	assert.Equal(t, 1, eventsNamed(events, "is held to more KV than its limit of 3.0 GiB"))
	require.Equal(t, 1, eventsNamed(events, "KVLimitSet"))
	for _, event := range events {
		if strings.Contains(event, "KVLimitSet") {
			assert.Contains(t, event, "KV limit of 3.0 GiB written over 5.0 GiB")
		}
	}

	// The change to the pod's annotation starts the next pass, which finds the
	// limit in force and routes the engine again.
	reconcileOnce(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Len(t, runtime.kvLimitCalls, 1)
}

// Nothing in a pass asks for the pass that follows a withdrawn route. The
// route is an annotation on the pod, and a change to a pod of the pool starts
// a pass of every claim in its namespace. That holds for a pod that carries
// both labels of the pool.
func TestAWithdrawnRouteStartsTheNextPassOfItsClaim(t *testing.T) {
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive, KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(300)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	reconcileOnce(t, r, pm.Name)
	routed := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pod), routed))
	require.Contains(t, routeOf(t, r, pod.Name, pm.Name), `"port":9001`)

	// The engine restarts, and its allocator puts its own limit back.
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5000)}
	reconcileOnce(t, r, pm.Name)
	withdrawn := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(pod), withdrawn))
	require.Contains(t, routeOf(t, r, pod.Name, pm.Name), `"port":0`)

	change := event.UpdateEvent{ObjectOld: routed, ObjectNew: withdrawn}
	require.True(t, modelPoolPodFilter().Update(change))
	assert.Contains(t, enqueueModelClaimsForPod(r.Client)(context.Background(), withdrawn), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: pm.Name},
	})

	// Without the name of its pool, the changes of a pod start nothing, and
	// the claim's own pace is all there is. The same holds for a pod that is
	// not enabled.
	unnamed := withdrawn.DeepCopy()
	delete(unnamed.Labels, constants.ModelPoolLabelName)
	assert.False(t, modelPoolPodFilter().Update(event.UpdateEvent{ObjectOld: routed, ObjectNew: unnamed}))
	disabled := withdrawn.DeepCopy()
	disabled.Labels[constants.ModelPoolLabelEnabled] = "false"
	assert.False(t, modelPoolPodFilter().Update(event.UpdateEvent{ObjectOld: routed, ObjectNew: disabled}))
	removed := withdrawn.DeepCopy()
	delete(removed.Labels, constants.ModelPoolLabelEnabled)
	assert.False(t, modelPoolPodFilter().Update(event.UpdateEvent{ObjectOld: routed, ObjectNew: removed}))
}

// A boot is watched from the moment it is dated to the end of the window, and
// a boot dated after the reading is not watched at all.
func TestEngineBootingWatchesABootForItsWindowOnly(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	for name, c := range map[string]struct {
		age     time.Duration
		booting bool
	}{
		"dated at the reading":                 {0, true},
		"a moment old":                         {time.Nanosecond, true},
		"a moment before the window ends":      {ActivatingRequeueWindow - time.Nanosecond, true},
		"as old as the window":                 {ActivatingRequeueWindow, false},
		"dated a second after the reading":     {-time.Second, false},
		"dated a nanosecond after the reading": {-time.Nanosecond, false},
		"dated half an hour after the reading": {-30 * time.Minute, false},
	} {
		engine := engineBootingFor(observedAt, c.age)
		assert.Equal(t, c.booting, engineBooting(&RuntimeSnapshot{ObservedAt: observedAt}, &engine), name)
	}
}

// An engine that is stopping sets the pace as one that boots does, while the
// runtime reports it alive. Its instance gets a new engine as soon as it has
// gone.
func TestEngineBootingCountsAnEngineThatIsStopping(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	engine := engineBootingFor(observedAt, time.Second)
	engine.Phase = runtimePhaseStopping

	assert.True(t, engineBooting(&RuntimeSnapshot{ObservedAt: observedAt}, &engine))

	engine.Alive = false
	assert.False(t, engineBooting(&RuntimeSnapshot{ObservedAt: observedAt}, &engine))
}

// The runtime dates an engine again at every try of a stop that fails. Such
// an engine would read as one that has just begun to stop, for as long as the
// stop fails. It carries the error of the stop, and keeps the usual pace.
func TestEngineBootingLeavesOutAnEngineWhoseStopFails(t *testing.T) {
	observedAt := time.Unix(1_700_000_000, 0)
	engine := engineBootingFor(observedAt, time.Second)
	engine.Phase = runtimePhaseStopping
	engine.LastError = "stop failed: operation not permitted"

	assert.False(t, engineBooting(&RuntimeSnapshot{ObservedAt: observedAt}, &engine))

	// An engine that boots after a start that failed carries an error as
	// well. It is watched as any boot is.
	engine.Phase = "booting"
	assert.True(t, engineBooting(&RuntimeSnapshot{ObservedAt: observedAt}, &engine))
}

// anEngineHeldToFiveGibibytes is a claim with one instance recorded at 3 GiB
// on a card of 10 GiB. Its engine is ready, and held to 5 GiB.
func anEngineHeldToFiveGibibytes(
	t *testing.T,
	phase modelv1alpha1.ModelClaimPhase,
) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim) {
	t.Helper()
	pm := claimWithCost(7<<30, 1<<30)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Port: 9001, Phase: phase, KVLimitBytes: 3 << 30,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 10<<30)
	snapshot.ObservedAt = time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{readyEngine(5 << 30)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	// The card's round is taken, so every read below is the health check's.
	r.Divisions.due(cardOf(pod), "taken")
	return r, runtime, pm
}

// The Events of an engine coming up say what was written over what, and what
// the engine reports when the limit did not take.
func TestTheEventsOfAnEngineComingUpCarryTheirFigures(t *testing.T) {
	r, _, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimActivating)
	reconcileOnce(t, r, pm.Name)
	events := drainEvents(t, r)
	require.Equal(t, 1, eventsNamed(events, "KVLimitSet"))
	assert.Equal(t, 1, eventsNamed(events, "KV limit set to 3.0 GiB, from 5.0 GiB"))

	// The limit does not take, and the engine reports a limit that is
	// neither the one written nor the one it had.
	deaf, runtime, claim := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimActivating)
	runtime.deafToKVLimits = true
	runtime.onKVLimit = func() { runtime.snapshots["10.0.0.1"].Models[0].KVCapacityBytes = 4 << 30 }
	reconcileOnce(t, deaf, claim.Name)
	events = drainEvents(t, deaf)
	require.Equal(t, 1, eventsNamed(events, "KVLimitFailed"))
	assert.Equal(t, 1, eventsNamed(events, "KV limit 3.0 GiB was written, and the engine still reports 4.0 GiB"))
}

// Only an engine on the route is left unread after its limit is written. An
// engine that slept and serves again has no route to lose. So it is read back
// and routed in the same pass, as an engine coming up is.
func TestReconcileReadsBackAnEngineThatWokeInThePassItsLimitIsWritten(t *testing.T) {
	r, runtime, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimSleeping)

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, 2, runtime.snapshotCalls, "the write is read back in this pass")
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"port":9001`)
	events := drainEvents(t, r)
	assert.Equal(t, 1, eventsNamed(events, "Woken"))
	assert.Equal(t, 1, eventsNamed(events, "KV limit set to 3.0 GiB, from 5.0 GiB"))
	assert.Zero(t, eventsNamed(events, "Unhealthy"))
}

// KVLimitNotHeld is also raised when the segment of a routed engine cannot be
// read: the engine is not known to be held to anything. Nothing is written
// then, since there is no segment to write into.
func TestReconcileTakesAnEngineOffItsRouteWhenItsSegmentCannotBeRead(t *testing.T) {
	r, runtime, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimActive)
	runtime.snapshots["10.0.0.1"].Models = []RuntimeSnapshotModel{readyEngine(kvLimitUnknown)}

	reconcileOnce(t, r, pm.Name)

	assert.Equal(t, 1, eventsNamed(drainEvents(t, r), "KVLimitNotHeld"))
	assert.Empty(t, runtime.kvLimitCalls)
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"port":0`)
}

// An engine that woke and waits for its limit is ready, so it is not called
// unhealthy. The Event of the limit says what it waits for. An engine that has
// gone by the read-back is called unhealthy, as before.
func TestReconcileDoesNotCallAnEngineUnhealthyThatWokeAndWaitsForItsLimit(t *testing.T) {
	for name, c := range map[string]struct {
		arrange   func(r *ModelClaimReconciler, runtime *fakeRuntime)
		failed    string
		unhealthy int
	}{
		"the engine reports another limit": {func(_ *ModelClaimReconciler, runtime *fakeRuntime) {
			runtime.deafToKVLimits = true
		}, "KV limit 3.0 GiB was written, and the engine still reports 5.0 GiB", 0},
		"the write fails": {func(r *ModelClaimReconciler, runtime *fakeRuntime) {
			r.Runtime = &failingKVLimits{fakeRuntime: runtime, failing: map[int]bool{1: true}}
		}, "KV limit 3.0 GiB could not be set", 0},
		"the read-back fails": {func(r *ModelClaimReconciler, runtime *fakeRuntime) {
			r.Runtime = &failingSnapshots{fakeRuntime: runtime, failing: map[int]bool{2: true}}
		}, "", 0},
		"the read-back shows no segment": {func(_ *ModelClaimReconciler, runtime *fakeRuntime) {
			runtime.onKVLimit = func() { runtime.snapshots["10.0.0.1"].Models[0].KVCapacityBytes = kvLimitUnknown }
		}, "", 0},
		"the engine has no segment": {func(_ *ModelClaimReconciler, runtime *fakeRuntime) {
			runtime.snapshots["10.0.0.1"].Models[0].KVCapacityBytes = kvLimitUnknown
		}, "", 0},
		"the read-back shows no engine": {func(_ *ModelClaimReconciler, runtime *fakeRuntime) {
			runtime.onKVLimit = func() { runtime.snapshots["10.0.0.1"].Models = nil }
		}, "", 1},
	} {
		r, runtime, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimSleeping)
		c.arrange(r, runtime)

		reconcileOnce(t, r, pm.Name)

		events := drainEvents(t, r)
		if c.failed == "" {
			assert.Zero(t, eventsNamed(events, "KVLimitFailed"), name)
		} else {
			assert.Equal(t, 1, eventsNamed(events, "KVLimitFailed"), name)
			assert.Equal(t, 1, eventsNamed(events, c.failed), name)
		}
		assert.Equal(t, c.unhealthy, eventsNamed(events, "Unhealthy"), name)
		assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase, name)
		assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"port":0`, name)
	}
}

// A failed instance is not on the route either. If its engine still serves,
// it is held to its record, and the write is read back in the same pass. The
// instance stays failed.
func TestReconcileReadsBackTheEngineOfAFailedInstanceInThePassItsLimitIsWritten(t *testing.T) {
	r, runtime, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimFailed)

	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.kvLimitCalls, 1)
	assert.Equal(t, 2, runtime.snapshotCalls, "the write is read back in this pass")
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"port":0`)
	assert.Equal(t, 1, eventsNamed(drainEvents(t, r), "KV limit set to 3.0 GiB, from 5.0 GiB"))
}
