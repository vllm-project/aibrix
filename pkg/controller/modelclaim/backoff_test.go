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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestPlacementBackoffWaitsLongerAfterEachRefusal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim))
	assert.Equal(t, 2*DefaultRequeueDuration, backoff.refused(claim))
	assert.Equal(t, 4*DefaultRequeueDuration, backoff.refused(claim))
}

func TestPlacementBackoffStopsDoublingAtItsCeiling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	wait := time.Duration(0)
	for i := 0; i < 40; i++ {
		wait = backoff.refused(claim)
	}

	assert.Equal(t, maximumPlacementBackoff, wait)
}

func TestPlacementBackoffHoldsAClaimUntilItsTurn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	due, left := backoff.due(claim)
	assert.True(t, due)
	assert.Zero(t, left)

	backoff.refused(claim)
	due, left = backoff.due(claim)
	assert.False(t, due)
	assert.Equal(t, DefaultRequeueDuration, left)

	now = now.Add(DefaultRequeueDuration)
	due, _ = backoff.due(claim)
	assert.True(t, due)
}

func TestPlacementBackoffStartsOverOnceAClaimIsPlaced(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	backoff := newPlacementBackoff(func() time.Time { return now })
	claim := types.NamespacedName{Namespace: testNamespace, Name: "qwen"}

	backoff.refused(claim)
	backoff.refused(claim)
	backoff.placed(claim)

	due, _ := backoff.due(claim)
	assert.True(t, due)
	assert.Equal(t, DefaultRequeueDuration, backoff.refused(claim))
}

// reconcileFor reconciles a claim once and returns how soon it asked to be
// reconciled again.
func reconcileFor(t *testing.T, r *ModelClaimReconciler, name string) time.Duration {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name},
	})
	require.NoError(t, err)
	return result.RequeueAfter
}

// aClaimWaitingForRoom is a claim that needs 800 of a 1000 card on which a
// neighbour is already promised 400, so it can never fit until the neighbour
// goes. The clock the backoff reads is the one returned.
func aClaimWaitingForRoom(t *testing.T) (*ModelClaimReconciler, *fakeRuntime, *modelv1alpha1.ModelClaim, *time.Time) {
	t.Helper()
	pm := claimWithCost(700, 100)
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	neighbour := claimOnPod("neighbour", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100)
	neighbour.Status.Instances[0].KVLimitBytes = 600
	snapshot.Models = []RuntimeSnapshotModel{engineHolding("neighbour", 100, 600)}
	r, runtime := newReconciler(t, pm, pod, neighbour)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	return r, runtime, pm, &now
}

func TestReconcileBacksOffAClaimNoCardCanHold(t *testing.T) {
	r, runtime, pm, clock := aClaimWaitingForRoom(t)

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	activations := len(runtime.activateCalls)
	asked := runtime.snapshotCalls
	require.NotZero(t, asked)

	// Inside the wait, the pool is not asked about the claim again, and the
	// claim comes back when its wait is up.
	*clock = clock.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration/2, reconcileFor(t, r, pm.Name))
	assert.Equal(t, asked, runtime.snapshotCalls)

	// Each try that is refused doubles the wait, up to a minute.
	*clock = clock.Add(DefaultRequeueDuration / 2)
	for _, want := range []time.Duration{20 * time.Second, 40 * time.Second, time.Minute, time.Minute} {
		assert.Equal(t, want, reconcileFor(t, r, pm.Name))
		*clock = clock.Add(want)
	}
	assert.Len(t, runtime.activateCalls, activations, "nothing could hold it, so nothing was started")
}

func TestReconcileForgetsTheWaitOnceTheModelIsPlaced(t *testing.T) {
	pm := claimWithCost(700, 100)
	small, smallSnapshot := sizedWarmPod("warm-small", "10.0.0.1", 500)
	roomy, roomySnapshot := sizedWarmPod("warm-roomy", "10.0.0.2", 2000)
	r, runtime := newReconciler(t, pm, small)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{
		small.Status.PodIP: smallSnapshot,
		roomy.Status.PodIP: roomySnapshot,
	}
	claim := types.NamespacedName{Namespace: testNamespace, Name: pm.Name}

	reconcileOnce(t, r, pm.Name)
	_, waiting := r.Backoff.attempts[claim]
	require.True(t, waiting)

	require.NoError(t, r.Create(context.Background(), roomy))
	now = now.Add(DefaultRequeueDuration)
	reconcileOnce(t, r, pm.Name)

	require.Len(t, runtime.activateCalls, 1)
	_, waiting = r.Backoff.attempts[claim]
	assert.False(t, waiting, "a placed claim that waits again starts from the shortest wait")
}

func TestReconcileChecksAPartlyPlacedClaimEveryRound(t *testing.T) {
	pm := claimWithCost(300, 100)
	two := int32(2)
	pm.Spec.Replicas = &two
	// One card, which takes one instance of the model and cannot take a
	// second, since an engine runs at most once on a pod.
	pod, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	r, runtime := newReconciler(t, pm, pod)
	now := time.Unix(1_700_000_000, 0)
	r.Backoff = newPlacementBackoff(func() time.Time { return now })
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
	require.Len(t, getModel(t, r, pm.Name).Status.Instances, 1)

	// The second replica waits, but the first still has its engine checked
	// every round, so the claim does not sleep through the wait.
	now = now.Add(DefaultRequeueDuration / 2)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, pm.Name))
}
