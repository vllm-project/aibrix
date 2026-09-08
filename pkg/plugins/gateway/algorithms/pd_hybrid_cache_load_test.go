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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
)

// hybridTestMessage is 40 characters, i.e. 10 prefix blocks under the
// character tokenizer and the default 4-byte block, so a pod seeded with the
// first n blocks reports a 10·n % match.
const hybridTestMessage = "0123456789abcdefghijklmnopqrstuvwxyzABCD"

// hybridTokenLoadTestConfig is tokenLoadTestConfig with session tracking on.
var hybridTokenLoadTestConfig = pd.TokenLoadConfig{KVWeight: 0.5, RequestCost: 0, TTL: 0, SessionTTL: 30 * time.Minute}

// newHybridCacheLoadTestRouter is newTokenLoadTestRouter on the
// hybrid_cache_load policy over a private prefix table. Nothing drains
// prefixUpdateCh, so the table only ever holds what a test seeds into it.
func newHybridCacheLoadTestRouter(client *http.Client, cfg pd.HybridCacheLoadConfig) (*pdRouter, *pd.TokenLoadTracker, *prefixcacheindexer.PrefixHashTable) {
	r, tokenLoad := newTokenLoadTestRouterWithConfig(client, hybridTokenLoadTestConfig)
	table := prefixcacheindexer.NewPrefixHashTable()
	r.prefixCacheIndexer = table
	r.prefillPolicy = pd.NewHybridCacheLoadPrefillPolicy(tokenizer.NewCharacterTokenizer(), table, tokenLoad, cfg)
	return r, tokenLoad, table
}

// seedHybridPrefix records the first matchPct percent of hybridTestMessage
// as resident on pod for model.
func seedHybridPrefix(t *testing.T, table *prefixcacheindexer.PrefixHashTable, model, pod string, matchPct int) {
	t.Helper()
	_, hashes := table.MatchPrefix([]byte(hybridTestMessage), model, nil)
	require.Len(t, hashes, 10)
	table.AddPrefix(hashes[:len(hashes)*matchPct/100], model, pod)
}

// hybridRequest is tokenLoadRequest with the prefix-matchable message and an
// optional session header.
func hybridRequest(t *testing.T, requestID string, bodyBytes int, sessionID string) *types.RoutingContext {
	t.Helper()
	ctx := tokenLoadRequest(t, requestID, bodyBytes)
	ctx.Message = hybridTestMessage
	if sessionID != "" {
		ctx.ReqHeaders = map[string]string{constants.HeaderSessionID: sessionID}
	}
	return ctx
}

func openGateClient() *http.Client {
	gate := make(chan struct{})
	close(gate)
	return &http.Client{Transport: &gatedTransport{gate: gate}}
}

// TestPDRouter_HybridCacheLoadFollowsPrefixOnIdlePods: both pods idle, one
// holds half the prompt. The request goes there and is charged only for the
// half the pod has to compute.
func TestPDRouter_HybridCacheLoadFollowsPrefixOnIdlePods(t *testing.T) {
	prefillPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
	}
	podList := &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefillPods...), burstPod("decode-0", "decode", "127.0.0.100"))}
	r, tokenLoad, table := newHybridCacheLoadTestRouter(openGateClient(), pd.HybridCacheLoadConfig{Factor: 0.5})

	ctx := hybridRequest(t, "warm", 4000, "") // 1000 estimated prompt tokens
	seedHybridPrefix(t, table, ctx.Model, "prefill-1", 50)

	_, err := r.Route(ctx, podList)
	require.NoError(t, err)
	assert.Equal(t, "prefill-1", ctx.RespHeaders[HeaderPrefillTargetPod])
	active, kv := tokenLoad.GetLoad("prefill-1")
	assert.Equal(t, float64(0), active, "prefill has returned")
	assert.Equal(t, float64(500), kv, "only the uncached half of the prompt is charged")
	_, kv = tokenLoad.GetLoad("prefill-0")
	assert.Equal(t, float64(0), kv)
}

// TestPDRouter_HybridCacheLoadLoadOutweighsPrefix: the pod holding the whole
// prompt is also holding a long prefill, so the idle pod wins and is charged
// the full prompt.
func TestPDRouter_HybridCacheLoadLoadOutweighsPrefix(t *testing.T) {
	prefillPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
	}
	podList := &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefillPods...), burstPod("decode-0", "decode", "127.0.0.100"))}
	r, tokenLoad, table := newHybridCacheLoadTestRouter(openGateClient(), pd.HybridCacheLoadConfig{Factor: 0.5})

	ctx := hybridRequest(t, "cold", 4000, "")
	seedHybridPrefix(t, table, ctx.Model, "prefill-1", 100)
	tokenLoad.AcquirePrefill("long", "prefill-1", 10000)

	_, err := r.Route(ctx, podList)
	require.NoError(t, err)
	assert.Equal(t, "prefill-0", ctx.RespHeaders[HeaderPrefillTargetPod])
	_, kv := tokenLoad.GetLoad("prefill-0")
	assert.Equal(t, float64(1000), kv)
}

