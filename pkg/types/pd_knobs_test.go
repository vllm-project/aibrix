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
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPDLegKnobsRoundTrip(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), "pd", "model", "message", "req-knobs", "user")
	assert.Nil(t, ctx.PDKnobs(), "a fresh incarnation has no overrides")

	knobs := &PDRuntimeKnobs{DecodeAbortTimeoutSeconds: intPtr(0), PromptLengthBucketing: boolPtr(true)}
	ctx.SetPDKnobs(knobs)
	assert.Same(t, knobs, ctx.PDKnobs())
	ctx.Delete()

	next := NewRoutingContext(context.Background(), "pd", "model", "message", "req-next", "user")
	defer next.Delete()
	assert.Nil(t, next.PDKnobs(), "reset installs a fresh leg, so knobs never leak between requests")

	var leg *PDLegState
	assert.Nil(t, leg.PDKnobs())
	leg.SetPDKnobs(knobs) // nil receiver: must not panic
}

func TestPDRuntimeKnobsAccessorsFallBack(t *testing.T) {
	var nilKnobs *PDRuntimeKnobs
	assert.Equal(t, 7*time.Second, nilKnobs.DecodeAbortTimeoutOrDefault(7*time.Second))
	assert.Equal(t, 8*time.Second, nilKnobs.DecodeAbortRetryDelayOrDefault(8*time.Second))
	assert.Equal(t, 9*time.Second, nilKnobs.PrefillRequestTimeoutOrDefault(9*time.Second))
	assert.Equal(t, 10*time.Second, nilKnobs.TokenLoadTTLOrDefault(10*time.Second))
	assert.Equal(t, 11*time.Second, nilKnobs.TokenLoadSessionTTLOrDefault(11*time.Second))
	assert.Equal(t, int32(16), nilKnobs.PrefillLoadImbalanceMinSpreadOrDefault(16))
	assert.Equal(t, 1.5, nilKnobs.DecodeLoadImbalanceMinSpreadOrDefault(1.5))
	assert.Equal(t, 2.5, nilKnobs.DecodeThroughputImbalanceMinSpreadOrDefault(2.5))
	assert.Equal(t, 3.5, nilKnobs.DecodeScoreRatioThresholdOrDefault(3.5))
	assert.True(t, nilKnobs.PromptLengthBucketingOrDefault(true))
	assert.Equal(t, 4.5, nilKnobs.DecodeLBWeightRunningOrDefault(4.5))
	assert.Equal(t, 5.5, nilKnobs.DecodeLBWeightThroughputOrDefault(5.5))
	assert.Equal(t, 6.5, nilKnobs.HybridCacheLoadFactorOrDefault(6.5))
	assert.Equal(t, 7.5, nilKnobs.MinMatchPctOrDefault(7.5))
	assert.Equal(t, 8.5, nilKnobs.TokenLoadKVWeightOrDefault(8.5))
	assert.Equal(t, 9.5, nilKnobs.TokenLoadRequestCostOrDefault(9.5))
	assert.Equal(t, 12, nilKnobs.TokenLoadMaxSessionsOrDefault(12))

	empty := &PDRuntimeKnobs{}
	assert.Equal(t, 1.0, empty.DecodeLBWeightRunningOrDefault(1.0))
	assert.False(t, empty.PromptLengthBucketingOrDefault(false))
	assert.Equal(t, int32(3), empty.PrefillLoadImbalanceMinSpreadOrDefault(3))
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }
