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

package types

// RoutingKnobs carries the routing knobs a model config profile sets for one
// request outside the prefill/decode (PD) path. It is the per-request
// counterpart of the AIBRIX_* variables those strategies read: the environment
// stays the default, and a profile that sets a knob overrides it for its own
// requests only.
//
// Every field is a pointer so that "unset" is distinguishable from a knob
// whose meaningful value is zero, and every accessor is nil-receiver-safe, so a
// read site can apply an override without a nil check of its own:
//
//	factor := routingCtx.RoutingKnobs().LoadBalanceImbalanceFactorOrDefault(envFactor)
//
// A nil *RoutingKnobs, or a nil field, means the read site keeps the default it
// passed, which is the value the gateway read from the environment. The knobs
// are resolved from the profile's routingConfig once per request by
// algorithms.ResolveRoutingKnobs and parked on the RoutingContext, because a
// single request may consult several strategies - a multi-strategy blend, the
// load-imbalance gate, the PD prefill scoring - that must agree on the values.
//
// Knobs that configure process-wide state instead of a routing decision - the
// Preble eviction loop and histogram window, the VTC token tracker's window,
// time unit, token weights and min/max floors, and the session-affinity local
// cache capacity - are deliberately not part of this struct: all models of a
// gateway process share that state, so profiled values could not be applied
// per request without corrupting it. Those keep their environment-only
// semantics.
type RoutingKnobs struct {
	// LoadBalanceImbalanceFactor overrides AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR:
	// the factor of the mean over which the load-imbalance gate flags the
	// busiest replica as a hotspot. Used for pools of three or more replicas.
	LoadBalanceImbalanceFactor *float64
	// LoadBalanceImbalanceMinGap overrides
	// AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP: the minimum absolute gap between
	// the busiest and the least busy replica required to trigger the gate.
	LoadBalanceImbalanceMinGap *int
	// LoadBalanceQueuedWeight overrides AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT: the
	// weight of queued requests in the load-balance score. 0 is the V1
	// formula, running requests only.
	LoadBalanceQueuedWeight *float64
	// LoadBalanceKVPressureAlpha overrides
	// AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA: the strength of the quadratic
	// KV-pressure penalty of the load-balance score. 0 drops the penalty.
	LoadBalanceKVPressureAlpha *float64
	// LoadBalanceKVCriticalFree overrides
	// AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE: the free-KV fraction below which a
	// replica scores +Inf in the load-balance score. 0 disables the guardrail.
	LoadBalanceKVCriticalFree *float64
	// PrefixCacheStandardDeviationFactor overrides
	// AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR: how many standard
	// deviations above the mean replica request count a prefix-match candidate
	// may sit before it is skipped. Read by prefix-cache and by the PD prefill
	// scorer.
	PrefixCacheStandardDeviationFactor *int
	// PrebleTargetGPU overrides AIBRIX_ROUTER_PREBLE_TARGET_GPU: the GPU the
	// prefix-cache-preble cost model assumes for its replicas. "A6000" and
	// "V100" are the known values.
	PrebleTargetGPU *string
	// PrebleDecodingLength overrides AIBRIX_ROUTER_PREBLE_DECODING_LENGTH: the
	// assumed number of decoding tokens per request of the preble cost model.
	PrebleDecodingLength *int
	// VTCMaxPodLoad overrides AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD: the
	// running-request count at which the VTC utilization score saturates.
	VTCMaxPodLoad *float64
	// VTCFairnessWeight overrides AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT: the
	// weight of the fairness term in the VTC score. 0 drops the term.
	VTCFairnessWeight *float64
	// VTCUtilizationWeight overrides
	// AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT: the weight of the
	// utilization term in the VTC score. 0 drops the term.
	VTCUtilizationWeight *float64
	// AutoBlendLoadBalanceWeight overrides
	// AIBRIX_ROUTING_AUTO_BLEND_LOAD_BALANCE_WEIGHT: the weight of the
	// load-balance scorer the gateway silently blends behind every
	// non-exclusive strategy. 0 disables the auto-blend for the request.
	AutoBlendLoadBalanceWeight *int
	// AutoBlendLeastRequestWeight overrides
	// AIBRIX_ROUTING_AUTO_BLEND_LEAST_REQUEST_WEIGHT: the weight of the
	// least-request scorer the auto-blend adds for multi-port pods. 0
	// disables that addition.
	AutoBlendLeastRequestWeight *int
	// AutoBlendPrefixCacheWeight overrides
	// AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_WEIGHT: the prefix-cache weight of
	// the dedicated prefix-cache/load-balance ratio a bare "prefix-cache"
	// request gets. 0 is rejected: it would drop the caller's own strategy
	// from the blend.
	AutoBlendPrefixCacheWeight *int
	// AutoBlendPrefixCacheLoadBalanceWeight overrides
	// AIBRIX_ROUTING_AUTO_BLEND_PREFIX_CACHE_LOAD_BALANCE_WEIGHT: the
	// load-balance weight of that ratio. 0 is accepted and leaves those
	// requests with prefix-cache scoring alone.
	AutoBlendPrefixCacheLoadBalanceWeight *int
}

// LoadBalanceImbalanceFactorOrDefault returns the profile's imbalance factor,
// or def when the request sets none.
func (k *RoutingKnobs) LoadBalanceImbalanceFactorOrDefault(def float64) float64 {
	if k == nil || k.LoadBalanceImbalanceFactor == nil {
		return def
	}
	return *k.LoadBalanceImbalanceFactor
}

