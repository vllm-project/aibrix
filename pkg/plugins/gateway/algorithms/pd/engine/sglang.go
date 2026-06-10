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
	"fmt"
	"math/rand"
	"strconv"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

const (
	sglangBootstrapPort           int64  = 8998
	sglangBootstrapPortIdentifier string = "model.aibrix.ai/sglang-bootstrap-port"
)

func init() {
	Register(&SGLangHandler{})
}

// SGLangHandler implements EngineHandler for SGLang.
// SGLang uses a bootstrap handshake (bootstrap_host/port/room) to coordinate
// KV transfer between prefill and decode pods out-of-band, so the prefill
// fires asynchronously (IsAsync returns true).
type SGLangHandler struct{}

func (h *SGLangHandler) Name() string  { return "sglang" }
func (h *SGLangHandler) IsAsync() bool { return true }

// AugmentPrefillRequest injects bootstrap_host, bootstrap_port, and a random
// bootstrap_room. It also propagates these fields into routingCtx.ReqBody so
// the decode pod receives the bootstrap address.
func (h *SGLangHandler) AugmentPrefillRequest(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	completionRequest map[string]any,
) error {
	completionRequest["bootstrap_host"] = pod.Status.PodIP
	completionRequest["bootstrap_port"] = sglangBootstrapPortFor(pod)
	completionRequest["bootstrap_room"] = rand.Int63n(1<<63 - 1)

	// Propagate bootstrap fields to the decode request body so the decode pod
	// can locate the prefill pod's bootstrap server.
	reqBody, err := sonic.Marshal(completionRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal SGLang prefill request body: %w", err)
	}
	routingCtx.ReqBody = reqBody
	return nil
}

// MergePrefillResponse is a no-op for SGLang: the bootstrap handshake is
// self-coordinating, so no data from the prefill response needs to be injected
// into the decode request.
func (h *SGLangHandler) MergePrefillResponse(
	_ *types.RoutingContext,
	_ map[string]any,
	_ *v1.Pod,
) error {
	return nil
}

func sglangBootstrapPortFor(pod *v1.Pod) int64 {
	if portStr, exists := pod.Annotations[sglangBootstrapPortIdentifier]; exists {
		if port, err := strconv.ParseInt(portStr, 10, 32); err == nil {
			return port
		}
	}
	return sglangBootstrapPort
}
