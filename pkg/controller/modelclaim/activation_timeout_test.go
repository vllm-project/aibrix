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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// stuckAt is a clock shared by a reconciler and the runtime reading of its one
// pod, both of which a test moves forward together.
type stuckAt struct {
	t0       time.Time
	now      time.Time
	snapshot *RuntimeSnapshot
}

func (c *stuckAt) set(r *ModelClaimReconciler, after time.Duration) {
	c.now = c.t0.Add(after)
	c.snapshot.ObservedAt = c.now
	r.Now = func() time.Time { return c.now }
}

// passAt runs one pass at t0 plus after, and returns its Events.
func passAt(t *testing.T, r *ModelClaimReconciler, clock *stuckAt, claim string, after time.Duration) []string {
	t.Helper()
	clock.set(r, after)
	reconcileOnce(t, r, claim)
	return drainEvents(t, r)
}

// passesTill runs a pass every five minutes from t0 plus from up to, but not
// including, t0 plus till, as the requeue keeps a stuck claim looked at. A
// clock not seen for stuckActivationForgetAfter would start over.
func passesTill(t *testing.T, r *ModelClaimReconciler, clock *stuckAt, claim string, from, till time.Duration) {
	t.Helper()
	for after := from; after < till; after += 5 * time.Minute {
		passAt(t, r, clock, claim, after)
	}
}

// aStuckBoot is a claim with one Activating instance on warm-1, whose engine
// has been booting for six minutes at t0: alive, not ready, and past the boot
// window. Other objects are added to the API server.
func aStuckBoot(t *testing.T, others ...client.Object) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *stuckAt) {
	t.Helper()
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	t0 := time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(t0, 6*time.Minute)}
	r, runtime := newReconciler(t, append([]client.Object{pm, pod}, others...)...)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, &stuckAt{t0: t0, snapshot: snapshot}
}

// aStuckBootBesideARoomyPod is aStuckBoot with a second Pod, warm-2, whose
// empty card has room for the claim.
func aStuckBootBesideARoomyPod(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *stuckAt) {
	t.Helper()
	roomy, roomySnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	r, runtime, pm, clock := aStuckBoot(t, roomy)
	runtime.snapshots[roomy.Status.PodIP] = roomySnapshot
	return r, runtime, pm, clock
}

// stopTaken makes the runtime of warm-1 report its engine as stopping once it
// takes a stop, as the runtime does until the engine has exited.
func stopTaken(runtime *fakeRuntime, clock *stuckAt) {
	runtime.onDeactivate = func(podIP string) {
		if podIP == "10.0.0.1" && len(clock.snapshot.Models) > 0 {
			clock.snapshot.Models[0].Phase = runtimePhaseStopping
		}
	}
}

