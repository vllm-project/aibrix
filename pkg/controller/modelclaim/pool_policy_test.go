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
