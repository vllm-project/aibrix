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

	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// Role is the role-name of encoder pods.
	Role = "encode"

	roleLabel          = "role-name"
	rolesetLabel       = "roleset-name"
	podGroupIndexLabel = "stormservice.orchestration.aibrix.ai/pod-group-index"
)

// IsEncoder reports whether pod is an encoder. Encoders only run the encode
// stage, so they must never be picked to serve a request.
func IsEncoder(pod *v1.Pod) bool {
	return pod.Labels[roleLabel] == Role
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
// The request body is left untouched. vLLM looks the encoder output up by the
// hash of the item's content, the same hash the encoder stored it under, and
// encodes the item itself when the output isn't there. So an encode failure
// costs latency but not correctness, and is only logged.
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
	roleset := target.Labels[rolesetLabel]
	if roleset == "" {
		return
	}
	var encoders []*v1.Pod
	for _, pod := range pods {
		if pod.Labels[rolesetLabel] == roleset && IsEncoder(pod) && hasHTTPServer(pod) {
			encoders = append(encoders, pod)
		}
	}
	if len(encoders) == 0 {
		return
	}
	if err := e.Execute(ctx, encoders, items, timeout); err != nil {
		klog.V(2).InfoS("encode_request_failed", "error", err, "request_id", ctx.RequestID,
			"roleset", roleset, "media_items", len(items))
		return
	}
	klog.V(4).InfoS("encode_request_end", "request_id", ctx.RequestID,
		"roleset", roleset, "media_items", len(items))
}

// hasHTTPServer reports whether pod runs the HTTP server of its pod group: in
// a multi-node group only the first pod does.
func hasHTTPServer(pod *v1.Pod) bool {
	index, ok := pod.Labels[podGroupIndexLabel]
	return !ok || index == "0"
}
