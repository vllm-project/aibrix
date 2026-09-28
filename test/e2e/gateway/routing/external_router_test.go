/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	framework "github.com/vllm-project/aibrix/test/e2e/framework"
)

type externalE2EMode struct {
	Mode        string `json:"mode"`
	TargetID    string `json:"targetId,omitempty"`
	DelayMillis int    `json:"delayMillis,omitempty"`
}

func externalAdminURL(t *testing.T) string {
	t.Helper()
	value := os.Getenv("AIBRIX_E2E_EXTERNAL_ROUTER_ADMIN_URL")
	if value == "" {
		t.Skip("AIBRIX_E2E_EXTERNAL_ROUTER_ADMIN_URL is not configured")
	}
	return strings.TrimSuffix(value, "/")
}

func resetExternalFixture(t *testing.T) {
	t.Helper()
	url := externalAdminURL(t) + "/__test/state"
	require.Eventually(t, func() bool {
		request, err := http.NewRequest(http.MethodDelete, url, nil)
		if err != nil {
			return false
		}
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusNoContent
	}, 30*time.Second, 200*time.Millisecond, "external router fixture did not become ready")
}

func setExternalFixtureMode(t *testing.T, mode externalE2EMode) {
	t.Helper()
	resetExternalFixture(t)
	t.Cleanup(func() { resetExternalFixture(t) })
	body, err := json.Marshal(mode)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPut, externalAdminURL(t)+"/__test/mode", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
}

func getExternalLastRequest(t *testing.T) map[string]any {
	t.Helper()
	return getExternalFixtureDocument(t, "/__test/last-request")
}

func getExternalFixtureDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(externalAdminURL(t) + path)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(body, &document))
	return document
}

func logExternalExchange(t *testing.T) map[string]any {
	t.Helper()
	request := getExternalLastRequest(t)
	response := getExternalFixtureDocument(t, "/__test/last-response")
	requestJSON, err := json.MarshalIndent(request, "", "  ")
	require.NoError(t, err)
	responseJSON, err := json.MarshalIndent(response, "", "  ")
	require.NoError(t, err)
	t.Logf("external router request JSON:\n%s", requestJSON)
	t.Logf("external router response JSON:\n%s", responseJSON)
	return request
}

func assertExternalRequestReachedNoBackend(t *testing.T, requestID string) {
	t.Helper()
	ctx := context.Background()
	client, _ := framework.InitializeClient(ctx, t)
	pods, err := client.CoreV1().Pods(e2eConfig.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "model.aibrix.ai/name=" + modelName,
	})
	require.NoError(t, err)
	for _, pod := range pods.Items {
		records, queryErr := framework.QueryMockRequests(ctx, client, e2eConfig.Namespace, pod.Name, requestID)
		require.NoError(t, queryErr)
		require.Empty(t, records, "backend %s unexpectedly received denied external request", pod.Name)
	}
}

func TestExternalRouterSelectedAndMinimalPayload(t *testing.T) {
	setExternalFixtureMode(t, externalE2EMode{Mode: "selected"})
	requestID := newRoutingRecorderRequestID()
	response := postOrdinaryChat(t, context.Background(), requestID, map[string]string{
		"routing-strategy": "external",
		"x-never-forward":  "secret-header",
	})
	require.Equal(t, http.StatusOK, response.status, "body=%s", response.body)
	require.NotEmpty(t, response.header.Get("target-pod"))

	document := logExternalExchange(t)
	encoded, err := json.Marshal(document)
	require.NoError(t, err)
	raw := string(encoded)
	require.Contains(t, raw, "routing.aibrix.ai/v1alpha1")
	require.Contains(t, raw, "ReplicaSelectionRequest")
	require.Contains(t, raw, "runningRequests")
	require.NotContains(t, raw, "gateway ordinary backend e2e")
	require.NotContains(t, raw, "Authorization")
	require.NotContains(t, raw, "secret-header")
	require.NotContains(t, raw, "address")
}

func TestExternalRouterDenied(t *testing.T) {
	setExternalFixtureMode(t, externalE2EMode{Mode: "denied"})
	requestID := newRoutingRecorderRequestID()
	response := postOrdinaryChat(t, context.Background(), requestID, map[string]string{"routing-strategy": "external"})
	logExternalExchange(t)
	require.Equal(t, http.StatusForbidden, response.status, "body=%s", response.body)
	require.Contains(t, string(response.body), "external_policy_denied")
	assertExternalRequestReachedNoBackend(t, requestID)
}

func TestExternalRouterInvalidTarget(t *testing.T) {
	setExternalFixtureMode(t, externalE2EMode{Mode: "invalid-target"})
	requestID := newRoutingRecorderRequestID()
	response := postOrdinaryChat(t, context.Background(), requestID, map[string]string{"routing-strategy": "external"})
	logExternalExchange(t)
	require.Equal(t, http.StatusServiceUnavailable, response.status, "body=%s", response.body)
	require.Contains(t, string(response.body), "external_router_unavailable")
	assertExternalRequestReachedNoBackend(t, requestID)
}

func TestExternalRouterTimeout(t *testing.T) {
	setExternalFixtureMode(t, externalE2EMode{Mode: "selected", DelayMillis: 1000})
	requestID := newRoutingRecorderRequestID()
	start := time.Now()
	response := postOrdinaryChat(t, context.Background(), requestID, map[string]string{"routing-strategy": "external"})
	logExternalExchange(t)
	require.Equal(t, http.StatusServiceUnavailable, response.status, "body=%s", response.body)
	require.Less(t, time.Since(start), 900*time.Millisecond)
	assertExternalRequestReachedNoBackend(t, requestID)
}
