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
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	framework "github.com/vllm-project/aibrix/test/e2e/framework"
)

const (
	backendRecorderTimeout = 5 * time.Second
	pdFaultLabel           = "e2e.aibrix.ai/backend-fault"
	pdFaultLabelValue      = "connection"
)

type gatewayResponse struct {
	status int
	header http.Header
	body   []byte
}

func newRecorderRequestID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// postPDChat sends a request with a caller-controlled trace ID. The gateway
// uses that trace ID as the internal request ID, which is also what the mock
// recorder receives. This keeps every assertion isolated from other traffic.
func postPDChat(
	t *testing.T,
	ctx context.Context,
	requestID string,
	extraHeaders map[string]string,
) gatewayResponse {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model": modelNameVLLM,
		"messages": []map[string]string{{
			"role": "user", "content": "gateway backend recorder e2e",
		}},
		"max_tokens": 9,
		"stream":     false,
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		gatewayURL+"/v1/chat/completions",
		bytes.NewReader(payload),
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("routing-strategy", "pd")
	req.Header.Set("traceparent", "00-"+requestID+"-0000000000000001-01")
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return gatewayResponse{status: resp.StatusCode, header: resp.Header, body: body}
}

func headerValue(headers map[string]string, key string) string {
	for name, value := range headers {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func decodedRecordBody(t *testing.T, record framework.MockRequestRecord) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(record.RawBodyBase64)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

func modelPods(t *testing.T, ctx context.Context, client *kubernetes.Clientset) []corev1.Pod {
	t.Helper()
	pods, err := client.CoreV1().Pods(e2eConfig.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "model.aibrix.ai/name=" + modelNameVLLM,
	})
	require.NoError(t, err)
	return pods.Items
}

func TestPDBackendRequestsMatchGatewayTransformationsAndTargets(t *testing.T) {
	ctx := context.Background()
	waitForPDDisaggregationRouting(t, modelNameVLLM)
	client, _ := initializeClient(ctx, t)
	requestID := newRecorderRequestID()

	response := postPDChat(t, ctx, requestID, nil)
	require.Equal(t, http.StatusOK, response.status, "body=%s", response.body)
	prefillPod := response.header.Get("prefill-target-pod")
	decodePod := response.header.Get("target-pod")
	require.NotEmpty(t, prefillPod)
	require.NotEmpty(t, decodePod)
	require.NotEqual(t, prefillPod, decodePod)

	prefillRecords, err := framework.WaitForMockRequestCount(
		ctx, client, e2eConfig.Namespace, prefillPod, requestID, 1, backendRecorderTimeout,
	)
	require.NoError(t, err)
	decodeRecords, err := framework.WaitForMockRequestCount(
		ctx, client, e2eConfig.Namespace, decodePod, requestID, 1, backendRecorderTimeout,
	)
	require.NoError(t, err)

	prefill := prefillRecords[0]
	decode := decodeRecords[0]
	assert.Equal(t, prefillPod, prefill.Pod, "gateway prefill target must receive the request")
	assert.Equal(t, decodePod, decode.Pod, "gateway decode target must receive the request")
	assert.Equal(t, requestID, headerValue(prefill.Headers, "x-request-id"))
	assert.Equal(t, requestID, headerValue(decode.Headers, "x-request-id"))
	assert.Equal(t, "pd", headerValue(prefill.Headers, "routing-strategy"))
	assert.Equal(t, response.header.Get("target-pod-ip"), headerValue(decode.Headers, "target-pod"),
		"Envoy must forward the target address that the gateway reported")

	prefillBody := decodedRecordBody(t, prefill)
	decodeBody := decodedRecordBody(t, decode)
	assert.Equal(t, float64(1), prefillBody["max_tokens"], "prefill must be transformed to one token")
	assert.Equal(t, false, prefillBody["stream"])
	transfer, ok := prefillBody["kv_transfer_params"].(map[string]any)
	require.True(t, ok, "prefill transfer parameters missing: %#v", prefillBody)
	assert.Equal(t, true, transfer["do_remote_decode"])
	assert.Equal(t, float64(9), decodeBody["max_tokens"], "decode must retain the client token limit")
	transfer, ok = decodeBody["kv_transfer_params"].(map[string]any)
	require.True(t, ok, "decode transfer parameters missing: %#v", decodeBody)
	assert.Equal(t, false, transfer["do_remote_decode"])
}

func TestPDDecodeBackendTimeoutIsPropagated(t *testing.T) {
	ctx := context.Background()
	waitForPDDisaggregationRouting(t, modelNameVLLM)
	client, _ := initializeClient(ctx, t)
	requestID := newRecorderRequestID()

	response := postPDChat(t, ctx, requestID, map[string]string{
		"x-aibrix-mock-delay-ms":         "2000",
		"x-envoy-upstream-rq-timeout-ms": "1000",
	})
	require.Equal(t, http.StatusGatewayTimeout, response.status, "body=%s", response.body)
	decodePod := response.header.Get("target-pod")
	require.NotEmpty(t, decodePod)

	records, err := framework.WaitForMockRequestCount(
		ctx, client, e2eConfig.Namespace, decodePod, requestID, 1, 3*time.Second,
	)
	require.NoError(t, err)
	assert.Equal(t, 2000, records[0].DelayMS)
	assert.Equal(t, "success", records[0].Outcome,
		"the decode backend finishes after Envoy's one-second route timeout")
}

func TestPDPrefillRequestConnectionFailureIsPropagatedBeforeEnvoyRouting(t *testing.T) {
	ctx := context.Background()
	waitForPDDisaggregationRouting(t, modelNameVLLM)
	client, _ := initializeClient(ctx, t)
	prefillPod, decodePod := selectPDRoleSetPods(t, ctx, client)

	framework.UpdatePodLabels(
		t, ctx, client, e2eConfig.Namespace, prefillPod.Name,
		map[string]string{pdFaultLabel: pdFaultLabelValue},
	)
	framework.UpdatePodLabels(
		t, ctx, client, e2eConfig.Namespace, decodePod.Name,
		map[string]string{pdFaultLabel: pdFaultLabelValue},
	)

	// First wait until the gateway sees the selector while the selected prefill
	// pod is still healthy. This avoids mistaking a stale pod cache for a
	// connection failure after its port is changed below.
	require.Eventually(t, func() bool {
		response := postPDChat(t, ctx, newRecorderRequestID(), map[string]string{
			"external-filter": pdFaultLabel + "=" + pdFaultLabelValue,
		})
		return response.status == http.StatusOK &&
			response.header.Get("prefill-target-pod") == prefillPod.Name &&
			response.header.Get("target-pod") == decodePod.Name
	}, 30*time.Second, 100*time.Millisecond, "gateway did not observe the selected PD role set")

	framework.UpdatePodLabels(
		t, ctx, client, e2eConfig.Namespace, prefillPod.Name,
		map[string]string{"model.aibrix.ai/port": "1"},
	)
	var response gatewayResponse
	var requestID string
	require.Eventually(t, func() bool {
		candidateID := newRecorderRequestID()
		candidate := postPDChat(t, ctx, candidateID, map[string]string{
			"external-filter": pdFaultLabel + "=" + pdFaultLabelValue,
		})
		if candidate.status != http.StatusServiceUnavailable ||
			candidate.header.Get("prefill-target-pod") != prefillPod.Name ||
			!strings.Contains(string(candidate.body), "error on selecting target pod") {
			return false
		}
		response, requestID = candidate, candidateID
		return true
	}, 30*time.Second, 100*time.Millisecond, "gateway did not propagate the prefill connection failure")

	assert.Equal(t, prefillPod.Name, response.header.Get("prefill-target-pod"))
	assert.Empty(t, response.header.Get("target-pod"),
		"the decode target must not be exposed when the prefill request fails before Envoy routing")
	records, err := framework.QueryMockRequests(ctx, client, e2eConfig.Namespace, prefillPod.Name, requestID)
	require.NoError(t, err)
	assert.Empty(t, records, "a refused prefill connection must not reach the backend recorder")
}

func selectPDRoleSetPods(t *testing.T, ctx context.Context, client *kubernetes.Clientset) (corev1.Pod, corev1.Pod) {
	t.Helper()
	type pair struct {
		prefill *corev1.Pod
		decode  *corev1.Pod
	}
	pairs := make(map[string]*pair)
	for _, pod := range modelPods(t, ctx, client) {
		roleSet := pod.Labels["roleset-name"]
		if roleSet == "" {
			continue
		}
		if pairs[roleSet] == nil {
			pairs[roleSet] = &pair{}
		}
		switch pod.Labels["role-name"] {
		case "prefill":
			podCopy := pod
			pairs[roleSet].prefill = &podCopy
		case "decode":
			podCopy := pod
			pairs[roleSet].decode = &podCopy
		}
	}
	for roleSet, pair := range pairs {
		if pair.prefill != nil && pair.decode != nil {
			t.Logf("using PD roleset %s: prefill=%s decode=%s", roleSet, pair.prefill.Name, pair.decode.Name)
			return *pair.prefill, *pair.decode
		}
	}
	t.Fatalf("no complete PD roleset found for model %s", modelNameVLLM)
	return corev1.Pod{}, corev1.Pod{}
}
