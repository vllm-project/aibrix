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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

func setResidencyPolicy(t *testing.T, r *ModelClaimReconciler, name, policy string) {
	t.Helper()
	claim := getModel(t, r, name)
	patch := client.MergeFrom(claim.DeepCopy())
	require.NoError(t, json.Unmarshal([]byte(`{"residencyPolicy":`+policy+`}`), &claim.Spec))
	require.NoError(t, r.Patch(context.Background(), claim, patch))
}

func TestWakePolicyCompatibilityDefaults(t *testing.T) {
	for _, tt := range []struct {
		name, policy string
		proactive    bool
	}{
		{"unset", `null`, false},
		{"empty", `{}`, false},
		{"pool default", `{"sleepPolicy":{"mode":"PoolDefault"}}`, false},
		// #2924 can add AfterIdle later. The resolver's conservative fallback
		// is OnDemand, though this draft CRD does not admit AfterIdle yet.
		{"future after idle", `{"sleepPolicy":{"mode":"AfterIdle","idleTimeout":"15m"}}`, false},
		{"never", `{"sleepPolicy":{"mode":"Never"}}`, true},
		{"explicit on demand", `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"OnDemand"}}`, false},
		{"ensure awake", `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"EnsureAwake"}}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, runtime, pm, _ := sleepingClaim(t)
			setResidencyPolicy(t, r, pm.Name, tt.policy)
			reconcileOnce(t, r, pm.Name)
			if tt.proactive {
				require.Len(t, runtime.wakeCalls, 1, "desired state must wake without user traffic")
			} else {
				assert.Empty(t, runtime.wakeCalls)
				askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:00:00Z")
				reconcileOnce(t, r, pm.Name)
				require.Len(t, runtime.wakeCalls, 1, "OnDemand preserves controller-driven request wakes")
			}
		})
	}
}

func TestWakeQueueOrdersFractionalSecondTimestamps(t *testing.T) {
	// Go's RFC3339 parser accepts fractional seconds even when the layout
	// omits them. Keep nanoseconds so a same-second gateway overwrite is
	// distinguishable from the controller-owned request.
	const earlier = "2026-10-01T08:00:00.000000001Z"
	const later = "2026-10-01T08:00:00.000000002Z"
	_, err := time.Parse(time.RFC3339, earlier)
	require.NoError(t, err)
	assert.True(t, askedBefore(earlier, "z", later, "a"))
	assert.False(t, askedBefore(later, "a", earlier, "z"))
}

func TestEnsureAwakeWaitsForSnapshotReadinessAndReusesOperationAcrossRestart(t *testing.T) {
	r, runtime, pm, port := sleepingClaim(t)
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"EnsureAwake"}}`)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	operationID := runtime.wakeCalls[0].OperationID
	assert.Equal(t, modelv1alpha1.ModelClaimSleeping, getModel(t, r, pm.Name).Status.Phase)
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)

	// Reconstruct the reconciler: only persisted Kubernetes state survives.
	r = &ModelClaimReconciler{
		Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme,
		Recorder: r.Recorder, Runtime: r.Runtime, Locality: r.Locality, Now: r.Now,
	}
	runtime.wakeAlreadyApplied = true
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 2)
	assert.Equal(t, operationID, runtime.wakeCalls[1].OperationID)
	assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WakeFailed")

	runtime.models[servedModelName(pm)] = ModelInfo{ModelName: servedModelName(pm), Port: port, Phase: "active"}
	runtime.notReady = true
	reconcileOnce(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Phase)
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)
	assert.Len(t, runtime.wakeCalls, 2)

	runtime.notReady = false
	reconcileOnce(t, r, pm.Name)
	claim := getModel(t, r, pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, claim.Status.Phase)
	assert.Equal(t, int32(1), claim.Status.ReadyReplicas)
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], fmt.Sprintf(`"port":%d`, port))
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	assert.Len(t, runtime.wakeCalls, 2)
}

