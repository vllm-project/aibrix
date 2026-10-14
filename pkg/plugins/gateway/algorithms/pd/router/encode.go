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

package router

import (
	"strings"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// encodeMedia runs the encode stage when the prefill pod's roleset has encode
// pods: every image, audio and video item of a vLLM chat request is sent to one
// of them before prefill.
//
// The request body is left untouched. vLLM's prefill instance looks the encoder
// output up by the hash of the item's content, the same hash the encoder stored
// it under, and encodes the item itself when the output isn't there. So an
// encode failure costs latency but not correctness, and is only logged.
func (r *pdRouter) encodeMedia(ctx *types.RoutingContext, readyPods []*v1.Pod, prefillPod *v1.Pod) {
	if r.encodeExecutor == nil || ctx.Engine != VLLMEngine {
		return
	}
	if path, _, _ := strings.Cut(ctx.ReqPath, "?"); path != "/v1/chat/completions" {
		return
	}
	items := encode.MediaItems(ctx.ReqBody)
	if len(items) == 0 {
		return
	}
	b := groupPDPodsByRoleset(readyPods)[prefillPod.Labels[PDRoleSetIdentifier]]
	if b == nil || len(b.encodes) == 0 {
		return
	}
	if err := r.encodeExecutor.Execute(ctx, b.encodes, items, ctx.PDOverrides().PrefillRequestTimeout); err != nil {
		klog.ErrorS(err, "encode_request_failed", "request_id", ctx.RequestID,
			"roleset", prefillPod.Labels[PDRoleSetIdentifier], "media_items", len(items))
		return
	}
	klog.V(4).InfoS("encode_request_end", "request_id", ctx.RequestID,
		"roleset", prefillPod.Labels[PDRoleSetIdentifier], "media_items", len(items))
}