// LoadBalanceImbalanceMinGapOrDefault returns the profile's imbalance min gap,
// or def when the request sets none.
func (k *RoutingKnobs) LoadBalanceImbalanceMinGapOrDefault(def int) int {
	if k == nil || k.LoadBalanceImbalanceMinGap == nil {
		return def
	}
	return *k.LoadBalanceImbalanceMinGap
}

// LoadBalanceQueuedWeightOrDefault returns the profile's queued-request weight,
// or def when the request sets none.
func (k *RoutingKnobs) LoadBalanceQueuedWeightOrDefault(def float64) float64 {
	if k == nil || k.LoadBalanceQueuedWeight == nil {
		return def
	}
	return *k.LoadBalanceQueuedWeight
}

// LoadBalanceKVPressureAlphaOrDefault returns the profile's KV-pressure
// penalty strength, or def when the request sets none.
func (k *RoutingKnobs) LoadBalanceKVPressureAlphaOrDefault(def float64) float64 {
	if k == nil || k.LoadBalanceKVPressureAlpha == nil {
		return def
	}
	return *k.LoadBalanceKVPressureAlpha
}

// LoadBalanceKVCriticalFreeOrDefault returns the profile's critical free-KV
// fraction, or def when the request sets none.
func (k *RoutingKnobs) LoadBalanceKVCriticalFreeOrDefault(def float64) float64 {
	if k == nil || k.LoadBalanceKVCriticalFree == nil {
		return def
	}
	return *k.LoadBalanceKVCriticalFree
}

// PrefixCacheStandardDeviationFactorOrDefault returns the profile's
// standard-deviation factor, or def when the request sets none.
func (k *RoutingKnobs) PrefixCacheStandardDeviationFactorOrDefault(def int) int {
	if k == nil || k.PrefixCacheStandardDeviationFactor == nil {
		return def
	}
	return *k.PrefixCacheStandardDeviationFactor
}

// PrebleTargetGPUOrDefault returns the profile's preble target GPU, or def when
// the request sets none.
func (k *RoutingKnobs) PrebleTargetGPUOrDefault(def string) string {
	if k == nil || k.PrebleTargetGPU == nil {
		return def
	}
	return *k.PrebleTargetGPU
}

// PrebleDecodingLengthOrDefault returns the profile's assumed decoding length,
// or def when the request sets none.
func (k *RoutingKnobs) PrebleDecodingLengthOrDefault(def int) int {
	if k == nil || k.PrebleDecodingLength == nil {
		return def
	}
	return *k.PrebleDecodingLength
}

// VTCMaxPodLoadOrDefault returns the profile's VTC saturation load, or def when
// the request sets none.
func (k *RoutingKnobs) VTCMaxPodLoadOrDefault(def float64) float64 {
	if k == nil || k.VTCMaxPodLoad == nil {
		return def
	}
	return *k.VTCMaxPodLoad
}

// VTCFairnessWeightOrDefault returns the profile's VTC fairness weight, or def
// when the request sets none.
func (k *RoutingKnobs) VTCFairnessWeightOrDefault(def float64) float64 {
	if k == nil || k.VTCFairnessWeight == nil {
		return def
	}
	return *k.VTCFairnessWeight
}

// VTCUtilizationWeightOrDefault returns the profile's VTC utilization weight,
// or def when the request sets none.
func (k *RoutingKnobs) VTCUtilizationWeightOrDefault(def float64) float64 {
	if k == nil || k.VTCUtilizationWeight == nil {
		return def
	}
	return *k.VTCUtilizationWeight
}

// AutoBlendLoadBalanceWeightOrDefault returns the profile's auto-blend
// load-balance weight, or def when the request sets none.
func (k *RoutingKnobs) AutoBlendLoadBalanceWeightOrDefault(def int) int {
	if k == nil || k.AutoBlendLoadBalanceWeight == nil {
		return def
	}
	return *k.AutoBlendLoadBalanceWeight
}

// AutoBlendLeastRequestWeightOrDefault returns the profile's auto-blend
// least-request weight, or def when the request sets none.
func (k *RoutingKnobs) AutoBlendLeastRequestWeightOrDefault(def int) int {
	if k == nil || k.AutoBlendLeastRequestWeight == nil {
		return def
	}
	return *k.AutoBlendLeastRequestWeight
}

// AutoBlendPrefixCacheWeightOrDefault returns the profile's prefix-cache weight
// of the prefix-cache/load-balance ratio, or def when the request sets none.
func (k *RoutingKnobs) AutoBlendPrefixCacheWeightOrDefault(def int) int {
	if k == nil || k.AutoBlendPrefixCacheWeight == nil {
		return def
	}
	return *k.AutoBlendPrefixCacheWeight
}

// AutoBlendPrefixCacheLoadBalanceWeightOrDefault returns the profile's
// load-balance weight of the prefix-cache/load-balance ratio, or def when the
// request sets none.
func (k *RoutingKnobs) AutoBlendPrefixCacheLoadBalanceWeightOrDefault(def int) int {
	if k == nil || k.AutoBlendPrefixCacheLoadBalanceWeight == nil {
		return def
	}
	return *k.AutoBlendPrefixCacheLoadBalanceWeight
}

// SetRoutingKnobs records the request's non-PD routing overrides. A nil knobs
// argument is a no-op: the read sites fall back to their environment defaults.
func (r *RoutingContext) SetRoutingKnobs(knobs *RoutingKnobs) {
	if r == nil || knobs == nil {
		return
	}
	r.routingKnobs.Store(knobs)
}

// RoutingKnobs returns the request's non-PD routing overrides, or nil when the
// profile sets none.
func (r *RoutingContext) RoutingKnobs() *RoutingKnobs {
	if r == nil {
		return nil
	}
	return r.routingKnobs.Load()
}
