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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestParsePoolPolicyUsesOneDeploymentAnnotation(t *testing.T) {
	policy, err := parsePoolPolicy(`{
		"reclaim": {
			"mode": "kv-first",
			"capacityBytes": 1000,
			"guaranteedFloorPercent": 20
		},
		"lifecycle": {"sleepAfterSeconds": 900}
	}`)

	require.NoError(t, err)
	require.NotNil(t, policy.Reclaim)
	assert.Equal(t, int64(1000), policy.Reclaim.CapacityBytes)
	assert.Equal(t, int32(20), policy.Reclaim.GuaranteedFloorPercent)
	require.NotNil(t, policy.Lifecycle)
	assert.Equal(t, int64(900), policy.Lifecycle.SleepAfterSeconds)
}

func TestComputePoolKVTargetsGivesActiveModelTheBorrowedCapacity(t *testing.T) {
	targets, err := computePoolKVTargets(1000, 20, []poolKVModel{
		{
			Name:            "hot",
			KVUsedBytes:     100,
			KVCapacityBytes: 200,
			Activity: poolRequestActivity{
				Active:           true,
				RequestsInFlight: 2,
				CompletionDelta:  3,
			},
		},
		{
			Name:            "idle",
			KVUsedBytes:     100,
			KVCapacityBytes: 800,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(800), targets["hot"])
	assert.Equal(t, int64(200), targets["idle"])
}

func TestComputePoolKVTargetsRefusesToShrinkBelowObservedKVUsage(t *testing.T) {
	targets, err := computePoolKVTargets(1000, 20, []poolKVModel{
		{
			Name:            "a",
			KVUsedBytes:     700,
			KVCapacityBytes: 800,
			Activity:        poolRequestActivity{Active: true},
		},
		{
			Name:            "b",
			KVUsedBytes:     500,
			KVCapacityBytes: 800,
		},
	})

	assert.ErrorIs(t, err, errPoolKVUsageExceedsCapacity)
	assert.Nil(t, targets)
}

func TestPoolPolicySleepReservationKeepsOneActiveReplica(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	manager := newPoolPolicyManager(func() time.Time { return now })
	claim := &modelv1alpha1.ModelClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "qwen",
			UID:       types.UID("claim-uid"),
		},
		Status: modelv1alpha1.ModelClaimStatus{Instances: []modelv1alpha1.ModelClaimInstance{
			{Pod: "pod-a", Phase: modelv1alpha1.ModelClaimActive},
			{Pod: "pod-b", Phase: modelv1alpha1.ModelClaimActive},
		}},
	}

	require.True(t, manager.reserveSleep(claim, "pod-a"))
	assert.False(t, manager.reserveSleep(claim, "pod-b"), "must retain one active replica")
	manager.releaseSleep(claim, "pod-a")
	assert.True(t, manager.reserveSleep(claim, "pod-b"))
}
