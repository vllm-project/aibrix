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

package encode

import (
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Role is the role-name of encoder pods.
const Role = "encode"

// IsEncoder reports whether pod is an encoder. Encoders only run the encode
// stage, so they must never be picked to serve a request.
func IsEncoder(pod *v1.Pod) bool {
	return pod.Labels[pd.RoleLabel] == Role
}

// SplitEncoders returns the pods that can serve requests and the encoders,
// keeping the order of pods in each.
func SplitEncoders(pods []*v1.Pod) (serving, encoders []*v1.Pod) {
	for _, pod := range pods {
		if IsEncoder(pod) {
			encoders = append(encoders, pod)
		} else {
			serving = append(serving, pod)
		}
	}
	return serving, encoders
}

// EncodeMedia runs the encode stage for a request that target will serve:
// every image, audio and video item of a vLLM chat request is sent to one of
// the encoders in target's roleset first. target is the prefill pod in PD
// disaggregation, or the pod serving the whole request in E+PD.
//
// The media items stay on the request. Any ec_transfer_params the client sent
// are dropped, and the encoders' are set instead. vLLM finds the encoder
// output by the item's content hash or through those params, and encodes the
// item itself when the output isn't there. So an encode failure costs latency
// but not correctness, and is only logged.
func (e *Executor) EncodeMedia(ctx *types.RoutingContext, pods []*v1.Pod, target *v1.Pod, timeout time.Duration) {
	if e == nil || target == nil {
		return
	}
	if path, _, _ := strings.Cut(ctx.ReqPath, "?"); path != "/v1/chat/completions" {
		return
	}
	items := MediaItems(ctx.ReqBody)
	if len(items) == 0 {
		return
	}
	roleset := target.Labels[pd.RoleSetLabel]
	if roleset == "" {
		return
	}
	var encoders []*v1.Pod
	for _, pod := range pods {
		if pod.Labels[pd.RoleSetLabel] == roleset && IsEncoder(pod) && pd.IsPodWithHTTPServer(pod) {
			encoders = append(encoders, pod)
		}
	}
	if len(encoders) == 0 {
		return
	}
	params, err := e.Execute(ctx, encoders, items, timeout)
	if body, setErr := setTransferParams(ctx.ReqBody, params); setErr != nil {
		klog.V(2).InfoS("encode_transfer_params_failed", "error", setErr, "request_id", ctx.RequestID)
	} else {
		ctx.ReqBody = body
	}
	if err != nil {
		klog.V(2).InfoS("encode_request_failed", "error", err, "request_id", ctx.RequestID,
			"roleset", roleset, "media_items", len(items))
		return
	}
	klog.V(4).InfoS("encode_request_end", "request_id", ctx.RequestID,
		"roleset", roleset, "media_items", len(items))
}
