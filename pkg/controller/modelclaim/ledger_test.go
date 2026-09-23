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
	"k8s.io/apimachinery/pkg/api/resource"
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
				MaximumFootprint: *resource.NewQuantity(footprint, resource.BinarySI),
				KVFloor:          *resource.NewQuantity(floor, resource.BinarySI),
			},
		},
		Status: modelv1alpha1.ModelClaimStatus{
			Instances: []modelv1alpha1.ModelClaimInstance{{Pod: pod, Phase: phase}},
		},
	}
	return claim
}

// sizedPodSnapshots is what one measurable card reports, with the engines
// given running on it.
func sizedPodSnapshots(pod string, usableBytes int64, models ...RuntimeSnapshotModel) map[string]*RuntimeSnapshot {
	return map[string]*RuntimeSnapshot{
		pod: {
			Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMUsableBytes: usableBytes}},
			Models:       models,
		},
	}
}

// engineHolding is one engine in a snapshot, serving the claim of the same name
// and holding the given KV.
func engineHolding(model string, kvUsedBytes, kvCapacityBytes int64) RuntimeSnapshotModel {
	return RuntimeSnapshotModel{
		ModelName:       model,
		Port:            9001,
		Phase:           "active",
		Alive:           true,
		Ready:           true,
		KVUsedBytes:     kvUsedBytes,
		KVCapacityBytes: kvCapacityBytes,
	}
}

func ledgerFor(t *testing.T, pod *corev1.Pod, snapshots map[string]*RuntimeSnapshot, claims ...client.Object) podLedger {
	t.Helper()
	r, _ := newReconciler(t, append(claims, pod)...)
	ledgers := r.collectPodLedgers(context.Background(), testNamespace, []corev1.Pod{*pod}, snapshots)
	ledger, found := ledgers[pod.Name]
	require.True(t, found)
	return ledger
}

func TestLedgerChargesEveryInstanceButFailedOnes(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000),
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
	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000),
		claimOnPod("elsewhere", "warm-2", modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(0), ledger.owedBytes)
	assert.Equal(t, int64(1000), ledger.maximumRoomBytes())
}

func TestLedgerHasAHoleWhenTheRuntimeDidNotAnswer(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)

	ledger := ledgerFor(t, pod, map[string]*RuntimeSnapshot{})

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "its runtime did not answer", ledger.blocked)
}

func TestLedgerHasAHoleWhenACardCouldNotBeMeasured(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	snapshots := map[string]*RuntimeSnapshot{
		pod.Name: {Accelerators: []RuntimeAcceleratorSnapshot{{ID: "GPU-0", HBMUsableBytes: -1}}},
	}

	ledger := ledgerFor(t, pod, snapshots)

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "its cards could not be measured", ledger.blocked)
}

func TestLedgerHasAHoleWhenAnInstanceDeclaresNoCost(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	undeclared := sampleModelClaim()
	undeclared.Name = "legacy"
	undeclared.Spec.PerGPU = nil
	undeclared.Status.Instances = []modelv1alpha1.ModelClaimInstance{
		{Pod: pod.Name, Phase: modelv1alpha1.ModelClaimActive},
	}

	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
		undeclared,
	)

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "legacy runs there, and spec.perGPU is missing", ledger.blocked)
	assert.Equal(t, int64(400), ledger.owedBytes)
}

func TestLedgerHoldsWhatEachEngineHasMapped(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	// The engine mapped 250 where its claim declared a floor of 100, so it
	// holds 150 more than the account alone would show.
	ledger := ledgerFor(t, pod,
		sizedPodSnapshots(pod.Name, 1000, engineHolding("grown", 250, 400)),
		claimOnPod("grown", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(600), ledger.maximumRoomBytes())
	assert.Equal(t, int64(450), ledger.heldRoomBytes())
	require.Len(t, ledger.engines, 1)
	assert.Equal(t, int64(250), ledger.engines[0].kvHeldBytes())
	assert.Equal(t, int64(400), ledger.engines[0].kvCapacityBytes)
}

func TestLedgerHoldsTheDeclaredFloorForAnEngineThatHasNotStarted(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000),
		claimOnPod("booting", pod.Name, modelv1alpha1.ModelClaimActivating, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(600), ledger.heldRoomBytes())
	require.Len(t, ledger.engines, 1)
	assert.Equal(t, int64(100), ledger.engines[0].kvHeldBytes())
	assert.Equal(t, kvLimitUnknown, ledger.engines[0].kvCapacityBytes)
}

