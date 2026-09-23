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

package types

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoutingKnobsRoundTrip(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "load-balance", "model", "message", "req-knobs", "user")
	assert.Nil(t, ctx.RoutingKnobs(), "a fresh incarnation has no overrides")

	knobs := &RoutingKnobs{
		LoadBalanceQueuedWeight: float64Ptr(0),
		PrebleTargetGPU:         stringPtr("A6000"),
	}
	ctx.SetRoutingKnobs(knobs)
	assert.Same(t, knobs, ctx.RoutingKnobs())
	ctx.Delete()

	next := NewRoutingContext(context.Background(), "load-balance", "model", "message", "req-next", "user")
	defer next.Delete()
	assert.Nil(t, next.RoutingKnobs(), "reset installs a fresh incarnation, so knobs never leak between requests")

	var unset *RoutingContext
	unset.SetRoutingKnobs(knobs) // nil receiver: must not panic
	assert.Nil(t, unset.RoutingKnobs())
}

func TestRoutingKnobsAccessorsFallBack(t *testing.T) {
	var nilKnobs *RoutingKnobs
	assert.Equal(t, 1.5, nilKnobs.LoadBalanceImbalanceFactorOrDefault(1.5))
	assert.Equal(t, 2, nilKnobs.LoadBalanceImbalanceMinGapOrDefault(2))
	assert.Equal(t, 3.5, nilKnobs.LoadBalanceQueuedWeightOrDefault(3.5))
	assert.Equal(t, 4.5, nilKnobs.LoadBalanceKVPressureAlphaOrDefault(4.5))
	assert.Equal(t, 5.5, nilKnobs.LoadBalanceKVCriticalFreeOrDefault(5.5))
	assert.Equal(t, 6, nilKnobs.PrefixCacheStandardDeviationFactorOrDefault(6))
	assert.Equal(t, "V100", nilKnobs.PrebleTargetGPUOrDefault("V100"))
	assert.Equal(t, 7, nilKnobs.PrebleDecodingLengthOrDefault(7))
	assert.Equal(t, 8.5, nilKnobs.VTCMaxPodLoadOrDefault(8.5))
	assert.Equal(t, 9.5, nilKnobs.VTCFairnessWeightOrDefault(9.5))
	assert.Equal(t, 10.5, nilKnobs.VTCUtilizationWeightOrDefault(10.5))
	assert.Equal(t, 11, nilKnobs.AutoBlendLoadBalanceWeightOrDefault(11))
	assert.Equal(t, 12, nilKnobs.AutoBlendLeastRequestWeightOrDefault(12))
	assert.Equal(t, 13, nilKnobs.AutoBlendPrefixCacheWeightOrDefault(13))
	assert.Equal(t, 14, nilKnobs.AutoBlendPrefixCacheLoadBalanceWeightOrDefault(14))

	empty := &RoutingKnobs{}
	assert.Equal(t, 1.5, empty.LoadBalanceImbalanceFactorOrDefault(1.5))
	assert.Equal(t, 2, empty.LoadBalanceImbalanceMinGapOrDefault(2))
	assert.Equal(t, 3.5, empty.LoadBalanceQueuedWeightOrDefault(3.5))
	assert.Equal(t, 4, empty.PrefixCacheStandardDeviationFactorOrDefault(4))
	assert.Equal(t, 5, empty.PrebleDecodingLengthOrDefault(5))
	assert.Equal(t, "A6000", empty.PrebleTargetGPUOrDefault("A6000"))
	assert.Equal(t, 6, empty.AutoBlendLeastRequestWeightOrDefault(6))
}

func float64Ptr(v float64) *float64 { return &v }
func stringPtr(v string) *string    { return &v }
