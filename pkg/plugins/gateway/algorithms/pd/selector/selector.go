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

// Package selector defines the PodSelector contract for PD-disaggregated routing.
// A PodSelector takes the full list of ready pods and returns the chosen prefill
// pod (may be nil for combined-role routing) and decode pod for a given request.
//
// The DefaultSelector returned by NewDefaultSelector wraps a scoring function
// supplied by the router; future phases will move the scoring logic here so it
// can be composed and tested independently of the router.
package selector

import (
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// PodSelector picks a prefill/decode pod pair from the full ready-pod list.
// A nil prefill pod signals combined-role routing (decode pod handles both phases).
type PodSelector interface {
	Select(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (prefill *v1.Pod, decode *v1.Pod, err error)
}

// SelectFunc is a function signature matching PodSelector.Select.
type SelectFunc func(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (*v1.Pod, *v1.Pod, error)

// DefaultSelector wraps a SelectFunc as a PodSelector.
// This lets the router register its existing filterPrefillDecodePods method
// behind the interface without moving the implementation prematurely.
type DefaultSelector struct {
	fn SelectFunc
}

// NewDefaultSelector returns a PodSelector backed by fn.
func NewDefaultSelector(fn SelectFunc) PodSelector {
	return &DefaultSelector{fn: fn}
}

func (s *DefaultSelector) Select(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
	return s.fn(routingCtx, readyPods)
}
