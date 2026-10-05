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

// End-to-end coverage for the PD prefill fail-fast path: when the asynchronous
// SGLang prefill leg fails, the gateway must answer the client immediately
// instead of waiting for the decode leg to time out, and must abort the decode
// leg so the engine stops waiting for a KV transfer that will never arrive.
//
// Both tests need the mock PD deployment of the e2e environment (see
// development/app) and a gateway built from this branch. The second test in
// addition needs a gateway deployed with a short AIBRIX_PREFILL_REQUEST_TIMEOUT;
// it skips itself otherwise. See the comment on each test.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
)

const (
	// Mock fault-injection headers. The delay applies to whichever leg receives
	// the request, so the role header is what keeps the prefill leg fast while
	// the decode leg hangs.
	mockDelayHeader     = "x-aibrix-mock-delay-ms"
	mockDelayRoleHeader = "x-aibrix-mock-delay-role"

	// Marker header the gateway sets on the client response it generates when a
	// PD prefill leg fails.
	prefillFailFastHeader = "x-error-pd-prefill"

	// How long the decode leg is held before it would answer on its own. The
	// client must be answered well before this elapses.
	decodeHoldDuration = 10 * time.Second

	// Env var that tells this suite the gateway under test runs with a short
	// AIBRIX_PREFILL_REQUEST_TIMEOUT, which is what the timeout test needs.
	prefillTimeoutEnv = "AIBRIX_E2E_PREFILL_REQUEST_TIMEOUT_SECONDS"
)

// findDecodeAbort looks for the /abort_request the gateway sends to the decode
// pod. The abort is matched by rid, not by the gateway request id, so the
// record is found by the rid prefix the gateway mints from the request id.
func findDecodeAbort(
	t *testing.T,
	client kubernetes.Interface,
	podNames []string,
	ridPrefix string,
) (MockRequestRecord, bool) {
	t.Helper()
	for _, podName := range podNames {
		records, err := queryAllMockRequests(context.Background(), client, e2eConfig.Namespace, podName)
		require.NoError(t, err)
		for _, record := range records {
			if record.Path == "/abort_request" && strings.HasPrefix(record.RequestID, ridPrefix) {
				return record, true
			}
		}
	}
	return MockRequestRecord{}, false
}

// requireDecodeAbort waits for the abort to land on one of the decode pods. The
// gateway sends it twice (the retry covers an engine that has not registered
// the request yet), so any one of them satisfies this.
func requireDecodeAbort(
	t *testing.T,
	client kubernetes.Interface,
	podNames []string,
	gatewayRequestID string,
) MockRequestRecord {
	t.Helper()
	ridPrefix := gatewayRequestID + "-"
	var abort MockRequestRecord
	require.Eventually(t, func() bool {
		record, found := findDecodeAbort(t, client, podNames, ridPrefix)
		if found {
			abort = record
		}
		return found
	}, 20*time.Second, 500*time.Millisecond,
		"the decode leg of %q was never aborted", gatewayRequestID)

	require.Equal(t, http.StatusOK, abort.StatusCode)
	var payload struct {
		RID string `json:"rid"`
	}
	require.NoError(t, json.Unmarshal(abort.ParsedJSON, &payload), "abort payload must be JSON")
	require.Equal(t, abort.RequestID, payload.RID)
	require.Greater(t, len(payload.RID), len(ridPrefix), "the abort must carry a non-empty rid")
	return abort
}

// requireFailFastResponse asserts the client response the gateway generates on a
// prefill failure: an error status, the marker header, an OpenAI-shaped error
// body and the request id the gateway logs the failure under.
func requireFailFastResponse(t *testing.T, result PDRequestResult, expectedStatus int) string {
	t.Helper()
	require.Equal(t, expectedStatus, result.StatusCode)
	require.Equal(t, "true", result.Headers.Get(prefillFailFastHeader),
		"the gateway must mark the response it generated for a prefill failure")

	var response map[string]any
	require.NoError(t, json.Unmarshal(result.Body, &response))
	errorBody := requireNestedMap(t, response, "error")
	message, ok := errorBody["message"].(string)
	require.True(t, ok, "error message must be a string, got %T", errorBody["message"])
	require.Contains(t, message, "prefill request failed")
	return requireGatewayRequestID(t, result)
}

