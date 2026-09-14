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

package transfer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

const clientBody = `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"u","detail":"low"}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"integer"}}}}}],"stream":true}`

func newCtx(body string) *types.RoutingContext {
	return &types.RoutingContext{
		RequestID: "req-1",
		ReqBody:   []byte(body),
		Context:   context.Background(),
	}
}

func assertNestedIdentical(t *testing.T, original, got []byte) {
	t.Helper()
	for _, path := range []string{"messages", "tools", "model"} {
		assert.Equal(t, gjson.GetBytes(original, path).Raw, gjson.GetBytes(got, path).Raw, "%s must be byte-identical", path)
	}
}

func TestSHFSAgent_AugmentPrefillRequest(t *testing.T) {
	a := &SHFSAgent{}
	in := []byte(`{"model":"m","kv_transfer_params":{"do_remote_decode":false,"junk":1},"messages":[{"role":"user","content":"hi"}]}`)
	orig := append([]byte(nil), in...)

	out, err := a.AugmentPrefillRequest(newCtx(""), &v1.Pod{}, in)
	require.NoError(t, err)
	assert.Equal(t, orig, in, "input must not be mutated")
	assertNestedIdentical(t, in, out)

	kv := gjson.GetBytes(out, "kv_transfer_params")
	require.True(t, kv.IsObject())
	assert.False(t, kv.Get("junk").Exists(), "client-supplied kv_transfer_params must be replaced")
	assert.True(t, kv.Get("do_remote_decode").Bool())
	assert.False(t, kv.Get("do_remote_prefill").Bool())
	for _, k := range []string{"remote_engine_id", "remote_block_ids", "remote_host", "remote_port"} {
		r := kv.Get(k)
		assert.True(t, r.Exists() && r.Type == gjson.Null, "%s must be explicit null: %s", k, kv.Raw)
	}

	// Stable across repeated calls.
	again, err := a.AugmentPrefillRequest(newCtx(""), &v1.Pod{}, in)
	require.NoError(t, err)
	assert.Equal(t, out, again)
}

func TestSHFSAgent_MergePrefillResponse(t *testing.T) {
	a := &SHFSAgent{}
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.1.1.1"}}
	resp := []byte(`{"id":"x","kv_transfer_params":{"remote_block_ids":[3,1,2],"remote_engine_id":"e","remote_host":"stale","remote_port":9}}`)

	ctx := newCtx(clientBody)
	require.NoError(t, a.MergePrefillResponse(ctx, resp, pod))
	assertNestedIdentical(t, []byte(clientBody), ctx.ReqBody)
	kv := gjson.GetBytes(ctx.ReqBody, "kv_transfer_params")
	assert.Equal(t, "10.1.1.1", kv.Get("remote_host").String(), "remote_host must be the prefill pod IP")
	assert.Equal(t, "[3,1,2]", kv.Get("remote_block_ids").Raw)
	assert.Equal(t, int64(9), kv.Get("remote_port").Int())
	assert.True(t, gjson.GetBytes(ctx.ReqBody, "stream").Bool(), "client fields untouched")

	// Missing kv_transfer_params → decode body unchanged.
	ctx = newCtx(clientBody)
	require.NoError(t, a.MergePrefillResponse(ctx, []byte(`{"id":"x"}`), pod))
	assert.Equal(t, clientBody, string(ctx.ReqBody))

	// Wrong type → error.
	ctx = newCtx(clientBody)
	err := a.MergePrefillResponse(ctx, []byte(`{"kv_transfer_params":"str"}`), pod)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected type")
	assert.Equal(t, clientBody, string(ctx.ReqBody), "ReqBody must be unchanged on error")

	// Invalid decode body → error.
	require.Error(t, a.MergePrefillResponse(newCtx(`invalid json`), resp, pod))
	require.Error(t, a.MergePrefillResponse(newCtx(`[]`), resp, pod))
}

func TestNIXLAgent(t *testing.T) {
	a := &NIXLAgent{}
	pod := &v1.Pod{Status: v1.PodStatus{PodIP: "10.1.1.1"}}

	out, err := a.AugmentPrefillRequest(newCtx(""), pod, []byte(clientBody))
	require.NoError(t, err)
	assert.Equal(t, clientBody, string(out), "NIXL prefill augmentation is a no-op")

	// The prefill response is embedded verbatim (surrounding whitespace trimmed),
	// including a large integer and unsorted keys that a map round trip would alter.
	resp := []byte(" {\"kv_transfer_params\":{\"remote_block_ids\":[3,1,2],\"remote_engine_id\":\"e\"},\"id\":9007199254740993}\n")
	ctx := newCtx(clientBody)
	require.NoError(t, a.MergePrefillResponse(ctx, resp, pod))
	assertNestedIdentical(t, []byte(clientBody), ctx.ReqBody)
	assert.Equal(t, `{"kv_transfer_params":{"remote_block_ids":[3,1,2],"remote_engine_id":"e"},"id":9007199254740993}`,
		gjson.GetBytes(ctx.ReqBody, "disagg_prefill_resp").Raw)

	require.Error(t, a.MergePrefillResponse(newCtx(`invalid json`), resp, pod))
}

func TestMooncakeAgent_NoOp(t *testing.T) {
	a := &MooncakeAgent{}
	out, err := a.AugmentPrefillRequest(newCtx(""), &v1.Pod{}, []byte(clientBody))
	require.NoError(t, err)
	assert.Equal(t, clientBody, string(out))

	ctx := newCtx(clientBody)
	require.NoError(t, a.MergePrefillResponse(ctx, []byte(`{"x":1}`), &v1.Pod{}))
	assert.Equal(t, clientBody, string(ctx.ReqBody))
}