func TestEnsureAwakeRetriesFailuresWithoutUserTraffic(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime refusal=%t", refusal), func(t *testing.T) {
			r, runtime, pm, _ := sleepingClaim(t)
			setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
			if refusal {
				runtime.failWake = true
			} else {
				runtime.wakeErr = fmt.Errorf("runtime: %w", errRuntimeSilent)
			}
			reconcileOnce(t, r, pm.Name)
			require.Len(t, runtime.wakeCalls, 1)
			claim := getModel(t, r, pm.Name)
			ready := meta.FindStatusCondition(claim.Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status)
			events := strings.Join(drainEvents(t, r), "\n")
			if refusal {
				assert.Equal(t, "WakeFailed", ready.Reason)
				assert.Contains(t, events, "WakeFailed")
			} else {
				assert.Equal(t, "EngineSleeping", ready.Reason)
				assert.NotContains(t, events, "WakeFailed", "an unanswered operation is not a runtime refusal")
			}
			assert.Contains(t, podNamed(t, r, "warm-1").Annotations[constants.ModelClaimPodAnnotationPrefix+pm.Name], `"port":0`)
			if refusal {
				now := r.now().Add(time.Second)
				r.Now = func() time.Time { return now }
			}
			reconcileOnce(t, r, pm.Name)
			if refusal {
				assert.Len(t, runtime.wakeCalls, 1, "refusals must back off")
				assert.NotContains(t, strings.Join(drainEvents(t, r), "\n"), "WakeFailed", "do not repeat failure events")
				now := r.now().Add(9 * time.Second)
				r.Now = func() time.Time { return now }
				reconcileOnce(t, r, pm.Name)
			}
			require.Len(t, runtime.wakeCalls, 2)
			if refusal {
				assert.NotEqual(t, runtime.wakeCalls[0].OperationID, runtime.wakeCalls[1].OperationID, "an explicit refusal needs a new attempt")
			} else {
				assert.Equal(t, runtime.wakeCalls[0].OperationID, runtime.wakeCalls[1].OperationID, "an unanswered operation may have been applied")
			}
			assert.Empty(t, runtime.deactivateCalls)
		})
	}
}

func TestPolicyWakeYieldsToLaterClientRequest(t *testing.T) {
	r, runtime := queueCard(t, 800,
		queued{name: "a", footprint: 300, floor: 100, idleSince: at0758},
		queued{name: "b", footprint: 300, floor: 100, asleep: true, askedAt: "2026-10-01T07:59:00Z"},
		queued{name: "waker", footprint: 300, floor: 100, asleep: true, askedAt: "2026-10-01T08:00:00Z"})
	claim := getModel(t, r, "b")
	pod := podNamed(t, r, "warm-1")
	patch := client.MergeFrom(pod.DeepCopy())
	request := policyWakeRequest{
		RequestedAt: pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claim.Name],
		OperationID: fmt.Sprintf("controller-policy-wake/%s/%s/test", pod.UID, claim.UID),
	}
	value, err := json.Marshal(request)
	require.NoError(t, err)
	pod.Annotations[constants.ModelClaimPolicyWakeAnnotationPrefix+claim.Name] = string(value)
	require.NoError(t, r.Patch(context.Background(), pod, patch))
	reconcileOnce(t, r, "waker")
	require.Len(t, runtime.sleepCalls, 1, "client traffic gets first access to room")
	assert.Equal(t, "a", runtime.sleepCalls[0].ModelName)
}

func TestDeactivationRemovesOrphanPolicyWake(t *testing.T) {
	r, _, pm, _ := sleepingClaim(t)
	pod := podNamed(t, r, "warm-1")
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, constants.ModelClaimPodAnnotationPrefix+pm.Name)
	delete(pod.Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	pod.Annotations[constants.ModelClaimPolicyWakeAnnotationPrefix+pm.Name] = "orphan"
	require.NoError(t, r.Patch(context.Background(), pod, patch))
	r.deannotateWarmPod(context.Background(), pm.Namespace, pod.Name, pm.Name)
	assert.NotContains(t, podNamed(t, r, pod.Name).Annotations, constants.ModelClaimPolicyWakeAnnotationPrefix+pm.Name)
}

