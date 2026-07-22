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

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

const toolRequestBody = `{"model":"test-model","messages":[{"role":"user","content":"Call the lookup tool"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup a value","parameters":{"type":"object","properties":{"query":{"type":"string","description":"Lookup query"}},"required":["query"]}}}],"max_tokens":256,"stream":true,"stream_options":{"include_usage":true},"min_tokens":10}`

func testPod() *v1.Pod {
	return &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}}
}

func TestPrepareSGLangRequestBodies_ToolsByteIdentity(t *testing.T) {
	original := []byte(toolRequestBody)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Extract the tools array from each body and compare byte-for-byte.
	originalTools := gjson.GetBytes(original, "tools").Raw
	prefillTools := gjson.GetBytes(prefillBody, "tools").Raw
	decodeTools := gjson.GetBytes(decodeBody, "tools").Raw

	assert.Equal(t, originalTools, prefillTools, "tools must be byte-identical in prefill body")
	assert.Equal(t, originalTools, decodeTools, "tools must be byte-identical in decode body")

	// Same for messages.
	originalMessages := gjson.GetBytes(original, "messages").Raw
	prefillMessages := gjson.GetBytes(prefillBody, "messages").Raw
	decodeMessages := gjson.GetBytes(decodeBody, "messages").Raw
	assert.Equal(t, originalMessages, prefillMessages, "messages must be byte-identical in prefill body")
	assert.Equal(t, originalMessages, decodeMessages, "messages must be byte-identical in decode body")
}

func TestPrepareSGLangRequestBodies_BootstrapFields(t *testing.T) {
	original := []byte(toolRequestBody)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Bootstrap fields must be present and equal in both bodies.
	for _, body := range [][]byte{prefillBody, decodeBody} {
		assert.Equal(t, "10.0.0.1", gjson.GetBytes(body, "bootstrap_host").String())
		assert.Equal(t, int64(8998), gjson.GetBytes(body, "bootstrap_port").Int())
		assert.Equal(t, int64(12345), gjson.GetBytes(body, "bootstrap_room").Int())
	}
}

func TestPrepareSGLangRequestBodies_BootstrapOverwritesClientValue(t *testing.T) {
	original := []byte(`{"model":"m","messages":[],"bootstrap_host":"client-value","bootstrap_port":1111,"bootstrap_room":2222}`)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 99999)
	require.NoError(t, err)

	assert.Equal(t, "10.0.0.1", gjson.GetBytes(decodeBody, "bootstrap_host").String())
	assert.Equal(t, int64(8998), gjson.GetBytes(decodeBody, "bootstrap_port").Int())
	assert.Equal(t, int64(99999), gjson.GetBytes(decodeBody, "bootstrap_room").Int())
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(prefillBody, "bootstrap_host").String())
	assert.Equal(t, int64(99999), gjson.GetBytes(prefillBody, "bootstrap_room").Int())
}

func TestPrepareSGLangRequestBodies_DecodeRetainsGenerationParams(t *testing.T) {
	original := []byte(toolRequestBody)
	_, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Decode body should retain the client's generation parameters.
	assert.Equal(t, int64(256), gjson.GetBytes(decodeBody, "max_tokens").Int())
	assert.True(t, gjson.GetBytes(decodeBody, "stream").Bool())
	assert.True(t, gjson.GetBytes(decodeBody, "stream_options").Exists())
	assert.Equal(t, int64(10), gjson.GetBytes(decodeBody, "min_tokens").Int())
}

func TestPrepareSGLangRequestBodies_PrefillControlFields(t *testing.T) {
	original := []byte(toolRequestBody)
	prefillBody, _, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_tokens").Int())
	assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_completion_tokens").Int())
	assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool())
	assert.False(t, gjson.GetBytes(prefillBody, "stream_options").Exists(), "stream_options should be deleted")
	assert.False(t, gjson.GetBytes(prefillBody, "min_tokens").Exists(), "min_tokens should be deleted")
}

func TestPrepareSGLangRequestBodies_MissingOptionalFields(t *testing.T) {
	original := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Should still set prefill control fields even if they were absent.
	assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_tokens").Int())
	assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool())

	// Deleting a non-existent field should not error — just no-op.
	assert.False(t, gjson.GetBytes(prefillBody, "stream_options").Exists())
	assert.False(t, gjson.GetBytes(prefillBody, "min_tokens").Exists())

	// Decode body should not have grown new fields.
	assert.False(t, gjson.GetBytes(decodeBody, "max_completion_tokens").Exists())
}

func TestPrepareSGLangRequestBodies_InvalidJSON(t *testing.T) {
	_, _, err := prepareSGLangRequestBodies([]byte(`{not json`), "10.0.0.1", 8998, 12345)
	assert.Error(t, err)
}

