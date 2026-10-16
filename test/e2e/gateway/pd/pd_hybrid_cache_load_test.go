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
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// hybridCacheLoadConfigProfile is the model.aibrix.ai/config profile declared
	// in development/app/config/mock/vllm-pd-config.yaml that selects
	// routingConfig.prefillScorePolicy=hybrid_cache_load for the llama2-7b-vllm
	// PD model.
	hybridCacheLoadConfigProfile = "hybrid-cache-load"

	// hybridCacheLoadAffinityTimeout bounds how long the repeat request may take
	// to land on the warm prefill pod. The e2e gateway runs two replicas that
	// exchange prefix-cache state through Redis once a second
	// (config/test/gateway/vtc-test-env-patch.yaml), so the first repeat can hit
	// a replica that has not yet seen the warm-up prompt.
	hybridCacheLoadAffinityTimeout = 30 * time.Second
	hybridCacheLoadAffinityPoll    = 2 * time.Second
)

// TestPDDisaggregationVLLMHybridCacheLoad verifies the hybrid_cache_load
// prefill score policy end to end. Selecting it through a model config profile
// keeps PD routing working; with every prefill pod idle the policy scores a
// pod holding the prompt's prefix strictly below the others, so repeating a
// prompt must route back to the prefill pod that served it first; and the
// token-load charges the policy shares with token_load are visible as
// pd_token_load_* gauges and released once the requests complete.
//
// The new_tokens charge (session delta, prefix-match discount) and the
// load-versus-cache trade-off are not asserted here: the mock engines answer
// in milliseconds, so the ledger is back at zero before it can be scraped,
// and the two gateway replicas keep independent ledgers. Those rules are
// covered by the router and tracker unit tests; this test pins the wiring
// and the cache-affinity half of the score.
func TestPDDisaggregationVLLMHybridCacheLoad(t *testing.T) {
	ctx := context.Background()

	waitForPDDisaggregationRouting(t, modelNameVLLM)

	k8sClient, _ := initializeClient(ctx, t)
	gatewayPods := listGatewayPluginPods(t, ctx, k8sClient)

	// A fresh prompt per run so the warm-up request is a true cache miss and
	// the repeat is decided by this run's prefix entry alone. The nonce comes
	// first: prefix hashes are chained from the start of the prompt, so any
	// shared leading text would already match entries left by earlier runs.
	nonce := make([]byte, 8)
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	prompt := hex.EncodeToString(nonce) + " " + strings.Repeat("hybrid cache load e2e prompt body ", 40)

	var dst *http.Response
	client := createOpenAIClientWithConfigProfile(gatewayURL, apiKey, hybridCacheLoadConfigProfile,
		option.WithResponseInto(&dst))

	prefillPods := map[string]struct{}{}
	send := func(label string) string {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
			Model:    modelNameVLLM,
		})
		require.NoError(t, err, "hybrid_cache_load PD chat completion (%s) failed", label)

		assert.Equal(t, "pd", dst.Header.Get("routing-strategy"),
			"%s: config profile %q should resolve to PD routing", label, hybridCacheLoadConfigProfile)
		prefillPod := dst.Header.Get("prefill-target-pod")
		decodePod := dst.Header.Get("target-pod")
		require.NotEmpty(t, prefillPod, "%s: prefill-target-pod header must be set", label)
		require.NotEmpty(t, decodePod, "%s: target-pod header must be set", label)
		assert.NotEqual(t, prefillPod, decodePod, "%s: prefill and decode pods should differ", label)
		prefillPods[prefillPod] = struct{}{}
		t.Logf("%s — prefill: %s, decode: %s", label, prefillPod, decodePod)
		return prefillPod
	}

	// Warm-up: the first request populates the prefix cache on whichever
	// prefill pod the policy picks while nothing is cached anywhere.
	warmPod := send("warm-up")

	// Repeat the same prompt. Once the gateway replica handling the request has
	// the warm-up's prefix entry, hybrid_cache_load scores warmPod as
	// 1 − 1² × factor (< 1) against a bare 1 for the cold pod and must pick it.
	var repeatPod string
	require.Eventually(t, func() bool {
		repeatPod = send("repeat")
		return repeatPod == warmPod
	}, hybridCacheLoadAffinityTimeout, hybridCacheLoadAffinityPoll,
		"expected the repeated prompt to route back to warm prefill pod %s, last saw %s", warmPod, repeatPod)

	lastGauges := assertTokenLoadGaugesSettle(t, ctx, k8sClient, gatewayPods, prefillPods, "hybrid_cache_load")
	t.Logf("hybrid_cache_load gauges after warm-up and repeat: %v", lastGauges)
}
