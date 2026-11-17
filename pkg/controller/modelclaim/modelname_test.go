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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

const sharedModelName = "shared-model"

func modelNameClaim(name string, created time.Time) *modelv1alpha1.ModelClaim {
	pm := withFinalizer(sampleModelClaim())
	pm.Name, pm.UID = name, types.UID(name)
	pm.CreationTimestamp = metav1.NewTime(created)
	served := sharedModelName
	pm.Spec.ModelName = &served
	return pm
}

// invalidParallelism is an engineConfig whose tensor parallelism no Pod can run.
func invalidParallelism() *modelv1alpha1.ModelClaimEngineConfig {
	return &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{"--tensor-parallel-size": "0"}}
}

// engineFor reports the shared model's engine on the warm Pods as started for
// owner, as the runtime agent does.
func engineFor(owner *modelv1alpha1.ModelClaim) map[string]*RuntimeSnapshot {
	return map[string]*RuntimeSnapshot{"10.0.0.1": {Models: []RuntimeSnapshotModel{{
		ModelName: sharedModelName, Port: 9001, Phase: "active", Alive: true, Ready: true,
		KVUsedBytes: kvLimitUnknown, KVCapacityBytes: kvLimitUnknown,
		ClaimRef: &ModelClaimRef{Namespace: owner.Namespace, Name: owner.Name, UID: string(owner.UID)},
	}}}}
}

func modelNameInUseEvents(t *testing.T, r *ModelClaimReconciler) []string {
	t.Helper()
	var events []string
	for _, event := range drainEvents(t, r) {
		if strings.Contains(event, constants.ModelClaimReasonModelNameInUse) {
			events = append(events, event)
		}
	}
	return events
}

func TestAClaimWaitsWhileAnotherClaimUsesItsModelName(t *testing.T) {
	first := modelNameClaim("team-a", at0758)
	second := modelNameClaim("team-b", at0759)
	r, runtime := newReconciler(t, first, second,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	)

	reconcileOnce(t, r, first.Name)
	require.Len(t, getModel(t, r, first.Name).Status.Instances, 1)

	reads := runtime.snapshotCalls
	for i := 0; i < 2; i++ {
		assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, second.Name))
	}
	assert.Equal(t, reads, runtime.snapshotCalls, "a claim that waits for its name reads no runtime")

	got := getModel(t, r, second.Name)
	assert.Empty(t, got.Status.Instances)
	assert.Equal(t, modelv1alpha1.ModelClaimPending, got.Status.Phase)
	condition := scheduled(t, r, second.Name)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, constants.ModelClaimReasonModelNameInUse, condition.Reason)
	assert.Equal(t, "model name shared-model is already used by ModelClaim team-a", condition.Message)
	require.Len(t, runtime.activateCalls, 1, "only the first claim starts an engine")
	assert.Equal(t, []string{"Warning ModelNameInUse model name shared-model is already used by ModelClaim team-a"},
		modelNameInUseEvents(t, r), "the Event is raised once, not on every pass")
}

// Neither claim is placed yet, so the older one comes first, whichever is
// reconciled first.
func TestTheOlderClaimComesFirstForAModelName(t *testing.T) {
	older := modelNameClaim("team-b", at0758)
	newer := modelNameClaim("team-a", at0759)
	r, runtime := newReconciler(t, older, newer, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, newer.Name))
	assert.Empty(t, runtime.activateCalls)
	assert.Equal(t, "model name shared-model is reserved for ModelClaim team-b, which waits to be placed",
		scheduled(t, r, newer.Name).Message)

	reconcileOnce(t, r, older.Name)
	require.Len(t, getModel(t, r, older.Name).Status.Instances, 1)
	require.Len(t, runtime.activateCalls, 1)
}

// A claim that already runs keeps its model name, even against an older claim,
// and the older claim's pass leaves its engine alone.
func TestAPlacedClaimKeepsItsModelName(t *testing.T) {
	older := modelNameClaim("team-a", at0758)
	running := modelNameClaim("team-b", at0759)
	running.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	r, runtime := newReconciler(t, older, running, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, older.Name))

	assert.Empty(t, getModel(t, r, older.Name).Status.Instances)
	assert.Equal(t, "model name shared-model is already used by ModelClaim team-b", scheduled(t, r, older.Name).Message)
	assert.Empty(t, runtime.activateCalls)
	assert.Empty(t, runtime.deactivateCalls)
}