func TestPrepareSGLangRequestBodies_NonObjectJSON(t *testing.T) {
	// A JSON array is valid JSON but not an object.
	_, _, err := prepareSGLangRequestBodies([]byte(`[1,2,3]`), "10.0.0.1", 8998, 12345)
	assert.Error(t, err)
}

func TestPrepareSGLangRequestBodies_LargeRequestBody(t *testing.T) {
	// Build a large body with many messages.
	large := `{"model":"m","messages":[`
	for i := 0; i < 100; i++ {
		if i > 0 {
			large += ","
		}
		large += fmt.Sprintf(`{"role":"user","content":"message number %d with some padding text to increase size"}`, i)
	}
	large += `],"max_tokens":512}`

	prefillBody, decodeBody, err := prepareSGLangRequestBodies([]byte(large), "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Verify messages are byte-identical.
	assert.Equal(t, gjson.GetBytes([]byte(large), "messages").Raw, gjson.GetBytes(prefillBody, "messages").Raw)
	assert.Equal(t, gjson.GetBytes([]byte(large), "messages").Raw, gjson.GetBytes(decodeBody, "messages").Raw)
}

func TestPrepareSGLangRequestBodies_SameRoomInBothBodies(t *testing.T) {
	original := []byte(toolRequestBody)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 77777)
	require.NoError(t, err)

	prefillRoom := gjson.GetBytes(prefillBody, "bootstrap_room").Int()
	decodeRoom := gjson.GetBytes(decodeBody, "bootstrap_room").Int()
	assert.Equal(t, prefillRoom, decodeRoom, "bootstrap_room must be the same in prefill and decode")
}

func TestPreparePrefillPayload_ErrorDoesNotModifyReqBody(t *testing.T) {
	originalBody := []byte(`{not json`)
	routingCtx := &types.RoutingContext{
		ReqBody: originalBody,
		Context: context.Background(),
	}
	handler := &SGLangHandler{}
	pod := testPod()

	_, err := handler.PreparePrefillPayload(routingCtx, pod)
	assert.Error(t, err)

	// ReqBody should be unchanged on error.
	assert.Equal(t, originalBody, routingCtx.ReqBody)
}

func TestPreparePrefillPayload_SuccessUpdatesReqBody(t *testing.T) {
	originalBody := []byte(toolRequestBody)
	routingCtx := &types.RoutingContext{
		ReqBody: originalBody,
		Context: context.Background(),
	}
	handler := &SGLangHandler{}
	pod := testPod()

	prefillBody, err := handler.PreparePrefillPayload(routingCtx, pod)
	require.NoError(t, err)

	// After success, ReqBody should be the decode body (with bootstrap fields).
	assert.NotEqual(t, originalBody, routingCtx.ReqBody, "ReqBody should be updated to decode body")
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(routingCtx.ReqBody, "bootstrap_host").String())

	// Prefill body should have the prefill control fields.
	assert.Equal(t, int64(1), gjson.GetBytes(prefillBody, "max_tokens").Int())
	assert.False(t, gjson.GetBytes(prefillBody, "stream").Bool())
}

func TestPrepareSGLangRequestBodies_SHA256Stability_100Iterations(t *testing.T) {
	original := []byte(toolRequestBody)

	var firstPrefillHash, firstDecodeHash string

	for i := 0; i < 100; i++ {
		prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
		require.NoError(t, err)

		// Hash only the non-bootstrap, non-control fields to check stability.
		// We compare the messages + tools bytes, which must be invariant.
		prefillPrompt := gjson.GetBytes(prefillBody, "messages").Raw + gjson.GetBytes(prefillBody, "tools").Raw
		decodePrompt := gjson.GetBytes(decodeBody, "messages").Raw + gjson.GetBytes(decodeBody, "tools").Raw

		prefillHash := sha256.Sum256([]byte(prefillPrompt))
		decodeHash := sha256.Sum256([]byte(decodePrompt))
		prefillHex := hex.EncodeToString(prefillHash[:])
		decodeHex := hex.EncodeToString(decodeHash[:])

		if i == 0 {
			firstPrefillHash = prefillHex
			firstDecodeHash = decodeHex
		} else {
			assert.Equal(t, firstPrefillHash, prefillHex, "prefill prompt hash must be stable across iterations")
			assert.Equal(t, firstDecodeHash, decodeHex, "decode prompt hash must be stable across iterations")
		}
	}
}

func TestSGLangHandlerImplementsRawPreparer(t *testing.T) {
	// Verify SGLangHandler satisfies the RawPrefillPayloadPreparer interface.
	handler := &SGLangHandler{}
	var _ RawPrefillPayloadPreparer = handler
	assert.NotNil(t, handler)
}

