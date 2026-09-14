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

// AugmentPrefillRequest replaces kv_transfer_params with the skeleton the
// prefill pod expects, so it populates remote block IDs for the decode side.
// Any client-supplied kv_transfer_params is dropped; nothing else in body changes.
func (a *SHFSAgent) AugmentPrefillRequest(
	_ *types.RoutingContext,
	_ *v1.Pod,
	body []byte,
) ([]byte, error) {
	return pd.NewJSONEditor(body).
		Delete("kv_transfer_params").
		Set("kv_transfer_params.do_remote_decode", true).
		Set("kv_transfer_params.do_remote_prefill", false).
		Set("kv_transfer_params.remote_engine_id", nil).
		Set("kv_transfer_params.remote_block_ids", nil).
		Set("kv_transfer_params.remote_host", nil).
		Set("kv_transfer_params.remote_port", nil).
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

	updatedReqBody, err := pd.NewJSONEditor(routingCtx.ReqBody).
		SetRaw("kv_transfer_params", []byte(kvTransferParams.Raw)).
		Set("kv_transfer_params.remote_host", prefillPod.Status.PodIP).
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
