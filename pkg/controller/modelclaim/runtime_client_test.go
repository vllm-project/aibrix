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

package modelclaim

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// clientForServer returns the default runtime client plus the host/port of a
// test server, exercising the real HTTP path against a mock runtime.
func clientForServer(srv *httptest.Server) (RuntimeClient, string, int) {
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return NewRuntimeClient(), u.Hostname(), port
}

func TestHTTPRuntimeActivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/activate", r.URL.Path)
		var req ActivateRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "m1", req.ModelName)
		assert.Equal(t, "kvc_m1", req.IPCName)
		require.NotNil(t, req.ClaimRef)
		assert.Equal(t, "claim-uid", req.ClaimRef.UID)
		require.NotNil(t, req.EngineConfig)
		assert.Equal(t, "2048", req.EngineConfig.Args["--max-model-len"])
		_ = json.NewEncoder(w).Encode(ActivateResponse{
			Status: "success", ModelName: req.ModelName, Port: 9123, IPCName: req.IPCName,
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	resp, err := c.Activate(context.Background(), host, port, &ActivateRequest{
		ModelName: "m1",
		IPCName:   "kvc_m1",
		ClaimRef:  &ModelClaimRef{Namespace: "default", Name: "m1", UID: "claim-uid"},
		EngineConfig: &modelv1alpha1.ModelClaimEngineConfig{
			Args: map[string]string{"--max-model-len": "2048"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(9123), resp.Port)
	assert.Equal(t, "kvc_m1", resp.IPCName)
}

// hangingRuntime is a runtime that takes each request and answers none of
// them until the test ends, as a runtime whose snapshot handler is stuck would.
// Once answering is set, it answers again. It counts the requests that reach
// it.
type hangingRuntime struct {
	host      string
	port      int
	requests  atomic.Int32
	answering atomic.Bool
}

func newHangingRuntime(t *testing.T) *hangingRuntime {
	t.Helper()
	runtime := &hangingRuntime{}
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runtime.requests.Add(1)
		if runtime.answering.Load() {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		<-released
	}))
	t.Cleanup(func() {
		close(released)
		srv.Close()
	})
	u, _ := url.Parse(srv.URL)
	runtime.host = u.Hostname()
	runtime.port, _ = strconv.Atoi(u.Port())
	return runtime
}

func TestHTTPRuntimeSnapshotGivesUpOnARuntimeThatDoesNotAnswer(t *testing.T) {
	runtime := newHangingRuntime(t)
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Minute, time.Now)

	start := time.Now()
	_, err := c.Snapshot(context.Background(), runtime.host, runtime.port)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, runtimeSnapshotTimeout, NewRuntimeClient().(*httpRuntimeClient).snapshotTimeout)
}

func TestHTTPRuntimeLeavesARuntimeThatDidNotAnswerAloneForAWhile(t *testing.T) {
	runtime := newHangingRuntime(t)
	now := time.Unix(1_700_000_000, 0)
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Minute, func() time.Time { return now })
	ctx := context.Background()

	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)
	require.Equal(t, int32(1), runtime.requests.Load())

	// Until the minute is up, every call to it fails at once.
	start := time.Now()
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	assert.ErrorIs(t, err, errRuntimeSilent)
	_, err = c.Activate(ctx, runtime.host, runtime.port, &ActivateRequest{ModelName: "m1"})
	assert.ErrorIs(t, err, errRuntimeSilent)
	assert.Less(t, time.Since(start), 50*time.Millisecond)
	assert.Equal(t, int32(1), runtime.requests.Load())

	// After that it is called again, and once it answers it is called as usual.
	runtime.answering.Store(true)
	now = now.Add(time.Minute)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	assert.Equal(t, int32(3), runtime.requests.Load())
}

func TestHTTPRuntimeStillStopsAnEngineOnARuntimeThatDidNotAnswer(t *testing.T) {
	// A claim is deleted or scaled down only once, so stopping its engine is
	// tried even on a runtime that did not answer a moment ago.
	runtime := newHangingRuntime(t)
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Minute, time.Now)
	ctx := context.Background()
	_, err := c.Snapshot(ctx, runtime.host, runtime.port)
	require.Error(t, err)

	runtime.answering.Store(true)
	require.NoError(t, c.Deactivate(ctx, runtime.host, runtime.port, &DeactivateRequest{ModelName: "m1"}))
	assert.Equal(t, int32(2), runtime.requests.Load())

	// It answered, so it is called as usual again.
	_, err = c.Snapshot(ctx, runtime.host, runtime.port)
	require.NoError(t, err)
	assert.Equal(t, int32(3), runtime.requests.Load())
}

func TestHTTPRuntimeCallsAgainARuntimeThatFailedFast(t *testing.T) {
	// A runtime that answers with an error, or refuses the connection, costs
	// nothing to call again, so it is not left alone.
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := listener.Addr().(*net.TCPAddr)
	require.NoError(t, listener.Close())
	c := newHTTPRuntimeClient(100*time.Millisecond, time.Minute, time.Now)
	ctx := context.Background()

	for range 2 {
		_, err := c.Snapshot(ctx, u.Hostname(), port)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errRuntimeSilent)
		_, err = c.Snapshot(ctx, refused.IP.String(), refused.Port)
		require.Error(t, err)
		assert.NotErrorIs(t, err, errRuntimeSilent)
	}
	assert.Equal(t, int32(2), requests.Load())
}

