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
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

func init() {
	Register(ConnectorTypeNIXL, func() KVTransferAgent { return &NIXLAgent{} })
}

// NIXLAgent implements KVTransferAgent for the NIXL backend (Neuron).
type NIXLAgent struct{}

func (a *NIXLAgent) Type() string { return ConnectorTypeNIXL }

// AugmentPrefillRequest is a no-op for NIXL: the backend manages KV transfer
// through its own mechanism and does not require a prefill request skeleton.
func (a *NIXLAgent) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	_ map[string]any,
) error {
	return nil
}

// MergePrefillResponse wraps the entire prefill response under disagg_prefill_resp
// so the NixlConnector on the decode side can locate and pull the KV blocks.
func (a *NIXLAgent) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	prefillResponse map[string]any,
	prefillPod *v1.Pod,
) error {
	var originalRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}

	originalRequest["disagg_prefill_resp"] = prefillResponse
	updatedReqBody, err := sonic.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with disagg_prefill_resp (NIXL)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP,
		"kv_connector_type", ConnectorTypeNIXL)
	return nil
}
