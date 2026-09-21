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
	routingRecorderTimeout = 5 * time.Second
	routingFaultLabel      = "e2e.aibrix.ai/backend-fault"
	routingFaultValue      = "connection"
	routingSentinelHeader  = "x-aibrix-e2e-sentinel"
	routingSentinelValue   = "ordinary-backend"
)

var ordinaryRequestBody = []byte(
	`{"model":"llama2-7b","messages":[{"role":"user","content":"gateway ordinary backend e2e"}],"max_tokens":9,"stream":false}`,
)

type routedGatewayResponse struct {
	status int
	header http.Header
	body   []byte
}

func newRoutingRecorderRequestID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func postOrdinaryChat(
	t *testing.T,
	ctx context.Context,
	requestID string,
	extraHeaders map[string]string,
) routedGatewayResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		gatewayURL+"/v1/chat/completions",
		bytes.NewReader(ordinaryRequestBody),
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("routing-strategy", "least-request")
	req.Header.Set("traceparent", "00-"+requestID+"-0000000000000001-01")
	req.Header.Set(routingSentinelHeader, routingSentinelValue)
	for key, value := range extraHeaders {
		req.Header.Set(key, value)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return routedGatewayResponse{status: resp.StatusCode, header: resp.Header, body: body}
}

func ordinaryRecordHeader(headers map[string]string, key string) string {
	for name, value := range headers {
		if strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func TestOrdinaryBackendReceivesGatewayBodyHeadersAndTarget(t *testing.T) {
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	requestID := newRoutingRecorderRequestID()

	response := postOrdinaryChat(t, ctx, requestID, nil)
	require.Equal(t, http.StatusOK, response.status, "body=%s", response.body)
	targetPod := response.header.Get("target-pod")
	require.NotEmpty(t, targetPod)
	require.NotEmpty(t, response.header.Get("target-pod-ip"))

	records, err := framework.WaitForMockRequestCount(
		ctx,
		client,
		e2eConfig.Namespace,
		targetPod,
		requestID,
		1,
		routingRecorderTimeout,
	)
	require.NoError(t, err)
	record := records[0]
	rawBody, err := base64.StdEncoding.DecodeString(record.RawBodyBase64)
	require.NoError(t, err)

	assert.Equal(t, targetPod, record.Pod, "the gateway-selected pod must receive the request")
	assert.Equal(t, ordinaryRequestBody, rawBody, "ordinary inference body must reach the backend byte-for-byte")
	assert.Equal(t, requestID, ordinaryRecordHeader(record.Headers, "x-request-id"))
	assert.Equal(t, "least-request", ordinaryRecordHeader(record.Headers, "routing-strategy"))
	assert.Equal(t, response.header.Get("target-pod-ip"), ordinaryRecordHeader(record.Headers, "target-pod"))
	assert.Equal(t, "true", ordinaryRecordHeader(record.Headers, "x-went-into-req-headers"))
	assert.Equal(t, routingSentinelValue, ordinaryRecordHeader(record.Headers, routingSentinelHeader))
}

func TestOrdinaryBackendRetriesFirstFailureThenSucceeds(t *testing.T) {
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	requestID := newRoutingRecorderRequestID()

	response := postOrdinaryChat(t, ctx, requestID, map[string]string{
		"x-aibrix-mock-fail":          "backend",
		"x-aibrix-mock-fail-attempts": "1",
		"x-envoy-retry-on":            "5xx",
		"x-envoy-max-retries":         "1",
	})
	require.Equal(t, http.StatusOK, response.status, "body=%s", response.body)
	targetPod := response.header.Get("target-pod")
	require.NotEmpty(t, targetPod)

	records, err := framework.WaitForMockRequestCount(
		ctx,
		client,
		e2eConfig.Namespace,
		targetPod,
		requestID,
		2,
		routingRecorderTimeout,
	)
	require.NoError(t, err)
	assert.Equal(t, []int{http.StatusInternalServerError, http.StatusOK}, []int{
		records[0].StatusCode,
		records[1].StatusCode,
	})
	assert.Equal(t, []string{"failed", "success"}, []string{
		records[0].Outcome,
		records[1].Outcome,
	})
}

func TestOrdinaryBackendErrorBodyIsPropagatedAfterRetries(t *testing.T) {
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	requestID := newRoutingRecorderRequestID()

	response := postOrdinaryChat(t, ctx, requestID, map[string]string{
		"x-aibrix-mock-fail":  "backend",
		"x-envoy-retry-on":    "5xx",
		"x-envoy-max-retries": "1",
	})
	require.Equal(t, http.StatusInternalServerError, response.status, "body=%s", response.body)
	assert.Contains(t, string(response.body), "mock failure injected for backend")
	assert.Equal(t, "true", response.header.Get("x-error-response-unknown"))
	targetPod := response.header.Get("target-pod")
	require.NotEmpty(t, targetPod)

	records, err := framework.WaitForMockRequestCount(
		ctx,
		client,
		e2eConfig.Namespace,
		targetPod,
		requestID,
		2,
		routingRecorderTimeout,
	)
	require.NoError(t, err)
	assert.Equal(t, []int{http.StatusInternalServerError, http.StatusInternalServerError}, []int{
		records[0].StatusCode,
		records[1].StatusCode,
	})
}

func TestOrdinaryBackendTimeoutIsPropagated(t *testing.T) {
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	requestID := newRoutingRecorderRequestID()

	response := postOrdinaryChat(t, ctx, requestID, map[string]string{
		"x-aibrix-mock-delay-ms":         "2000",
		"x-envoy-upstream-rq-timeout-ms": "1000",
	})
	require.Equal(t, http.StatusGatewayTimeout, response.status, "body=%s", response.body)
	targetPod := response.header.Get("target-pod")
	require.NotEmpty(t, targetPod)

	records, err := framework.WaitForMockRequestCount(
		ctx,
		client,
		e2eConfig.Namespace,
		targetPod,
		requestID,
		1,
		3*time.Second,
	)
	require.NoError(t, err)
	assert.Equal(t, 2000, records[0].DelayMS)
	assert.Equal(t, "success", records[0].Outcome,
		"the backend should finish after Envoy has returned the one-second route timeout")
}

func TestOrdinaryBackendConnectionFailureIsPropagated(t *testing.T) {
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	pod := selectOrdinaryBackendPod(t, ctx, client)

	framework.UpdatePodLabels(t, ctx, client, e2eConfig.Namespace, pod.Name, map[string]string{
		routingFaultLabel: routingFaultValue,
	})

	// Confirm the gateway has observed the unique selector before changing the
	// selected backend's port. This separates a connection error from cache lag.
	require.Eventually(t, func() bool {
		response := postOrdinaryChat(t, ctx, newRoutingRecorderRequestID(), map[string]string{
			"external-filter": routingFaultLabel + "=" + routingFaultValue,
		})
		return response.status == http.StatusOK && response.header.Get("target-pod") == pod.Name
	}, 30*time.Second, 100*time.Millisecond, "gateway did not observe the selected ordinary backend")

	framework.UpdatePodLabels(t, ctx, client, e2eConfig.Namespace, pod.Name, map[string]string{
		"model.aibrix.ai/port": "1",
	})
	var response routedGatewayResponse
	var requestID string
	require.Eventually(t, func() bool {
		candidateID := newRoutingRecorderRequestID()
		candidate := postOrdinaryChat(t, ctx, candidateID, map[string]string{
			"external-filter": routingFaultLabel + "=" + routingFaultValue,
		})
		if candidate.status != http.StatusServiceUnavailable || candidate.header.Get("target-pod") != pod.Name {
			return false
		}
		response, requestID = candidate, candidateID
		return true
	}, 30*time.Second, 100*time.Millisecond, "gateway did not propagate the ordinary backend connection failure")

	assert.Equal(t, pod.Name, response.header.Get("target-pod"))
	assert.True(t, strings.HasSuffix(response.header.Get("target-pod-ip"), ":1"),
		"target address should contain the deliberately closed port: %s", response.header.Get("target-pod-ip"))
	records, err := framework.QueryMockRequests(ctx, client, e2eConfig.Namespace, pod.Name, requestID)
	require.NoError(t, err)
	assert.Empty(t, records, "a refused connection must not reach the backend recorder")
}

func selectOrdinaryBackendPod(t *testing.T, ctx context.Context, client *kubernetes.Clientset) corev1.Pod {
	t.Helper()
	pods, err := client.CoreV1().Pods(e2eConfig.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "model.aibrix.ai/name=" + modelName + ",app=mock-llama2-7b",
	})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items, "no ordinary mock backend found")
	return pods.Items[0]
}