func TestHTTPRuntimeSnapshot(t *testing.T) {
	observedAt := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	requestSuccessTotal := int64(12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/snapshot", r.URL.Path)
		_ = json.NewEncoder(w).Encode(RuntimeSnapshot{
			ObservedAt: observedAt,
			Accelerators: []RuntimeAcceleratorSnapshot{{
				ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700,
			}},
			Models: []RuntimeSnapshotModel{{
				ModelName: "m1", ArtifactURL: "hf://Org/M1", Port: 9001,
				IPCName: "kvc_m1", Phase: "active", Alive: true, Ready: true,
				RestartCount: 2, LastError: "previous launch failed", LastTransition: &observedAt,
				KVUsedBytes: 10, KVCapacityBytes: 100,
				HBMPeakBytes:           456,
				RequestMetricsObserved: true, RequestsRunning: 2, RequestsWaiting: 1,
				RequestSuccessTotal: &requestSuccessTotal,
				ClaimRef:            &ModelClaimRef{Namespace: "default", Name: "m1", UID: "claim-uid"},
			}},
			CachedArtifacts: []string{"hf://Org/M1"},
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	snapshot, err := c.Snapshot(context.Background(), host, port)

	require.NoError(t, err)
	require.Len(t, snapshot.Accelerators, 1)
	assert.Equal(t, int64(700), snapshot.Accelerators[0].HBMFreeBytes)
	require.Len(t, snapshot.Models, 1)
	assert.Equal(t, "claim-uid", snapshot.Models[0].ClaimRef.UID)
	assert.True(t, snapshot.Models[0].Alive)
	assert.Equal(t, 2, snapshot.Models[0].RestartCount)
	assert.Equal(t, "previous launch failed", snapshot.Models[0].LastError)
	require.NotNil(t, snapshot.Models[0].LastTransition)
	assert.Equal(t, observedAt, *snapshot.Models[0].LastTransition)
	assert.Equal(t, int64(456), snapshot.Models[0].HBMPeakBytes)
	assert.True(t, snapshot.Models[0].RequestMetricsObserved)
	assert.Equal(t, int64(2), snapshot.Models[0].RequestsRunning)
	assert.Equal(t, int64(1), snapshot.Models[0].RequestsWaiting)
	require.NotNil(t, snapshot.Models[0].RequestSuccessTotal)
	assert.Equal(t, int64(12), *snapshot.Models[0].RequestSuccessTotal)
	assert.Equal(t, []string{"hf://Org/M1"}, snapshot.CachedArtifacts)
}

func TestHTTPRuntimeSetKVLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/kv-limit", r.URL.Path)
		var req SetKVLimitRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "m1", req.ModelName)
		assert.Equal(t, int64(4096), req.LimitBytes)
		assert.Equal(t, "limit-1", req.OperationID)
		_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
			Status: "success", ModelName: "m1", OperationID: "limit-1", Applied: true, Phase: "active",
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	response, err := c.SetKVLimit(context.Background(), host, port, &SetKVLimitRequest{
		ModelName: "m1", LimitBytes: 4096, OperationID: "limit-1",
	})

	require.NoError(t, err)
	assert.True(t, response.Applied)
	assert.Equal(t, "active", response.Phase)
}

func TestHTTPRuntimeSleepAndWake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/runtime/models/sleep":
			var req SleepRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "m1", req.ModelName)
			assert.Equal(t, 2, req.Level)
			assert.Equal(t, "sleep-1", req.OperationID)
			_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
				Status: "success", ModelName: "m1", OperationID: "sleep-1", Applied: true, Phase: "sleeping",
			})
		case "/v1/runtime/models/wake":
			var req WakeRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "m1", req.ModelName)
			assert.Equal(t, "wake-1", req.OperationID)
			_ = json.NewEncoder(w).Encode(RuntimeOperationResponse{
				Status: "success", ModelName: "m1", OperationID: "wake-1", Applied: true, Phase: "active",
			})
		default:
			t.Fatalf("unexpected runtime control path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	slept, err := c.Sleep(context.Background(), host, port, &SleepRequest{
		ModelName: "m1", Level: 2, OperationID: "sleep-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "sleeping", slept.Phase)
	woken, err := c.Wake(context.Background(), host, port, &WakeRequest{
		ModelName: "m1", OperationID: "wake-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "active", woken.Phase)
}

func TestHTTPRuntimeActivate_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ActivateResponse{Status: "error", Message: "out of HBM"})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of HBM")
}

func TestHTTPRuntimeActivate_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	_, err := c.Activate(context.Background(), host, port, &ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
}

func TestHTTPRuntimeDeactivate(t *testing.T) {
	var gotMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models/deactivate", r.URL.Path)
		var req DeactivateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotMode = string(req.Mode)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	err := c.Deactivate(context.Background(), host, port, &DeactivateRequest{ModelName: "m1", Mode: DeactivateStop})
	require.NoError(t, err)
	assert.Equal(t, "stop", gotMode)
}

func TestHTTPRuntimeListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/runtime/models", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string][]ModelInfo{
			"models": {{ModelName: "m1", Port: 9001, Phase: "active", KVUsedBytes: 10, KVTotalBytes: 100}},
		})
	}))
	defer srv.Close()

	c, host, port := clientForServer(srv)
	models, err := c.ListModels(context.Background(), host, port)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "m1", models[0].ModelName)
	assert.Equal(t, int32(9001), models[0].Port)
	assert.Equal(t, int64(10), models[0].KVUsedBytes)
	assert.Equal(t, int64(100), models[0].KVTotalBytes)
}
