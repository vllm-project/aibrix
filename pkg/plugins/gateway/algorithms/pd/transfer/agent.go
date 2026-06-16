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
type KVTransferAgent interface {
	// Type returns the connector identifier (e.g. "shfs", "nixl", "mooncake").
	Type() string

	// AugmentPrefillRequest mutates completionRequest with any fields the prefill
	// pod requires before the HTTP request is sent (e.g. kv_transfer_params for SHFS).
	AugmentPrefillRequest(
		routingCtx *types.RoutingContext,
		prefillPod *v1.Pod,
		completionRequest map[string]any,
	) error

	// MergePrefillResponse injects connector-specific metadata from the prefill
	// response into routingCtx.ReqBody before the request is forwarded to the decode pod.
	MergePrefillResponse(
		routingCtx *types.RoutingContext,
		prefillResponse map[string]any,
		prefillPod *v1.Pod,
	) error
}