func TestModelNameHolder(t *testing.T) {
	failed := []modelv1alpha1.ModelClaimInstance{{Pod: "warm-1", Phase: modelv1alpha1.ModelClaimFailed}}
	live := []modelv1alpha1.ModelClaimInstance{{Pod: "warm-2", Phase: modelv1alpha1.ModelClaimActive}}
	claim := func(name string, created time.Time, instances []modelv1alpha1.ModelClaimInstance) modelv1alpha1.ModelClaim {
		pm := modelNameClaim(name, created)
		pm.Status.Instances = instances
		return *pm
	}
	deleting := claim("deleting", at0759, live)
	deleting.DeletionTimestamp = &metav1.Time{Time: at0759}
	invalidPerGPU := claim("older", at0758, nil)
	invalidPerGPU.Spec.PerGPU = nil
	invalidEngine := claim("older", at0758, nil)
	invalidEngine.Spec.EngineConfig = invalidParallelism()
	renamed := claim("renamed", at0758, nil)
	other := "other-model"
	renamed.Spec.ModelName = &other
	elsewhere := claim("elsewhere", at0758, nil)
	elsewhere.Namespace = "another-namespace"

	for _, tt := range []struct {
		name   string
		pm     modelv1alpha1.ModelClaim
		others []modelv1alpha1.ModelClaim
		want   string
	}{
		{"an older unplaced claim comes first", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{claim("older", at0758, nil)}, "older"},
		{"a newer unplaced claim does not", claim("me", at0758, nil),
			[]modelv1alpha1.ModelClaim{claim("newer", at0759, nil)}, ""},
		{"the name breaks a tie", claim("me", at0758, nil),
			[]modelv1alpha1.ModelClaim{claim("aaa", at0758, nil), claim("zzz", at0758, nil)}, "aaa"},
		{"a placed claim comes first, even with a failed engine", claim("me", at0758, nil),
			[]modelv1alpha1.ModelClaim{claim("newer", at0759, failed)}, "newer"},
		{"a placed claim that needs a replacement keeps its name", claim("me", at0759, failed),
			[]modelv1alpha1.ModelClaim{claim("older", at0758, nil)}, ""},
		{"of two placed claims, the older one comes first", claim("me", at0759, failed),
			[]modelv1alpha1.ModelClaim{claim("older", at0758, failed)}, "older"},
		{"a claim whose engine runs comes before one whose engine failed", claim("me", at0758, failed),
			[]modelv1alpha1.ModelClaim{claim("newer", at0759, live)}, "newer"},
		{"a placed claim beats an older unplaced one", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{claim("oldest", time.Unix(0, 0), nil), claim("placed", at0759.Add(time.Hour), failed)}, "placed"},
		{"a claim being deleted keeps its name while its engine runs", claim("me", at0758, nil),
			[]modelv1alpha1.ModelClaim{deleting}, "deleting"},
		{"an older claim with an invalid perGPU does not keep the name", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{invalidPerGPU}, ""},
		{"an older claim with an invalid engineConfig does not keep the name", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{invalidEngine}, ""},
		{"another model name does not count", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{renamed}, ""},
		{"another namespace does not count", claim("me", at0759, nil),
			[]modelv1alpha1.ModelClaim{elsewhere}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			list := &modelv1alpha1.ModelClaimList{Items: append([]modelv1alpha1.ModelClaim{tt.pm}, tt.others...)}
			holder := modelNameHolder(&tt.pm, list)
			if tt.want == "" {
				assert.Nil(t, holder)
				return
			}
			require.NotNil(t, holder)
			assert.Equal(t, tt.want, holder.Name)
		})
	}
	assert.Nil(t, modelNameHolder(&modelv1alpha1.ModelClaim{}, nil))
}

