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

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
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

// ControlledFields returns kv_transfer_params, written on both bodies.
func (a *SHFSAgent) ControlledFields() []string { return []string{"kv_transfer_params"} }

// shfsPrefillKVTransferParams is the kv_transfer_params skeleton the prefill
// pod expects; it fills in remote_engine_id / remote_block_ids / remote_port
// for the decode side.
const shfsPrefillKVTransferParams = `{"do_remote_decode":true,"do_remote_prefill":false,` +
	`"remote_engine_id":null,"remote_block_ids":null,"remote_host":null,"remote_port":null}`

// AugmentPrefillRequest replaces kv_transfer_params with the skeleton the
// prefill pod expects, in a single top-level edit. Any client-supplied
// kv_transfer_params is dropped; nothing else in body changes.
func (a *SHFSAgent) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	body []byte,
) ([]byte, error) {
	return pd.NewJSONEditor(body).
		SetRaw("kv_transfer_params", []byte(shfsPrefillKVTransferParams)).
		Result()
}

// MergePrefillResponse copies kv_transfer_params verbatim from the prefill
// response into routingCtx.ReqBody, with remote_host set to the prefill pod
// IP, so the decode pod can pull the KV cache blocks. A response without
// kv_transfer_params leaves the decode body unchanged.
func (a *SHFSAgent) MergePrefillResponse(
	routingCtx *types.RoutingContext,
	prefillResponse []byte,
	prefillPod *v1.Pod,
) error {
	if err := pd.ValidateJSONObject(routingCtx.ReqBody, "original request body"); err != nil {
		return err
	}

	kvTransferParams := gjson.GetBytes(prefillResponse, "kv_transfer_params")
	if !kvTransferParams.Exists() {
		klog.InfoS("no kv_transfer_params in prefill response (SHFS)", "request_id", routingCtx.RequestID)
		return nil
	}
	if !kvTransferParams.IsObject() {
		return fmt.Errorf("kv_transfer_params has unexpected type %s, expected object", kvTransferParams.Type.String())
	}

	// Patch remote_host on the small kv_transfer_params fragment first, then
	// splice it into the (much larger) request body with a single edit.
	params, err := sjson.SetBytes([]byte(kvTransferParams.Raw), "remote_host", prefillPod.Status.PodIP)
	if err != nil {
		return fmt.Errorf("failed to set kv_transfer_params.remote_host: %w", err)
	}
	updatedReqBody, err := pd.NewJSONEditor(routingCtx.ReqBody).
		SetRaw("kv_transfer_params", params).
		Result()
	if err != nil {
		return fmt.Errorf("failed to update request body: %w", err)
	}
	routingCtx.ReqBody = updatedReqBody

	klog.InfoS("updated routing context with kv_transfer_params (SHFS)",
		"request_id", routingCtx.RequestID,
		"prefill_pod", prefillPod.Name,
		"prefill_host", prefillPod.Status.PodIP,
		"kv_connector_type", ConnectorTypeSHFS)
	return nil
}
