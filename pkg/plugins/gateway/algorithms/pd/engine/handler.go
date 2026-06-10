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

package engine

import (
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// EngineHandler encapsulates engine-specific behaviour for PD-disaggregated
// prefill: how to augment the outbound prefill request body and how to merge
// the prefill response back into the decode request body.
type EngineHandler interface {
	// Name returns the engine identifier (e.g. "vllm", "sglang", "trtllm").
	Name() string
	// IsAsync returns true when the engine's prefill handshake is
	// self-coordinating and should be fired in a goroutine (e.g. SGLang).
	IsAsync() bool
	// AugmentPrefillRequest adds engine-specific fields to completionRequest
	// before it is POSTed to the prefill pod.
	AugmentPrefillRequest(routingCtx *types.RoutingContext, pod *v1.Pod, completionRequest map[string]any) error
	// MergePrefillResponse injects engine-specific data from the prefill
	// response into routingCtx.ReqBody before the decode pod receives it.
	MergePrefillResponse(routingCtx *types.RoutingContext, responseData map[string]any, pod *v1.Pod) error
}
