/*
Copyright 2024 The Aibrix Team.

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

package types

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
)

var (
	nilPod     = &v1.Pod{}
	ErrUnknown = errors.New("unknown error")
)

const (
	statusInitial = iota // 0: initial state
	statusAdded          // 1: added
	statusDone           // 2: done
)

type RequestFeatures []float64

// ResolvedConfigProfile holds the resolved model config profile for a request.
// Populated from model.aibrix.ai/config annotation based on config-profile header or defaultProfile.
// Nil when no config is present;
type ResolvedConfigProfile struct {
	// LockedRoutingStrategy pins the routing strategy model-wide when set.
	// It takes precedence over the routing-strategy request header, the profile
	// RoutingStrategy and the ROUTING_ALGORITHM environment variable.
	LockedRoutingStrategy string
	// AuthoritativeRoutingPolicy records that routing diagnostics must be omitted
	// from the client response. Client routing inputs are cleared before the
	// resolved profile is stored here.
	AuthoritativeRoutingPolicy bool
	RoutingStrategy            string
	RoutingConfig              json.RawMessage
	// Routing is RoutingConfig in its typed form, parsed once when this profile
	// is resolved. Nil when the profile sets no routingConfig or it does not
	// parse; routingalgorithms.ResolveRoutingOverrides turns it into the
	// request's concrete overrides.
	Routing *RoutingConfig
	// RequestsPerSecond is the per-model request-rate limit enforced by enforceModelRPS,
	// resolved from the profile's requestsPerSecond or from its requestsPerSecondPerReplica
	// (which takes precedence and scales it by the model's current routable replica count).
	// Zero means unset/unlimited.
	RequestsPerSecond int64
	// RateWindowSeconds is the window size, in seconds, that RequestsPerSecond is enforced
	// over. Defaults to a 1s window (0 or 1 both mean "1s") when unset; set above 1 for
	// sub-1 RPS values expressed as "1 request every N seconds".
	RateWindowSeconds int64
	// RequestsInflight is the maximum number of concurrent (in-flight) requests allowed on
	// a single replica, enforced per pod rather than as an aggregate. Zero means unset.
	RequestsInflight int64
	// TTFTThresholdS is the per-model time-to-first-token threshold in seconds, resolved
	// from the profile's ttftThresholdS. Zero means unset, in which case the response-body
	// path keeps using the process-wide AIBRIX_TTFT_THRESHOLD_S default.
	TTFTThresholdS int64
}

// RoutingAlgorithm defines the routing algorithms
type RoutingAlgorithm string

// Polarity indicates whether a higher or lower score is better for a routing strategy
type Polarity int

const (
	PolarityLeast Polarity = iota // Lower score is better
	PolarityMost                  // Higher score is better
)

// PodScorer defines the interface for strategies that support batch soft-scoring
type PodScorer interface {
	ScoreAll(ctx *RoutingContext, readyPodList PodList) (scores []float64, scored []bool, err error)
	Polarity() Polarity
}

// PostRouteUpdater defines the interface for strategies that need to update
// internal state after a target pod has been selected.
type PostRouteUpdater interface {
	PostRouteUpdate(ctx *RoutingContext, readyPodList PodList, targetPod *v1.Pod) error
}

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	context.Context
	Algorithm RoutingAlgorithm
	Model     string
	// BaseModel is the served base-model name when Model is a LoRA adapter.
	// Empty for base-model requests.
	BaseModel      string
	Engine         string
	Stream         bool
	Message        string
	RequestID      string
	User           *string
	Span           trace.Span // record attrs for main span
	RequestTime    time.Time  // Time when the routing context is created.
	RequestEndTime time.Time  // Time when the routing is done and sent to inference engine.
	PendingLoad    float64    // Normalized pending load of request, available after AddRequestCount call. See cache.PendingLoadProvider
	TraceTerm      int64      // Trace term identifier, available after AddRequestCount call.
	RoutedTime     time.Time  // Time consumed during routing.

	// PrefixMatchText, when set, replaces Message as the text prefix-matching policies
	// hash. The gateway sets it for chat requests whose chat template renders
	// request-level fields (the tool definitions) ahead of the messages, so that
	// requests differing only in those fields do not look like a shared prefix.
	// Prompt-size estimates keep using Message. Read it through PrefixText.
	PrefixMatchText string

	ReqHeaders       map[string]string
	ReqBody          []byte
	ReqPath          string
	ReqConfigProfile string
	// AsyncJobBackendID is the backend-private identifier used for a pinned
	// asynchronous job request. It is retained only for the lifetime of this
	// routing context so gateway error processing can replace it with the public
	// ID in an upstream error body.
	AsyncJobBackendID string

	PrefillStartTime time.Time // Time when prefill request is started.
	PrefillEndTime   time.Time // Time consumed during prefill.

	// FirstTokenTime records the arrival time of the first response body chunk.
	// Used to compute TTFT/KV-transfer time for streaming responses, where
	// request_end metrics are only emitted on the final chunk.
	FirstTokenTime time.Time

	// RespHeaders holds response headers that the router intends to set.
	// These are typically used to propagate control information back to the client,
	// such as session affinity id.
	// The router implementation (e.g., sessionAffinityRouter) may populate this field
	// during the Route() call.
	RespHeaders map[string]string

	// ConfigProfile holds the resolved model config profile for this request.
	// Set in HandleRequestBody from model.aibrix.ai/config (annotation)
	// based on config-profile header. Nil when no config is present.
	ConfigProfile *ResolvedConfigProfile

	// ReplicaInflightAdmitted is true once the gateway's replica-inflight admission check
	// (enforceReplicaInflight, backed by cache.Store.AdmitPodRunningRequest) has atomically
	// admitted this request AND, in doing so, already applied this gateway's own +1 to the
	// target pod's cross-gateway running-requests counter. Consumers that also increment
	// that counter (addPodStats) must check this first and skip their own increment, or the
	// pod's count would be inflated by an extra, never-decremented +1 for this request.
	ReplicaInflightAdmitted bool

	targetPodSet chan struct{}
	targetPod    atomic.Pointer[v1.Pod]
	targetPort   atomic.Int32
	lastError    atomic.Pointer[error]

	// routingOverrides holds the resolved routing overrides of this request's
	// model config profile. The gateway's routing entry resolves them once per
	// request so every strategy on the routing path reads the same validated
	// values. Nil when the profile sets none, and reads then fall back to the
	// process defaults. Written on the request goroutine before routing starts
	// and read by that same goroutine, so no atomic is needed here (the PD leg
	// keeps one for the async prefill and abort paths).
	routingOverrides *RoutingOverrides
	tokens           []int           // Cache of tokenized prompts
	predictor        OutputPredictor // OutputPredictor gained from cache
	statsUpdated     int32           // Use to flag if in-memory realtime statistics has been updated for the request.
	traceAdded       int32           // Use to flag if trace has been added to cache

	// pdLeg holds the prefill/decode leg state of the current incarnation of
	// this request. It is a separate heap object, replaced wholesale on reset,
	// because the async prefill leg outlives the client stream and must not
	// report onto whichever request next takes this pooled object. See
	// PDLegState.
	pdLeg atomic.Pointer[PDLegState]

	// Fields for unit tests
	debugDelay time.Duration
}

var requestPool = sync.Pool{
	New: func() any { return &RoutingContext{} },
}

// NewContext gets a RoutingContext with current RoutingAlgorithm.
func (alg RoutingAlgorithm) NewContext(ctx context.Context, model, message, requestID, user string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, alg, model, message, requestID, user)
	return request
}

// NewRoutingContext gets a RoutingContext from a context pool.
func NewRoutingContext(ctx context.Context, algorithms RoutingAlgorithm, model, message, requestID, user string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	request.reset(ctx, algorithms, model, message, requestID, user)
	return request
}

// SetOutputPredictor enables RoutingContext to use existing OutputPredictor to predict output length.
func (r *RoutingContext) SetOutputPredictor(predictor OutputPredictor) (old OutputPredictor) {
	old = r.predictor
	r.predictor = predictor
	return
}

// Delete resolves all waiting TargetPod() calls and releases the RoutingContext to the pool.
func (r *RoutingContext) Delete() {
	r.SetTargetPod(nil) // Unblock waiting TargetPod() call
	requestPool.Put(r)
}

// Elapsed returns the elapsed time since the request was created.
func (r *RoutingContext) Elapsed(currentTime time.Time) time.Duration {
	return currentTime.Sub(r.RequestTime)
}

// PrefixText returns the text prefix-matching policies should hash: PrefixMatchText
// when set, Message otherwise.
func (r *RoutingContext) PrefixText() string {
	if r.PrefixMatchText != "" {
		return r.PrefixMatchText
	}
	return r.Message
}

// PromptTokens returns the tokenized prompt of the request.
func (r *RoutingContext) PromptTokens() ([]int, error) {
	if r.tokens == nil {
		var err error
		r.tokens, err = utils.TokenizeInputText(r.Message)
		if err != nil {
			return nil, err
		}
	}
	return r.tokens, nil
}

// PromptLength returns the length of the prompt of the request.
func (r *RoutingContext) PromptLength() (int, error) {
	tokens, err := r.PromptTokens()
	if err != nil {
		return 0, err
	}
	return len(tokens), nil
}

// TokenLength returns the predicted output token length.
func (r *RoutingContext) TokenLength() (int, error) {
	promptLen, err := r.PromptLength()
	if err != nil {
		return 0, err
	}

	if r.predictor == nil {
		return 0, fmt.Errorf("output predictor not set")
	}

	return r.predictor.Predict(promptLen), nil
}

// Features returns the features corresponding to the request.
// The feature of a request is defined by the output length and prompt length.
func (r *RoutingContext) Features() (RequestFeatures, error) {
	promptLen, err := r.PromptLength()
	if err != nil {
		return nil, err
	}

	outputLen, err := r.TokenLength()
	if err != nil {
		return nil, err
	}

	return RequestFeatures{float64(outputLen), float64(promptLen)}, nil
}

// SetTargetPod sets the target pod of the routing context. All routers call this to set the target pod.
func (r *RoutingContext) SetTargetPod(pod *v1.Pod) {
	if r.targetPod.CompareAndSwap(nilPod, pod) { // Use CompareAndSwap to ensure close channel only once
		r.RoutedTime = time.Now()
		close(r.targetPodSet)
	}
}

// SetError sets the error of the routing context asynchronously.
// Do not call this function from synchronize routers. Asynchronize routers call this to set an error.
func (r *RoutingContext) SetError(err error) {
	if err == nil {
		r.lastError.Store(&ErrUnknown)
	} else {
		r.lastError.Store(&err)
	}
	r.SetTargetPod(nil)
}

// TargetPod returns the routing target pod of the request.
// TargetPod blocks until the target pod is set or an error is set.
func (r *RoutingContext) TargetPod() *v1.Pod {
	targetPod := r.targetPod.Load()
	if targetPod == nilPod {
		r.debugWait()
		select {
		case <-r.Done():
			r.SetError(r.Err())
		case <-r.targetPodSet: // No blocking if targetPod is set after last "targetPod == nil"
		}
		targetPod = r.targetPod.Load()
	}

	return targetPod
}

func (r *RoutingContext) TargetPort() int {
	return int(r.targetPort.Load())
}

func (r *RoutingContext) SetTargetPort(port int) {
	r.targetPort.Store(int32(port))
}

// GetError returns the error of the routing context.
func (r *RoutingContext) GetError() error {
	if r.TargetPod() == nil {
		return r.getError()
	}
	return nil
}

// TargetAddress returns the routing target address of the request.
func (r *RoutingContext) TargetAddress() string {
	pod := r.TargetPod()
	if pod == nil {
		return ""
	}

	port := r.TargetPort()
	if port != 0 {
		return r.targetAddressWithPort(pod.Status.PodIP, port)
	}
	return r.targetAddress(r.TargetPod())
}

// HasRouted returns true if the request has been routed or an error has been set.
func (r *RoutingContext) HasRouted() bool {
	pod := r.targetPod.Load()
	return pod != nilPod && pod != nil
}

// HasError returns true if the request has an error.
func (r *RoutingContext) HasError() bool {
	pod := r.targetPod.Load()
	return pod == nil && r.getError() != nil
}

// CanAddStats returns true if the first time trying update in-memory realtime statistics.
func (r *RoutingContext) CanAddStats() bool {
	return atomic.CompareAndSwapInt32(&r.statsUpdated, statusInitial, statusAdded)
}

func (r *RoutingContext) CanDoneStats() bool {
	return atomic.CompareAndSwapInt32(&r.statsUpdated, statusAdded, statusDone)
}

// CanAddTrace returns true if the first time trying add trace to cache.
func (r *RoutingContext) CanAddTrace() bool {
	return atomic.CompareAndSwapInt32(&r.traceAdded, statusInitial, statusAdded)
}

// CanDoneTrace returns true only the first time a request finishes its trace
// bookkeeping. It pairs with CanAddTrace: the model-level pendingRequests
// counter is incremented once under CanAddTrace, so it must be decremented
// exactly once here, even though several completion paths (response headers,
// response body and the receive-error exits) may all call into DoneRequest*.
func (r *RoutingContext) CanDoneTrace() bool {
	return atomic.CompareAndSwapInt32(&r.traceAdded, statusAdded, statusDone)
}

// GetRoutingDelay returns the time duration used for routing the request.
// Returns 0 if routing did not complete (e.g., prefill failure before SetTargetPod was called).
func (r *RoutingContext) GetRoutingDelay() time.Duration {
	if r.RoutedTime.IsZero() {
		return 0
	}
	return r.RoutedTime.Sub(r.RequestTime)
}

func (r *RoutingContext) targetAddress(pod *v1.Pod) string {
	if port, ok := utils.ModelClaimPortForPod(pod, r.Model); ok {
		if port > 0 {
			return r.targetAddressWithPort(pod.Status.PodIP, port)
		}
		// Known ModelClaim model that is not yet routable (port 0). Do not fall
		// back to the default deployment port on this warm pool pod.
		return ""
	}
	return fmt.Sprintf("%v:%v", pod.Status.PodIP, utils.GetModelPortForPod(r.RequestID, pod))
}

func (r *RoutingContext) targetAddressWithPort(podIP string, port int) string {
	return fmt.Sprintf("%v:%v", podIP, port)
}

// MetricModel returns the model name to emit on gateway metrics.
// For a LoRA request this is the attached base model; otherwise the requested name.
func (r *RoutingContext) MetricModel() string {
	if r == nil {
		return ""
	}
	if r.BaseModel != "" {
		return r.BaseModel
	}
	return r.Model
}

// MetricLoraAdapter returns the adapter name for gateway metrics.
// Empty for base-model requests.
func (r *RoutingContext) MetricLoraAdapter() string {
	if r == nil || r.BaseModel == "" || r.BaseModel == r.Model {
		return ""
	}
	return r.Model
}

func (r *RoutingContext) getError() (err error) {
	errAddr := r.lastError.Load()
	if errAddr != nil {
		return *errAddr
	}
	return
}

func (r *RoutingContext) reset(ctx context.Context, algorithms RoutingAlgorithm, model, message, requestID, user string) {
	r.Context = ctx
	r.Algorithm = algorithms
	r.Model = model
	r.BaseModel = ""
	r.Engine = ""
	r.Stream = false
	r.Message = message
	r.PrefixMatchText = ""
	r.RequestID = requestID
	if user != "" {
		r.User = &user
	} else {
		r.User = nil
	}
	r.RequestTime = time.Now()
	r.RequestEndTime = time.Time{}
	r.PendingLoad = 0
	r.TraceTerm = 0

	r.ReqHeaders = map[string]string{}
	r.ReqPath = ""
	r.ReqConfigProfile = ""
	r.ReqBody = []byte{}
	r.AsyncJobBackendID = ""
	r.PrefillStartTime = time.Time{}
	r.PrefillEndTime = time.Time{}
	r.FirstTokenTime = time.Time{}
	// RoutedTime will not be reset, it must before ReqeustTime at this time.

	r.Span = nil
	r.RespHeaders = map[string]string{}
	r.ConfigProfile = nil
	// The profile is gone, so the overrides derived from it must go too: a
	// pooled context handed to the next request would otherwise steer models
	// the new profile never configured (ResolveRoutingOverrides sets them once
	// per request, and a request without a profile leaves them unset).
	r.ClearRoutingOverrides()
	r.ReplicaInflightAdmitted = false
	r.targetPodSet = make(chan struct{}) // Initialize channel
	r.targetPod.Store(nilPod)
	r.targetPort.Store(0)
	r.lastError.Store(nil)
	// debugDelay will be reset by tests.
	r.tokens = nil
	r.predictor = nil
	r.statsUpdated = statusInitial
	r.traceAdded = statusInitial
	// A fresh leg per incarnation: any prefill goroutine still holding the
	// previous one can then only mutate an object nothing reads any more.
	//
	// The retired leg is deliberately not cancelled here. Its decode abort is
	// exactly the work that has to outlive the client stream, and this object
	// can be handed to a new request microseconds after that stream ended, so
	// cancelling on reuse would routinely kill the abort that fail-fast exists
	// to send. It stops on its own: both the abort POST and the retry delay
	// are bounded, and the goroutine releases the leg's abort context when it
	// exits (PDLegState.FinishDecodeAbort).
	r.pdLeg.Store(newPDLegState())
}

func (r *RoutingContext) debugWait() {
	if r.debugDelay > 0 {
		time.Sleep(r.debugDelay)
	}
}
