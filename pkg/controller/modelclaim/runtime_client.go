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

// RuntimeClient is the control-plane view of the per-runtime sidecar. The
// interface keeps the controller testable with an in-process fake.
type RuntimeClient interface {
	Activate(ctx context.Context, podIP string, port int, req *ActivateRequest) (*ActivateResponse, error)
	Deactivate(ctx context.Context, podIP string, port int, req *DeactivateRequest) error
	ListModels(ctx context.Context, podIP string, port int) ([]ModelInfo, error)
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

func (c *httpRuntimeClient) ListModels(ctx context.Context, podIP string, port int) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeURL(podIP, port, modelListPath), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtime list models returned %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode runtime model list: %w", err)
	}
	return out.Models, nil
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