// refuseClaimWrites makes the API server refuse, once, a write of the claim's
// status that refuse picks.
func refuseClaimWrites(r *ModelClaimReconciler, refuse func(*modelv1alpha1.ModelClaim) bool) *bool {
	refused := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if claim, ok := obj.(*modelv1alpha1.ModelClaim); ok && !refused && refuse(claim) {
				refused = true
				return apierrors.NewConflict(schema.GroupResource{Group: "model.aibrix.ai", Resource: "modelclaims"},
					obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	return &refused
}

// assertTimedOut checks that a pass stopped the stuck engine on warm-1 and
// failed its instance, and said so once.
func assertTimedOut(t *testing.T, r *ModelClaimReconciler, runtime *fakeRuntime, claim string, events []string, timeout string) {
	t.Helper()
	require.Len(t, runtime.deactivateCalls, 1, "the stuck engine is stopped")
	assert.Equal(t, "qwen2-7b", runtime.deactivateCalls[0].ModelName)
	assert.Equal(t, DeactivateStop, runtime.deactivateCalls[0].Mode)
	assert.Equal(t, []string{"10.0.0.1"}, runtime.deactivatedOn)
	inst := getModel(t, r, claim).Status.Instances[0]
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, inst.Phase)
	assert.Equal(t, instanceReasonActivationTimedOut, inst.Reason)
	assert.Equal(t, 1, eventsNamed(events, instanceReasonActivationTimedOut))
	assert.Equal(t, 1, eventsNamed(events, "did not become routable within "+timeout))
	assert.Zero(t, eventsNamed(events, "EngineFailed"), "the timeout says why, and is not said twice")
	assert.Contains(t, routeOf(t, r, "warm-1", claim), `"state":"failed"`)
}

// An engine that never becomes ready is stopped, and its instance fails, once
// the controller has seen it booting for the default timeout.
func TestAnEngineThatNeverGetsReadyFailsAfterTheActivationTimeout(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// A first boot counts from its start. A pass sees the engine as soon as it
// starts, and the instance fails at the timeout after that, with no boot
// window added.
func TestAFirstBootThatNeverGetsReadyFailsAtTheTimeoutFromItsStart(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	clock.snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(clock.t0, 0)}

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls)

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// A ready engine whose KV limit never takes is not routed, and fails the same
// way. The runtime does not date it, so the controller's own clock counts.
func TestAReadyEngineWhoseLimitNeverTakesFailsAfterTheActivationTimeout(t *testing.T) {
	r, runtime, pm, _, snapshot := anEngineComingUp(t)
	runtime.deafToKVLimits = true
	clock := &stuckAt{t0: snapshot.ObservedAt, snapshot: snapshot}

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// An engine the runtime still reports restarting or stopping is never timed
// out, however long the controller has watched it. Those are the runtime's to
// settle.
func TestAnEngineTheRuntimeStillSettlesIsNotTimedOut(t *testing.T) {
	cases := map[string]func(t0, at time.Time) RuntimeSnapshotModel{
		// Restarting since before t0, in a long wait between tries.
		"restarting": func(t0, _ time.Time) RuntimeSnapshotModel {
			engine := engineBootingFor(t0, 6*time.Minute)
			engine.Phase, engine.Alive = "restarting", false
			return engine
		},
		// Stopping since before t0, its processes not yet gone.
		"stopping": func(t0, _ time.Time) RuntimeSnapshotModel {
			engine := engineBootingFor(t0, 6*time.Minute)
			engine.Phase = runtimePhaseStopping
			return engine
		},
	}
	for name, engineAt := range cases {
		t.Run(name, func(t *testing.T) {
			r, runtime, pm, clock := aStuckBoot(t)
			for after := time.Duration(0); after <= 2*defaultActivationTimeout; after += 5 * time.Minute {
				clock.snapshot.Models = []RuntimeSnapshotModel{engineAt(clock.t0, clock.t0.Add(after))}
				passAt(t, r, clock, pm.Name, after)
			}
			assert.Empty(t, runtime.deactivateCalls)
			assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
		})
	}
}

// An engine stuck booting that then gets ready, but whose KV limit does not
// take, starts its timeout over: the runtime dates the change to ready.
func TestAnEngineThatGetsReadyLateStartsItsTimeoutOver(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	runtime.deafToKVLimits = true
	passesTill(t, r, clock, pm.Name, 0, 10*time.Minute)

	readyAt := clock.t0.Add(10 * time.Minute)
	ready := readyEngine(5000)
	ready.LastTransition = &readyAt
	clock.snapshot.Models = []RuntimeSnapshotModel{ready}
	passesTill(t, r, clock, pm.Name, 10*time.Minute, 10*time.Minute+defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, 10*time.Minute+defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls, "the time before it got ready does not count")

	events := passAt(t, r, clock, pm.Name, 10*time.Minute+defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// A restart dates the engine anew, and its timeout counts from the restart.
func TestAnEngineRestartStartsItsActivationTimeoutOver(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	passesTill(t, r, clock, pm.Name, 0, 10*time.Minute)

	// At 10 minutes the engine has restarted, and boots again for a minute.
	restarted := clock.t0.Add(9 * time.Minute)
	clock.snapshot.Models[0].LastTransition = &restarted
	passesTill(t, r, clock, pm.Name, 10*time.Minute, 9*time.Minute+defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, 9*time.Minute+defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls, "the time before the restart does not count")

	events := passAt(t, r, clock, pm.Name, 9*time.Minute+defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// An engine that served for hours and then lost its route, held to more KV
// than its record, is dated hours back by the runtime. It does not fail at
// once: its time counts from the pass that took its route.
func TestAnEngineThatServedAndLostItsRouteGetsTheWholeTimeout(t *testing.T) {
	r, runtime, pm := anEngineHeldToFiveGibibytes(t, modelv1alpha1.ModelClaimActive)
	runtime.deafToKVLimits = true
	snapshot := runtime.snapshots["10.0.0.1"]
	t0 := snapshot.ObservedAt
	readyAt := t0.Add(-5 * time.Hour)
	snapshot.Models[0].LastTransition = &readyAt
	clock := &stuckAt{t0: t0, snapshot: snapshot}

	passAt(t, r, clock, pm.Name, 0)
	require.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase,
		"held to more than its record, the engine loses its route")
	passAt(t, r, clock, pm.Name, time.Minute)
	assert.Empty(t, runtime.deactivateCalls, "dated five hours back, it still does not fail at once")
	passesTill(t, r, clock, pm.Name, 5*time.Minute, defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls)

	passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	require.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Instances[0].Phase)
}

// A pool sets its own timeout in its annotation, which can be shorter than the
// five minutes after which a boot is looked at less often.
func TestAPoolSetsItsActivationTimeout(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects("")
	deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = "1m"
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 300,
	}}
	_, snapshot := sizedWarmPod(pod.Name, pod.Status.PodIP, 1000)
	t0 := time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(t0, 0)}
	r, runtime := newReconciler(t, pm, deployment, replicaSet, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	clock := &stuckAt{t0: t0, snapshot: snapshot}

	passAt(t, r, clock, pm.Name, 0)
	passAt(t, r, clock, pm.Name, 59*time.Second)
	assert.Empty(t, runtime.deactivateCalls)

	events := passAt(t, r, clock, pm.Name, 60*time.Second)
	assertTimedOut(t, r, runtime, pm.Name, events, "1m0s")
}

// A stop the runtime does not take is asked again on every pass. The instance
// fails all the same, so that a request goes to another instance.
func TestAStopThatIsNotTakenIsAskedAgain(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	runtime.deactivateErr = errors.New("runtime busy")

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")

	events = passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	require.Len(t, runtime.deactivateCalls, 2, "the stop is asked again")
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Zero(t, eventsNamed(events, instanceReasonActivationTimedOut), "the timeout is said once")

	// Once the runtime has taken the stop, it is not asked again.
	runtime.deactivateErr = nil
	stopTaken(runtime, clock)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout+20*time.Second)
	require.Len(t, runtime.deactivateCalls, 3)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout+30*time.Second)
	assert.Len(t, runtime.deactivateCalls, 3)
}

