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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

var (
	nilPod       = &v1.Pod{}
	unknownError = errors.New("unknown error")
)

type RequestFeatures []float64

// RoutingAlgorithms defines the routing algorithms
type RoutingAlgorithm string

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	context.Context
	Algorithm   RoutingAlgorithm
	Model       string
	Message     string
	RequestID   string
	User        *string
	RequestTime time.Time // Time when the routing context is created.
	PendingLoad float64   // Normalized pending load of request, available after AddRequestCount call.
	TraceTerm   int64     // Trace term identifier, available after AddRequestCount call.
	RoutedTime  time.Time

	targetPodSet chan struct{}
	targetPod    atomic.Pointer[v1.Pod]
	lastError    atomic.Pointer[error]
	debugDelay   time.Duration
	tokens       []int
	predictor    OutputPredictor
	statsUpdated int32 // Use to flag if in-memory realtime statistics has been updated for the request.
	traceAdded   int32 // Use to flag if trace has been added to cache
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

// SetOutputPreditor enables RoutingContext to use existing OutputPredictor to predict output length.
func (r *RoutingContext) SetOutputPreditor(predictor OutputPredictor) (old OutputPredictor) {
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
		r.lastError.Store(&unknownError)
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
		case <-r.Context.Done():
			r.SetError(r.Context.Err())
		case <-r.targetPodSet: // No blocking if targetPod is set after last "targetPod == nil"
		}
		targetPod = r.targetPod.Load()
	}

	return targetPod
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

// CanUpdateStats returns true if the first time trying update in-memory realtime statistics.
func (r *RoutingContext) CanUpdateStats() bool {
	return atomic.CompareAndSwapInt32(&r.statsUpdated, 0, 1)
}

// CanAddTrace returns true if the first time trying add trace to cache.
func (r *RoutingContext) CanAddTrace() bool {
	return atomic.CompareAndSwapInt32(&r.traceAdded, 0, 1)
}

// GetRoutingDelay returns the time duration used for routing the request.
func (r *RoutingContext) GetRoutingDelay() time.Duration {
	return r.RoutedTime.Sub(r.RequestTime)
}

func (r *RoutingContext) targetAddress(pod *v1.Pod) string {
	return fmt.Sprintf("%v:%v", pod.Status.PodIP, utils.GetModelPortForPod(r.RequestID, pod))
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
	r.Message = message
	r.RequestID = requestID
	if user != "" {
		r.User = &user
	} else {
		r.User = nil
	}
	r.RequestTime = time.Now()
	r.PendingLoad = 0
	r.TraceTerm = 0
	// RoutedTime will not be reset, it must before ReqeustTime at this time.

	r.targetPodSet = make(chan struct{}) // Initialize channel
	r.targetPod.Store(nilPod)
	r.lastError.Store(nil)
	// debugDelay will be reset by tests.
	r.tokens = nil
	r.predictor = nil
	r.statsUpdated = 0
}

func (r *RoutingContext) debugWait() {
	if r.debugDelay > 0 {
		time.Sleep(r.debugDelay)
	}
}