func TestLedgerChargesAFailedInstanceWhoseEngineIsStillThere(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod,
		sizedPodSnapshots(pod.Name, 1000, engineHolding("failed", 250, 400)),
		claimOnPod("failed", pod.Name, modelv1alpha1.ModelClaimFailed, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(450), ledger.heldRoomBytes())
}

func TestLedgerHasAHoleWhenAnEngineAnswersToNoClaim(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod,
		sizedPodSnapshots(pod.Name, 1000, engineHolding("stranger", 250, 400)),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.False(t, ledger.judgeable)
	assert.Equal(t, "the engine serving stranger there answers to no claim", ledger.blocked)
}

func TestLedgerAcceptsACardWhoseEnginesAllAnswerToAClaim(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	ledger := ledgerFor(t, pod,
		sizedPodSnapshots(pod.Name, 1000, engineHolding("declared", 50, 400)),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.True(t, ledger.judgeable)
	assert.Equal(t, int64(600), ledger.maximumRoomBytes())
}

func TestLedgerIgnoresAnEngineThatIsNoLongerAlive(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)
	gone := engineHolding("stranger", 250, 400)
	gone.Alive = false

	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000, gone),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
	)

	assert.True(t, ledger.judgeable)
}

func TestLedgerHasAHoleWhenAnInstanceDeclaresAZeroFloor(t *testing.T) {
	pod := warmPodWithGPUs("warm-1", "b300-pool-a", 1)

	ledger := ledgerFor(t, pod, sizedPodSnapshots(pod.Name, 1000),
		claimOnPod("declared", pod.Name, modelv1alpha1.ModelClaimActive, 300, 100),
		claimOnPod("zero", pod.Name, modelv1alpha1.ModelClaimActive, 300, 0),
	)

	// The zero was meant as a number, not as "this model takes no room", and
	// the engine behind it is on the card either way.
	assert.False(t, ledger.judgeable)
	assert.Equal(t, "zero runs there, and spec.perGPU.kvFloor is 0, which is not positive", ledger.blocked)
	assert.Equal(t, int64(400), ledger.owedBytes)
}

func TestPerGPUBytesOfSaysWhatIsWrongWithADeclaration(t *testing.T) {
	declaring := func(footprint, floor string) *modelv1alpha1.ModelClaim {
		pm := sampleModelClaim()
		pm.Spec.PerGPU = &modelv1alpha1.ModelClaimPerGPU{
			MaximumFootprint: resource.MustParse(footprint),
			KVFloor:          resource.MustParse(floor),
		}
		return pm
	}
	missing := sampleModelClaim()
	missing.Spec.PerGPU = nil

	for name, tc := range map[string]struct {
		claim *modelv1alpha1.ModelClaim
		want  string
	}{
		"missing":         {missing, "spec.perGPU is missing"},
		"zero footprint":  {declaring("0", "10Gi"), "spec.perGPU.maximumFootprint is 0, which is not positive"},
		"negative floor":  {declaring("30Gi", "-1Gi"), "spec.perGPU.kvFloor is -1Gi, which is not positive"},
		"zero both, once": {declaring("0", "0"), "spec.perGPU.maximumFootprint is 0, which is not positive"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := perGPUBytesOf(tc.claim)
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}

	perGPU, err := perGPUBytesOf(declaring("30Gi", "10Gi"))
	require.NoError(t, err)
	assert.Equal(t, int64(30)<<30, perGPU.maximumFootprintBytes)
	assert.Equal(t, int64(10)<<30, perGPU.kvFloorBytes)
	assert.Equal(t, int64(40)<<30, perGPU.minimumReserveBytes())
}