func TestPolicyWakeBackoffPersistsAndCapsAcrossRestart(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	runtime.failWake = true
	now := r.now()
	r.Now = func() time.Time { return now }
	for i, delay := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		reconcileOnce(t, r, pm.Name)
		require.Len(t, runtime.wakeCalls, i+1)
		pod := podNamed(t, r, "warm-1")
		request, owned := policyWakeOnPod(getModel(t, r, pm.Name), pod)
		require.True(t, owned)
		retryAt, err := time.Parse(time.RFC3339Nano, request.RetryAfter)
		require.NoError(t, err)
		assert.Equal(t, now.Add(delay), retryAt)
		// A fresh reconciler has no in-memory retry clock.
		r = &ModelClaimReconciler{Client: r.Client, Scheme: r.Scheme, Recorder: r.Recorder, Runtime: runtime, Now: func() time.Time { return now }}
		now = retryAt.Add(-time.Nanosecond)
		reconcileOnce(t, r, pm.Name)
		assert.Len(t, runtime.wakeCalls, i+1)
		now = retryAt
	}
	assert.Equal(t, 1, strings.Count(strings.Join(drainEvents(t, r), "\n"), "WakeFailed"), "only the transition into failure emits an event")
}

func TestPolicyWakeRequestExpiresAfterEngineStartsBooting(t *testing.T) {
	r, runtime, pm, port := sleepingClaim(t)
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	now := r.now()
	r.Now = func() time.Time { return now }
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	runtime.models[servedModelName(pm)] = ModelInfo{ModelName: servedModelName(pm), Port: port, Phase: "active"}
	runtime.notReady = true
	now = now.Add(10 * time.Second)
	reconcileOnce(t, r, pm.Name)
	assert.Contains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	now = now.Add(wakeRequestLifetime + time.Second)
	reconcileOnce(t, r, pm.Name)
	pod := podNamed(t, r, "warm-1")
	assert.NotContains(t, pod.Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	assert.NotContains(t, pod.Annotations, constants.ModelClaimPolicyWakeAnnotationPrefix+pm.Name)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, getModel(t, r, pm.Name).Status.Phase)
	assert.Len(t, runtime.wakeCalls, 1)
}

func TestEnsureAwakeRespectsCapacityAdmissionBeyondRequestLifetime(t *testing.T) {
	r, runtime, pm := overcommittedSleeper(t)
	r.takeBackWakeRequest(context.Background(), podNamed(t, r, "warm-1"), constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	for _, at := range []time.Time{at0759, at0759.Add(6 * time.Minute), at0759.Add(7 * time.Minute)} {
		r.Now = func() time.Time { return at }
		reconcileOnce(t, r, pm.Name)
		assert.Empty(t, runtime.wakeCalls, "policy wakes use the same memory ledger as request wakes")
		claim := getModel(t, r, pm.Name)
		assert.Equal(t, modelv1alpha1.ModelClaimSleeping, claim.Status.Phase)
		assert.Equal(t, "WaitingForRoom", claim.Status.Instances[0].Reason)
	}
}

func TestNeverCannotBeSleptToMakeRoom(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setResidencyPolicy(t, r, "a", `{"sleepPolicy":{"mode":"Never"}}`)
	newClaim(t, r, "x", at0759)
	reconcileOnce(t, r, "x")
	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "b", runtime.sleepCalls[0].ModelName, "the protected idlest engine stays awake")
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "a").Status.Instances[0].Phase)
}

func TestNeverCannotBeSleptByPoolIdlePolicy(t *testing.T) {
	r, runtime := servingPool(t, `{"lifecycle":{"sleepAfterSeconds":60}}`, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	setResidencyPolicy(t, r, "a", `{"sleepPolicy":{"mode":"Never"}}`)
	r.reconcilePoolPolicies(context.Background(), []corev1.Pod{*podNamed(t, r, "warm-1")}, newRuntimeReadings(r.Runtime))
	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, "b", runtime.sleepCalls[0].ModelName)
	assert.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, "a").Status.Instances[0].Phase)
}

