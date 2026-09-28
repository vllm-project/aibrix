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

// TestHTTPRuntimeSnapshotReadsEveryFieldTheRuntimeSends decodes a snapshot as
// the runtime writes it, with a value in every field, so that a name changed
// in the client fails a test.
func TestHTTPRuntimeSnapshotReadsEveryFieldTheRuntimeSends(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"observed_at": "2026-09-21T10:00:00.123456Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": 950}],
			"models": [{"model_name": "m1", "artifact_url": "hf://Org/M1",
				"claim_ref": {"namespace": "default", "name": "claim-1", "uid": "uid-1"}, "port": 9001,
				"ipc_name": "kvc_m1", "phase": "active", "alive": true, "ready": true, "restart_count": 2,
				"last_error": "engine exited", "last_transition": "2026-09-21T09:59:58.000001Z",
				"kv_used_bytes": 300, "kv_capacity_bytes": 600, "hbm_peak_bytes": 456,
				"request_metrics_observed": true, "requests_running": 3, "requests_waiting": 1,
				"request_success_total": 12}],
			"cached_artifacts": ["hf://Org/M1"]}`))
	}))
	defer srv.Close()
	c, host, port := clientForServer(srv)

	snapshot, err := c.Snapshot(context.Background(), host, port)

	require.NoError(t, err)
	lastTransition := time.Date(2026, time.September, 21, 9, 59, 58, 1000, time.UTC)
	requestSuccessTotal := int64(12)
	assert.Equal(t, &RuntimeSnapshot{
		ObservedAt: time.Date(2026, time.September, 21, 10, 0, 0, 123456000, time.UTC),
		Accelerators: []RuntimeAcceleratorSnapshot{
			{ID: "GPU-0", HBMTotalBytes: 1000, HBMFreeBytes: 700, HBMUsableBytes: 950},
		},
		Models: []RuntimeSnapshotModel{{
			ModelName: "m1", ArtifactURL: "hf://Org/M1",
			ClaimRef: &ModelClaimRef{Namespace: "default", Name: "claim-1", UID: "uid-1"},
			Port:     9001, IPCName: "kvc_m1", Phase: "active", Alive: true, Ready: true, RestartCount: 2,
			LastError: "engine exited", LastTransition: &lastTransition,
			KVUsedBytes: 300, KVCapacityBytes: 600, HBMPeakBytes: 456,
			RequestMetricsObserved: true, RequestsRunning: 3, RequestsWaiting: 1,
			RequestSuccessTotal: &requestSuccessTotal,
		}},
		CachedArtifacts: []string{"hf://Org/M1"},
	}, snapshot)
}

// TestHTTPRuntimeSnapshotReadsWhatTheRuntimeSends decodes what the runtime
// sends for an engine that is still booting, and for a card it could not
// measure.
func TestHTTPRuntimeSnapshotReadsWhatTheRuntimeSends(t *testing.T) {
	for name, tc := range map[string]struct {
		payload        string
		hbmUsableBytes int64
		sized          bool
		engines        int
	}{
		"a card the runtime measured": {`{
			"observed_at": "2026-09-21T10:00:00.123456Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": 950}],
			"models": [{"model_name": "m1", "artifact_url": "hf://Org/M1", "claim_ref": null, "port": 9001,
				"ipc_name": "kvc_m1", "phase": "booting", "alive": true, "ready": false, "restart_count": 0,
				"last_error": null, "last_transition": "2026-09-21T09:59:58.000001Z",
				"kv_used_bytes": -1, "kv_capacity_bytes": -1, "hbm_peak_bytes": 0,
				"request_metrics_observed": false, "requests_running": 0, "requests_waiting": 0,
				"request_success_total": null}],
			"cached_artifacts": []}`, 950, true, 1},
		"a card it could not measure": {`{
			"observed_at": "2026-09-21T10:00:00Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700, "hbm_usable_bytes": -1}],
			"models": [], "cached_artifacts": []}`, -1, false, 0},
		"a runtime from before the field": {`{
			"observed_at": "2026-09-21T10:00:00Z",
			"accelerators": [{"id": "GPU-0", "hbm_total_bytes": 1000, "hbm_free_bytes": 700}],
			"models": [], "cached_artifacts": []}`, 0, false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.payload))
			}))
			defer srv.Close()
			c, host, port := clientForServer(srv)

			snapshot, err := c.Snapshot(context.Background(), host, port)

			require.NoError(t, err)
			require.Len(t, snapshot.Accelerators, 1)
			assert.Equal(t, tc.hbmUsableBytes, snapshot.Accelerators[0].HBMUsableBytes)
			hbmUsableBytes, sized := snapshot.hbmUsableBytes()
			assert.Equal(t, tc.sized, sized)
			if sized {
				assert.Equal(t, tc.hbmUsableBytes, hbmUsableBytes)
			}
			require.Len(t, snapshot.Models, tc.engines)
			for _, model := range snapshot.Models {
				assert.Equal(t, int64(-1), model.KVUsedBytes)
				assert.Equal(t, int64(-1), model.KVCapacityBytes)
				assert.Nil(t, model.ClaimRef)
				assert.Empty(t, model.LastError)
				require.NotNil(t, model.LastTransition)
			}
		})
	}
}

// A start is taken back only when the engine is known not to have started.
// This goes through the real client, so it holds what the client makes of
// each answer, and of each call that got none.
func TestHTTPRuntimeTellsAStartThatWasRefusedFromOneThatMayHaveBeenDone(t *testing.T) {
	answering := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	hijacked := func(then func(conn net.Conn)) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			then(conn)
		}
	}
	ownError := `{"status": "error", "model_name": "m1", "port": 0, "ipc_name": "", "message": "boom"}`

	for name, tc := range map[string]struct {
		runtime http.HandlerFunc
		notDone bool
	}{
		// What the runtime itself sends when it rejects a request, and when
		// starting the engine failed.
		"the runtime's own 400":                {answering(http.StatusBadRequest, ownError), true},
		"the runtime's own 500":                {answering(http.StatusInternalServerError, ownError), true},
		"a success status with an error in it": {answering(http.StatusOK, ownError), true},
		// A request that did not pass the API's validation never reached the
		// runtime.
		"a 422 from the API": {answering(http.StatusUnprocessableEntity, `{"detail": []}`), true},
		// Something between the controller and the runtime gave up waiting.
		// The runtime may still be starting the engine.
		"a 504 that is not the runtime's":   {answering(http.StatusGatewayTimeout, "upstream request timeout"), false},
		"a 502 that is not the runtime's":   {answering(http.StatusBadGateway, "<html>bad gateway</html>"), false},
		"closed after the request was read": {hijacked(func(conn net.Conn) { _ = conn.Close() }), false},
		"reset after the request was read": {hijacked(func(conn net.Conn) {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0)
			}
			_ = conn.Close()
		}), false},
		"no answer in time": {hijacked(func(conn net.Conn) {
			time.Sleep(time.Second)
			_ = conn.Close()
		}), false},
		"a success status that cannot be read": {answering(http.StatusOK, "<html>ok</html>"), false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.runtime)
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			port, _ := strconv.Atoi(u.Port())
			c := NewRuntimeClient().(*httpRuntimeClient)
			c.httpClient.Timeout = 300 * time.Millisecond

			_, err := c.Activate(context.Background(), u.Hostname(), port, &ActivateRequest{ModelName: "m1"})

			require.Error(t, err)
			assert.Equal(t, tc.notDone, callNotDone(err), "%v", err)
		})
	}

	// Nobody listens, so the call was never sent.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	refused := listener.Addr().(*net.TCPAddr)
	require.NoError(t, listener.Close())
	_, err = NewRuntimeClient().Activate(context.Background(), refused.IP.String(), refused.Port,
		&ActivateRequest{ModelName: "m1"})
	require.Error(t, err)
	assert.True(t, callNotDone(err), "%v", err)
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
