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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestEngineArgCapacityProviderRejectsOversubscribedSnapshot(t *testing.T) {
	provider := engineArgCapacityProvider{}
	pm := &modelv1alpha1.ModelClaim{
		Spec: modelv1alpha1.ModelClaimSpec{
			Engine: "vllm",
			EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
				"--gpu-memory-utilization": "0.45",
			}},
		},
	}

	requirement, err := provider.Requirement(pm)
	require.NoError(t, err)
	assert.Equal(t, 0.45, requirement.ReservationFraction)

	state := provider.State(&RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{{
			ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700,
		}},
		Models: []RuntimeSnapshotModel{{
			ModelName: "resident", HBMReservationFraction: 0.60,
		}},
	}, requirement, 1)

	assert.True(t, state.Known)
	assert.False(t, state.Fits)
	assert.InDelta(t, 0.60, state.ReservedFraction, 0.0001)
	assert.InDelta(t, 0.70, state.FreeFraction, 0.0001)
}

func TestEngineArgCapacityProviderFallsBackForResidentWithoutEnvelope(t *testing.T) {
	provider := engineArgCapacityProvider{}
	pm := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen"},
		Spec: modelv1alpha1.ModelClaimSpec{
			Engine: "vllm",
			EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
				"--gpu-memory-utilization": "0.45",
			}},
		},
	}

	requirement, err := provider.Requirement(pm)
	require.NoError(t, err)
	state := provider.State(&RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{{
			ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700,
		}},
		Models: []RuntimeSnapshotModel{{
			ModelName: "legacy", HBMPeakBytes: 500,
		}},
	}, requirement, 1)

	// A measured peak is a lower bound, not a future allocation limit. An old
	// resident without its launch envelope cannot participate in a safe ledger.
	assert.False(t, state.Known)
	assert.False(t, state.Fits)
	assert.Zero(t, state.ReservedFraction)
}

func TestEngineArgCapacityProviderChargesPeakAboveEnvelope(t *testing.T) {
	provider := engineArgCapacityProvider{}
	requirement := CapacityRequirement{Known: true, ReservationFraction: 0.45}

	state := provider.State(&RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{{
			ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 900,
		}},
		Models: []RuntimeSnapshotModel{{
			ModelName: "resident", HBMReservationFraction: 0.45, HBMPeakBytes: 600,
		}},
	}, requirement, 1)

	assert.True(t, state.Known)
	assert.False(t, state.Fits)
	assert.InDelta(t, 0.60, state.ReservedFraction, 0.0001)
}

func TestEngineArgCapacityProviderUsesLeastFreeGPUForFixedTopology(t *testing.T) {
	provider := engineArgCapacityProvider{}
	requirement := CapacityRequirement{Known: true, ReservationFraction: 0.45}

	state := provider.State(&RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 800},
			{ID: "GPU-1", HBMTotalBytes: 1000, HBMFreeBytes: 400},
		},
	}, requirement, 2)

	assert.True(t, state.Known)
	assert.False(t, state.Fits)
	assert.InDelta(t, 0.40, state.FreeFraction, 0.0001)
}

func TestEngineArgCapacityProviderFallsBackForMismatchedVisibleTopology(t *testing.T) {
	provider := engineArgCapacityProvider{}
	requirement := CapacityRequirement{Known: true, ReservationFraction: 0.45}

	state := provider.State(&RuntimeSnapshot{
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 800},
			{ID: "GPU-1", HBMTotalBytes: 1000, HBMFreeBytes: 800},
		},
	}, requirement, 1)

	assert.False(t, state.Known)
	assert.False(t, state.Fits)
}

func TestEngineArgCapacityProviderRejectsInvalidVLLMReservation(t *testing.T) {
	provider := engineArgCapacityProvider{}
	pm := &modelv1alpha1.ModelClaim{
		Spec: modelv1alpha1.ModelClaimSpec{
			Engine: "vllm",
			EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{Args: map[string]string{
				"--gpu-memory-utilization": "1.2",
			}},
		},
	}

	_, err := provider.Requirement(pm)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--gpu-memory-utilization must be in (0, 1]")
}
