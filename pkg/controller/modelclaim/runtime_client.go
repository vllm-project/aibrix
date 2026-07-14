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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// This file defines the engine <-> control-plane co-design protocol for
// ModelClaim, mirroring the ModelAdapter <-> runtime-sidecar protocol
// (pkg/controller/modeladapter/lora_client.go). The controller drives a
// per-runtime sidecar (the aibrix runtime sidecar) over HTTP to activate/deactivate
// a model as a kvcached-enabled engine process.

const (
	// DefaultRuntimePort is the port the runtime sidecar (aibrix runtime sidecar)
	// listens on, matching the ModelAdapter runtime API port.
	DefaultRuntimePort = 8080

	activatePath   = "/v1/runtime/models/activate"
	deactivatePath = "/v1/runtime/models/deactivate"
	modelListPath  = "/v1/runtime/models"
	snapshotPath   = "/v1/runtime/snapshot"
	kvLimitPath    = "/v1/runtime/models/kv-limit"
	sleepPath      = "/v1/runtime/models/sleep"
	wakePath       = "/v1/runtime/models/wake"

	defaultRuntimeHTTPTimeout = 60 * time.Second
)

// DeactivateMode selects how a model is torn down.
type DeactivateMode string

const (
	// DeactivateStop terminates the engine process entirely.
	DeactivateStop DeactivateMode = "stop"
)

// ActivateRequest asks the runtime sidecar to bring a model online as its own
// kvcached-enabled engine process sharing the pod's GPU.
type ActivateRequest struct {
	ModelName string `json:"model_name"`
	// ArtifactURL is the weight location (hf://, s3://, ...).
	ArtifactURL string `json:"artifact_url"`
	// Engine is "vllm" or "sglang".
	Engine string `json:"engine"`
	// Port is the engine port to serve on. 0 lets the runtime pick a free port,
	// which it returns in ActivateResponse.Port.
	Port int32 `json:"port,omitempty"`
	// IPCName is the kvcached shared-memory segment name; must be unique per
	// model on the GPU (KVCACHED_IPC_NAME). Empty lets the runtime derive one.
	IPCName string `json:"ipc_name,omitempty"`
	// Credentials and engine-specific startup settings.
	Credentials  map[string]string                     `json:"credentials,omitempty"`
	EngineConfig *modelv1alpha1.ModelClaimEngineConfig `json:"engine_config,omitempty"`
	// HBMReservationFraction is derived from vLLM's existing
	// --gpu-memory-utilization argument. It is controller/runtime bookkeeping,
	// not a new ModelClaim resource field.
	HBMReservationFraction float64        `json:"hbm_reservation_fraction,omitempty"`
	ClaimRef               *ModelClaimRef `json:"claim_ref,omitempty"`
}

// ModelClaimRef identifies the ModelClaim that owns a runtime engine without
// relying on the served model name, which may not be unique across namespaces.
type ModelClaimRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// ActivateResponse reports the resulting engine instance.
type ActivateResponse struct {
	Status    string `json:"status"` // "success" | "error"
	ModelName string `json:"model_name"`
	Port      int32  `json:"port"`
	IPCName   string `json:"ipc_name"`
	Message   string `json:"message,omitempty"`
}

// DeactivateRequest tears a model down per Mode.
type DeactivateRequest struct {
	ModelName string         `json:"model_name"`
	Mode      DeactivateMode `json:"mode"`
}

// SetKVLimitRequest applies a kvcached limit to a resident model through the
// runtime sidecar. OperationID makes retries idempotent at the runtime.
type SetKVLimitRequest struct {
	ModelName   string `json:"model_name"`
	LimitBytes  int64  `json:"limit_bytes"`
	OperationID string `json:"operation_id"`
}

// SleepRequest puts a vLLM engine to sleep through its runtime sidecar.
type SleepRequest struct {
	ModelName   string `json:"model_name"`
	Level       int    `json:"level"`
	OperationID string `json:"operation_id"`
}

