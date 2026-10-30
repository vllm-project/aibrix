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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const mockPrefillFailureHeader = "x-aibrix-mock-fail"

func requirePDGatewayFailure(t *testing.T, result PDRequestResult) {
	t.Helper()
	require.GreaterOrEqual(t, result.StatusCode, http.StatusInternalServerError)

	var response map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &response))
	errorBody := requireNestedMap(t, response, "error")
	require.NotEmpty(t, errorBody["message"])
}

// requirePrefillEngineError asserts the client got the engine's own OpenAI-shaped
// error, with the message the engine wrote and no gateway envelope around it.
func requirePrefillEngineError(t *testing.T, result PDRequestResult, message string) map[string]any {
	t.Helper()
	var response map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &response))
	errorBody := requireNestedMap(t, response, "error")
	body, ok := errorBody["message"].(string)
	require.True(t, ok, "error message must be a string, got %T", errorBody["message"])
	require.Contains(t, body, message)
	return errorBody
}

func requireNoSuccessfulPDLeg(t *testing.T, records []MockRequestRecord, role string) {
	t.Helper()
	for _, record := range records {
		if record.Role == role && record.Outcome == "success" && record.StatusCode == http.StatusOK {
			t.Fatalf("unexpected successful %s leg for request %q on pod %q", role, record.RequestID, record.Pod)
		}
	}
}

func modelRolePods(t *testing.T, client kubernetes.Interface, modelName, role string) []string {
	t.Helper()
	pods, err := client.CoreV1().Pods(e2eConfig.Namespace).List(
		context.Background(),
		v1.ListOptions{LabelSelector: "role-name=" + role + ",model.aibrix.ai/name=" + modelName},
	)
	require.NoError(t, err)
	result := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		result = append(result, pod.Name)
	}
	require.NotEmpty(t, result, "expected %s pods for model %s", role, modelName)
	return result
}

func requireNoSuccessfulPDLegOnPods(
	t *testing.T,
	client kubernetes.Interface,
	podNames []string,
	requestID, role string,
) {
	t.Helper()
	for _, podName := range podNames {
		records, err := queryMockRequests(
			context.Background(), client, e2eConfig.Namespace, podName, requestID,
		)
		require.NoError(t, err)
		requireNoSuccessfulPDLeg(t, records, role)
	}
}

func runSynchronousPrefillFailure(
	t *testing.T,
	modelName, engine, requestPrefix string,
	body []byte,
) {
	t.Helper()
	waitForPDDisaggregationRouting(t, modelName)
	requestID := newRequestID(requestPrefix)
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, body,
		http.Header{mockPrefillFailureHeader: []string{"prefill"}},
	)
	require.Error(t, err)
	// The prefill engine answered, so its status and payload are the client's
	// answer: no routing-failure 503, and the marker says which leg failed.
	require.Equal(t, http.StatusInternalServerError, result.StatusCode,
		"the prefill engine's status must reach the client unchanged")
	require.Equal(t, "true", result.Headers.Get(prefillFailFastHeader),
		"the gateway must mark the engine error as a prefill failure")
	require.Empty(t, result.Headers.Get("x-error-routing"),
		"an engine answer is not a routing failure")
	errorBody := requirePrefillEngineError(t, result, "mock failure injected for role prefill")
	require.Equal(t, "api_error", errorBody["type"])
	gatewayRequestID := requireGatewayRequestID(t, result)

	k8sClient := initializeKubernetesClient(t)
	prefill := waitForPDLegOutcomeOnPods(
		t, k8sClient, e2eConfig.Namespace,
		modelRolePods(t, k8sClient, modelName, "prefill"),
		gatewayRequestID, engine, "prefill", "failed",
	)
	require.Equal(t, http.StatusInternalServerError, prefill.StatusCode)
	require.Contains(t, prefill.Error, "mock failure injected for role prefill")
	requireNoSuccessfulPDLegOnPods(
		t, k8sClient, modelRolePods(t, k8sClient, modelName, "decode"), gatewayRequestID, "decode",
	)
}

func TestPDFailureVLLMSynchronousPrefill(t *testing.T) {
	runSynchronousPrefillFailure(t, modelNameVLLM, "vllm", "vllm-prefill-failure", []byte(`{
		"model":"llama2-7b-vllm",
		"messages":[{"role":"user","content":"trigger vLLM prefill failure"}],
		"max_tokens":8,
		"stream":false
	}`))
}

func TestPDFailureTRTLLMSynchronousPrefill(t *testing.T) {
	runSynchronousPrefillFailure(t, modelNameTRTLLM, "trtllm", "trtllm-prefill-failure", []byte(`{
		"model":"llama2-7b-trtllm",
		"messages":[{"role":"user","content":"trigger TRT-LLM prefill failure"}],
		"max_tokens":8,
		"stream":false
	}`))
}

// TestPDFailureVLLMSynchronousPrefillRejected covers the engine rejecting the
// request itself: the prefill leg answers 400, and the client must see that same
// 400 and payload. No marker is added, because the request would be rejected
// the same way wherever it was routed.
func TestPDFailureVLLMSynchronousPrefillRejected(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameVLLM)
	requestID := newRequestID("vllm-prefill-rejected")
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-vllm",
			"messages":[{"role":"user","content":"trigger a vLLM prefill rejection"}],
			"max_tokens":8,
			"stream":false
		}`),
		// The mock validates its fault headers before serving the prefill request
		// and answers 400 for an unparsable one.
		http.Header{"x-aibrix-mock-delay-ms": []string{"not-a-number"}},
	)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, result.StatusCode,
		"the engine's 400 must reach the client unchanged")
	require.Empty(t, result.Headers.Get(prefillFailFastHeader),
		"a rejected request must not carry the prefill failure marker")
	require.Empty(t, result.Headers.Get("x-error-routing"),
		"a rejected request must not be reported as a routing failure")
	errorBody := requirePrefillEngineError(t, result, "invalid x-aibrix-mock-delay-ms")
	require.Equal(t, "invalid_request_error", errorBody["type"])
}

func TestPDFailureSGLangAsyncPrefillHandoff(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-prefill-failure")
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"trigger SGLang prefill failure"}],
			"max_tokens":8,
			"stream":false
		}`),
		http.Header{mockPrefillFailureHeader: []string{"prefill"}},
	)
	require.Error(t, err)
	requirePDGatewayFailure(t, result)
	gatewayRequestID := requireGatewayRequestID(t, result)

	k8sClient := initializeKubernetesClient(t)
	prefill := waitForPDLegOutcomeOnPods(
		t, k8sClient, e2eConfig.Namespace,
		modelRolePods(t, k8sClient, modelNameSGLang, "prefill"),
		gatewayRequestID, "sglang", "prefill", "failed",
	)
	decode := waitForPDLegOutcomeOnPods(
		t, k8sClient, e2eConfig.Namespace,
		modelRolePods(t, k8sClient, modelNameSGLang, "decode"),
		gatewayRequestID, "sglang", "decode", "failed",
	)
	require.Equal(t, http.StatusInternalServerError, prefill.StatusCode)
	require.Equal(t, http.StatusInternalServerError, decode.StatusCode)
	require.Contains(t, prefill.Error, "mock failure injected for role prefill")
	require.Contains(t, decode.Error, "prefill handoff")
}
