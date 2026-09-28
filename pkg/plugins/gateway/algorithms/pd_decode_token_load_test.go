/*
Copyright 2024 The Aibrix Team.

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// newDecodeTokenLoadTestRouter is newTokenLoadTestRouter with the token_load
// decode policy and a prefill call that returns immediately.
func newDecodeTokenLoadTestRouter(t *testing.T) (*pdRouter, *pd.TokenLoadTracker) {
	t.Helper()
	gate := make(chan struct{})
	close(gate)
	r, tokenLoad := newTokenLoadTestRouter(t, &http.Client{Transport: &gatedTransport{gate: gate}})
	r.prefillPolicy = pd.NewLeastRequestPrefillPolicy()
	r.decodePolicy = pd.TokenLoadDecodePolicy{}
	return r, tokenLoad
}

// TestPDRouter_DecodeTokenLoadSpreadsByPromptCost routes one long prompt and
// two short ones, with no engine metrics to tell the decode pods apart. The
// long prompt's KV lands on one decode pod, so token_load sends both short
// prompts to the other one; request counts alone would treat the long prompt
// like a short one.
func TestPDRouter_DecodeTokenLoadSpreadsByPromptCost(t *testing.T) {
	const (
		longBytes  = 40_000 // 10_000 tokens
		shortBytes = 400    // 100 tokens
	)
	decodePods := []*v1.Pod{
		burstPod("decode-0", "decode", "127.0.0.100"),
		burstPod("decode-1", "decode", "127.0.0.101"),
	}
	readyPods := append([]*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1")}, decodePods...)
	r, tokenLoad := newDecodeTokenLoadTestRouter(t)

	_, longDecode, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "long", longBytes), readyPods)
	require.NoError(t, err)

	// The decode scores are the ledger itself: the pod holding the long prompt
	// scores its tokens, the other one scores zero and wins.
	run := r.scoreDecodePods(tokenLoadRequest(t, "probe", shortBytes), append([]*v1.Pod{}, decodePods...),
		1, 1, 1, map[string]float64{}, map[string]float64{}, map[string]float64{}, pd.TokenLoadDecodePolicy{})
	require.NoError(t, run.Err)
	assert.Equal(t, float64(longBytes/4), run.MaxScore)
	pick := run.PerRoleset["burst-rs"]
	assert.NotEqual(t, longDecode.Name, pick.Pod.Name)
	assert.Equal(t, float64(0), pick.Score)

	for _, id := range []string{"short-0", "short-1"} {
		_, decode, err := r.filterPrefillDecodePods(tokenLoadRequest(t, id, shortBytes), readyPods)
		require.NoError(t, err)
		assert.NotEqualf(t, longDecode.Name, decode.Name, "%s must avoid the decode pod holding the long prompt", id)
	}

	assert.Equal(t, float64(longBytes/4), tokenLoad.GetDecodeLoad(burstPodKey(longDecode.Name)))
	other := decodePods[0].Name
	if other == longDecode.Name {
		other = decodePods[1].Name
	}
	assert.Equal(t, float64(2*shortBytes/4), tokenLoad.GetDecodeLoad(burstPodKey(other)))

	// Completion releases each request's decode charge.
	r.DoneRequestCount(nil, "long", "token-load-model", 0)
	r.DoneRequestTrace(nil, "short-0", "token-load-model", 0, 0, 0)
	assert.Equal(t, float64(0), tokenLoad.GetDecodeLoad(burstPodKey(longDecode.Name)))
	assert.Equal(t, float64(shortBytes/4), tokenLoad.GetDecodeLoad(burstPodKey(other)), "short-1 has not completed yet")
	r.DoneRequestCount(nil, "short-1", "token-load-model", 0)
	assert.Equal(t, float64(0), tokenLoad.GetDecodeLoad(burstPodKey(other)))
}

// TestPDRouter_DecodeTokenLoadIgnoresColdStartScore checks that a decode pod
// without scraped metrics is scored from the ledger like any other. The
// cold-start score (1 + pending) is in the units of the metric-based
// policies; next to token counts it would make an unmeasured pod look almost
// free and draw every request.
func TestPDRouter_DecodeTokenLoadIgnoresColdStartScore(t *testing.T) {
	warm := burstPod("decode-warm", "decode", "127.0.0.100")
	cold := burstPod("decode-cold", "decode", "127.0.0.101")
	readyPods := []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), warm, cold}
	r, tokenLoad := newDecodeTokenLoadTestRouter(t)
	r.cache = cache.NewWithPodsMetricsForTest(readyPods, "token-load-model", map[string]map[string]metrics.MetricValue{
		warm.Name: {metrics.RealtimeNumRequestsRunning: &metrics.SimpleMetricValue{Value: 1}},
	})
	require.True(t, r.decodePodMetricsReady(tokenLoadRequest(t, "probe", 400), warm))
	require.False(t, r.decodePodMetricsReady(tokenLoadRequest(t, "probe", 400), cold))

	tokenLoad.AcquireDecodeWithTTL("earlier-warm", burstPodKey(warm.Name), 3000, 0)
	tokenLoad.AcquireDecodeWithTTL("earlier-cold", burstPodKey(cold.Name), 5000, 0)

	_, decode, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "next", 400), readyPods)
	require.NoError(t, err)
	assert.Equal(t, warm.Name, decode.Name, "the cold pod holds more tokens and must not win on a cold-start score")
}

// TestPDRouter_DecodeTokenLoadReleasedAfterRoute checks the charge outlives
// Route, which only covers selection and the prefill call, and is released
// by completion; and that a failed prefill leaves nothing charged.
func TestPDRouter_DecodeTokenLoadReleasedAfterRoute(t *testing.T) {
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	podList := &utils.PodArray{Pods: []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), decodePod}}
	decodeKey := utils.GeneratePodKey(decodePod.Namespace, decodePod.Name)

	r, tokenLoad := newDecodeTokenLoadTestRouter(t)
	_, err := r.Route(tokenLoadRequest(t, "ok", 4000), podList)
	require.NoError(t, err)
	assert.Equal(t, float64(1000), tokenLoad.GetDecodeLoad(decodeKey), "the decode pod holds the KV until the request completes")
	r.DoneRequestCount(nil, "ok", "token-load-model", 0)
	assert.Equal(t, float64(0), tokenLoad.GetDecodeLoad(decodeKey))

	failing, failingLoad := newTokenLoadTestRouter(t, &http.Client{Transport: failingTransport{}})
	failing.prefillPolicy = pd.NewLeastRequestPrefillPolicy()
	failing.decodePolicy = pd.TokenLoadDecodePolicy{}
	_, err = failing.Route(tokenLoadRequest(t, "doomed", 4000), podList)
	require.Error(t, err)
	assert.Equal(t, float64(0), failingLoad.GetDecodeLoad(decodeKey), "a failed prefill releases the decode charge")
}

// TestPDRouter_DecodeTokenLoadChargedOnlyForTokenLoadPolicy checks that the
// decode ledger is left alone under the default policy, and that
// routingConfig can switch a request to token_load.
func TestPDRouter_DecodeTokenLoadChargedOnlyForTokenLoadPolicy(t *testing.T) {
	decodePod := burstPod("decode-0", "decode", "127.0.0.100")
	readyPods := []*v1.Pod{burstPod("prefill-0", "prefill", "127.0.0.1"), decodePod}
	decodeKey := utils.GeneratePodKey(decodePod.Namespace, decodePod.Name)

	r, tokenLoad := newDecodeTokenLoadTestRouter(t)
	r.decodePolicy = pd.LoadBalancingDecodePolicy{}

	_, _, err := r.filterPrefillDecodePods(tokenLoadRequest(t, "load-balancing", 4000), readyPods)
	require.NoError(t, err)
	assert.Equal(t, float64(0), tokenLoad.GetDecodeLoad(decodeKey), "load_balancing must not charge the decode ledger")

	routingConfig := json.RawMessage(fmt.Sprintf(`{"decodeScorePolicy":%q}`, pd.ScorePolicyTokenLoad))
	ctx := tokenLoadRequest(t, "via-routing-config", 4000)
	ctx.ConfigProfile = &types.ResolvedConfigProfile{
		RoutingConfig: routingConfig,
		Routing:       configprofiles.ParseRoutingConfig(routingConfig),
	}
	_, decodePol, err := r.effectiveScorePolicies(ctx)
	require.NoError(t, err)
	assert.Equal(t, pd.DecodePolicyTokenLoad, decodePol.Name())

	_, _, err = r.filterPrefillDecodePods(ctx, readyPods)
	require.NoError(t, err)
	assert.Equal(t, float64(1000), tokenLoad.GetDecodeLoad(decodeKey), "token_load selected through routingConfig charges the decode ledger")
}