func TestOnDemandCancelsPendingPolicyWake(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	runtime.wakeErr = errRuntimeSilent
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	pod := podNamed(t, r, "warm-1")
	_, err := time.Parse(time.RFC3339Nano, pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+pm.Name])
	require.NoError(t, err, "policy requests must retain the gateway queue's timestamp format")

	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"OnDemand"}}`)
	reconcileOnce(t, r, pm.Name)
	assert.Len(t, runtime.wakeCalls, 1)
	pod = podNamed(t, r, "warm-1")
	assert.NotContains(t, pod.Annotations, constants.ModelClaimWakeAnnotationPrefix+pm.Name)
	assert.NotContains(t, pod.Annotations, constants.ModelClaimPolicyWakeAnnotationPrefix+pm.Name)
	ready := meta.FindStatusCondition(getModel(t, r, pm.Name).Status.Conditions, string(modelv1alpha1.ModelClaimConditionReady))
	require.NotNil(t, ready)
	assert.Equal(t, "EngineSleeping", ready.Reason)

	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:01:00Z")
	reconcileOnce(t, r, pm.Name)
	assert.Len(t, runtime.wakeCalls, 2, "OnDemand must still honor gateway requests")
}

func TestOnDemandKeepsAGatewayRequestThatReplacedPolicyWake(t *testing.T) {
	r, runtime, pm, _ := sleepingClaim(t)
	runtime.wakeErr = errRuntimeSilent
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	askWake(t, r, "warm-1", pm.Name, "2026-10-01T08:01:00Z")
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"},"wakePolicy":{"mode":"OnDemand"}}`)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 2)
	assert.Contains(t, runtime.wakeCalls[1].OperationID, "2026-10-01T08:01:00Z")
	assert.NotContains(t, podNamed(t, r, "warm-1").Annotations, constants.ModelClaimPolicyWakeAnnotationPrefix+pm.Name)
}

func TestEnsureAwakeMovesARefusedWakeThroughExistingRecovery(t *testing.T) {
	pm := claimWithCost(300, 100)
	pm.UID = types.UID("claim-uid")
	pm.Status.Phase = modelv1alpha1.ModelClaimSleeping
	pm.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimSleeping, KVLimitBytes: 100}}
	sleeper, snapshot := sizedWarmPod("warm-1", "10.0.0.1", 1000)
	snapshot.Models = []RuntimeSnapshotModel{{
		ModelName: servedModelName(pm), Port: 9001, Phase: runtimePhaseSleeping, Alive: true,
		KVUsedBytes: 100, KVCapacityBytes: 100,
		ClaimRef: &ModelClaimRef{Namespace: pm.Namespace, Name: pm.Name, UID: string(pm.UID)},
	}}
	roomy, roomySnapshot := sizedWarmPod("warm-2", testPeerIP, 1000)
	r, runtime := newReconciler(t, pm, sleeper, roomy)
	runtime.snapshots = map[string]*RuntimeSnapshot{sleeper.Status.PodIP: snapshot, roomy.Status.PodIP: roomySnapshot}
	runtime.failWake = true
	setResidencyPolicy(t, r, pm.Name, `{"sleepPolicy":{"mode":"Never"}}`)
	reconcileOnce(t, r, pm.Name)
	require.Len(t, runtime.wakeCalls, 1)
	got := getModel(t, r, pm.Name)
	require.Len(t, got.Status.Instances, 1)
	assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
	assert.Equal(t, modelv1alpha1.ModelClaimActivating, got.Status.Instances[0].Phase)
	assert.Equal(t, []string{roomy.Status.PodIP}, runtime.activatedOn)
	require.Len(t, runtime.deactivateCalls, 1)
	assert.Contains(t, strings.Join(drainEvents(t, r), "\n"), "Moving")
}