// The failure of an instance that timed out is written before its engine is
// stopped. A write that is refused leaves the engine alone, and the next pass
// fails the instance at once.
func TestATimedOutInstanceFailsBeforeItsEngineIsStopped(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	refused := refuseClaimWrites(r, func(claim *modelv1alpha1.ModelClaim) bool {
		return len(claim.Status.Instances) == 1 && claim.Status.Instances[0].Reason == instanceReasonActivationTimedOut
	})

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	require.True(t, *refused)
	assert.Empty(t, runtime.deactivateCalls, "the engine is not stopped before its failure is written")
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Zero(t, eventsNamed(events, instanceReasonActivationTimedOut))
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"state":"activating"`)

	events = passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// A route that cannot be written after the failure does not bring the engine
// back. The failure stands, so the next pass writes the route and does not
// start the engine again.
func TestATimedOutEngineIsNotStartedAgainWhenItsRouteCannotBeWritten(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	// The runtime takes the stop, and its next reading shows no engine.
	runtime.onDeactivate = func(string) { clock.snapshot.Models = nil }
	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	failRoutes := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod && failRoutes {
				return fmt.Errorf("the API server did not answer")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	require.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Equal(t, 1, eventsNamed(events, instanceReasonActivationTimedOut))

	failRoutes = false
	passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	assert.Empty(t, runtime.activateCalls, "the engine is not started again")
	assert.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"state":"failed"`)
}