// TestPDPrefillFailFastRejectedPrefill covers the terminal http_status failure
// class: the prefill engine answers 500 straight away while the decode leg is
// still being held. The client must not wait for the decode leg, and the decode
// leg must be aborted.
//
// Requirements to run: the standard PD e2e environment (`make dev-*`, mock
// engines from development/app) with a gateway built from this branch. No
// gateway configuration change is needed - the prefill leg fails on its own,
// rather than by hitting AIBRIX_PREFILL_REQUEST_TIMEOUT.
func TestPDPrefillFailFastRejectedPrefill(t *testing.T) {
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-prefill-fail-fast")

	start := time.Now()
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, []byte(`{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"trigger SGLang prefill fail fast"}],
			"max_tokens":8,
			"stream":false
		}`),
		http.Header{
			mockPrefillFailureHeader: []string{"prefill"},
			mockDelayHeader:          []string{strconv.Itoa(int(decodeHoldDuration.Milliseconds()))},
			mockDelayRoleHeader:      []string{"decode"},
		},
	)
	elapsed := time.Since(start)
	require.Error(t, err, "the gateway must fail the request")

	// A rejected prefill leg carries the engine's own status code through to the
	// client; only the classes that have no upstream status become a 503.
	gatewayRequestID := requireFailFastResponse(t, result, http.StatusInternalServerError)
	require.Less(t, elapsed, decodeHoldDuration,
		"the client was answered only after the decode leg gave up, so the gateway did not fail fast")

	k8sClient := initializeKubernetesClient(t)
	decodePods := modelRolePods(t, k8sClient, modelNameSGLang, "decode")
	abort := requireDecodeAbort(t, k8sClient, decodePods, gatewayRequestID)

	// The rid the decode leg was started with must be the rid that was aborted:
	// that is the whole point of the gateway minting it and injecting it into
	// both legs. The decode leg record only exists if the proxy forwarded the
	// request before the gateway closed the stream, which is a race, so this is
	// asserted only when the record is there.
	for _, podName := range decodePods {
		records, err := queryMockRequests(
			context.Background(), k8sClient, e2eConfig.Namespace, podName, gatewayRequestID,
		)
		require.NoError(t, err)
		for _, record := range records {
			if record.Role != "decode" || len(record.ParsedJSON) == 0 {
				continue
			}
			var body struct {
				RID string `json:"rid"`
			}
			require.NoError(t, json.Unmarshal(record.ParsedJSON, &body))
			require.Equal(t, abort.RequestID, body.RID,
				"the aborted rid must be the rid the decode leg carries")
		}
	}
}

// TestPDPrefillFailFastPrefillTimeout covers the terminal timeout failure class:
// the prefill leg is held past AIBRIX_PREFILL_REQUEST_TIMEOUT, so the gateway
// gives up on it with no upstream status and answers 503.
//
// Requirements to run: everything the test above needs, plus a gateway deployed
// with AIBRIX_PREFILL_REQUEST_TIMEOUT set to at most 20 seconds (the default in
// config/gateway/gateway-plugin/gateway-plugin.yaml is 60, which is beyond both
// the mock's 30s injected-delay cap and this suite's 30s client timeout). Set
// AIBRIX_E2E_PREFILL_REQUEST_TIMEOUT_SECONDS to the value the gateway runs with
// to enable the test; without it the test skips, since it cannot tell how long
// the prefill leg has to hang.
func TestPDPrefillFailFastPrefillTimeout(t *testing.T) {
	raw := os.Getenv(prefillTimeoutEnv)
	if raw == "" {
		t.Skipf("%s is unset: deploy the gateway with a short AIBRIX_PREFILL_REQUEST_TIMEOUT "+
			"and set %s to that value to run this test", prefillTimeoutEnv, prefillTimeoutEnv)
	}
	prefillTimeoutSeconds, err := strconv.Atoi(raw)
	require.NoError(t, err, "%s must be an integer number of seconds", prefillTimeoutEnv)
	require.Greater(t, prefillTimeoutSeconds, 0, "%s must be positive", prefillTimeoutEnv)
	require.LessOrEqual(t, prefillTimeoutSeconds, 20,
		"%s must leave room for the injected delay (mock cap 30s) and the client timeout (30s)",
		prefillTimeoutEnv)

	prefillHold := time.Duration(prefillTimeoutSeconds)*time.Second + 5*time.Second
	waitForPDDisaggregationRouting(t, modelNameSGLang)
	requestID := newRequestID("sglang-prefill-timeout-fail-fast")

	start := time.Now()
	result, err := sendPDRequestWithHeaders(
		context.Background(), e2eConfig, "pd", requestID, fmt.Appendf(nil, `{
			"model":"llama2-7b-sglang",
			"messages":[{"role":"user","content":"hold the SGLang prefill leg for %dms"}],
			"max_tokens":8,
			"stream":false
		}`, prefillHold.Milliseconds()),
		http.Header{
			mockDelayHeader:     []string{strconv.Itoa(int(prefillHold.Milliseconds()))},
			mockDelayRoleHeader: []string{"prefill"},
		},
	)
	elapsed := time.Since(start)
	require.Error(t, err, "the gateway must fail the request")

	// A timed-out prefill leg never produced a status code, so the gateway
	// reports the decode leg as unavailable rather than inventing an upstream
	// status.
	gatewayRequestID := requireFailFastResponse(t, result, http.StatusServiceUnavailable)
	require.Less(t, elapsed, prefillHold,
		"the client was answered only after the prefill leg returned, so the gateway did not fail fast")

	k8sClient := initializeKubernetesClient(t)
	requireDecodeAbort(t, k8sClient, modelRolePods(t, k8sClient, modelNameSGLang, "decode"), gatewayRequestID)
}