// TestPDRouter_HybridCacheLoadSessionDelta: consecutive turns of one session
// are charged for their growth only; another session pays for its whole
// prompt.
func TestPDRouter_HybridCacheLoadSessionDelta(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	podList := &utils.PodArray{Pods: []*v1.Pod{prefillPod, burstPod("decode-0", "decode", "127.0.0.100")}}
	r, tokenLoad, _ := newHybridCacheLoadTestRouter(openGateClient(), pd.HybridCacheLoadConfig{Factor: 0.5})

	steps := []struct {
		requestID string
		bodyBytes int
		sessionID string
		wantKV    float64 // cumulative resident KV on the pod
		why       string
	}{
		{"s1-turn-1", 4000, "s1", 1000, "first turn: whole prompt"},
		{"s1-turn-2", 6000, "s1", 1500, "second turn: 500 new tokens"},
		{"s1-turn-3", 6400, "s1", 1600, "third turn: 100 new tokens"},
		{"s2-turn-1", 4000, "s2", 2600, "another session: whole prompt"},
		{"anonymous", 4000, "", 3600, "no session header: whole prompt"},
	}
	for _, step := range steps {
		_, err := r.Route(hybridRequest(t, step.requestID, step.bodyBytes, step.sessionID), podList)
		require.NoError(t, err)
		_, kv := tokenLoad.GetLoad(prefillPod.Name)
		assert.Equalf(t, step.wantKV, kv, "%s: %s", step.requestID, step.why)
	}
}

// TestPDRouter_HybridCacheLoadMinMatch: a 30 % match is treated as no match
// once AIBRIX_MIN_MATCH_PCT-style threshold is above it, so the charge is
// the whole prompt whichever pod wins; without the threshold the matching
// pod wins and pays for 70 %.
func TestPDRouter_HybridCacheLoadMinMatch(t *testing.T) {
	prefillPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
	}
	podList := &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefillPods...), burstPod("decode-0", "decode", "127.0.0.100"))}

	for _, tc := range []struct {
		name    string
		minPct  float64
		wantPod string // "" when either pod may win
		wantKV  float64
	}{
		{"weak match counts by default", 0, "prefill-1", 700},
		{"weak match ignored above the threshold", 60, "", 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, tokenLoad, table := newHybridCacheLoadTestRouter(openGateClient(), pd.HybridCacheLoadConfig{Factor: 0.5, MinMatchPct: tc.minPct})
			ctx := hybridRequest(t, "req", 4000, "")
			seedHybridPrefix(t, table, ctx.Model, "prefill-1", 30)

			_, err := r.Route(ctx, podList)
			require.NoError(t, err)
			selected := ctx.RespHeaders[HeaderPrefillTargetPod]
			if tc.wantPod != "" {
				assert.Equal(t, tc.wantPod, selected)
			}
			_, kv := tokenLoad.GetLoad(selected)
			assert.Equal(t, tc.wantKV, kv)
		})
	}
}

// TestPDRouter_HybridCacheLoadViaRoutingConfig: a routingConfig switches a
// request on a token_load router to hybrid_cache_load, which charges too.
func TestPDRouter_HybridCacheLoadViaRoutingConfig(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	readyPods := []*v1.Pod{prefillPod, burstPod("decode-0", "decode", "127.0.0.100")}
	r, tokenLoad := newTokenLoadTestRouter(openGateClient())
	r.prefixCacheIndexer = prefixcacheindexer.NewPrefixHashTable()

	ctx := hybridRequest(t, "via-routing-config", 4000, "")
	ctx.ConfigProfile = &types.ResolvedConfigProfile{
		RoutingConfig: json.RawMessage(fmt.Sprintf(`{"prefillScorePolicy":%q}`, pd.PrefillScorePolicyHybridCacheLoad)),
	}
	pre, _, err := r.effectiveScorePolicies(ctx)
	require.NoError(t, err)
	assert.Equal(t, pd.PrefillScorePolicyHybridCacheLoad, pre.Name())

	_, _, err = r.filterPrefillDecodePods(ctx, readyPods)
	require.NoError(t, err)
	active, kv := tokenLoad.GetLoad(prefillPod.Name)
	assert.Equal(t, float64(1000), active)
	assert.Equal(t, float64(1000), kv)
}