// WakeRequest wakes a vLLM engine through its runtime sidecar.
type WakeRequest struct {
	ModelName   string `json:"model_name"`
	OperationID string `json:"operation_id"`
}

// RuntimeOperationResponse reports the result of an idempotent control action.
type RuntimeOperationResponse struct {
	Status      string `json:"status"`
	ModelName   string `json:"model_name"`
	OperationID string `json:"operation_id"`
	Applied     bool   `json:"applied"`
	Phase       string `json:"phase"`
}

// ModelInfo is one entry returned by the runtime sidecar's model listing.
type ModelInfo struct {
	ModelName string `json:"model_name"`
	Port      int32  `json:"port"`
	IPCName   string `json:"ipc_name"`
	Phase     string `json:"phase"`
	// Ready reports whether the engine can serve right now (runtime /health
	// probe). The controller gates routability on it: a model's annotation
	// stays at the non-routable marker (port 0) until Ready, so requests never
	// route to a still-booting engine.
	Ready        bool  `json:"ready"`
	KVUsedBytes  int64 `json:"kv_used_bytes,omitempty"`
	KVTotalBytes int64 `json:"kv_total_bytes,omitempty"`
}

// RuntimeAcceleratorSnapshot is one GPU visible to a warm runtime pod.
type RuntimeAcceleratorSnapshot struct {
	ID            string `json:"id"`
	HBMTotalBytes int64  `json:"hbm_total_bytes"`
	HBMFreeBytes  int64  `json:"hbm_free_bytes"`
}

// RuntimeSnapshotModel is one engine reported by a runtime snapshot.
type RuntimeSnapshotModel struct {
	ModelName   string         `json:"model_name"`
	ArtifactURL string         `json:"artifact_url"`
	ClaimRef    *ModelClaimRef `json:"claim_ref,omitempty"`
	Port        int32          `json:"port"`
	IPCName     string         `json:"ipc_name"`
	Phase       string         `json:"phase"`
	// Alive is process liveness, separate from readiness: a booting engine is
	// alive but not routable, while a restarting or terminal engine is not.
	Alive           bool       `json:"alive"`
	Ready           bool       `json:"ready"`
	RestartCount    int        `json:"restart_count"`
	LastError       string     `json:"last_error,omitempty"`
	LastTransition  *time.Time `json:"last_transition,omitempty"`
	KVUsedBytes     int64      `json:"kv_used_bytes"`
	KVCapacityBytes int64      `json:"kv_capacity_bytes"`
	HBMPeakBytes    int64      `json:"hbm_peak_bytes"`
	// HBMReservationFraction is the vLLM envelope carried at activation time.
	// A zero value means an old or non-vLLM runtime did not report one.
	HBMReservationFraction float64 `json:"hbm_reservation_fraction"`
	// RequestMetricsObserved distinguishes a zero metric from an unavailable
	// scrape. Pool policy must not infer idleness unless the completion counter
	// is also present.
	RequestMetricsObserved bool   `json:"request_metrics_observed"`
	RequestsRunning        int64  `json:"requests_running"`
	RequestsWaiting        int64  `json:"requests_waiting"`
	RequestSuccessTotal    *int64 `json:"request_success_total,omitempty"`
}

// RuntimeSnapshot is the point-in-time source for controller placement. It is
// cached in memory only; the runtime sidecar remains authoritative.
type RuntimeSnapshot struct {
	ObservedAt      time.Time                    `json:"observed_at"`
	Accelerators    []RuntimeAcceleratorSnapshot `json:"accelerators"`
	Models          []RuntimeSnapshotModel       `json:"models"`
	CachedArtifacts []string                     `json:"cached_artifacts"`
}

