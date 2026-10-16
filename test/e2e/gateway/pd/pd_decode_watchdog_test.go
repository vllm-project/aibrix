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

// End-to-end coverage for the PD decode watchdog: when the SGLang prefill leg
// succeeds but the decode pod never starts answering, or starts and then goes
// silent, the gateway must fail the request instead of leaving the client
// waiting, and must abort the decode leg.

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	// decodeWatchdogConfigProfile is the model.aibrix.ai/config profile declared
	// in development/app/config/mock/sglang-pd-config.yaml that sets 3s decode
	// watchdog timeouts for the llama2-7b-sglang PD model.
	decodeWatchdogConfigProfile = "decode-watchdog"

	// Mock fault-injection header that holds a streaming response after its
	// first chunk.
	mockStreamStallHeader = "x-aibrix-mock-stream-stall-ms"

	// Mock fault-injection header that sends a streaming response's headers and
	// then holds it before its first chunk.
	mockFirstTokenStallHeader = "x-aibrix-mock-first-token-stall-ms"

	// Marker header the gateway sets on the client response it generates when the
	// decode watchdog fires.
	decodeWatchdogHeader = "x-error-pd-decode"
)

// TestPDDecodeWatchdogFirstResponse holds the decode leg of a streaming SGLang
// request well past the profile's 3s first-response timeout while the prefill
// leg answers at once. The client must get the gateway's 504 before the decode
// leg would have answered, and the decode leg must be aborted.
//
// Requirements to run: the standard PD e2e environment (`make dev-*`, mock
// engines from development/app) with a gateway built from this branch. The
// short timeout comes from the decode-watchdog config profile, so no gateway
// configuration change is needed.
func TestPDDecodeWatchdogFirstResponse(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-decode-watchdog")

	start := time.Now()
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"hold the SGLang decode leg"}],
			"max_tokens":8,
			"stream":true
		}`),
		http.Header{
			"config-profile":    []string{decodeWatchdogConfigProfile},
			mockDelayHeader:     []string{strconv.Itoa(int(decodeHoldDuration.Milliseconds()))},
			mockDelayRoleHeader: []string{"decode"},
		},
	)
	elapsed := time.Since(start)
	require.Error(t, err, "the gateway must fail the request")

	require.Equal(t, http.StatusGatewayTimeout, result.StatusCode)
	require.Equal(t, "true", result.Headers.Get(decodeWatchdogHeader),
		"the gateway must mark the response it generated for a decode watchdog kill")
	var response map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &response))
	errorBody := requireNestedMap(t, response, "error")
	message, ok := errorBody["message"].(string)
	require.True(t, ok, "error message must be a string, got %T", errorBody["message"])
	require.Contains(t, message, "did not start responding within")
	require.Less(t, elapsed, decodeHoldDuration,
		"the client was answered only after the decode leg did, so the watchdog did not fire")

	gatewayRequestID := requireGatewayRequestID(t, result)
	k8sClient := initializeKubernetesClient(t)
	requireDecodeAbort(t, k8sClient, modelRolePods(t, k8sClient, modelNameSGLang, "decode"), gatewayRequestID)
}

// TestPDDecodeWatchdogFirstResponseAfterHeaders is the shape a real SGLang
// decode pod shows when it wedges before its first token: a streaming
// response's headers go out as soon as the request is accepted, and then no
// token follows. The headers must not satisfy the first-response watchdog. As
// they already reached the client, the gateway cannot answer with its 504: it
// must cut the stream before the decode leg would have produced a token, and
// must abort the decode leg.
//
// Requirements to run: same as TestPDDecodeWatchdogFirstResponse.
func TestPDDecodeWatchdogFirstResponseAfterHeaders(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-decode-first-token")

	start := time.Now()
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"hold the SGLang first token"}],
			"max_tokens":8,
			"stream":true
		}`),
		http.Header{
			"config-profile":          []string{decodeWatchdogConfigProfile},
			mockFirstTokenStallHeader: []string{strconv.Itoa(int(decodeHoldDuration.Milliseconds()))},
			mockDelayRoleHeader:       []string{"decode"},
		},
	)
	elapsed := time.Since(start)

	// The decode pod's headers reached the client; no token ever does.
	require.Error(t, err, "the stream must be cut short, not completed")
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.NotContains(t, string(result.Body), "data:",
		"a token reached the client, so the decode leg was not held before its first one")
	require.Less(t, elapsed, decodeHoldDuration,
		"the stream ended only after the decode leg resumed, so the watchdog did not fire")

	gatewayRequestID := requireGatewayRequestID(t, result)
	k8sClient := initializeKubernetesClient(t)
	requireDecodeAbort(t, k8sClient, modelRolePods(t, k8sClient, modelNameSGLang, "decode"), gatewayRequestID)
}

// TestPDDecodeWatchdogStreamIdle lets the decode leg of a streaming SGLang
// request send its first chunk and then stall well past the profile's 3s
// stream-idle timeout. The response has already started, so the gateway cannot
// answer with an error status: it must cut the stream short before the decode
// leg would have resumed, and must abort the decode leg.
//
// Requirements to run: same as TestPDDecodeWatchdogFirstResponse.
func TestPDDecodeWatchdogStreamIdle(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-decode-stream-idle")

	start := time.Now()
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"stall the SGLang decode stream"}],
			"max_tokens":8,
			"stream":true
		}`),
		http.Header{
			"config-profile":      []string{decodeWatchdogConfigProfile},
			mockStreamStallHeader: []string{strconv.Itoa(int(decodeHoldDuration.Milliseconds()))},
			mockDelayRoleHeader:   []string{"decode"},
		},
	)
	elapsed := time.Since(start)

	// The decode pod's headers and first chunk reached the client; the rest of
	// the stream never does.
	require.Error(t, err, "the stream must be cut short, not completed")
	require.Equal(t, http.StatusOK, result.StatusCode)
	require.NotContains(t, string(result.Body), "[DONE]",
		"the stream completed, so the watchdog did not fire")
	require.Less(t, elapsed, decodeHoldDuration,
		"the stream ended only after the decode leg resumed, so the watchdog did not fire")

	gatewayRequestID := requireGatewayRequestID(t, result)
	k8sClient := initializeKubernetesClient(t)
	requireDecodeAbort(t, k8sClient, modelRolePods(t, k8sClient, modelNameSGLang, "decode"), gatewayRequestID)
}
