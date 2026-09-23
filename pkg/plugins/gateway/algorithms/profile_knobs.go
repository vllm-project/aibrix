/*
Copyright 2024 The Aibrix Team.

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

package routingalgorithms

import (
	"encoding/json"
	"math"

	"github.com/bytedance/sonic"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
)

// routingProfileConfig holds the non-PD routing knobs a model config profile
// may set under routingConfig. Each group mirrors a family of AIBRIX_*
// variables the matching strategy reads: types.RoutingKnobs documents the
// mapping knob by knob. A knob the profile leaves unset, or sets to a value the
// matching environment variable would reject, keeps the environment default, so
// a profile can only narrow or sharpen routing behaviour, never silently drop a
// threshold.
//
// Knobs that configure process-wide state - the Preble eviction loop and
// histogram window, the VTC token tracker's window, time unit, token weights
// and min/max floors, and the session-affinity local cache capacity - have no
// group here on purpose: all models of a gateway process share that state, so a
// profiled value could not be applied per request without corrupting it. Those
// keep their environment-only semantics.
type routingProfileConfig struct {
	LoadBalance *loadBalanceProfileConfig `json:"loadBalance,omitempty"`
	PrefixCache *prefixCacheProfileConfig `json:"prefixCache,omitempty"`
	Preble      *prebleProfileConfig      `json:"preble,omitempty"`
	VTC         *vtcProfileConfig         `json:"vtc,omitempty"`
	AutoBlend   *autoBlendProfileConfig   `json:"autoBlend,omitempty"`
}

// loadBalanceProfileConfig mirrors the load-balance gate and score knobs.
type loadBalanceProfileConfig struct {
	ImbalanceFactor *float64 `json:"imbalanceFactor,omitempty"`
	ImbalanceMinGap *int     `json:"imbalanceMinGap,omitempty"`
	QueuedWeight    *float64 `json:"queuedWeight,omitempty"`
	KVPressureAlpha *float64 `json:"kvPressureAlpha,omitempty"`
	KVCriticalFree  *float64 `json:"kvCriticalFree,omitempty"`
}

// prefixCacheProfileConfig mirrors AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR.
type prefixCacheProfileConfig struct {
	StandardDeviationFactor *int `json:"standardDeviationFactor,omitempty"`
}

// prebleProfileConfig mirrors the prefix-cache-preble cost-model knobs that are
// read per request.
type prebleProfileConfig struct {
	TargetGPU      *string `json:"targetGPU,omitempty"`
	DecodingLength *int    `json:"decodingLength,omitempty"`
}

// vtcProfileConfig mirrors the vtc-basic score knobs that are read per request.
type vtcProfileConfig struct {
	MaxPodLoad        *float64 `json:"maxPodLoad,omitempty"`
	FairnessWeight    *float64 `json:"fairnessWeight,omitempty"`
	UtilizationWeight *float64 `json:"utilizationWeight,omitempty"`
}

// autoBlendProfileConfig mirrors the AIBRIX_ROUTING_AUTO_BLEND_* weights.
type autoBlendProfileConfig struct {
	LoadBalanceWeight            *int `json:"loadBalanceWeight,omitempty"`
	LeastRequestWeight           *int `json:"leastRequestWeight,omitempty"`
	PrefixCacheWeight            *int `json:"prefixCacheWeight,omitempty"`
	PrefixCacheLoadBalanceWeight *int `json:"prefixCacheLoadBalanceWeight,omitempty"`
}

// parseRoutingProfileConfig parses the non-PD routing knobs out of the generic
// RoutingConfig. It returns nil when raw is empty or unparsable, which keeps
// every knob at its environment default.
func parseRoutingProfileConfig(raw json.RawMessage) *routingProfileConfig {
	if len(raw) == 0 {
		return nil
	}
	var cfg routingProfileConfig
	if err := sonic.Unmarshal(raw, &cfg); err != nil {
		klog.ErrorS(err, "failed to unmarshal routing profile config, using environment defaults", "rawConfig", string(raw))
		return nil
	}
	return &cfg
}

// nonNegativeFloat returns v when it is zero or positive and nil otherwise. Use
// it for weights and strengths whose zero setting is documented, such as the
// load-balance queued weight (0 is the V1 formula) and the KV-pressure alpha
// (0 drops the penalty).
func nonNegativeFloat(v *float64) *float64 {
	if v == nil || *v < 0 || math.IsNaN(*v) {
		return nil
	}
	return v
}

// intInRange returns v when it lies within [lo, hi] and nil otherwise. Use it
// for the auto-blend weights, whose range mirrors the coefficient bounds
// ParseMultiRouterConfig enforces on a routing string: a weight outside them
// would produce a blend string the router then refuses to parse.
func intInRange(v *int, lo, hi int) *int {
	if v == nil || *v < lo || *v > hi {
		return nil
	}
	return v
}

// prebleTargetGPU returns v when it names a GPU the preble cost model knows and
// nil otherwise. An unknown name would otherwise make every scored request log
// a warning and silently assume V100.
func prebleTargetGPU(v *string) *string {
	if v == nil {
		return nil
	}
	switch *v {
	case "A6000", "V100":
		return v
	}
	return nil
}

// runtimeKnobs converts a parsed profile into the per-request non-PD routing
// overrides. It returns nil when the profile sets no such knob at all, so a
// request without one pays a single nil check on the routing path. Values are
// validated here, at the profile boundary, instead of at every read site.
func (c *routingProfileConfig) runtimeKnobs() *types.RoutingKnobs {
	if c == nil || (c.LoadBalance == nil && c.PrefixCache == nil && c.Preble == nil && c.VTC == nil && c.AutoBlend == nil) {
		return nil
	}
	knobs := &types.RoutingKnobs{}
	if lb := c.LoadBalance; lb != nil {
		knobs.LoadBalanceImbalanceFactor = positiveFloat(lb.ImbalanceFactor)
		knobs.LoadBalanceImbalanceMinGap = positiveInt(lb.ImbalanceMinGap)
		knobs.LoadBalanceQueuedWeight = nonNegativeFloat(lb.QueuedWeight)
		knobs.LoadBalanceKVPressureAlpha = nonNegativeFloat(lb.KVPressureAlpha)
		knobs.LoadBalanceKVCriticalFree = floatInRange(lb.KVCriticalFree, 0, 1)
	}
	if pc := c.PrefixCache; pc != nil {
		knobs.PrefixCacheStandardDeviationFactor = positiveInt(pc.StandardDeviationFactor)
	}
	if pb := c.Preble; pb != nil {
		knobs.PrebleTargetGPU = prebleTargetGPU(pb.TargetGPU)
		knobs.PrebleDecodingLength = positiveInt(pb.DecodingLength)
	}
	if v := c.VTC; v != nil {
		knobs.VTCMaxPodLoad = positiveFloat(v.MaxPodLoad)
		knobs.VTCFairnessWeight = nonNegativeFloat(v.FairnessWeight)
		knobs.VTCUtilizationWeight = nonNegativeFloat(v.UtilizationWeight)
	}
	if ab := c.AutoBlend; ab != nil {
		knobs.AutoBlendLoadBalanceWeight = intInRange(ab.LoadBalanceWeight, 0, maxWeightCoefficient)
		knobs.AutoBlendLeastRequestWeight = intInRange(ab.LeastRequestWeight, 0, maxWeightCoefficient)
		knobs.AutoBlendPrefixCacheWeight = intInRange(ab.PrefixCacheWeight, 1, maxWeightCoefficient)
		knobs.AutoBlendPrefixCacheLoadBalanceWeight = intInRange(ab.PrefixCacheLoadBalanceWeight, 0, maxWeightCoefficient)
	}
	return knobs
}

// effectiveRoutingKnobs returns the non-PD routing overrides of this request's
// model config profile, or nil when the profile sets none.
func effectiveRoutingKnobs(routingCtx *types.RoutingContext) *types.RoutingKnobs {
	if routingCtx == nil || routingCtx.ConfigProfile == nil || len(routingCtx.ConfigProfile.RoutingConfig) == 0 {
		return nil
	}
	return parseRoutingProfileConfig(routingCtx.ConfigProfile.RoutingConfig).runtimeKnobs()
}

// ResolveRoutingKnobs parses this request's non-PD routing overrides from its
// model config profile and parks them on the routing context, so the strategies
// and gates on the routing path read the same validated values with a single
// parse per request. It is a no-op when the profile sets none, and the read
// sites fall back to their environment defaults in that case.
func ResolveRoutingKnobs(routingCtx *types.RoutingContext) {
	routingCtx.SetRoutingKnobs(effectiveRoutingKnobs(routingCtx))
}
