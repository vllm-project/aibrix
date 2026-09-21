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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// claimOnPod is a claim that declares its per-GPU cost and already has one
// instance of the given phase recorded on a pod.
func claimOnPod(name, pod string, phase modelv1alpha1.ModelClaimPhase, footprint, floor int64) *modelv1alpha1.ModelClaim {
	claim := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: modelv1alpha1.ModelClaimSpec{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{constants.ModelPoolLabelName: "b300-pool-a"},
			},
			ArtifactURL: "huggingface://Org/" + name,
			PerGPU: &modelv1alpha1.ModelClaimPerGPU{
				MaximumFootprintBytes: footprint,
				KVFloorBytes:          floor,
			},
		},
		Status: modelv1alpha1.ModelClaimStatus{
			Instances: []modelv1alpha1.ModelClaimInstance{{Pod: pod, Phase: phase}},
		},
	}
	return claim
}

func sizedPodStates(pod string, usableBytes int64) map[string]PodPlacementState {
	return map[string]PodPlacementState{
		pod: {SnapshotKnown: true, HBMUsableBytes: usableBytes, HBMUsableKnown: true},
	}
}

func ledgerFor(t *testing.T, pod *corev1.Pod, states map[string]PodPlacementState, claims ...client.Object) podLedger {
	t.Helper()
	return ledgerWithEngines(t, pod, states, nil, claims...)
}

// ledgerWithEngines is ledgerFor for a card that is running something.
func ledgerWithEngines(
	t *testing.T,
	pod *corev1.Pod,
	states map[string]PodPlacementState,
	engines []RuntimeSnapshotModel,
	claims ...client.Object,
) podLedger {
	t.Helper()
	r, _ := newReconciler(t, append(claims, pod)...)
	snapshots := map[string]*RuntimeSnapshot{pod.Name: {Models: engines}}
	ledgers := r.collectPodLedgers(
		context.Background(), testNamespace, []corev1.Pod{*pod}, states, snapshots)
	ledger, found := ledgers[pod.Name]
	require.True(t, found)
	return ledger
}

// liveEngine is an engine a runtime reports as running.
func liveEngine(model string) RuntimeSnapshotModel {
	return RuntimeSnapshotModel{
		ModelName: model, Port: 9001, Phase: "active", Alive: true, Ready: true,
	}
}

func TestLedgerHasAHoleWhenAnEngineAnswersToNoClaim(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)

	ledger := ledgerWithEngines(t, pod, sizedPodStates(pod.Name, 1000),
		[]RuntimeSnapshotModel{liveEngine("stranger")},
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "the engine serving stranger there answers to no claim", ledger.blocked)
}

func TestLedgerAcceptsACardWhoseEnginesAllAnswerToAClaim(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)

	ledger := ledgerWithEngines(t, pod, sizedPodStates(pod.Name, 1000),
		[]RuntimeSnapshotModel{liveEngine("declared")},
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(600), ledger.maximumRoomBytes())
}

func TestLedgerIgnoresAnEngineThatIsNoLongerAlive(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	gone := liveEngine("stranger")
	gone.Alive = false

	ledger := ledgerWithEngines(t, pod, sizedPodStates(pod.Name, 1000),
		[]RuntimeSnapshotModel{gone},
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100))

	assert.True(t, ledger.judgeable)
}

func TestLedgerChargesEveryInstanceButFailedOnes(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod, sizedPodStates(pod.Name, 1000),
		claimOnPod("active", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
		claimOnPod("activating", pod.Name, modelv1alpha1.ModelClaimActivating, 200, 50),
		claimOnPod("failed", pod.Name, modelv1alpha1.ModelClaimFailed, 400, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(650), ledger.owedBytes)
	assert.Equal(t, int64(350), ledger.maximumRoomBytes())
}

func TestLedgerIgnoresInstancesOnOtherPods(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod, sizedPodStates(pod.Name, 1000),
		claimOnPod("elsewhere", "warm-2", modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(0), ledger.owedBytes)
	assert.Equal(t, int64(1000), ledger.maximumRoomBytes())
}

func TestLedgerHasAHoleWhenTheRuntimeDidNotAnswer(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)

	ledger := ledgerFor(t, pod, map[string]PodPlacementState{})

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "its runtime did not answer", ledger.blocked)
}

func TestLedgerHasAHoleWhenACardCouldNotBeMeasured(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	states := map[string]PodPlacementState{
		pod.Name: {SnapshotKnown: true, HBMUsableKnown: false},
	}

	ledger := ledgerFor(t, pod, states)

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "its cards could not be measured", ledger.blocked)
}

func TestLedgerHasAHoleWhenAnInstanceDeclaresNoCost(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	undeclared := sampleModelClaim()
	undeclared.Name = "legacy"
	undeclared.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActive},
	}

	ledger := ledgerFor(t, pod, sizedPodStates(pod.Name, 1000),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
		undeclared,
	)

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "legacy runs there and declares no per-GPU cost", ledger.blocked)
	assert.Equal(t, int64(400), ledger.owedBytes)
}
