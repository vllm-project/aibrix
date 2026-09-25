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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
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

	// runtimeSnapshotTimeout bounds one snapshot read. A read normally takes a
	// fraction of a second, and the runtime gives each engine's probes at most
	// 1.5 s. Calls that change state keep the longer timeout above.
	runtimeSnapshotTimeout = 10 * time.Second

	// runtimeSilenceWindow is how long a runtime that did not answer in time is
	// not called again.
	runtimeSilenceWindow = time.Minute
)

// errRuntimeSilent is returned, without calling the runtime, for a runtime that
// did not answer in time within the last runtimeSilenceWindow.
var errRuntimeSilent = errors.New("did not answer in time recently; not calling it again yet")

// runtimeRefusal is an answer that says no. It is a body in which the runtime
// reports an error, or a status that says the request was at fault. Such a
// status is one from 400 to 499. The runtime did not do what it was asked.
type runtimeRefusal struct {
	message string
}

func (e *runtimeRefusal) Error() string { return e.message }

// statusError is the error for an answer with an error status.
//
// It is a refusal when the status says that the request was at fault, or when
// the body is the runtime's own report of an error. A server error with any
// other body can come from something between the controller and the runtime,
// such as a proxy that gave up waiting. It says nothing about what the runtime
// did, so it is no refusal. Neither is a status below 400 that the runtime
// does not send for this call.
func statusError(method, url string, status int, body []byte) error {
	message := fmt.Sprintf("runtime %s %s returned %d: %s", method, url, status, body)
	var answer struct {
		Status string `json:"status"`
	}
	reportsAnError := json.Unmarshal(body, &answer) == nil && answer.Status == "error"
	atFault := status >= http.StatusBadRequest && status < http.StatusInternalServerError
	if atFault || reportsAnError {
		return &runtimeRefusal{message}
	}
	return errors.New(message)
}

// callNotDone reports whether a failed call to a runtime is known to have
// changed nothing there: the runtime said no, or the call was never sent. After
// any other failure, such as an answer that did not arrive in time, the
// runtime may have done what it was asked.
func callNotDone(err error) bool {
	var refusal *runtimeRefusal
	if errors.As(err, &refusal) {
		return true
	}
	// A call is not sent when its address cannot be read, when no
	// connection could be made, or when its runtime is left alone for now.
	if errors.Is(err, errRuntimeSilent) {
		return true
	}
	var unsent *url.Error
	if errors.As(err, &unsent) && unsent.Op == "parse" {
		return true
	}
	var failed *net.OpError
	return errors.As(err, &failed) && failed.Op == "dial"
}

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
	ClaimRef     *ModelClaimRef                        `json:"claim_ref,omitempty"`
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
	// HBMUsableBytes is how much of this card an engine can ever take: the
	// total less what the driver keeps for itself. Unlike HBMFreeBytes it does
	// not move with traffic, so a card can be sized by it. Negative when the
	// runtime could not measure the card.
	HBMUsableBytes int64 `json:"hbm_usable_bytes"`
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
	Alive          bool       `json:"alive"`
	Ready          bool       `json:"ready"`
	RestartCount   int        `json:"restart_count"`
	LastError      string     `json:"last_error,omitempty"`
	LastTransition *time.Time `json:"last_transition,omitempty"`
	// KVUsedBytes is the KV memory this engine has mapped, its pages in use and
	// the ones it holds in reserve together. KVCapacityBytes is the limit its
	// KV allocator currently holds, which is what the engine obeys and not
	// necessarily what the controller last asked for. Both are negative while
	// the engine has no KV allocator to read, which a starting engine and one
	// that never built a segment have in common.
	KVUsedBytes     int64 `json:"kv_used_bytes"`
	KVCapacityBytes int64 `json:"kv_capacity_bytes"`
	HBMPeakBytes    int64 `json:"hbm_peak_bytes"`
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
	httpClient      *http.Client
	snapshotTimeout time.Duration
	silence         *runtimeSilence
}

// NewRuntimeClient returns the default HTTP-backed runtime client.
func NewRuntimeClient() RuntimeClient {
	return newHTTPRuntimeClient(runtimeSnapshotTimeout, runtimeSilenceWindow, time.Now)
}

func newHTTPRuntimeClient(snapshotTimeout, silenceWindow time.Duration, now func() time.Time) *httpRuntimeClient {
	return &httpRuntimeClient{
		httpClient:      &http.Client{Timeout: defaultRuntimeHTTPTimeout},
		snapshotTimeout: snapshotTimeout,
		silence:         &runtimeSilence{window: silenceWindow, now: now, until: map[string]time.Time{}},
	}
}

// runtimeSilence remembers the runtimes that did not answer in time. Every call
// runs on the controller's only worker, and every claim with an engine on a pod
// reads that pod's runtime on every pass. Without it, one runtime that stopped
// answering would hold each of those passes for a whole timeout. A call that
// fails fast, such as a refused connection or an error status, is not
// remembered, since trying again costs nothing.
type runtimeSilence struct {
	mu     sync.Mutex
	window time.Duration
	now    func() time.Time
	until  map[string]time.Time
}

// silent reports whether a runtime did not answer in time within the window.
func (s *runtimeSilence) silent(runtime string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Before(s.until[runtime])
}

// observe records how a call to a runtime ended. A timeout starts the window
// again, and anything else ends it. Windows that are over are dropped then, so
// the runtimes of pods that are gone are not kept.
func (s *runtimeSilence) observe(runtime string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		delete(s.until, runtime)
		return
	}
	now := s.now()
	for other, until := range s.until {
		if !now.Before(until) {
			delete(s.until, other)
		}
	}
	s.until[runtime] = now.Add(s.window)
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
		return out, &runtimeRefusal{fmt.Sprintf("runtime failed to activate %s: %s", req.ModelName, out.Message)}
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

// Snapshot reads a runtime under its own deadline. Placement reads every
// candidate this way, and the health check reads each instance's pod, one after
// another on the controller's only worker. So a runtime that does not answer
// must not hold that worker for long.
func (c *httpRuntimeClient) Snapshot(ctx context.Context, podIP string, port int) (*RuntimeSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, c.snapshotTimeout)
	defer cancel()
	out := &RuntimeSnapshot{}
	if err := c.getJSON(ctx, runtimeURL(podIP, port, snapshotPath), out); err != nil {
		return nil, err
	}
	return out, nil
}

// do sends a request to a runtime, unless that runtime did not answer in time a
// short while ago. Stopping an engine is sent even then: a claim is deleted or
// scaled down only once, and an engine left running would keep its memory.
func (c *httpRuntimeClient) do(req *http.Request) (*http.Response, error) {
	runtime := req.URL.Host
	if req.URL.Path != deactivatePath && c.silence.silent(runtime) {
		return nil, fmt.Errorf("runtime %s %w", runtime, errRuntimeSilent)
	}
	resp, err := c.httpClient.Do(req)
	c.silence.observe(runtime, err)
	return resp, err
}

func (c *httpRuntimeClient) getJSON(ctx context.Context, url string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return statusError(http.MethodGet, url, resp.StatusCode, body)
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

	resp, err := c.do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return statusError(http.MethodPost, url, resp.StatusCode, body)
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode runtime response: %w", err)
		}
	}
	return nil
}