// RuntimeClient is the control-plane view of the per-runtime sidecar. The
// interface keeps the controller testable with an in-process fake.
type RuntimeClient interface {
	Activate(ctx context.Context, podIP string, port int, req *ActivateRequest) (*ActivateResponse, error)
	Deactivate(ctx context.Context, podIP string, port int, req *DeactivateRequest) error
	ListModels(ctx context.Context, podIP string, port int) ([]ModelInfo, error)
	Snapshot(ctx context.Context, podIP string, port int) (*RuntimeSnapshot, error)
	SetKVLimit(ctx context.Context, podIP string, port int, req *SetKVLimitRequest) (*RuntimeOperationResponse, error)
	Sleep(ctx context.Context, podIP string, port int, req *SleepRequest) (*RuntimeOperationResponse, error)
	Wake(ctx context.Context, podIP string, port int, req *WakeRequest) (*RuntimeOperationResponse, error)
}

// httpRuntimeClient talks to the runtime sidecar over HTTP.
type httpRuntimeClient struct {
	httpClient *http.Client
}

// NewRuntimeClient returns the default HTTP-backed runtime client.
func NewRuntimeClient() RuntimeClient {
	return &httpRuntimeClient{
		httpClient: &http.Client{Timeout: defaultRuntimeHTTPTimeout},
	}
}

func runtimeURL(podIP string, port int, path string) string {
	return fmt.Sprintf("http://%s:%d%s", podIP, port, path)
}

func (c *httpRuntimeClient) Activate(ctx context.Context, podIP string, port int, req *ActivateRequest) (*ActivateResponse, error) {
	out := &ActivateResponse{}
	if err := c.postJSON(ctx, runtimeURL(podIP, port, activatePath), req, out); err != nil {
		return nil, err
	}
	if out.Status == "error" {
		return out, fmt.Errorf("runtime failed to activate %s: %s", req.ModelName, out.Message)
	}
	return out, nil
}

func (c *httpRuntimeClient) Deactivate(ctx context.Context, podIP string, port int, req *DeactivateRequest) error {
	return c.postJSON(ctx, runtimeURL(podIP, port, deactivatePath), req, nil)
}

func (c *httpRuntimeClient) SetKVLimit(ctx context.Context, podIP string, port int, req *SetKVLimitRequest) (*RuntimeOperationResponse, error) {
	out := &RuntimeOperationResponse{}
	if err := c.postJSON(ctx, runtimeURL(podIP, port, kvLimitPath), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpRuntimeClient) Sleep(ctx context.Context, podIP string, port int, req *SleepRequest) (*RuntimeOperationResponse, error) {
	out := &RuntimeOperationResponse{}
	if err := c.postJSON(ctx, runtimeURL(podIP, port, sleepPath), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpRuntimeClient) Wake(ctx context.Context, podIP string, port int, req *WakeRequest) (*RuntimeOperationResponse, error) {
	out := &RuntimeOperationResponse{}
	if err := c.postJSON(ctx, runtimeURL(podIP, port, wakePath), req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpRuntimeClient) ListModels(ctx context.Context, podIP string, port int) ([]ModelInfo, error) {
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := c.getJSON(ctx, runtimeURL(podIP, port, modelListPath), &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func (c *httpRuntimeClient) Snapshot(ctx context.Context, podIP string, port int) (*RuntimeSnapshot, error) {
	out := &RuntimeSnapshot{}
	if err := c.getJSON(ctx, runtimeURL(podIP, port, snapshotPath), out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *httpRuntimeClient) getJSON(ctx context.Context, url string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime GET %s returned %d: %s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode runtime response: %w", err)
	}
	return nil
}

// postJSON marshals req, POSTs it, and (optionally) decodes the response into
// out. A non-2xx status is returned as an error including the response body.
func (c *httpRuntimeClient) postJSON(ctx context.Context, url string, req any, out any) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("runtime POST %s returned %d: %s", url, resp.StatusCode, body)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode runtime response: %w", err)
		}
	}
	return nil
}