// A claim that waits for its model name is not placed, so it holds up no claim
// behind it, for a placement turn or for room.
func TestAClaimThatWaitsForItsModelNameHoldsUpNoOne(t *testing.T) {
	holder := modelNameClaim("team-a", at0758)
	waiting := modelNameClaim("team-b", at0759)
	behind := modelNameClaim("team-c", at0800)
	other := "other-model"
	behind.Spec.ModelName = &other
	pods := []corev1.Pod{
		*warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		*warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	}
	r, runtime := newReconciler(t, holder, waiting, behind, &pods[0], &pods[1])
	reconcileOnce(t, r, holder.Name)
	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, waiting.Name))

	line, _ := r.waitingClaims(context.Background(), getModel(t, r, behind.Name), pods)
	assert.Equal(t, []string{"team-c"}, claimNames(line), "team-b is not in line for room")

	reconcileOnce(t, r, behind.Name)
	assert.Len(t, getModel(t, r, behind.Name).Status.Instances, 1, "team-b does not hold up team-c's turn")
	assert.Len(t, runtime.activateCalls, 2)
}

// Two claims placed before model names were kept apart share one engine, which
// runs for team-a. team-b finds that the engine is not its own, so it lets go of
// the Pod and its route, and waits like any other claim. No engine is started
// for it, so this holds with a runtime that would hand it team-a's engine.
func TestAClaimThatSharedAnEngineLetsGoOfIt(t *testing.T) {
	for _, phase := range []modelv1alpha1.ModelClaimPhase{
		modelv1alpha1.ModelClaimActivating, modelv1alpha1.ModelClaimActive, modelv1alpha1.ModelClaimFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			owner := modelNameClaim("team-a", at0758)
			owner.Status.Instances = []modelv1alpha1.ModelClaimInstance{
				{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
			}
			sharer := modelNameClaim("team-b", at0759)
			sharer.Status.Instances = []modelv1alpha1.ModelClaimInstance{{Pod: "warm-1", Port: 9001, Phase: phase}}
			pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
			route := `{"model":"shared-model","port":9001,"state":"active"}`
			pod.Annotations = map[string]string{
				constants.ModelClaimPodAnnotationPrefix + owner.Name:  route,
				constants.ModelClaimPodAnnotationPrefix + sharer.Name: `{"model":"shared-model","port":0,"state":"activating"}`,
			}
			r, runtime := newReconciler(t, owner, sharer, pod)
			runtime.snapshots = engineFor(owner)

			reconcileOnce(t, r, sharer.Name)
			assert.Empty(t, getModel(t, r, sharer.Name).Status.Instances)
			assert.Empty(t, routeOf(t, r, "warm-1", sharer.Name))

			assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, sharer.Name))
			assert.Equal(t, modelv1alpha1.ModelClaimPending, getModel(t, r, sharer.Name).Status.Phase)
			assert.Equal(t, constants.ModelClaimReasonModelNameInUse, scheduled(t, r, sharer.Name).Reason)
			assert.Empty(t, runtime.activateCalls, "no engine is started for team-b")
			assert.Empty(t, runtime.deactivateCalls, "team-a's engine keeps running")
			assert.Equal(t, route, routeOf(t, r, "warm-1", owner.Name))
		})
	}
}

// Two claims placed before model names were kept apart share one engine on a
// Pod, which runs for team-a. Deleting team-b leaves that engine to team-a.
func TestDeletingAClaimLeavesAnEngineThatRunsForAnotherClaim(t *testing.T) {
	kept := modelNameClaim("team-a", at0758)
	kept.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	gone := modelNameClaim("team-b", at0759)
	deleted := metav1.NewTime(time.Now().Add(-deletionDrainGrace))
	gone.DeletionTimestamp = &deleted
	gone.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	route := `{"model":"shared-model","port":9001,"state":"active"}`
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + kept.Name: route,
		constants.ModelClaimPodAnnotationPrefix + gone.Name: route,
	}
	r, runtime := newReconciler(t, kept, gone, pod)
	runtime.snapshots = engineFor(kept)

	reconcileOnce(t, r, gone.Name)

	assert.Empty(t, runtime.deactivateCalls, "the engine that runs for team-a is not stopped")
	assert.Empty(t, routeOf(t, r, "warm-1", gone.Name))
	assert.Equal(t, route, routeOf(t, r, "warm-1", kept.Name))
	got := &modelv1alpha1.ModelClaim{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: gone.Name}, got)
	assert.True(t, apierrors.IsNotFound(err) || !controllerutil.ContainsFinalizer(got, ModelClaimFinalizer),
		"the deleted claim lets go")
}

