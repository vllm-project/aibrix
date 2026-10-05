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

package transfer

import (
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

// KVTransferAgent owns connector-specific request and response mutation for one
// KV transfer backend. Each implementation handles one backend (SHFS, NIXL, Mooncake, …).
//
// Both methods work on raw JSON bytes and must only touch top-level keys (or
// the gateway-owned kv_transfer_params object); see engine.EngineHandler for
// why a map[string]any round trip is not acceptable here.
type KVTransferAgent interface {
	// Type returns the connector identifier (e.g. "shfs", "nixl", "mooncake").
	Type() string

	// AugmentPrefillRequest returns body with any fields the prefill pod
	// requires before the HTTP request is sent (e.g. kv_transfer_params for
	// SHFS). body is the client request already validated as a JSON object.
	AugmentPrefillRequest(
		routingCtx *types.RoutingContext,
		prefillPod *v1.Pod,
		body []byte,
	) ([]byte, error)

	// MergePrefillResponse injects connector-specific metadata from the raw
	// prefill response into routingCtx.ReqBody before the request is forwarded
	// to the decode pod.
	MergePrefillResponse(
		routingCtx *types.RoutingContext,
		prefillResponse []byte,
		prefillPod *v1.Pod,
	) error

	// ControlledFields returns the top-level keys, in addition to
	// pd.CommonControlledFields, that AugmentPrefillRequest or
	// MergePrefillResponse may write. Client bodies repeating any of them
	// are rejected before routing.
	ControlledFields() []string
}
