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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clearBootstrapCache() {
	bootstrapCache.Lock()
	bootstrapCache.m = make(map[string]*mooncakeBootstrapInfo)
	bootstrapCache.Unlock()
}

func makePrefillPod(name, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     v1.PodStatus{PodIP: ip},
	}
}

func makeRoutingContext(requestID string, body any) *types.RoutingContext {
	rc := types.NewRoutingContext(context.Background(), "", "", "", requestID, "")
	raw, err := sonic.Marshal(body)
	if err != nil {
		panic("makeRoutingContext: " + err.Error())
	}
	rc.ReqBody = raw
	return rc
}

func newBootstrapServer(engineID string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"0": map[string]any{
					"engine_id": engineID,
					"worker_addr": map[string]any{
						"0": map[string]string{"0": "tcp://127.0.0.1:9000"},
					},
				},
			}
			_ = sonic.ConfigDefault.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func listenerPort(tb testing.TB, srv *httptest.Server) int {
	tb.Helper()
	_, port, err := splitHostPort(srv.URL)
	require.NoError(tb, err)
	return port
}

// splitHostPort extracts host and numeric port from a URL like "http://127.0.0.1:12345".
func splitHostPort(rawURL string) (string, int, error) {
	start := 0
	for i, c := range rawURL {
		if c == '/' && i > 0 && rawURL[i-1] == '/' {
			start = i + 1
			break
		}
	}
	if start == 0 {
		return "", 0, fmt.Errorf("cannot parse URL: %s", rawURL)
	}
	rest := rawURL[start:]
	colon := -1
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return rest, 0, nil
	}
	var port int
	_, err := fmt.Sscanf(rest[colon+1:], "%d", &port)
	return rest[:colon], port, err
}

// ---------------------------------------------------------------------------
// AugmentPrefillRequest
// ---------------------------------------------------------------------------

func TestMooncakeAugmentPrefillRequest(t *testing.T) {
	agent := &MooncakeAgent{}
	rc := makeRoutingContext("req-abc", map[string]any{"model": "test"})

	completionRequest := map[string]any{
		"model":  "test",
		"stream": true,
	}

	err := agent.AugmentPrefillRequest(rc, nil, completionRequest)
	require.NoError(t, err)

	params, ok := completionRequest["kv_transfer_params"].(map[string]any)
	require.True(t, ok, "kv_transfer_params should be a map")

	assert.Equal(t, true, params["do_remote_decode"], "P node should do remote decode")
	assert.Equal(t, false, params["do_remote_prefill"], "P node should not do remote prefill")

	transferID, ok := params["transfer_id"].(string)
	require.True(t, ok, "transfer_id should be a string")
	assert.Equal(t, "xfer-req-abc", transferID)
}

func TestMooncakeAugmentPrefillRequest_PreservesExistingFields(t *testing.T) {
	agent := &MooncakeAgent{}
	rc := makeRoutingContext("req-123", map[string]any{})

	completionRequest := map[string]any{
		"model":      "qwen3-8B",
		"max_tokens": 100,
	}

	err := agent.AugmentPrefillRequest(rc, nil, completionRequest)
	require.NoError(t, err)

	assert.Equal(t, "qwen3-8B", completionRequest["model"])
	assert.Equal(t, 100, completionRequest["max_tokens"])
}

// ---------------------------------------------------------------------------
// MergePrefillResponse
// ---------------------------------------------------------------------------

func TestMooncakeMergePrefillResponse(t *testing.T) {
	clearBootstrapCache()

	bs := newBootstrapServer("engine-mooncake-01")
	defer bs.Close()

	origPort := mooncakeBootstrapPort
	mooncakeBootstrapPort = listenerPort(t, bs)
	t.Cleanup(func() { mooncakeBootstrapPort = origPort })

	agent := &MooncakeAgent{}

	originalBody := map[string]any{"model": "qwen3-8B", "stream": true}
	rc := makeRoutingContext("req-456", originalBody)
	prefillPod := makePrefillPod("prefill-0", "127.0.0.1")

	err := agent.MergePrefillResponse(rc, map[string]any{"id": "ignored"}, prefillPod)
	require.NoError(t, err)

	var updatedReq map[string]any
	err = sonic.Unmarshal(rc.ReqBody, &updatedReq)
	require.NoError(t, err)

	params, ok := updatedReq["kv_transfer_params"].(map[string]any)
	require.True(t, ok, "kv_transfer_params should be injected")

	assert.Equal(t, false, params["do_remote_decode"], "D node should not do remote decode")
	assert.Equal(t, true, params["do_remote_prefill"], "D node should do remote prefill")
	assert.Equal(t, "engine-mooncake-01", params["remote_engine_id"],
		"remote_engine_id from bootstrap")

	bsAddr, ok := params["remote_bootstrap_addr"].(string)
	require.True(t, ok)
	assert.Contains(t, bsAddr, "http://127.0.0.1")

	transferID, _ := params["transfer_id"].(string)
	assert.Equal(t, "xfer-req-456", transferID)

	assert.Equal(t, "qwen3-8B", updatedReq["model"])
	assert.Equal(t, true, updatedReq["stream"])
}