// Verify that non-SGLang handlers (DefaultHandler, VLLMHandler, TRTLLMHandler)
// do NOT implement RawPrefillPayloadPreparer, ensuring they keep using the
// unmarshal/marshal path.
func TestOtherHandlersDoNotImplementRawPreparer(t *testing.T) {
	handlers := []EngineHandler{
		&DefaultHandler{},
		&VLLMHandler{},
		&TRTLLMHandler{},
	}
	for _, h := range handlers {
		_, ok := h.(RawPrefillPayloadPreparer)
		assert.False(t, ok, "%s should not implement RawPrefillPayloadPreparer", h.Name())
	}
}

// countTopLevelKeyOccurrences counts how many times a key appears at the top
// level of a JSON object by iterating the root object once with gjson.ForEach.
// This avoids relying on sjson.DeleteBytes (which shares the same per-key
// semantics as the code under test) and provides an independent oracle.
func countTopLevelKeyOccurrences(body []byte, key string) int {
	count := 0
	gjson.ParseBytes(body).ForEach(func(k, _ gjson.Result) bool {
		if k.String() == key {
			count++
		}
		return true
	})
	return count
}

func TestValidateSGLangRequest_DuplicateBootstrapKeys(t *testing.T) {
	// Input with duplicate bootstrap keys — must be rejected, not silently merged.
	original := []byte(`{"model":"m","messages":[],"bootstrap_host":"client-a","bootstrap_host":"client-b","bootstrap_port":1111,"bootstrap_port":2222,"bootstrap_room":3333,"bootstrap_room":4444}`)
	err := ValidateSGLangRequest(original)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	var invalidReqErr *InvalidRequestError
	assert.ErrorAs(t, err, &invalidReqErr, "must be *InvalidRequestError")
}

func TestValidateSGLangRequest_DuplicatePrefillControlKeys(t *testing.T) {
	// Duplicate stream, max_tokens, stream_options, min_tokens in the input — must be rejected.
	original := []byte(`{"model":"m","messages":[],"max_tokens":256,"max_tokens":512,"stream":true,"stream":false,"stream_options":{"include_usage":true},"stream_options":{"include_usage":false},"min_tokens":5,"min_tokens":10}`)
	err := ValidateSGLangRequest(original)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	var invalidReqErr *InvalidRequestError
	assert.ErrorAs(t, err, &invalidReqErr)
}

func TestValidateSGLangRequest_DuplicateBootstrapKeysDecodeOnly(t *testing.T) {
	// Even a single duplicated bootstrap field must be rejected.
	original := []byte(`{"model":"m","messages":[],"bootstrap_host":"evil","bootstrap_host":"evil2"}`)
	err := ValidateSGLangRequest(original)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")

	var invalidReqErr *InvalidRequestError
	assert.ErrorAs(t, err, &invalidReqErr)
}

func TestValidateSGLangRequest_DuplicateNonControlledKeyAllowed(t *testing.T) {
	// Duplicate keys that are NOT controlled by the gateway should not cause rejection.
	original := []byte(`{"model":"m","messages":[],"extra":"a","extra":"b"}`)
	err := ValidateSGLangRequest(original)
	require.NoError(t, err)
}

func TestValidateSGLangRequest_ValidBody(t *testing.T) {
	err := ValidateSGLangRequest([]byte(toolRequestBody))
	require.NoError(t, err)
}

func TestValidateSGLangRequest_InvalidJSON(t *testing.T) {
	err := ValidateSGLangRequest([]byte(`{not json`))
	require.Error(t, err)

	var invalidReqErr *InvalidRequestError
	assert.ErrorAs(t, err, &invalidReqErr)
}

func TestValidateSGLangRequest_NonObjectJSON(t *testing.T) {
	err := ValidateSGLangRequest([]byte(`[1,2,3]`))
	require.Error(t, err)

	var invalidReqErr *InvalidRequestError
	assert.ErrorAs(t, err, &invalidReqErr)
}

func TestPrepareSGLangRequestBodies_DuplicateNonControlledKeyAllowed(t *testing.T) {
	// Duplicate keys that are NOT controlled by the gateway should not cause rejection.
	// (This is valid JSON per RFC 8259, though unusual.)
	original := []byte(`{"model":"m","messages":[],"extra":"a","extra":"b"}`)
	prefillBody, decodeBody, err := prepareSGLangRequestBodies(original, "10.0.0.1", 8998, 12345)
	require.NoError(t, err)

	// Bootstrap fields must still be set correctly.
	assert.Equal(t, "10.0.0.1", gjson.GetBytes(decodeBody, "bootstrap_host").String())
	assert.Equal(t, int64(8998), gjson.GetBytes(prefillBody, "bootstrap_port").Int())
	// Controlled fields should each appear exactly once.
	assert.Equal(t, 1, countTopLevelKeyOccurrences(prefillBody, "max_tokens"))
	assert.Equal(t, 1, countTopLevelKeyOccurrences(prefillBody, "stream"))
	assert.Equal(t, 0, countTopLevelKeyOccurrences(prefillBody, "stream_options"))
}
