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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const vllmSHFSOpaqueSentinel = "aibrix-pd-contract-opaque-sentinel"

const modelNameVLLMNIXL = "llama2-7b-vllm-nixl"

func decodeMockRequestBody(t *testing.T, record MockRequestRecord) map[string]any {
	t.Helper()
	rawBody, err := base64.StdEncoding.DecodeString(record.RawBodyBase64)
	require.NoError(t, err, "decode recorder body for %s request", record.Role)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &body), "parse recorder body for %s request", record.Role)
	return body
}

func requireNestedMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key]
	require.True(t, ok, "missing %q", key)
	nested, ok := value.(map[string]any)
	require.True(t, ok, "%q must be an object, got %T", key, value)
	return nested
}

func requireNestedSlice(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	value, ok := parent[key]
	require.True(t, ok, "missing %q", key)
	nested, ok := value.([]any)
	require.True(t, ok, "%q must be an array, got %T", key, value)
	require.NotEmpty(t, nested, "%q must be non-empty", key)
	return nested
}

func requireSuccessfulCompletion(t *testing.T, result PDRequestResult, modelName string) {
	t.Helper()
	require.Equal(t, http.StatusOK, result.StatusCode)

	var completion map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &completion), "decode OpenAI completion")
	require.Equal(t, modelName, completion["model"])
	choices := requireNestedSlice(t, completion, "choices")
	choice, ok := choices[0].(map[string]any)
	require.True(t, ok, "completion choice must be an object, got %T", choices[0])
	message := requireNestedMap(t, choice, "message")
	content, ok := message["content"].(string)
	require.True(t, ok, "completion message content must be a string, got %T", message["content"])
	require.NotEmpty(t, content, "completion message content must be non-empty")
}

func TestDecodeMockRequestBody(t *testing.T) {
	record := MockRequestRecord{RawBodyBase64: "eyJtb2RlbCI6ImxsYW1hIiwia2V5cyI6WzFdfQ=="}

	body := decodeMockRequestBody(t, record)

	require.Equal(t, "llama", body["model"])
	require.Equal(t, []any{float64(1)}, requireNestedSlice(t, body, "keys"))
}

