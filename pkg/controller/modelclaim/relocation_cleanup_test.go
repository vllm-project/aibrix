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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type relocationStopRuntime struct {
	RuntimeClient
	attempted []string
	block     bool
}

func (f *relocationStopRuntime) Deactivate(ctx context.Context, ip string, _ int, _ *DeactivateRequest) error {
	f.attempted = append(f.attempted, ip)
	if f.block {
		<-ctx.Done()
	}
	return context.DeadlineExceeded
}

func TestRelocationCleanupBoundsStopWorkAndServicesOtherPods(t *testing.T) {
	ctx := context.Background()
	claim := claimWithCost(300, 100)
	claim.UID = "claim-uid"
	objs := []client.Object{claim}
	var pods []corev1.Pod
	snapshots := map[string]*RuntimeSnapshot{}
	for n := 1; n <= 3; n++ {
		ip := fmt.Sprintf("10.0.0.%d", n)
		pod, snapshot := sizedWarmPod(fmt.Sprintf("source-%d", n), ip, 1000)
		pod.UID = types.UID(pod.Name)
		snapshot.Models = []RuntimeSnapshotModel{{
			ModelName: servedModelName(claim), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
			ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
		}}
		objs = append(objs, pod)
		pods = append(pods, *pod)
		snapshots[ip] = snapshot
	}
	r, runtime := newReconciler(t, objs...)
	runtime.snapshots = snapshots
	blocked := &relocationStopRuntime{RuntimeClient: runtime, block: true}
	r.Runtime = blocked
	now := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	pass := func() {
		readings := newRuntimeReadings(r.Runtime)
		for i := range pods {
			readings.replace(&pods[i], snapshots[pods[i].Status.PodIP])
		}
		r.reconcileRelocationCleanup(ctx, claim.Namespace, pods, readings)
	}

	pass()
	require.Empty(t, blocked.attempted)
	now = now.Add(time.Minute)
	start := time.Now()
	pass()
	require.Len(t, blocked.attempted, 1, "one stuck stop is the only cleanup in the pass")
	assert.Less(t, time.Since(start), 6*time.Second)
	blocked.block = false
	for range 2 {
		now = now.Add(time.Minute)
		pass()
	}
	assert.ElementsMatch(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, blocked.attempted)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Warning RelocatedEngineStopFailed")
}

func TestRelocationCleanupDoesNotReadAuthoritativeClaimsOnAHealthyPool(t *testing.T) {
	ctx := context.Background()
	claim := claimWithCost(300, 100)
	claim.UID = "claim-uid"
	source, snapshot := sizedWarmPod("warm-source", "10.0.0.1", 1000)
	target, targetSnapshot := sizedWarmPod("warm-target", testPeerIP, 1000)
	source.UID, target.UID = "source-uid", "target-uid"
	claim.Status.Instances = []modelv1alpha1.ModelClaimInstance{{
		Pod: source.Name, Port: 9001, Phase: modelv1alpha1.ModelClaimActive,
	}}
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(claim), Port: 9001, Phase: "active", Alive: true,
		ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
	}}
	r, runtime := newReconciler(t, claim, source, target)
	runtime.snapshots = map[string]*RuntimeSnapshot{
		source.Status.PodIP: snapshot,
		target.Status.PodIP: targetSnapshot,
	}
	calls := 0
	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			calls++
			return c.List(ctx, list, opts...)
		},
	})
	pods := []corev1.Pod{*source, *target}
	r.reconcileRelocationCleanup(ctx, claim.Namespace, pods, newRuntimeReadings(runtime))
	r.reconcileRelocationCleanup(ctx, claim.Namespace, pods, newRuntimeReadings(runtime))
	assert.Zero(t, calls)
	assert.Len(t, runtime.snapshotCallsTo, 1, "only one extra discovery read is allowed per namespace interval")
}

func TestRelocationCleanupBudgetDoesNotSilenceRuntime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c, runtimes := hangingRuntimes(1, func() time.Time { return now })
	runtime := runtimes[0]
	c.snapshotTimeout = 5 * time.Second
	ctx, cancel := relocationCleanupContext(context.Background())
	defer cancel()

	_, err := c.Snapshot(ctx, runtime.host, runtime.port)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, c.silence.runtimes, runtime.address())
	runtime.asked(t, 1)
}

func TestRelocationCleanupBacksOffAcceptedStopsWhileTheEngineRemains(t *testing.T) {
	ctx := context.Background()
	claim := claimWithCost(300, 100)
	claim.UID = "claim-uid"
	pod, snapshot := sizedWarmPod("old-source", "10.0.0.1", 1000)
	pod.UID = "source-uid"
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(claim), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		ClaimRef: &ModelClaimRef{Namespace: claim.Namespace, Name: claim.Name, UID: string(claim.UID)},
	}}
	r, runtime := newReconciler(t, claim, pod)
	runtime.snapshots = map[string]*RuntimeSnapshot{pod.Status.PodIP: snapshot}
	start := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		elapsed time.Duration
		stops   int
	}{
		{0, 0},
		{35 * time.Second, 1},
		{70 * time.Second, 2},
		{105 * time.Second, 2},
		{140 * time.Second, 3},
	} {
		r.Now = func() time.Time { return start.Add(tc.elapsed) }
		r.reconcileRelocationCleanup(ctx, claim.Namespace, []corev1.Pod{*pod}, newRuntimeReadings(runtime))
		assert.Len(t, runtime.deactivateCalls, tc.stops, "after %s", tc.elapsed)
	}
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Normal RelocatedEngineStopRequested")

	missing := relocationCleanupTotal.WithLabelValues("skipped", "missing_claim_ref")
	before := testutil.ToFloat64(missing)
	snapshot.Models[0].ClaimRef = nil
	r.Now = func() time.Time { return start.Add(5 * time.Minute) }
	r.reconcileRelocationCleanup(ctx, claim.Namespace, []corev1.Pod{*pod}, newRuntimeReadings(runtime))
	assert.Len(t, runtime.deactivateCalls, 3)
	assert.Equal(t, before+1, testutil.ToFloat64(missing))
}

