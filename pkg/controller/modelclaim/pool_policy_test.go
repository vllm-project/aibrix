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
	"k8s.io/apimachinery/pkg/types"
)

func TestParsePoolPolicyUsesOneDeploymentAnnotation(t *testing.T) {
	policy, err := parsePoolPolicy(`{
		"reclaim": {
			"mode": "kv-first",
			"capacityBytes": 1000,
			"guaranteedFloorPercent": 20
		}
	}`)

	require.NoError(t, err)
	require.NotNil(t, policy.Reclaim)
	assert.Equal(t, int64(1000), policy.Reclaim.CapacityBytes)
	assert.Equal(t, int32(20), policy.Reclaim.GuaranteedFloorPercent)
}

func TestParsePoolPolicyRejectsUnsupportedLifecyclePolicy(t *testing.T) {
	policy, err := parsePoolPolicy(`{
		"reclaim": {"capacityBytes": 1000},
		"lifecycle": {"sleepAfterSeconds": 900}
	}`)

	require.Error(t, err)
	assert.Nil(t, policy)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestParsePoolPolicyClassifiesConfigurationErrors(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		class string
	}{
		{"truncated JSON", `{"reclaim":`, poolPolicyErrorInvalidJSON},
		{"multiple JSON values", `{} {}`, poolPolicyErrorInvalidJSON},
		{"unknown field", `{"lifecycle":{}}`, poolPolicyErrorUnknownField},
		{"unsupported mode", `{"reclaim":{"mode":"weight-first","capacityBytes":1000}}`, poolPolicyErrorUnsupportedMode},
		{"non-positive capacity", `{"reclaim":{"capacityBytes":0}}`, poolPolicyErrorInvalidCapacity},
		{"floor above 100", `{"reclaim":{"capacityBytes":1000,"guaranteedFloorPercent":101}}`, poolPolicyErrorInvalidFloor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := parsePoolPolicy(tt.raw)

			require.Error(t, err)
			assert.Nil(t, policy)
			assert.Equal(t, tt.class, poolPolicyErrorClass(err))
		})
	}
}

func TestPoolPolicyManagerObserveConfigDeduplicatesWarnings(t *testing.T) {
	manager := newPoolPolicyManager(nil)
	pool := types.NamespacedName{Namespace: "default", Name: "warm-pool"}

	warn, recovered := manager.observeConfig(pool, `{"bad"`, poolPolicyErrorInvalidJSON)
	assert.True(t, warn)
	assert.False(t, recovered)

	// The unchanged invalid annotation must not warn again.
	warn, recovered = manager.observeConfig(pool, `{"bad"`, poolPolicyErrorInvalidJSON)
	assert.False(t, warn)
	assert.False(t, recovered)

	// An edited annotation deserves fresh feedback even when it fails the
	// same way.
	warn, _ = manager.observeConfig(pool, `{"worse"`, poolPolicyErrorInvalidJSON)
	assert.True(t, warn)

	warn, recovered = manager.observeConfig(pool, `{}`, "")
	assert.False(t, warn)
	assert.True(t, recovered)

	// A policy that stays valid produces no further signals.
	_, recovered = manager.observeConfig(pool, `{}`, "")
	assert.False(t, recovered)

	// Removing and re-adding the annotation starts a fresh configuration.
	assert.True(t, manager.forgetConfig(pool))
	warn, _ = manager.observeConfig(pool, `{"bad"`, poolPolicyErrorInvalidJSON)
	assert.True(t, warn)
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
