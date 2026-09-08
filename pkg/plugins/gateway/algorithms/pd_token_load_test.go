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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// tokenLoadTestConfig has no fixed per-request cost, so a request's charge is
// exactly len(body)/4 and the assertions below can be read off the bodies.
var tokenLoadTestConfig = pd.TokenLoadConfig{KVWeight: 0.5, RequestCost: 0, TTL: 0}

// newTokenLoadTestRouter builds a pd router on the token_load policy whose
// prefill calls go through client, mirroring NewPDRouter's wiring of the
// tracker into the policy and the executor.
func newTokenLoadTestRouter(client *http.Client) (*pdRouter, *pd.TokenLoadTracker) {
	return newTokenLoadTestRouterWithConfig(client, tokenLoadTestConfig)
}

func newTokenLoadTestRouterWithConfig(client *http.Client, cfg pd.TokenLoadConfig) (*pdRouter, *pd.TokenLoadTracker) {
	tokenLoad := pd.NewTokenLoadTrackerWithConfig(cfg)
	tracker := pd.NewPrefillRequestTracker()
	r := &pdRouter{
		cache:                 cache.NewForTest(),
		prefillPolicy:         pd.NewTokenLoadPrefillPolicy(tokenLoad),
		decodePolicy:          pd.LoadBalancingDecodePolicy{},
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		tokenLoadTracker:      tokenLoad,
		httpClient:            client,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}
	r.podSelector = selector.NewDefaultSelector(r.filterPrefillDecodePods)
	r.prefillExecutor = prefill.NewDefaultExecutor(client, tracker, prefillRequestTimeout, prefill.WithTokenLoadTracker(tokenLoad))
	return r, tokenLoad
}

// tokenLoadRequest builds a vLLM chat request whose body is padded to exactly
// bodyBytes bytes, i.e. bodyBytes/4 estimated prompt tokens.
func tokenLoadRequest(t *testing.T, requestID string, bodyBytes int) *types.RoutingContext {
	t.Helper()
	const frame = `{"messages":[{"role":"user","content":""}],"stream":true}`
	require.Greater(t, bodyBytes, len(frame))
	body := strings.Replace(frame, `"content":""`, `"content":"`+strings.Repeat("x", bodyBytes-len(frame))+`"`, 1)
	require.Len(t, body, bodyBytes)

	ctx := types.NewRoutingContext(context.Background(), "pd", "token-load-model", "x", requestID, "user")
	ctx.Engine = "vllm"
	ctx.ReqPath = testChatCompletionsPath
	ctx.ReqBody = []byte(body)
	return ctx
}