func newRelocationProbeFixture(t *testing.T, size, orphan int) (*ModelClaimReconciler, *fakeRuntime, []corev1.Pod, *time.Time) {
	t.Helper()
	owner := claimWithCost(300, 100)
	owner.UID = "claim-uid"
	objs := []client.Object{owner}
	pods := make([]corev1.Pod, 0, size)
	snapshots := map[string]*RuntimeSnapshot{}
	for n := 1; n <= size; n++ {
		pod, snapshot := sizedWarmPod(fmt.Sprintf("warm-%02d", n), fmt.Sprintf("10.0.0.%d", n), 1000)
		pod.UID = types.UID(pod.Name)
		if n == orphan {
			snapshot.Models = []RuntimeSnapshotModel{{
				ModelName: servedModelName(owner), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
				ClaimRef: &ModelClaimRef{Namespace: owner.Namespace, Name: owner.Name, UID: string(owner.UID)},
			}}
		}
		objs = append(objs, pod)
		pods = append(pods, *pod)
		snapshots[pod.Status.PodIP] = snapshot
	}
	r, runtime := newReconciler(t, objs...)
	runtime.snapshots = snapshots
	now := time.Date(2026, time.October, 1, 8, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	return r, runtime, pods, &now
}

func TestRelocationCleanupConfirmsAnOrphanBeforeDiscoveringTheEntirePool(t *testing.T) {
	r, runtime, pods, now := newRelocationProbeFixture(t, 21, 1)
	for range 3 {
		r.reconcileRelocationCleanup(context.Background(), testNamespace, pods, newRuntimeReadings(runtime))
		*now = now.Add(30 * time.Second)
	}
	assert.Equal(t, []string{pods[0].Status.PodIP}, runtime.deactivatedOn)
	assert.Positive(t, runtime.snapshotCallsTo[pods[1].Status.PodIP], "confirmation must leave room for discovery")
}

func TestRelocationCleanupProbesAcrossDisjointSelectors(t *testing.T) {
	r, runtime, pods, now := newRelocationProbeFixture(t, 2, 2)
	for range 6 {
		// The first pool always reconciles before the second.
		for i := range pods {
			r.reconcileRelocationCleanup(context.Background(), testNamespace, pods[i:i+1], newRuntimeReadings(runtime))
		}
		*now = now.Add(30 * time.Second)
	}
	assert.Contains(t, runtime.deactivatedOn, pods[1].Status.PodIP)
	assert.Positive(t, runtime.snapshotCallsTo[pods[0].Status.PodIP])
}

func TestRelocationCleanupKeepsAnUnconfirmedOrphanPastThePruneWindow(t *testing.T) {
	r, runtime, pods, now := newRelocationProbeFixture(t, 21, 1)
	r.reconcileRelocationCleanup(context.Background(), testNamespace, pods, newRuntimeReadings(runtime))
	require.Empty(t, runtime.deactivatedOn)
	*now = now.Add(11 * time.Minute)
	r.reconcileRelocationCleanup(context.Background(), testNamespace, pods, newRuntimeReadings(runtime))
	assert.Equal(t, []string{pods[0].Status.PodIP}, runtime.deactivatedOn)
}

func TestRelocationCleanupDiscoveryStillAdvancesWithConfirmationsWaiting(t *testing.T) {
	r, runtime, pods, now := newRelocationProbeFixture(t, 3, 1)
	ctx := context.Background()
	first, second, third := pods[0].Status.PodIP, pods[1].Status.PodIP, pods[2].Status.PodIP
	runtime.snapshots[second].Models = []RuntimeSnapshotModel{runtime.snapshots[first].Models[0]}
	readings := newRuntimeReadings(runtime)
	readings.replace(&pods[0], runtime.snapshots[first])
	r.reconcileRelocationCleanup(ctx, testNamespace, pods, readings)
	require.Equal(t, 1, runtime.snapshotCallsTo[second])

	*now = now.Add(30 * time.Second)
	r.reconcileRelocationCleanup(ctx, testNamespace, pods, newRuntimeReadings(runtime))
	require.Equal(t, []string{first}, runtime.deactivatedOn)
	*now = now.Add(30 * time.Second)
	r.reconcileRelocationCleanup(ctx, testNamespace, pods, newRuntimeReadings(runtime))
	assert.Equal(t, []string{first}, runtime.deactivatedOn, "discovery gets its turn")
	assert.Equal(t, 1, runtime.snapshotCallsTo[third])
	*now = now.Add(30 * time.Second)
	r.reconcileRelocationCleanup(ctx, testNamespace, pods, newRuntimeReadings(runtime))
	assert.Equal(t, []string{first, second}, runtime.deactivatedOn)
}

func TestRelocationCleanupPrunesAnExpiredObservationWhenThePodIsGone(t *testing.T) {
	r, runtime, pods, now := newRelocationProbeFixture(t, 1, 1)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(&pods[0])
	r.reconcileRelocationCleanup(ctx, testNamespace, pods, newRuntimeReadings(runtime))
	require.NotEmpty(t, r.relocationChecks.pods[key].engines)
	require.NoError(t, r.Delete(ctx, &pods[0]))

	*now = now.Add(11 * time.Minute)
	r.reconcileRelocationCleanup(ctx, testNamespace, nil, newRuntimeReadings(runtime))
	assert.NotContains(t, r.relocationChecks.pods, key)
	assert.Empty(t, runtime.deactivateCalls)
}