// An instance that timed out moves in the same pass to a Pod with room, once
// the runtime has taken the stop of its engine.
func TestATimedOutInstanceMovesInTheSamePass(t *testing.T) {
	r, runtime, pm, clock := aStuckBootBesideARoomyPod(t)
	stopTaken(runtime, clock)
	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	require.Empty(t, runtime.activateCalls)

	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assert.Equal(t, 1, eventsNamed(events, instanceReasonActivationTimedOut))
	assert.Equal(t, 1, eventsNamed(events, "because it did not become routable in time, from pod warm-1 to pod warm-2"))
	assert.Equal(t, []string{testPeerIP}, runtime.activatedOn)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Empty(t, routeOf(t, r, "warm-1", pm.Name), "the route on warm-1 is taken back")
	require.NotEmpty(t, runtime.deactivatedOn)
	assert.Equal(t, "10.0.0.1", runtime.deactivatedOn[0], "the engine on warm-1 is stopped first")
}

// An instance that timed out does not move while the runtime has not taken
// the stop of its engine. Moved, it would leave an engine that nothing stops.
func TestATimedOutInstanceMovesOnlyOnceItsEngineTakesTheStop(t *testing.T) {
	r, runtime, pm, clock := aStuckBootBesideARoomyPod(t)
	runtime.deactivateErrOn = map[string]error{"10.0.0.1": errors.New("runtime busy")}
	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)

	passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	assert.Empty(t, runtime.activateCalls, "the claim does not move yet")
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)
	assert.Contains(t, routeOf(t, r, "warm-1", pm.Name), `"state":"failed"`)

	runtime.deactivateErrOn = nil
	stopTaken(runtime, clock)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	assert.Equal(t, []string{testPeerIP}, runtime.activatedOn, "the claim moves once the stop is taken")
	assert.Equal(t, "warm-2", getModel(t, r, pm.Name).Status.Instances[0].Pod)
}

// A move whose write is refused after the engine was stopped does not bring
// the engine back, since the failure was written first. The next pass moves
// the claim, and warm-1 gets no engine again.
func TestATimedOutInstanceWhoseMoveIsRefusedIsNotStartedAgain(t *testing.T) {
	r, runtime, pm, clock := aStuckBootBesideARoomyPod(t)
	runtime.onDeactivate = func(podIP string) {
		if podIP == "10.0.0.1" {
			clock.snapshot.Models = nil
		}
	}
	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	refused := refuseClaimWrites(r, func(claim *modelv1alpha1.ModelClaim) bool {
		return len(claim.Status.Instances) == 1 && claim.Status.Instances[0].Pod == "warm-2"
	})

	passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	require.True(t, *refused)
	assert.Empty(t, runtime.activateCalls)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-1", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimFailed, got.Status.Instances[0].Phase)

	passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	assert.Equal(t, []string{testPeerIP}, runtime.activatedOn)
	assert.Equal(t, "warm-2", getModel(t, r, pm.Name).Status.Instances[0].Pod)
}

// An engine that no stop has reached is any engine the runtime lists, except
// one that is stopping, or one that failed and is no longer alive.
func TestEngineToStop(t *testing.T) {
	cases := map[string]struct {
		phase string
		alive bool
		want  bool
	}{
		"booting":                   {"booting", true, true},
		"active":                    {"active", true, true},
		"restarting, not alive":     {"restarting", false, true},
		"failed, still alive":       {runtimePhaseFailed, true, true},
		"failed and gone":           {runtimePhaseFailed, false, false},
		"stopping":                  {runtimePhaseStopping, true, false},
		"stopping, no longer alive": {runtimePhaseStopping, false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, engineToStop(&RuntimeSnapshotModel{Phase: tc.phase, Alive: tc.alive}))
		})
	}
	assert.False(t, engineToStop(nil), "an engine the runtime does not list")
}

func TestActivationTimeoutOfAPool(t *testing.T) {
	cases := map[string]struct {
		timeout string
		want    time.Duration
	}{
		"none":                 {"", defaultActivationTimeout},
		"its own":              {"10m", 10 * time.Minute},
		"in seconds":           {"90s", 90 * time.Second},
		"zero":                 {"0s", defaultActivationTimeout},
		"negative":             {"-5m", defaultActivationTimeout},
		"without a unit":       {"30", defaultActivationTimeout},
		"not a duration":       {"soon", defaultActivationTimeout},
		"more than a duration": {"9999999999h", defaultActivationTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deployment, _, _ := warmPoolObjects(`{"lifecycle":{"sleepAfterSeconds":60}}`)
			if tc.timeout != "" {
				deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = tc.timeout
			}
			assert.Equal(t, tc.want, activationTimeoutOf(deployment))
		})
	}
}

