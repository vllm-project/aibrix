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

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
)

// doSessionAffinityRequest sends a single "session-affinity" chat completion carrying
// headers (e.g. x-session-id or x-aibrix-session-key) and returns the target-pod and
// x-session-id response headers. It performs no test assertions so it is safe to call
// from concurrent goroutines (testify's require/assert.FailNow must only ever run on the
// main test goroutine); callers assert on the returned values themselves.
func doSessionAffinityRequest(ctx context.Context, message string, headers map[string]string) (
	targetPod, sessionID string, err error) {
	var dst *http.Response
	client := createOpenAIClientWithRoutingStrategy(gatewayURL, apiKey, "session-affinity", option.WithResponseInto(&dst))

	opts := make([]option.RequestOption, 0, len(headers))
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}

	_, err = client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage(message),
		},
		Model: modelName,
	}, opts...)
	if err != nil {
		return "", "", err
	}
	return dst.Header.Get("target-pod"), dst.Header.Get(constants.HeaderSessionID), nil
}

// getSessionAffinityTarget is the sequential-test convenience wrapper around
// doSessionAffinityRequest: it fails the test immediately on error or a missing
// target-pod header, matching the getTargetPodFromChatCompletion pattern used
// elsewhere in this package.
func getSessionAffinityTarget(t *testing.T, message string, headers map[string]string) (targetPod, sessionID string) {
	t.Helper()
	targetPod, sessionID, err := doSessionAffinityRequest(context.Background(), message, headers)
	require.NoError(t, err, "session-affinity chat completion failed: %v", err)
	require.NotEmpty(t, targetPod, "target-pod header missing")
	return targetPod, sessionID
}

// TestSessionAffinityXSessionIDStickiness covers the sequential multi-turn flow described
// in docs/source/features/agentic-routing.rst: the first request gets a gateway-issued
// x-session-id, and echoing it back on the next request routes to the same pod.
func TestSessionAffinityXSessionIDStickiness(t *testing.T) {
	const msg = "session affinity x-session-id stickiness test"

	pod1, sessionID := getSessionAffinityTarget(t, msg, nil)
	require.NotEmpty(t, sessionID, "first request should return an x-session-id header to carry forward")
	t.Logf("first request (no x-session-id) routed to %s, issued x-session-id=%s", pod1, sessionID)

	pod2, sessionID2 := getSessionAffinityTarget(t, msg, map[string]string{constants.HeaderSessionID: sessionID})
	assert.Equal(t, pod1, pod2, "request carrying the issued x-session-id should route back to the same pod")
	assert.NotEmpty(t, sessionID2, "response should keep returning an x-session-id on every request")
	t.Logf("second request (x-session-id=%s) routed to %s", sessionID, pod2)
}

// TestSessionAffinityConcurrentAgenticRequestsShareSessionKey covers the core agentic-routing
// scenario from docs/source/features/agentic-routing.rst: an agent run mints its own
// x-aibrix-session-key up front and fans out several sub-requests (parallel tool calls,
// sub-agent turns) concurrently, before any of them has produced an x-session-id to reuse.
// All of them must still land on the same ready pod.
func TestSessionAffinityConcurrentAgenticRequestsShareSessionKey(t *testing.T) {
	sessionKey := "agent-run-" + uuid.New().String()
	const concurrency = 8

	var wg sync.WaitGroup
	pods := make([]string, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("concurrent agentic sub-request %d for session key %s", i, sessionKey)
			headers := map[string]string{constants.HeaderSessionKey: sessionKey}
			pod, _, err := doSessionAffinityRequest(context.Background(), msg, headers)
			pods[i] = pod
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "concurrent sub-request %d failed", i)
		require.NotEmpty(t, pods[i], "concurrent sub-request %d returned no target-pod header", i)
	}

	want := pods[0]
	for i, pod := range pods {
		assert.Equal(t, want, pod,
			"sub-request %d routed to %s; all %d concurrent requests sharing session key %s must land on the same pod",
			i, pod, concurrency, sessionKey)
	}
	t.Logf("%d concurrent agentic sub-requests sharing session key %s all routed to pod %s", concurrency, sessionKey, want)
}

// TestSessionAffinitySessionKeyStickyAcrossReplicas exercises the Redis-backed pinning
// path described in the "Staying sticky across gateway plugin replicas" section of
// docs/source/features/agentic-routing.rst. config/test/gateway patches
// aibrix-gateway-plugins to run 2 replicas, and NewOpenAIClient disables HTTP keep-alives,
// so each of these requests is a fresh connection Envoy is free to load-balance to either
// replica. Once a session key is pinned, every follow-up request -- regardless of which
// gateway-plugin replica happens to serve it -- must resolve to the same pod via the
// shared Redis mapping (see resolveSessionPod/storeSessionKeyLocal in
// pkg/plugins/gateway/algorithms/simple_session_affinity.go), not just whichever replica
// originally set the pin.
//
// Note this alone doesn't distinguish Redis-backed pinning from pure rendezvous hashing,
// which is also deterministic across replicas as long as they observe the same ready pod
// set (see the doc's "Without Redis" note) -- the two are only observably different when
// the ready pod set changes between requests, which this test does not induce. What this
// test does guard end-to-end is the documented cross-replica contract: a session key stays
// pinned to one pod for the life of the run no matter which replica handles each request.
func TestSessionAffinitySessionKeyStickyAcrossReplicas(t *testing.T) {
	sessionKey := "agent-run-" + uuid.New().String()
	headers := map[string]string{constants.HeaderSessionKey: sessionKey}

	warmPod, _ := getSessionAffinityTarget(t, "session key warm-up request", headers)
	t.Logf("warm-up pinned session key %s to pod %s", sessionKey, warmPod)

	const requests = 20
	seen := map[string]int{}
	for i := 0; i < requests; i++ {
		msg := fmt.Sprintf("session key follow-up request %d for %s", i, sessionKey)
		pod, _ := getSessionAffinityTarget(t, msg, headers)
		seen[pod]++
		assert.Equal(t, warmPod, pod,
			"request %d: session key %s should stay pinned to warm pod %s across gateway-plugin replicas, got %s",
			i, sessionKey, warmPod, pod)
	}
	t.Logf("%d follow-up requests for session key %s: pod distribution %v", requests, sessionKey, seen)
}

// TestSessionAffinityOversizedSessionKeyFallsBack covers the documented 256-byte cap: a
// session key longer than that is treated as unset rather than rejected, so the request
// still succeeds via normal session-affinity resolution (see validSessionKey in
// pkg/plugins/gateway/algorithms/simple_session_affinity.go).
func TestSessionAffinityOversizedSessionKeyFallsBack(t *testing.T) {
	oversizedKey := strings.Repeat("k", 257)
	headers := map[string]string{constants.HeaderSessionKey: oversizedKey}
	pod, _ := getSessionAffinityTarget(t, "oversized session key fallback test", headers)
	assert.NotEmpty(t, pod, "an oversized session key should fall back to normal session-affinity "+
		"resolution, not fail the request")
}
