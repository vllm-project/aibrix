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

func init() {
	Register(ConnectorTypeMooncake, func() KVTransferAgent { return &MooncakeAgent{} })
}

// MooncakeAgent implements KVTransferAgent for the Mooncake KV transfer backend.
// TODO: implement based on confirmed vLLM MooncakeConnector contract.
type MooncakeAgent struct{}

func (a *MooncakeAgent) Type() string { return ConnectorTypeMooncake }

// AugmentPrefillRequest adds any Mooncake-specific fields to the prefill request.
// TODO: implement once vLLM MooncakeConnector prefill request contract is confirmed.
func (a *MooncakeAgent) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	_ map[string]any,
) error {
	return nil
}

// MergePrefillResponse injects Mooncake-specific metadata from the prefill response
// into routingCtx.ReqBody before the request is forwarded to the decode pod.
// TODO: implement once vLLM MooncakeConnector response contract is confirmed.
func (a *MooncakeAgent) MergePrefillResponse(
	_ *types.RoutingContext,
	_ map[string]any,
	_ *v1.Pod,
) error {
	return nil
}
