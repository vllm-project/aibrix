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

package pd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

func TestTokenLoadDecodePolicy_ResolvesAndScoresFromLedger(t *testing.T) {
	policy, name, unknown := ResolveDecodePolicy(" Token_Load ")
	assert.False(t, unknown)
	assert.Equal(t, DecodePolicyTokenLoad, name)
	assert.True(t, UsesDecodeTokenLoad(policy))
	assert.False(t, UsesDecodeTokenLoad(LoadBalancingDecodePolicy{}))
	assert.False(t, UsesDecodeTokenLoad(nil))

	ctx := types.NewRoutingContext(context.Background(), "pd", "m", "msg", "req", "")
	score := policy.ScoreDecodePod(ctx, &v1.Pod{}, DecodePodInput{RunningReqs: 7, FreeGPUPercent: 1, DecodeTokens: 1234})
	assert.Equal(t, float64(1234), score, "only the ledger counts, not request counts or scraped KV")
}

func TestValidDecodePolicyNames_ListsBuiltinsOnce(t *testing.T) {
	RegisterDecodePolicy("token_load", func() DecodeScorePolicy { return TokenLoadDecodePolicy{} })
	RegisterDecodePolicy("custom-decode-test", func() DecodeScorePolicy { return LeastRequestDecodePolicy{} })
	t.Cleanup(func() {
		decodePolicyRegistryMu.Lock()
		delete(decodePolicyRegistryCustom, "token_load")
		delete(decodePolicyRegistryCustom, "custom-decode-test")
		decodePolicyRegistryMu.Unlock()
	})
	assert.Equal(t, []string{"conductor", "custom-decode-test", "least_request", "load_balancing", "token_load"}, ValidDecodePolicyNames())
}
