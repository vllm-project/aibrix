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
	Register(ConnectorTypeSHFS, func() KVTransferAgent { return &SHFSAgent{} })
}

// SHFSAgent implements KVTransferAgent for the AIBrix SHFS/KVCacheManager backend (GPU).
type SHFSAgent struct{}

func (a *SHFSAgent) Type() string { return ConnectorTypeSHFS }

// AugmentPrefillRequest adds a kv_transfer_params skeleton so the prefill pod
// knows to populate remote block IDs for the decode side.
func (a *SHFSAgent) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	completionRequest map[string]any,
) error {
	completionRequest["kv_transfer_params"] = map[string]any{
		"do_remote_decode":  true,
		"do_remote_prefill": false,
		"remote_engine_id":  nil,
		"remote_block_ids":  nil,
		"remote_host":       nil,
		"remote_port":       nil,
	}
	return nil
}

// MergePrefillResponse extracts kv_transfer_params from the prefill response,
// sets remote_host to the prefill pod IP, and writes the merged params into
// routingCtx.ReqBody so the decode pod can pull the KV cache blocks.
func (a *SHFSAgent) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	prefillResponse map[string]any,
	prefillPod *v1.Pod,
) error {
	var originalRequest map[string]any
	if err := sonic.Unmarshal(routingCtx.ReqBody, &originalRequest); err != nil {
		return fmt.Errorf("failed to unmarshal original request body: %w", err)
	}

	kvTransferParams, exists := prefillResponse["kv_transfer_params"]
	if !exists {
		klog.InfoS("no kv_transfer_params in prefill response (SHFS)", "request_id", routingCtx.RequestID)
		return nil
	}

	kvTransferParamsMap, ok := kvTransferParams.(map[string]any)
	if !ok {
		return fmt.Errorf("kv_transfer_params has unexpected type %T, expected map[string]any", kvTransferParams)
	}
	kvTransferParamsMap["remote_host"] = prefillPod.Status.PodIP

	originalRequest["kv_transfer_params"] = kvTransferParams
	updatedReqBody, err := sonic.Marshal(originalRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal updated request body: %w", err)
	}
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with kv_transfer_params (SHFS)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP,
		"kv_connector_type", ConnectorTypeSHFS)
	return nil
}