// The same pair, but the claim deleted is team-a, which the engine runs for.
// Its engine is stopped, though team-b records an instance there. While the
// runtime still reports the engine as team-a's, team-b lets go of the Pod and
// is not started there.
func TestDeletingTheClaimAnEngineRunsForStopsIt(t *testing.T) {
	gone := modelNameClaim("team-a", at0758)
	deleted := metav1.NewTime(time.Now().Add(-deletionDrainGrace))
	gone.DeletionTimestamp = &deleted
	gone.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	sharer := modelNameClaim("team-b", at0759)
	sharer.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActivating},
	}
	pod := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
	route := `{"model":"shared-model","port":9001,"state":"active"}`
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + gone.Name:   route,
		constants.ModelClaimPodAnnotationPrefix + sharer.Name: route,
	}
	r, runtime := newReconciler(t, gone, sharer, pod)
	runtime.snapshots = engineFor(gone)

	reconcileOnce(t, r, gone.Name)

	require.Len(t, runtime.deactivateCalls, 1, "the engine of a deleted claim is not left behind")
	assert.Equal(t, sharedModelName, runtime.deactivateCalls[0].ModelName)
	assert.Empty(t, routeOf(t, r, "warm-1", gone.Name))
	got := &modelv1alpha1.ModelClaim{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: gone.Name}, got)
	assert.True(t, apierrors.IsNotFound(err) || !controllerutil.ContainsFinalizer(got, ModelClaimFinalizer))

	runtime.snapshots["10.0.0.1"].Models[0].Phase = "stopping"
	reconcileOnce(t, r, sharer.Name)
	assert.Empty(t, getModel(t, r, sharer.Name).Status.Instances)
	reconcileOnce(t, r, sharer.Name)
	assert.Contains(t, scheduled(t, r, sharer.Name).Message, "warm-1 still runs model shared-model for ModelClaim team-a")
	assert.Empty(t, runtime.activateCalls)
}

// team-b waits for the name that team-a keeps, and team-a is deleted. team-b
// waits until team-a is gone. While the runtime still reports team-a's engine
// stopping on the only Pod, team-b is refused there, since the runtime would
// refuse to start team-b's engine next to it. Once the engine is gone, team-b
// is placed.
func TestAClaimTakesOverAModelNameOnceTheOtherClaimAndItsEngineAreGone(t *testing.T) {
	gone := modelNameClaim("team-a", at0758)
	deleted := metav1.NewTime(time.Now().Add(-deletionDrainGrace))
	gone.DeletionTimestamp = &deleted
	gone.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: "warm-1", Port: 9001, Phase: modelv1alpha1.ModelClaimActive},
	}
	next := modelNameClaim("team-b", at0759)
	r, runtime := newReconciler(t, gone, next, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))
	runtime.snapshots = engineFor(gone)

	assert.Equal(t, DefaultRequeueDuration, reconcileFor(t, r, next.Name))
	assert.Equal(t, "model name shared-model is already used by ModelClaim team-a", scheduled(t, r, next.Name).Message)

	reconcileOnce(t, r, gone.Name)
	require.Len(t, runtime.deactivateCalls, 1)
	runtime.snapshots["10.0.0.1"].Models[0].Phase = "stopping"
	reconcileOnce(t, r, next.Name)
	assert.Empty(t, runtime.activateCalls, "nothing is started next to the engine that stops")
	assert.Equal(t, modelv1alpha1.ModelClaimPending, getModel(t, r, next.Name).Status.Phase)
	assert.Contains(t, scheduled(t, r, next.Name).Message, "warm-1 still runs model shared-model for ModelClaim team-a")

	runtime.snapshots = nil
	r.backoff().startOver(types.NamespacedName{Namespace: testNamespace, Name: next.Name})
	reconcileOnce(t, r, next.Name)
	assert.Len(t, getModel(t, r, next.Name).Status.Instances, 1)
	assert.Len(t, runtime.activateCalls, 1)
}

