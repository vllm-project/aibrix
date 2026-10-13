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

package router

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file covers the PD halves of the profile knob flow that are specific to
// the PD path: the prompt-length bucketing switch and the parking of the
// resolved PD overrides on the request's leg, where the async prefill and
// decode-abort paths read them after the pooled routing context is recycled.
// The tests hand the router the overrides a profile would resolve to, since the
// resolver lives in the routing algorithm package; the resolution rules
// themselves are covered by routing_profile_knobs_test.go there.

// pdProbeDefaults is the process default table these tests run against, so "kept the process
// default" is asserted against a known value instead of whatever the environment configures.
func pdProbeDefaults() *types.RoutingOverrides {
	return &types.RoutingOverrides{
		PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 2},
		PD: types.PDOverrides{
			Abort: types.PDAbortOverrides{Timeout: 3 * time.Second, RetryDelay: 2 * time.Second},
			Spreads: types.PDSpreadOverrides{
				PrefillLoadImbalanceMinSpread:      16,
				DecodeLoadImbalanceMinSpread:       16,
				DecodeThroughputImbalanceMinSpread: 2048,
				DecodeScoreRatioThreshold:          1.5,
			},
			DecodeLB:              types.PDDecodeLBOverrides{WeightRunning: 1, WeightThroughput: 1},
			TokenLoad:             types.PDTokenLoadOverrides{KVWeight: 0.3, RequestCost: 3500, TTL: time.Minute, SessionTTL: 30 * time.Minute},
			HybridCacheLoadFactor: 0.5,
			MinMatchPct:           0,
			BucketServe:           false,
			BucketServeMode:       string(pd.BucketModeRPS),
			PrefillRequestTimeout: 30 * time.Second,
		},
	}
}

// withDefaultOverrides swaps the process default table for one test and
// restores the previous one afterwards.
func withDefaultOverrides(t *testing.T, defaults *types.RoutingOverrides) {
	t.Helper()
	restore := types.DefaultRoutingOverrides()
	types.SetDefaultRoutingOverrides(defaults)
	t.Cleanup(func() { types.SetDefaultRoutingOverrides(restore) })
}

// pdKnobsContext builds a PD request the way the gateway does: a routing
// context that has a PD leg. These tests read the overrides off that leg, where
// the PD router parks them.
func pdKnobsContext(t *testing.T) *types.RoutingContext {
	t.Helper()
	ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "message", "req-pd-knobs", "user")
	t.Cleanup(ctx.Delete)
	return ctx
}

// resolvedOverrides is what a profile that sets one knob resolves to: the process defaults with
// that knob changed.
func resolvedOverrides(mutate func(*types.RoutingOverrides)) *types.RoutingOverrides {
	resolved := *types.DefaultRoutingOverrides()
	mutate(&resolved)
	return &resolved
}

// parkOnLeg is the parking step of pdRouter.Route: the request's resolved PD
// overrides, which the PD read sites then read off the request's leg rather
// than the pooled routing context.
func parkOnLeg(ctx *types.RoutingContext) {
	pdOverrides := ctx.RoutingOverrides().PD
	ctx.SetPDOverrides(&pdOverrides)
}

func TestEffectivePromptLengthBucketing(t *testing.T) {
	withPromptLengthBucketing(t, false)
	assert.False(t, effectivePromptLengthBucketing(pdKnobsContext(t)),
		"without a profile the process default decides")

	profiled := pdKnobsContext(t)
	profiled.SetRoutingOverrides(resolvedOverrides(func(o *types.RoutingOverrides) { o.PD.PromptLengthBucketing = true }))
	parkOnLeg(profiled)
	assert.True(t, effectivePromptLengthBucketing(profiled),
		"the profile turns bucketing on for its requests only")

	withPromptLengthBucketing(t, true)
	off := pdKnobsContext(t)
	off.SetRoutingOverrides(resolvedOverrides(func(o *types.RoutingOverrides) { o.PD.PromptLengthBucketing = false }))
	parkOnLeg(off)
	assert.False(t, effectivePromptLengthBucketing(off), "and off again")
}

func TestRouteParksProfileKnobsOnTheLeg(t *testing.T) {
	withDefaultOverrides(t, pdProbeDefaults())

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

	ctx := pdKnobsContext(t)
	ctx.Engine = VLLMEngine
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	ctx.SetRoutingOverrides(resolvedOverrides(func(o *types.RoutingOverrides) { o.PD.Abort.Timeout = 0 }))

	_, err := newRouter().Route(ctx, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)

	assert.Equal(t, time.Duration(0), ctx.PDOverrides().Abort.Timeout,
		"Route must park the resolved profile overrides on the request's PD leg")

	plain := pdKnobsContext(t)
	plain.Engine = VLLMEngine
	plain.ReqPath = "/v1/chat/completions"
	plain.ReqBody = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)

	_, err = newRouter().Route(plain, &utils.PodArray{Pods: []*v1.Pod{}})
	require.NoError(t, err)
	assert.Equal(t, pdProbeDefaults().PD.Abort.Timeout, plain.PDOverrides().Abort.Timeout,
		"a request without profile knobs keeps the process defaults")
}

func TestPrefillSigmaKnobControlsPrefillCandidacy(t *testing.T) {
	newCtx := func(requestID string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), RouterPD, "model", "hello world", requestID, "user")
		t.Cleanup(ctx.Delete)
		return ctx
	}

	pods := []*v1.Pod{
		makePDPod("prefill-1", "roleset-1", "", nil),
		makePDPod("prefill-2", "roleset-2", "", nil),
		makePDPod("prefill-3", "roleset-3", "", nil),
		makePDPod("prefill-hot", "roleset-hot", "", nil),
	}
	tracker := pd.NewPrefillRequestTracker()
	addRequests(tracker, "prefill-1", 1)
	addRequests(tracker, "prefill-2", 1)
	addRequests(tracker, "prefill-3", 1)
	addRequests(tracker, "prefill-hot", 10)

	r := &pdRouter{
		prefillPolicy:         pd.NewPrefixCachePrefillPolicy(tokenizer.NewCharacterTokenizer(), prefixcacheindexer.NewPrefixHashTable()),
		prefillRequestTracker: tracker,
	}
	hotIsCandidate := func(scores map[string]*Scores) bool {
		for _, score := range scores {
			if score.Pod.Name == "prefill-hot" {
				return true
			}
		}
		return false
	}

	// Counts are [1,1,1,10]: the hot prefill replica sits several deviations above
	// the pool mean, so the sigma of the request decides its candidacy, the same
	// knob the prefix-cache strategies read.
	sharp := newCtx("req-prefill-sharp")
	sharp.SetRoutingOverrides(&types.RoutingOverrides{PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 1}})
	sharpScores, _, _ := r.scorePrefillPods(sharp, pods, nil)
	assert.Len(t, sharpScores, 3)
	assert.False(t, hotIsCandidate(sharpScores), "a sharp sigma drops the loaded prefill replica")

	wide := newCtx("req-prefill-wide")
	wide.SetRoutingOverrides(&types.RoutingOverrides{PrefixCache: types.PrefixCacheOverrides{StandardDeviationFactor: 4}})
	wideScores, _, _ := r.scorePrefillPods(wide, pods, nil)
	assert.Len(t, wideScores, 4)
	assert.True(t, hotIsCandidate(wideScores), "a generous sigma keeps it a candidate")
}