func TestMooncakeMergePrefillResponse_CacheHit(t *testing.T) {
	clearBootstrapCache()

	podIP := "10.0.0.2"
	cachedInfo := &mooncakeBootstrapInfo{
		engineID:      "cached-engine",
		bootstrapAddr: "http://10.0.0.2:18999",
		fetchedAt:     time.Now(),
	}
	bootstrapCache.Lock()
	bootstrapCache.m[podIP] = cachedInfo
	bootstrapCache.Unlock()

	agent := &MooncakeAgent{}
	rc := makeRoutingContext("req-cache", map[string]any{"model": "test-model"})
	prefillPod := makePrefillPod("prefill-cached", podIP)

	err := agent.MergePrefillResponse(rc, map[string]any{}, prefillPod)
	require.NoError(t, err)

	var updatedReq map[string]any
	err = sonic.Unmarshal(rc.ReqBody, &updatedReq)
	require.NoError(t, err)

	params, ok := updatedReq["kv_transfer_params"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "cached-engine", params["remote_engine_id"],
		"should use cached engine_id without HTTP call")
}

func TestMooncakeMergePrefillResponse_BootstrapUnreachable(t *testing.T) {
	clearBootstrapCache()

	origPort := mooncakeBootstrapPort
	mooncakeBootstrapPort = 19898
	t.Cleanup(func() { mooncakeBootstrapPort = origPort })

	agent := &MooncakeAgent{}
	rc := makeRoutingContext("req-fail", map[string]any{"model": "test"})
	prefillPod := makePrefillPod("prefill-offline", "10.0.0.99")

	err := agent.MergePrefillResponse(rc, map[string]any{}, prefillPod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "query mooncake bootstrap")
}

func TestMooncakeMergePrefillResponse_EmptyReqBody(t *testing.T) {
	clearBootstrapCache()
	rc := types.NewRoutingContext(context.Background(), "", "", "", "req-empty", "")
	agent := &MooncakeAgent{}
	prefillPod := makePrefillPod("prefill-empty", "10.0.0.1")

	err := agent.MergePrefillResponse(rc, map[string]any{}, prefillPod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "original request body")
}

// ---------------------------------------------------------------------------
// getMooncakeBootstrapInfo (internal)
// ---------------------------------------------------------------------------

func TestMooncakeGetBootstrapInfo_CacheExpiry(t *testing.T) {
	clearBootstrapCache()

	bs := newBootstrapServer("engine-expiry")
	defer bs.Close()

	origPort := mooncakeBootstrapPort
	mooncakeBootstrapPort = listenerPort(t, bs)
	t.Cleanup(func() { mooncakeBootstrapPort = origPort })

	rc := makeRoutingContext("bootstrap-expire", map[string]any{})
	pod := makePrefillPod("prefill-expire", "127.0.0.1")

	// First call: query HTTP server.
	info1, err := getMooncakeBootstrapInfo(rc, pod)
	require.NoError(t, err)
	assert.Equal(t, "engine-expiry", info1.engineID)

	// Second call: hit cache (same pod IP, port unchanged).
	info2, err := getMooncakeBootstrapInfo(rc, pod)
	require.NoError(t, err)
	assert.Equal(t, "engine-expiry", info2.engineID, "should return cached value")

	// Expire the cache entry manually.
	bootstrapCache.Lock()
	bootstrapCache.m[pod.Status.PodIP].fetchedAt = time.Now().Add(-(mooncakeBootstrapCacheTTL + time.Minute))
	bootstrapCache.Unlock()

	// Third call: re-query HTTP server because TTL expired.
	info3, err := getMooncakeBootstrapInfo(rc, pod)
	require.NoError(t, err)
	assert.Equal(t, "engine-expiry", info3.engineID, "should re-query after TTL expiry")
}

func TestMooncakeGetBootstrapInfo_NoPodIP(t *testing.T) {
	clearBootstrapCache()
	rc := makeRoutingContext("bootstrap-nopodip", map[string]any{})
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ip"},
	}
	_, err := getMooncakeBootstrapInfo(rc, pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no PodIP")
}

func TestMooncakeGetBootstrapInfo_EmptyResponse(t *testing.T) {
	clearBootstrapCache()

	bs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bs.Close()

	origPort := mooncakeBootstrapPort
	mooncakeBootstrapPort = listenerPort(t, bs)
	t.Cleanup(func() { mooncakeBootstrapPort = origPort })

	rc := makeRoutingContext("bootstrap-empty", map[string]any{})
	pod := makePrefillPod("prefill-empty-resp", "127.0.0.1")
	_, err := getMooncakeBootstrapInfo(rc, pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no engine_id")
}

func TestMooncakeGetBootstrapInfo_Non200Response(t *testing.T) {
	clearBootstrapCache()

	bs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bs.Close()

	origPort := mooncakeBootstrapPort
	mooncakeBootstrapPort = listenerPort(t, bs)
	t.Cleanup(func() { mooncakeBootstrapPort = origPort })

	rc := makeRoutingContext("bootstrap-non200", map[string]any{})
	pod := makePrefillPod("prefill-err", "127.0.0.1")
	_, err := getMooncakeBootstrapInfo(rc, pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "returned status")
}