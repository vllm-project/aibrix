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

package gateway

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
)

// priorityTiers maps the tier a request declares through the
// x-aibrix-priority-tier header to the priority the gateway sets on the
// upstream vLLM request. vLLM serves smaller priority values first and preempts
// the largest one, so every entry below only de-prioritizes: a request that
// declares no tier, or a tier outside this table, keeps the engine default 0.
// The table is fixed on purpose for now; per-tier values a deployment can
// tune, and an explicit priority for latency-sensitive tiers, are follow-ups.
var priorityTiers = map[string]int{
	"batch":      100,
	"background": 1000,
}

// priorityForTier returns the upstream priority mapped to a declared tier. Tier
// names are matched case-insensitively; the RequestHeaders phase already trims
// the header value.
func priorityForTier(tier string) (int, bool) {
	priority, ok := priorityTiers[strings.ToLower(tier)]
	return priority, ok
}

// applyPriorityTier rewrites routingCtx.ReqBody in place when the deployment
// opted in, the request declares a tier this gateway maps, and the caller did
// not set the priority itself.
//
// The rewrite happens before a pod is selected, so every reader of the routing
// context sees the same body: the body that is forwarded upstream, and the
// prefill and decode bodies the PD router derives from it while it selects the
// pods (see pd/prefill.PreparePayload). Callers therefore never have to pass a
// separately rewritten body around.
func (s *Server) applyPriorityTier(routingCtx *types.RoutingContext) {
	if !s.priorityTier {
		return
	}

	tier := routingCtx.ReqHeaders[HeaderPriorityTier]
	priority, ok := priorityForTier(tier)
	if !ok {
		return
	}

	body := routingCtx.ReqBody
	// sjson extends whatever it is handed: given a JSON scalar or array it
	// returns a fresh object instead of an error, which would replace a body
	// this feature must not touch. Only a well formed JSON object is extended;
	// anything else is forwarded untouched, like every other body the mapping
	// does not apply to.
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return
	}

	// A priority the caller already set is an explicit choice and wins.
	if gjson.GetBytes(body, "priority").Exists() {
		return
	}

	// sjson keeps the rest of the body byte-for-byte, the same way the PD path
	// edits its payloads.
	rewritten, err := sjson.SetBytes(body, "priority", priority)
	if err != nil {
		// A body sjson cannot extend is forwarded untouched: the tier is a
		// routing hint and never fails a request.
		klog.V(4).InfoS("request priority not applied", "requestID", routingCtx.RequestID, "tier", tier, "error", err)
		return
	}
	routingCtx.ReqBody = rewritten
	klog.V(4).InfoS("request priority applied", "requestID", routingCtx.RequestID, "tier", tier, "priority", priority)
}