// waitForActiveTokens polls until the sum of active tokens over pods equals
// want, i.e. until the in-flight prefills charged so far are all visible.
func waitForActiveTokens(t *testing.T, tracker *pd.TokenLoadTracker, pods []*v1.Pod, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		total := 0.0
		for _, pod := range pods {
			active, _ := tracker.GetLoad(pod.Name)
			total += active
		}
		if total == want {
			return
		}
		require.Truef(t, time.Now().Before(deadline), "active tokens stuck at %g, want %g", total, want)
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPDRouter_TokenLoadSpreadsByPromptCost holds one long prefill open and
// then routes two short prompts. Under least_request the second short prompt
// would go to the pod holding the long one (both pods have one request);
// under token_load both short prompts go to the other pod because two short
// prompts cost far less than one long one. The test then walks the charge
// through its lifecycle: prefill return releases the active part, request
// completion (the cache.RequestTracker callback) releases the resident KV.
func TestPDRouter_TokenLoadSpreadsByPromptCost(t *testing.T) {
	const (
		longBytes  = 40_000 // 10_000 tokens
		shortBytes = 400    // 100 tokens
	)
	prefillPods := []*v1.Pod{
		burstPod("prefill-0", "prefill", "127.0.0.1"),
		burstPod("prefill-1", "prefill", "127.0.0.2"),
	}
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	podList := &utils.PodArray{Pods: append(append([]*v1.Pod{}, prefillPods...), decodePod)}

	gate := make(chan struct{})
	r, tokenLoad := newTokenLoadTestRouter(&http.Client{Transport: &gatedTransport{gate: gate}})

	longCtx := tokenLoadRequest(t, "long", longBytes)
	shortCtxs := []*types.RoutingContext{
		tokenLoadRequest(t, "short-0", shortBytes),
		tokenLoadRequest(t, "short-1", shortBytes),
	}

	errs := make(chan error, 3)
	go func() { _, err := r.Route(longCtx, podList); errs <- err }()
	waitForActiveTokens(t, tokenLoad, prefillPods, longBytes/4)

	for i, ctx := range shortCtxs {
		go func(ctx *types.RoutingContext) { _, err := r.Route(ctx, podList); errs <- err }(ctx)
		waitForActiveTokens(t, tokenLoad, prefillPods, longBytes/4+float64(i+1)*shortBytes/4)
	}

	// While everything is held: the long prompt is alone on its pod and both
	// short prompts share the other one. (The routing contexts are still owned
	// by the parked Route calls, so the ledger is what is inspected here.)
	longPod, shortPod := prefillPods[0].Name, prefillPods[1].Name
	if active, _ := tokenLoad.GetLoad(longPod); active != longBytes/4 {
		longPod, shortPod = shortPod, longPod
	}
	active, kv := tokenLoad.GetLoad(longPod)
	assert.Equal(t, float64(longBytes/4), active)
	assert.Equal(t, float64(longBytes/4), kv)
	active, kv = tokenLoad.GetLoad(shortPod)
	assert.Equal(t, float64(2*shortBytes/4), active)
	assert.Equal(t, float64(2*shortBytes/4), kv)

	// Prefill calls return: active charges drop, KV stays resident.
	close(gate)
	for i := 0; i < 3; i++ {
		require.NoError(t, <-errs)
	}
	assert.Equal(t, longPod, longCtx.RespHeaders[HeaderPrefillTargetPod])
	for _, ctx := range shortCtxs {
		assert.Equalf(t, shortPod, ctx.RespHeaders[HeaderPrefillTargetPod], "%s must avoid the pod holding the long prompt", ctx.RequestID)
	}
	for _, pod := range prefillPods {
		active, kv := tokenLoad.GetLoad(pod.Name)
		assert.Equalf(t, float64(0), active, "%s active tokens after prefill returned", pod.Name)
		assert.Greaterf(t, kv, float64(0), "%s kv tokens must stay resident until completion", pod.Name)
	}
	assert.Equal(t, float64(0.5*longBytes/4), tokenLoad.GetPriority(longPod), "only weighted KV is left")

	// Request completion via the cache.RequestTracker callbacks clears the
	// KV charge; unknown request IDs and a nil context are harmless.
	r.DoneRequestCount(nil, "long", longCtx.Model, 0)
	r.DoneRequestTrace(nil, "short-0", longCtx.Model, 0, 0, 0)
	r.DoneRequestCount(nil, "never-routed", longCtx.Model, 0)
	_, kv = tokenLoad.GetLoad(longPod)
	assert.Equal(t, float64(0), kv)
	_, kv = tokenLoad.GetLoad(shortPod)
	assert.Equal(t, float64(shortBytes/4), kv, "short-1 has not completed yet")

	r.DoneRequestCount(nil, "short-1", longCtx.Model, 0)
	_, kv = tokenLoad.GetLoad(shortPod)
	assert.Equal(t, float64(0), kv)
	assert.Equal(t, int64(0), r.AddRequestCount(longCtx, "long", longCtx.Model))
}

// failingTransport fails every prefill call.
type failingTransport struct{}

func (failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, errors.New("prefill pod unreachable")
}

// TestPDRouter_TokenLoadReleasedOnPrefillFailure checks that a failed prefill
// leaves nothing charged: the executor releases the active part when the
// call returns and Route's error path releases the rest.
func TestPDRouter_TokenLoadReleasedOnPrefillFailure(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	podList := &utils.PodArray{Pods: []*v1.Pod{prefillPod, burstPod("decode-0", "decode", "127.0.0.100")}}
	r, tokenLoad := newTokenLoadTestRouter(&http.Client{Transport: failingTransport{}})

	_, err := r.Route(tokenLoadRequest(t, "doomed", 4000), podList)
	require.Error(t, err)

	active, kv := tokenLoad.GetLoad(prefillPod.Name)
	assert.Equal(t, float64(0), active)
	assert.Equal(t, float64(0), kv)
	assert.Equal(t, 0, r.prefillRequestTracker.GetPrefillRequestCountsForPod(prefillPod.Name))
}

// TestPDRouter_TokenLoadChargedOnlyForTokenLoadPolicy checks that the tracker
// is left alone when another policy scores the request, and that
// routingConfig can switch a request to token_load on a router whose env
// default is a different policy.
func TestPDRouter_TokenLoadChargedOnlyForTokenLoadPolicy(t *testing.T) {
	prefillPod := burstPod("prefill-0", "prefill", "127.0.0.1")
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	readyPods := []*v1.Pod{prefillPod, decodePod}

	gate := make(chan struct{})
	close(gate) // prefill returns immediately
	r, tokenLoad := newTokenLoadTestRouter(&http.Client{Transport: &gatedTransport{gate: gate}})
	r.prefillPolicy = pd.NewLeastRequestPrefillPolicy()

	_, _, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "least-request", 4000), readyPods)
	require.NoError(t, err)
	active, kv := tokenLoad.GetLoad(prefillPod.Name)
	assert.Equal(t, float64(0), active, "least_request must not charge the token-load tracker")
	assert.Equal(t, float64(0), kv)

	ctx := tokenLoadRequest(t, "via-routing-config", 4000)
	ctx.ConfigProfile = &types.ResolvedConfigProfile{
		RoutingConfig: json.RawMessage(fmt.Sprintf(`{"prefillScorePolicy":%q}`, pd.PrefillScorePolicyTokenLoad)),
	}
	pre, _, err := r.effectiveScorePolicies(ctx)
	require.NoError(t, err)
	assert.Equal(t, pd.PrefillScorePolicyTokenLoad, pre.Name())

	_, _, err = r.filterPrefillDecodePods(ctx, readyPods)
	require.NoError(t, err)
	active, kv = tokenLoad.GetLoad(prefillPod.Name)
	assert.Equal(t, float64(1000), active, "token_load selected through routingConfig charges the tracker")
	assert.Equal(t, float64(1000), kv)
}
