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

// Tests for the gateway-owned engine-visible request id ("rid") that both PD
// legs carry, and that /abort_request matches on when the prefill leg fails.

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ridTestCtx(requestID, body string) *types.RoutingContext {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("pd"), "test-model", "message", requestID, "user")
	ctx.ReqPath = "/v1/chat/completions"
	ctx.Engine = "sglang"
	ctx.ReqBody = []byte(body)
	return ctx
}

func ridTestPod(name, ip string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1.PodStatus{PodIP: ip}}
}

func TestPDRequestIDInjectedIntoBothLegs(t *testing.T) {
	ctx := ridTestCtx("req-both-legs", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	prefillPod := ridTestPod("prefill-1", "10.0.0.1")

	base, err := (&SGLangHandler{}).AugmentPrefillRequest(ctx, prefillPod, ctx.ReqBody)
	require.NoError(t, err)
	// The prefill leg is derived from the decode body, exactly as
	// prefill.PreparePayload does it.
	prefillBody, err := pd.ApplyPrefillControlFields(base, "sglang")
	require.NoError(t, err)

	prefillRID := gjson.GetBytes(prefillBody, "rid").String()
	decodeRID := gjson.GetBytes(ctx.ReqBody, "rid").String()

	assert.NotEmpty(t, prefillRID, "prefill leg must carry a rid")
	assert.Equal(t, prefillRID, decodeRID, "both legs must carry the identical rid")
	assert.Equal(t, prefillRID, ctx.PDRequestID(), "the rid must be recorded on the routing context")

	// The rid keeps the gateway request id as its readable prefix and adds a
	// fixed-width nonce, so no gateway rid can be a proper prefix of another
	// (SGLang aborts by rid prefix).
	assert.True(t, strings.HasPrefix(prefillRID, "req-both-legs-"), "rid should start with the request id: %s", prefillRID)
	assert.Len(t, prefillRID, len("req-both-legs")+1+16)

	// The bootstrap fields the decode leg needs must survive rid injection.
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(ctx.ReqBody, "bootstrap_host").String())
	assert.True(t, gjson.GetBytes(ctx.ReqBody, "bootstrap_room").Exists())
	assert.True(t, gjson.GetBytes(ctx.ReqBody, "stream").Bool(), "decode leg keeps the client's stream flag")
	assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool(), "prefill leg is never streamed")
}

func TestPDRequestIDOverridesClientSuppliedRID(t *testing.T) {
	ctx := ridTestCtx("req-override", `{"rid":"client-supplied-rid","messages":[{"role":"user","content":"hi"}]}`)

	base, err := (&SGLangHandler{}).AugmentPrefillRequest(ctx, ridTestPod("prefill-1", "10.0.0.1"), ctx.ReqBody)
	require.NoError(t, err)
	prefillBody, err := pd.ApplyPrefillControlFields(base, "sglang")
	require.NoError(t, err)

	prefillRID := gjson.GetBytes(prefillBody, "rid").String()
	decodeRID := gjson.GetBytes(ctx.ReqBody, "rid").String()
	assert.NotEqual(t, "client-supplied-rid", prefillRID)
	assert.NotEqual(t, "client-supplied-rid", decodeRID)
	assert.Equal(t, prefillRID, decodeRID)
	assert.Equal(t, ctx.PDRequestID(), prefillRID)
}

// A retrying client can hand the gateway the same RequestID twice (the
// traceparent path makes RequestID the trace id), so the rid must still differ
// per attempt or the engine's in-flight request state carries residue across
// them.
func TestPDRequestIDUniquePerAttempt(t *testing.T) {
	seen := map[string]bool{}
	length := 0
	for i := 0; i < 64; i++ {
		rid := NewPDRequestID("same-request-id")
		assert.False(t, seen[rid], "rid repeated across attempts: %s", rid)
		seen[rid] = true
		if length == 0 {
			length = len(rid)
		}
		assert.Equal(t, length, len(rid), "rids must be fixed-length for prefix-safe aborts")
	}
}

func TestSGLangDecodeBodyRejectsEmptyRID(t *testing.T) {
	_, err := sglangDecodeBody([]byte(`{}`), "10.0.0.1", 8998, 12345, "")
	assert.Error(t, err, "an empty rid would prefix-match every live request on the engine")
}

// A failed augmentation must leave the routing context untouched, rid included:
// the gateway must never hold a rid that no engine leg has seen.
func TestAugmentPrefillRequestLeavesNoRIDOnFailure(t *testing.T) {
	ctx := ridTestCtx("req-bad-body", `[1,2,3]`)
	original := ctx.ReqBody

	_, err := (&SGLangHandler{}).AugmentPrefillRequest(ctx, ridTestPod("prefill-1", "10.0.0.1"), ctx.ReqBody)
	require.Error(t, err)
	assert.Equal(t, string(original), string(ctx.ReqBody))
	assert.Empty(t, ctx.PDRequestID())
}
