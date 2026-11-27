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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
// window.
func aStuckBoot(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *stuckAt) {
	t.Helper()
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: "warm-1", Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 300,
	}}
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	t0 := time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(t0, 6*time.Minute)}
	r, runtime := newReconciler(t, pm, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, &stuckAt{t0: t0, snapshot: snapshot}
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
	assert.Equal(t, 1, eventsNamed(events, "within "+timeout+" after its engine stopped booting"))
	assert.Zero(t, eventsNamed(events, "EngineFailed"), "the timeout says why, and is not said twice")
	assert.Contains(t, routeOf(t, r, "warm-1", claim), `"state":"failed"`)
}

// An engine that never becomes ready is stopped, and its instance fails, once
// it has been stuck for the default timeout after its boot window.
func TestAnEngineThatNeverGetsReadyFailsAfterTheActivationTimeout(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)

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

// An engine the runtime still reports booting, restarting or stopping is
// never timed out, however long the controller has watched it. Those are the
// runtime's to settle.
func TestAnEngineTheRuntimeStillSettlesIsNotTimedOut(t *testing.T) {
	cases := map[string]func(t0, at time.Time) RuntimeSnapshotModel{
		// Each reading shows a boot that began a minute before it.
		"booting": func(_, at time.Time) RuntimeSnapshotModel { return engineBootingFor(at, time.Minute) },
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
	passesTill(t, r, clock, pm.Name, 0, 25*time.Minute)

	readyAt := clock.t0.Add(25 * time.Minute)
	ready := readyEngine(5000)
	ready.LastTransition = &readyAt
	clock.snapshot.Models = []RuntimeSnapshotModel{ready}
	passesTill(t, r, clock, pm.Name, 25*time.Minute, 25*time.Minute+defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, 25*time.Minute+defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls, "the time before it got ready does not count")

	events := passAt(t, r, clock, pm.Name, 25*time.Minute+defaultActivationTimeout)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

// A restart dates the engine anew. Its timeout counts from when it was seen
// stuck again after the new boot window.
func TestAnEngineRestartStartsItsActivationTimeoutOver(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	passesTill(t, r, clock, pm.Name, 0, 25*time.Minute)

	// At 25 minutes the engine restarts, and boots again for a minute.
	restarted := clock.t0.Add(24 * time.Minute)
	clock.snapshot.Models[0].LastTransition = &restarted
	passAt(t, r, clock, pm.Name, 25*time.Minute)
	// Past its new boot window, it is stuck again from 31 minutes on.
	passesTill(t, r, clock, pm.Name, 31*time.Minute, 31*time.Minute+defaultActivationTimeout)
	passAt(t, r, clock, pm.Name, 31*time.Minute+defaultActivationTimeout-time.Second)
	assert.Empty(t, runtime.deactivateCalls, "the time before the restart does not count")

	events := passAt(t, r, clock, pm.Name, 31*time.Minute+defaultActivationTimeout)
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

// A pool sets its own timeout in its policy.
func TestAPoolSetsItsActivationTimeout(t *testing.T) {
	deployment, replicaSet, pod := warmPoolObjects(`{"activation":{"timeoutSeconds":120}}`)
	pm := claimWithCost(700, 100)
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActivating, KVLimitBytes: 300,
	}}
	_, snapshot := sizedWarmPod(pod.Name, pod.Status.PodIP, 1000)
	t0 := time.Unix(1_700_000_000, 0)
	snapshot.Models = []RuntimeSnapshotModel{engineBootingFor(t0, 6*time.Minute)}
	r, runtime := newReconciler(t, pm, deployment, replicaSet, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	clock := &stuckAt{t0: t0, snapshot: snapshot}

	passAt(t, r, clock, pm.Name, 0)
	passAt(t, r, clock, pm.Name, 119*time.Second)
	assert.Empty(t, runtime.deactivateCalls)

	events := passAt(t, r, clock, pm.Name, 120*time.Second)
	assertTimedOut(t, r, runtime, pm.Name, events, "2m0s")
}

// A stop the runtime does not take leaves the instance Activating, and is
// asked again on the next pass. The instance fails only once the engine is
// stopped.
func TestAStopThatIsNotTakenIsAskedAgain(t *testing.T) {
	r, runtime, pm, clock := aStuckBoot(t)
	runtime.deactivateErr = errors.New("runtime busy")

	passesTill(t, r, clock, pm.Name, 0, defaultActivationTimeout)
	events := passAt(t, r, clock, pm.Name, defaultActivationTimeout)
	require.Len(t, runtime.deactivateCalls, 1)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)
	assert.Zero(t, eventsNamed(events, instanceReasonActivationTimedOut))

	passAt(t, r, clock, pm.Name, defaultActivationTimeout+10*time.Second)
	require.Len(t, runtime.deactivateCalls, 2, "the stop is asked again")
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Instances[0].Phase)

	runtime.deactivateErr = nil
	runtime.deactivateCalls, runtime.deactivatedOn = nil, nil
	events = passAt(t, r, clock, pm.Name, defaultActivationTimeout+20*time.Second)
	assertTimedOut(t, r, runtime, pm.Name, events, "30m0s")
}

func TestActivationTimeoutOfAPool(t *testing.T) {
	cases := map[string]struct {
		policy string
		want   time.Duration
	}{
		"no policy":               {"", defaultActivationTimeout},
		"a policy without it":     {`{"lifecycle":{"sleepAfterSeconds":60}}`, defaultActivationTimeout},
		"its own timeout":         {`{"activation":{"timeoutSeconds":600}}`, 10 * time.Minute},
		"beside a lifecycle":      {`{"lifecycle":{"sleepAfterSeconds":60},"activation":{"timeoutSeconds":90}}`, 90 * time.Second},
		"a policy that not valid": {`{"activation":{"timeoutSeconds":0}}`, defaultActivationTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{constants.ModelPoolPolicyAnnotationKey: tc.policy},
			}}
			assert.Equal(t, tc.want, activationTimeoutOf(deployment))
		})
	}
}

func TestAnActivationTimeoutMustBePositive(t *testing.T) {
	for _, raw := range []string{
		`{"activation":{"timeoutSeconds":0}}`,
		`{"activation":{"timeoutSeconds":-5}}`,
		`{"activation":{}}`,
		// One second more than a time.Duration holds.
		`{"activation":{"timeoutSeconds":9223372037}}`,
	} {
		_, err := parsePoolPolicy(raw)
		var configErr *poolPolicyConfigError
		require.ErrorAs(t, err, &configErr, raw)
		assert.Equal(t, poolPolicyErrorInvalidTimeout, configErr.class, raw)
		assert.Contains(t, err.Error(), "activation.timeoutSeconds must be positive and at most 9223372036", raw)
	}
	// The longest timeout that a time.Duration holds is taken as it is.
	longest, err := parsePoolPolicy(`{"activation":{"timeoutSeconds":9223372036}}`)
	require.NoError(t, err)
	assert.Equal(t, int64(9223372036), longest.Activation.TimeoutSeconds)
	assert.Positive(t, activationTimeoutOf(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{constants.ModelPoolPolicyAnnotationKey: `{"activation":{"timeoutSeconds":9223372036}}`},
	}}))
	_, err = parsePoolPolicy(`{"activation":{"timeout":60}}`)
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