// The timeout is a positive Go duration, and an empty value sets none.
func TestAnActivationTimeoutMustBeAPositiveDuration(t *testing.T) {
	for _, raw := range []string{"0s", "-5m", "30", "soon", "9999999999h"} {
		_, err := parseActivationTimeout(raw)
		assert.Error(t, err, raw)
	}
	timeout, err := parseActivationTimeout("1h30m")
	require.NoError(t, err)
	assert.Equal(t, 90*time.Minute, timeout)
	timeout, err = parseActivationTimeout("")
	require.NoError(t, err)
	assert.Zero(t, timeout)
}

// A timeout that is not valid is said once for each value, and the pool gets
// the default. Its pool policy is not touched.
func TestAnInvalidActivationTimeoutIsReportedOnce(t *testing.T) {
	deployment, _, _ := warmPoolObjects(`{"lifecycle":{"sleepAfterSeconds":60}}`)
	deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = "0s"
	r, _ := newReconciler(t)
	manager := newPoolPolicyManager(time.Now)

	r.reportInvalidActivationTimeout(deployment, manager)
	events := drainEvents(t, r)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "InvalidActivationTimeout")
	assert.Contains(t, events[0], "the default activation timeout of 30m0s applies")
	assert.Equal(t, defaultActivationTimeout, activationTimeoutOf(deployment))
	policy := r.resolvePoolPolicy(deployment, manager)
	require.NotNil(t, policy, "the sleep policy applies")
	require.NotNil(t, policy.Lifecycle)
	assert.Empty(t, drainEvents(t, r), "the pool policy is valid")

	r.reportInvalidActivationTimeout(deployment, manager)
	assert.Empty(t, drainEvents(t, r), "the same value is not reported again")

	deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = "45m"
	r.reportInvalidActivationTimeout(deployment, manager)
	assert.Empty(t, drainEvents(t, r))
	assert.Equal(t, 45*time.Minute, activationTimeoutOf(deployment))
	deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = "0s"
	r.reportInvalidActivationTimeout(deployment, manager)
	assert.Len(t, drainEvents(t, r), 1, "a value that is not valid again is reported again")
}

// A pass over a claim checks the timeout of its pool, and reports one that is
// not valid once.
func TestAPassReportsAnInvalidActivationTimeout(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects("")
	deployment.Annotations[constants.ModelPoolActivationTimeoutAnnotationKey] = "soon"
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 300,
	}}
	_, snapshot := sizedWarmPod(pod.Name, pod.Status.PodIP, 1000)
	t0 := time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(t0, 0)}
	r, runtime := newReconciler(t, pm, deployment, replicaSet, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	clock := &stuckAt{t0: t0, snapshot: snapshot}

	events := passAt(t, r, clock, pm.Name, 0)
	assert.Equal(t, 1, eventsNamed(events, "InvalidActivationTimeout"))
	events = passAt(t, r, clock, pm.Name, 10*time.Second)
	assert.Zero(t, eventsNamed(events, "InvalidActivationTimeout"), "the same value is said once")
}

// The pool policy has no activation section. It is an unknown field there, as
// on a controller that predates the timeout.
func TestAPoolPolicyHasNoActivationSection(t *testing.T) {
	_, err := parsePoolPolicy(`{"activation":{"timeoutSeconds":60}}`)
	var configErr *poolPolicyConfigError
	require.ErrorAs(t, err, &configErr)
	assert.Equal(t, poolPolicyErrorUnknownField, configErr.class)
}

// A clock not seen for a while is dropped, so the instances of claims that
// are gone do not pile up.
func TestAStuckClockNotSeenForAWhileIsDropped(t *testing.T) {
	clocks := &stuckActivations{}
	t0 := time.Unix(1_700_000_000, 0)
	assert.Equal(t, t0, clocks.observe("gone", t0))
	assert.Equal(t, t0, clocks.observe("kept", t0))
	assert.Equal(t, t0, clocks.observe("kept", t0.Add(stuckActivationForgetAfter-time.Second)))
	later := t0.Add(stuckActivationForgetAfter)
	assert.Equal(t, t0, clocks.observe("kept", later), "seen on every pass, the clock keeps its start")
	assert.NotContains(t, clocks.seen, "gone")
	assert.Equal(t, later, clocks.observe("gone", later), "seen again, it starts over")
}
