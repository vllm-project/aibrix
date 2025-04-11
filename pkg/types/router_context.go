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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
)

const podMetricPort = "8000"

var nilPod = &v1.Pod{}

// RoutingAlgorithm defines the routing algorithms
type RoutingAlgorithm string

// RoutingContext encapsulates the context information required for routing.
// It can be extended with more fields as needed in the future.
type RoutingContext struct {
	context.Context
	Algorithm RoutingAlgorithm
	Model     string
	Message   string
	RequestID string
	User      *string

	targetPodSet chan struct{}
	targetPod    atomic.Pointer[v1.Pod]
	debugDelay   time.Duration
}

var requestPool = sync.Pool{
	New: func() any { return &RoutingContext{} },
}

func (alg RoutingAlgorithm) NewContext(ctx context.Context, model string, message string, requestID string, user string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	var userPtr *string
	if user != "" {
		userPtr = &user
	}
	request.reset(ctx, alg, model, message, requestID, userPtr)
	return request
}

func NewRoutingContext(ctx context.Context, algorithms RoutingAlgorithm, model string, message string, requestID string, user string) *RoutingContext {
	request := requestPool.Get().(*RoutingContext)
	var userPtr *string
	if user != "" {
		userPtr = &user
	}
	request.reset(ctx, algorithms, model, message, requestID, userPtr)
	return request
}

func (r *RoutingContext) Delete() {
	r.SetTargetPod(nil) // Unblock waiting TargetPod() call
	requestPool.Put(r)
}

func (r *RoutingContext) SetTargetPod(pod *v1.Pod) {
	if r.targetPod.CompareAndSwap(nilPod, pod) { // Use CompareAndSwap to ensure close channel only once
		close(r.targetPodSet) // Notify waiting TargetPod() call
	}
}

func (r *RoutingContext) TargetPod() *v1.Pod {
	targetPod := r.targetPod.Load()
	if targetPod == nilPod {
		r.debugWait()
		<-r.targetPodSet // No blocking if targetPod is set after last "targetPod == nil"
		targetPod = r.targetPod.Load()
	}

	return targetPod
}

func (r *RoutingContext) TargetAddress() string {
	return r.targetAddress(r.TargetPod())
}

func (r *RoutingContext) HasRouted() bool {
	return r.targetPod.Load() != nilPod
}

func (r *RoutingContext) targetAddress(pod *v1.Pod) string {
	return fmt.Sprintf("%v:%v", pod.Status.PodIP, podMetricPort)
}

func (r *RoutingContext) reset(ctx context.Context, algorithms RoutingAlgorithm, model string, message string, requestID string, user *string) {
	r.Context = ctx
	r.Algorithm = algorithms
	r.Model = model
	r.Message = message
	r.RequestID = requestID
	r.User = user
	r.targetPodSet = make(chan struct{}) // Initialize channel
	r.targetPod.Store(nilPod)
}

func (r *RoutingContext) debugWait() {
	if r.debugDelay > 0 {
		time.Sleep(r.debugDelay)
	}
}
