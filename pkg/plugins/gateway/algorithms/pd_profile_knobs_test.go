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

package routingalgorithms

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// knobsContext builds a request the way the gateway does: a routing context
// that already has a PD leg, carrying the profile under test.
func knobsContext(t *testing.T, routingConfig string) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "message", "req-knobs", "user")
	t.Cleanup(ctx.Delete)
	if routingConfig != "" {
		ctx.ConfigProfile = &types.ResolvedConfigProfile{RoutingConfig: json.RawMessage(routingConfig)}
	}
	return ctx
}

func TestRuntimeKnobsFromRoutingConfig(t *testing.T) {
	tests := []struct {
		name          string
		routingConfig string
		wantNil       bool
		check         func(t *testing.T, knobs *types.PDRuntimeKnobs)
	}{
		{
			name:          "no routingConfig keeps every env default",
			routingConfig: "",
			wantNil:       true,
		},
		{
			name:          "routingConfig without pd section keeps every env default",
			routingConfig: `{"prefillScorePolicy":"prefix_cache"}`,
			wantNil:       true,
		},
		{
			name:          "abort and prompt-length bucketing",
			routingConfig: `{"promptLengthBucketing":true,"pd":{"abort":{"timeout":0,"retryDelay":2}}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.True(t, knobs.PromptLengthBucketingOrDefault(false))
				assert.Equal(t, time.Duration(0), knobs.DecodeAbortTimeoutOrDefault(time.Second),
					"an explicit zero is an override, not an unset value")
				assert.Equal(t, 2*time.Second, knobs.DecodeAbortRetryDelayOrDefault(time.Second))
			},
		},
		{
			name: "spreads",
			routingConfig: `{"pd":{"spreads":{"prefillLoadImbalanceMinSpread":4,
				"decodeLoadImbalanceMinSpread":1.5,"decodeThroughputImbalanceMinSpread":512,
				"decodeScoreRatioThreshold":2.5}}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, int32(4), knobs.PrefillLoadImbalanceMinSpreadOrDefault(-1))
				assert.Equal(t, 1.5, knobs.DecodeLoadImbalanceMinSpreadOrDefault(-1))
				assert.Equal(t, 512.0, knobs.DecodeThroughputImbalanceMinSpreadOrDefault(-1))
				assert.Equal(t, 2.5, knobs.DecodeScoreRatioThresholdOrDefault(-1))
			},
		},
		{
			name:          "decode load-balancing weights",
			routingConfig: `{"pd":{"decodeLBWeights":{"running":3,"throughput":0.5}}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 3.0, knobs.DecodeLBWeightRunningOrDefault(-1))
				assert.Equal(t, 0.5, knobs.DecodeLBWeightThroughputOrDefault(-1))
			},
		},
		{
			name:          "hybrid cache load, min match and prefill timeout",
			routingConfig: `{"pd":{"hybridCacheLoadFactor":0.25,"minMatchPct":0,"prefillRequestTimeout":90}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 0.25, knobs.HybridCacheLoadFactorOrDefault(-1))
				assert.Equal(t, 0.0, knobs.MinMatchPctOrDefault(-1), "zero disables the minimum-match clamp")
				assert.Equal(t, 90*time.Second, knobs.PrefillRequestTimeoutOrDefault(time.Second))
			},
		},
		{
			name: "token load",
			routingConfig: `{"pd":{"tokenLoad":{"kvWeight":0.25,"requestCost":120,"ttlSeconds":0,
				"sessionTTLSeconds":0,"maxSessions":64}}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 0.25, knobs.TokenLoadKVWeightOrDefault(-1))
				assert.Equal(t, 120.0, knobs.TokenLoadRequestCostOrDefault(-1))
				assert.Equal(t, time.Duration(0), knobs.TokenLoadTTLOrDefault(time.Second), "zero disables the sweep for the request")
				assert.Equal(t, time.Duration(0), knobs.TokenLoadSessionTTLOrDefault(time.Second), "zero disables the session delta")
				assert.Equal(t, 64, knobs.TokenLoadMaxSessionsOrDefault(-1))
			},
		},
		{
			name: "values the environment would reject keep the environment default",
			routingConfig: `{"pd":{"abort":{"timeout":-3,"retryDelay":-1},
				"spreads":{"prefillLoadImbalanceMinSpread":0,"decodeLoadImbalanceMinSpread":-1,
				"decodeThroughputImbalanceMinSpread":0,"decodeScoreRatioThreshold":-2},
				"decodeLBWeights":{"running":-1,"throughput":0},
				"hybridCacheLoadFactor":2,"minMatchPct":101,"prefillRequestTimeout":0,
				"tokenLoad":{"kvWeight":-1,"requestCost":0,"ttlSeconds":-1,"sessionTTLSeconds":-1,"maxSessions":0}}}`,
			check: func(t *testing.T, knobs *types.PDRuntimeKnobs) {
				require.NotNil(t, knobs)
				assert.Equal(t, 5*time.Second, knobs.DecodeAbortTimeoutOrDefault(5*time.Second))
				assert.Equal(t, 6*time.Second, knobs.DecodeAbortRetryDelayOrDefault(6*time.Second))
				assert.Equal(t, int32(16), knobs.PrefillLoadImbalanceMinSpreadOrDefault(16))
				assert.Equal(t, 3.0, knobs.DecodeLoadImbalanceMinSpreadOrDefault(3))
				assert.Equal(t, 4.0, knobs.DecodeThroughputImbalanceMinSpreadOrDefault(4))
				assert.Equal(t, 5.0, knobs.DecodeScoreRatioThresholdOrDefault(5))
				assert.Equal(t, 6.0, knobs.DecodeLBWeightRunningOrDefault(6))
				assert.Equal(t, 7.0, knobs.DecodeLBWeightThroughputOrDefault(7))
				assert.Equal(t, 8.0, knobs.HybridCacheLoadFactorOrDefault(8))
				assert.Equal(t, 9.0, knobs.MinMatchPctOrDefault(9))
				assert.Equal(t, 10*time.Second, knobs.PrefillRequestTimeoutOrDefault(10*time.Second))
				assert.Equal(t, 11.0, knobs.TokenLoadKVWeightOrDefault(11))
				assert.Equal(t, 12.0, knobs.TokenLoadRequestCostOrDefault(12))
				assert.Equal(t, 13*time.Second, knobs.TokenLoadTTLOrDefault(13*time.Second))
				assert.Equal(t, 14*time.Second, knobs.TokenLoadSessionTTLOrDefault(14*time.Second))
				assert.Equal(t, 15, knobs.TokenLoadMaxSessionsOrDefault(15))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := knobsContext(t, tt.routingConfig)
			knobs := effectivePDKnobs(ctx)
			if tt.wantNil {
				assert.Nil(t, knobs)
				return
			}
			require.NotNil(t, knobs)
			tt.check(t, knobs)
		})
	}
}

func TestEffectivePromptLengthBucketing(t *testing.T) {
	restore := aibrixPromptLengthBucketing
	aibrixPromptLengthBucketing = false
	t.Cleanup(func() { aibrixPromptLengthBucketing = restore })

	assert.False(t, effectivePromptLengthBucketing(knobsContext(t, "")), "without a profile the env switch decides")

	ctx := knobsContext(t, `{"promptLengthBucketing":true}`)
	ctx.SetPDKnobs(effectivePDKnobs(ctx))
	assert.True(t, effectivePromptLengthBucketing(ctx), "the profile turns bucketing on for its requests only")

	ctxOff := knobsContext(t, `{"promptLengthBucketing":false}`)
	aibrixPromptLengthBucketing = true
	ctxOff.SetPDKnobs(effectivePDKnobs(ctxOff))
	assert.False(t, effectivePromptLengthBucketing(ctxOff), "and off again")
}

func TestRouteParksProfileKnobsOnTheLeg(t *testing.T) {
	decodePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decode-1"},
		Status:     v1.PodStatus{PodIP: "127.0.0.2"},
	}
	newRouter := func() *pdRouter {
		r := &pdRouter{pendingDecodeTracker: pd.NewPendingDecodeTracker()}
		r.podSelector = selector.NewDefaultSelector(func(_ *types.RoutingContext, _ []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
			return nil, decodePod, nil
		})
		return r
	}

	ctx := knobsContext(t, `{"pd":{"abort":{"timeout":0}}}`)
	ctx.Engine = VLLMEngine
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, err := newRouter().Route(ctx, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)

	knobs := ctx.PDKnobs()
	require.NotNil(t, knobs, "Route must park the resolved profile knobs on the request's PD leg")
	assert.Equal(t, time.Duration(0), knobs.DecodeAbortTimeoutOrDefault(time.Second))

	plain := knobsContext(t, "")
	plain.Engine = VLLMEngine
	plain.ReqPath = "/v1/chat/completions"
	plain.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err = newRouter().Route(plain, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)
	assert.Nil(t, plain.PDKnobs(), "a request without profile knobs keeps the env defaults")
}