// A Pod still runs an engine under the name for a claim that is gone, in any
// phase. The claim is placed on the other Pod of the pool, though the first
// one has its artifact cached.
func TestAClaimIsPlacedBesideAPodThatRunsAnotherClaimsEngine(t *testing.T) {
	for _, phase := range []string{"active", "stopping", "failed"} {
		t.Run(phase, func(t *testing.T) {
			pm := modelNameClaim("team-b", at0759)
			taken := warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning)
			clean := warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning)
			clean.Status.PodIP = "10.0.0.2"
			r, runtime := newReconciler(t, pm, taken, clean)
			gone := modelNameClaim("gone", at0758)
			runtime.snapshots = engineFor(gone)
			runtime.snapshots["10.0.0.1"].Models[0].Phase = phase
			runtime.snapshots["10.0.0.1"].Models[0].Alive = phase != "failed"
			runtime.snapshots["10.0.0.1"].CachedArtifacts = []string{pm.Spec.ArtifactURL}

			reconcileOnce(t, r, pm.Name)

			got := getModel(t, r, pm.Name)
			require.Len(t, got.Status.Instances, 1)
			assert.Equal(t, "warm-2", got.Status.Instances[0].Pod)
			assert.Equal(t, []string{"10.0.0.2"}, runtime.activatedOn)
		})
	}
}

// An older claim that could not be placed as it is written does not keep the
// name from a newer claim that can be.
func TestAnOlderClaimThatCannotBePlacedDoesNotKeepTheName(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*modelv1alpha1.ModelClaim)
	}{
		{"invalid perGPU", func(pm *modelv1alpha1.ModelClaim) { pm.Spec.PerGPU = nil }},
		{"invalid engineConfig", func(pm *modelv1alpha1.ModelClaim) { pm.Spec.EngineConfig = invalidParallelism() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			older := modelNameClaim("team-a", at0758)
			tt.mutate(older)
			newer := modelNameClaim("team-b", at0759)
			r, runtime := newReconciler(t, older, newer, warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning))

			reconcileOnce(t, r, newer.Name)

			assert.Len(t, getModel(t, r, newer.Name).Status.Instances, 1)
			assert.Len(t, runtime.activateCalls, 1)
		})
	}
}

// The gauges of a model name are labelled by namespace and name. A claim that
// waits for the name leaves them to the claim that keeps it, and so does its
// deletion.
func TestAClaimThatWaitsForItsModelNameLeavesTheGaugesAlone(t *testing.T) {
	served := "gauge-shared-model"
	holder := modelNameClaim("gauge-a", at0758)
	holder.Spec.ModelName = &served
	waiting := modelNameClaim("gauge-b", at0759)
	waiting.Spec.ModelName = &served
	r, _ := newReconciler(t, holder, waiting,
		warmPod("warm-1", "b300-pool-a", true, corev1.PodRunning),
		warmPod("warm-2", "b300-pool-a", true, corev1.PodRunning),
	)
	ready := func() float64 { return testutil.ToFloat64(claimReadyReplicas.WithLabelValues(testNamespace, served)) }
	reconcileOnce(t, r, holder.Name)
	reconcileOnce(t, r, holder.Name)
	require.Equal(t, modelv1alpha1.ModelClaimActive, getModel(t, r, holder.Name).Status.Phase)
	require.Equal(t, 1.0, ready())

	reconcileOnce(t, r, waiting.Name)
	assert.Equal(t, 1.0, ready(), "the waiting claim does not write the holder's gauges")

	require.NoError(t, r.Delete(context.Background(), getModel(t, r, waiting.Name)))
	reconcileOnce(t, r, waiting.Name)
	assert.Equal(t, 1.0, ready(), "deleting the waiting claim keeps the holder's gauges")
}

// A Pod still holds a failed engine under the name x for a claim that is gone,
// beside a resident model. A claim for x cannot use that Pod, so no room is made
// for it there: the resident stays awake, whatever the room and the policy.
func TestNoRoomIsMadeOnAPodTakenByAnotherClaimsEngine(t *testing.T) {
	for _, tt := range []struct {
		name      string
		residents []serving
		policy    modelv1alpha1.ModelClaimPreemptionPolicy
	}{
		{"Never with room", []serving{{"a", "warm-1", 300, 100, at0758}}, modelv1alpha1.ModelClaimPreemptionNever},
		{"Never without room", []serving{{"a", "warm-1", 300, 100, at0758}, {"b", "warm-1", 300, 100, at0759}},
			modelv1alpha1.ModelClaimPreemptionNever},
		{"SleepLowerPriority without room", []serving{{"a", "warm-1", 300, 100, at0758}, {"b", "warm-1", 300, 100, at0759}},
			modelv1alpha1.ModelClaimPreemptionSleepLowerPriority},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, runtime := servingPool(t, keepNoWakeReserve, 1, tt.residents...)
			ip := podNamed(t, r, "warm-1").Status.PodIP
			runtime.snapshots[ip].Models = append(runtime.snapshots[ip].Models, RuntimeSnapshotModel{
				ModelName: "x", Phase: "failed", Alive: false,
				KVUsedBytes: kvLimitUnknown, KVCapacityBytes: kvLimitUnknown,
				ClaimRef: &ModelClaimRef{Namespace: testNamespace, Name: "gone", UID: "gone-uid"},
			})
			claim := withFinalizer(claimOnPod("newcomer", "", modelv1alpha1.ModelClaimActive, 300, 100))
			claim.UID = "newcomer-uid"
			claim.Status.Instances = nil
			claim.Spec.ModelName = ptr.To("x")
			claim.Spec.Priority = 10
			claim.Spec.PreemptionPolicy = tt.policy
			require.NoError(t, r.Create(context.Background(), claim))

			reconcileOnce(t, r, claim.Name)

			assert.Empty(t, runtime.sleepCalls, "the residents stay awake")
			assert.Empty(t, runtime.activateCalls)
			assert.Empty(t, getModel(t, r, claim.Name).Status.Instances)
			condition := scheduledOf(t, r, claim.Name)
			assert.NotEqual(t, scheduledReasonMakingRoom, condition.Reason)
			assert.Contains(t, condition.Message, "warm-1 still runs model x for ModelClaim gone")
			assert.NotContains(t, condition.Message, "other pod", "warm-1 is turned away once")
		})
	}
}

// takenForX gives the pool's warm-1 a failed engine under the name x for a
// claim that is gone, and adds an older claim for x, which cannot use warm-1,
// and a newer claim for y, which can.
func takenForX(t *testing.T, r *ModelClaimReconciler, runtime *fakeRuntime) (older, newer *modelv1alpha1.ModelClaim) {
	t.Helper()
	ip := podNamed(t, r, "warm-1").Status.PodIP
	runtime.snapshots[ip].Models = append(runtime.snapshots[ip].Models, RuntimeSnapshotModel{
		ModelName: "x", Phase: "failed", Alive: false,
		KVUsedBytes: kvLimitUnknown, KVCapacityBytes: kvLimitUnknown,
		ClaimRef: &ModelClaimRef{Namespace: testNamespace, Name: "gone", UID: "gone-uid"},
	})
	for _, c := range []struct {
		name, model string
		created     time.Time
	}{{"older", "x", at0758}, {"newer", "y", at0759}} {
		claim := withFinalizer(claimOnPod(c.name, "", modelv1alpha1.ModelClaimActive, 300, 100))
		claim.UID = types.UID(c.name + "-uid")
		claim.Status.Instances = nil
		claim.CreationTimestamp = metav1.NewTime(c.created)
		claim.Spec.ModelName = ptr.To(c.model)
		claim.Spec.PreemptionPolicy = modelv1alpha1.ModelClaimPreemptionNever
		require.NoError(t, r.Create(context.Background(), claim))
	}
	return getModel(t, r, "older"), getModel(t, r, "newer")
}

// An older claim whose only Pod is taken under its name could not be placed,
// so it does not hold the placement turn of a newer claim.
func TestAClaimThatOnlyATakenPodCouldHoldKeepsNoTurn(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1, serving{"a", "warm-1", 300, 100, at0758})
	_, newer := takenForX(t, r, runtime)

	reconcileOnce(t, r, newer.Name)

	assert.Len(t, getModel(t, r, newer.Name).Status.Instances, 1)
	assert.Len(t, runtime.activateCalls, 1)
}

// Nor does it come first to have room made: room made on the taken Pod would
// not help it, so the newer claim has room made for it.
func TestAClaimThatOnlyATakenPodCouldHoldDoesNotComeFirstForRoom(t *testing.T) {
	r, runtime := servingPool(t, keepNoWakeReserve, 1,
		serving{"a", "warm-1", 300, 100, at0758}, serving{"b", "warm-1", 300, 100, at0759})
	_, newer := takenForX(t, r, runtime)

	reconcileOnce(t, r, newer.Name)

	require.Len(t, runtime.sleepCalls, 1)
	assert.Equal(t, scheduledReasonMakingRoom, scheduledOf(t, r, newer.Name).Reason)
}