func TestRequireSuccessfulCompletion(t *testing.T) {
	result := PDRequestResult{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"model":"llama2-7b-vllm","choices":[{"message":{"content":"hello"}}]}`),
	}

	requireSuccessfulCompletion(t, result, modelNameVLLM)
}

func TestPDContractVLLMSHFS(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameVLLM)
	requestID := newRequestID("vllm-shfs")
	body := []byte(`{"model":"llama2-7b-vllm","messages":[{"role":"user","content":"Say this is a test for vLLM SHFS PD contract"}],"max_tokens":8,"stream":false}`)

	result, err := sendPDRequest(context.Background(), e2eConfig, "pd", requestID, body)
	require.NoError(t, err)
	requireSuccessfulCompletion(t, result, modelNameVLLM)
	k8sClient, _ := initializeClient(context.Background(), t)

	prefillPod := result.Headers.Get("prefill-target-pod")
	decodePod := result.Headers.Get("target-pod")
	require.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")
	require.NotEmpty(t, decodePod, "target-pod header must be set")
	require.NotEqual(t, prefillPod, decodePod, "prefill and decode pods must differ")

	prefill, decode := waitForSuccessfulPDLegs(t, k8sClient, e2eConfig.Namespace,
		prefillPod, decodePod, requestID, "vllm-aibrix-shfs", "vllm")
	require.Equal(t, "/v1/chat/completions", prefill.Path)
	require.Equal(t, "/v1/chat/completions", decode.Path)
	require.Equal(t, "prefill", prefill.Role)
	require.Equal(t, "decode", decode.Role)
	require.Equal(t, "vllm", prefill.Engine)
	require.Equal(t, "vllm", decode.Engine)
	require.NotEqual(t, prefill.Pod, decode.Pod)
	require.Equal(t, prefillPod, prefill.Pod)
	require.Equal(t, decodePod, decode.Pod)

	prefillBody := decodeMockRequestBody(t, prefill)
	prefillTransfer := requireNestedMap(t, prefillBody, "kv_transfer_params")
	require.Equal(t, true, prefillTransfer["do_remote_decode"])

	decodeBody := decodeMockRequestBody(t, decode)
	decodeTransfer := requireNestedMap(t, decodeBody, "kv_transfer_params")
	require.Equal(t, false, decodeTransfer["do_remote_decode"])
	require.Equal(t, true, decodeTransfer["do_remote_prefill"])
	require.NotEmpty(t, decodeTransfer["remote_engine_id"])
	require.NotEmpty(t, requireNestedSlice(t, decodeTransfer, "remote_block_ids"))
	require.NotEmpty(t, decodeTransfer["remote_host"])
	remotePort, ok := decodeTransfer["remote_port"].(float64)
	require.True(t, ok, "remote_port must be a JSON number, got %T", decodeTransfer["remote_port"])
	require.Positive(t, remotePort)
	require.Equal(t, vllmSHFSOpaqueSentinel, decodeTransfer["opaque"])
}

func TestPDContractVLLMNIXL(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameVLLMNIXL)
	requestID := newRequestID("vllm-nixl")
	body := []byte(`{"model":"llama2-7b-vllm-nixl","messages":[{"role":"user","content":"Say this is a test for vLLM NIXL PD contract","metadata":{"marker":"nixl-nested-marker"}}],"max_tokens":8,"stream":false}`)

	result, err := sendPDRequest(context.Background(), e2eConfig, "pd", requestID, body)
	require.NoError(t, err)
	requireSuccessfulCompletion(t, result, modelNameVLLMNIXL)
	k8sClient, _ := initializeClient(context.Background(), t)

	prefillPod := result.Headers.Get("prefill-target-pod")
	decodePod := result.Headers.Get("target-pod")
	require.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")
	require.NotEmpty(t, decodePod, "target-pod header must be set")
	require.NotEqual(t, prefillPod, decodePod, "prefill and decode pods must differ")

	prefill, decode := waitForSuccessfulPDLegs(t, k8sClient, e2eConfig.Namespace,
		prefillPod, decodePod, requestID, "vllm-aibrix-nixl", "vllm")
	for _, record := range []MockRequestRecord{prefill, decode} {
		require.Equal(t, "/v1/chat/completions", record.Path)
		require.Equal(t, "vllm", record.Engine)
		require.Equal(t, "success", record.Outcome)
	}
	require.Equal(t, "prefill", prefill.Role)
	require.Equal(t, "decode", decode.Role)
	require.Equal(t, prefillPod, prefill.Pod)
	require.Equal(t, decodePod, decode.Pod)
	require.NotEqual(t, prefill.Pod, decode.Pod)

	prefillBody := decodeMockRequestBody(t, prefill)
	var originalBody map[string]any
	require.NoError(t, json.Unmarshal(body, &originalBody))
	require.NotContains(t, prefillBody, "kv_transfer_params")
	require.Equal(t, modelNameVLLMNIXL, prefillBody["model"])
	require.Equal(t, false, prefillBody["stream"])
	require.Equal(t, originalBody["messages"], prefillBody["messages"])

	decodeBody := decodeMockRequestBody(t, decode)
	require.Equal(t, modelNameVLLMNIXL, decodeBody["model"])
	require.Equal(t, false, decodeBody["stream"])
	require.Equal(t, float64(8), decodeBody["max_tokens"])
	require.Equal(t, vllmSHFSOpaqueSentinel, requireNestedMap(t, decodeBody, "disagg_prefill_resp")["opaque"])
	require.Equal(t, originalBody["messages"], decodeBody["messages"])
}

func TestPDContractSGLangRawJSON(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-raw-json")
	body := []byte(`{"model":"llama2-7b-sglang","messages":[{"role":"user","content":"Use the lookup tool","metadata":{"nested":{"marker":"sglang-nested-marker","values":[1,{"name":"value"}]}}}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}],"max_tokens":8,"stream":false}`)

	result, err := sendPDRequest(context.Background(), e2eConfig, "pd", requestID, body)
	require.NoError(t, err)
	requireSuccessfulCompletion(t, result, modelNameSGLang)
	k8sClient, _ := initializeClient(context.Background(), t)

	prefillPod := result.Headers.Get("prefill-target-pod")
	decodePod := result.Headers.Get("target-pod")
	require.NotEmpty(t, prefillPod, "prefill-target-pod header must be set")
	require.NotEmpty(t, decodePod, "target-pod header must be set")
	require.NotEqual(t, prefillPod, decodePod, "prefill and decode pods must differ")

	prefill, decode := waitForSuccessfulPDLegs(t, k8sClient, e2eConfig.Namespace,
		prefillPod, decodePod, requestID, "sglang-http", "sglang")
	for _, record := range []MockRequestRecord{prefill, decode} {
		require.Equal(t, "/v1/chat/completions", record.Path)
		require.Equal(t, "sglang", record.Engine)
		require.Equal(t, "success", record.Outcome)
	}
	require.Equal(t, "prefill", prefill.Role)
	require.Equal(t, "decode", decode.Role)
	require.Equal(t, prefillPod, prefill.Pod)
	require.Equal(t, decodePod, decode.Pod)
	require.NotEqual(t, prefill.Pod, decode.Pod)

	originalMessages := gjson.GetBytes(body, "messages").Raw
	originalTools := gjson.GetBytes(body, "tools").Raw
	for _, record := range []MockRequestRecord{prefill, decode} {
		rawBody, err := base64.StdEncoding.DecodeString(record.RawBodyBase64)
		require.NoError(t, err, "decode recorder body for %s request", record.Role)
		require.True(t, bytes.Contains(rawBody, []byte(originalMessages)), "messages changed in %s body", record.Role)
		require.True(t, bytes.Contains(rawBody, []byte(originalTools)), "tools changed in %s body", record.Role)
	}

	prefillBody := decodeMockRequestBody(t, prefill)
	decodeBody := decodeMockRequestBody(t, decode)
	prefillRoom := requireBootstrapFields(t, prefillBody)
	decodeRoom := requireBootstrapFields(t, decodeBody)
	require.Equal(t, prefillRoom, decodeRoom, "prefill and decode bootstrap rooms must match")
}

func requireBootstrapFields(t *testing.T, body map[string]any) any {
	t.Helper()
	require.NotEmpty(t, body["bootstrap_host"])
	require.Positive(t, body["bootstrap_port"])
	require.NotEmpty(t, body["bootstrap_room"])
	return body["bootstrap_room"]
}
