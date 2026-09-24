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

package routingalgorithms

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/vtc"
	"github.com/vllm-project/aibrix/pkg/types"
)

// This file owns the routing knob defaults and the single place where a model
// config profile's routingConfig turns into the request's resolved overrides:
//
//  1. configprofiles parses the profile's routingConfig once per request into
//     types.RoutingConfig, whose pointer fields keep "unset" visible.
//  2. ResolveRoutingOverrides applies it to a copy of the process defaults and
//     validates every value against the rule the matching AIBRIX_* variable
//     enforces. A value the environment would reject is dropped with a
//     warning, so a profile can sharpen a knob but never set one the
//     environment could not have.
//  3. Read sites use the concrete values with no default plumbing of their
//     own, for example:
//
//	queuedWeight := ctx.RoutingOverrides().LoadBalance.QueuedWeight
//
// A request whose profile sets no applicable value carries no overrides, and
// its reads land on the process default table installed below.

func init() {
	types.SetDefaultRoutingOverrides(processRoutingOverrides())
}

// processRoutingOverrides assembles the process defaults from the AIBRIX_*
// variables: the values every read site falls back to for a request whose
// profile sets no knob of that family.
//
// The pd and vtc packages own the knobs only they read, so they expose their
// environment defaults (pd.EnvOverrides, vtc.EnvOverrides); the rest are the
// package variables of the matching strategy.
func processRoutingOverrides() *types.RoutingOverrides {
	pdDefaults := pd.EnvOverrides()
	pdDefaults.Spreads = types.PDSpreadOverrides{
		PrefillLoadImbalanceMinSpread:      aibrixPrefillLoadImbalanceMinSpread,
		DecodeLoadImbalanceMinSpread:       aibrixDecodeLoadImbalanceMinSpread,
		DecodeThroughputImbalanceMinSpread: aibrixDecodeThroughputImbalanceMinSpread,
		DecodeScoreRatioThreshold:          aibrixDecodeScoreRatioThreshold,
	}
	pdDefaults.PromptLengthBucketing = aibrixPromptLengthBucketing
	pdDefaults.PrefillRequestTimeout = time.Duration(prefillRequestTimeout) * time.Second

	return &types.RoutingOverrides{
		LoadBalance: types.LoadBalanceOverrides{
			ImbalanceFactor: podRunningRequestImbalanceFactor,
			ImbalanceMinGap: podRunningRequestImbalanceMinGap,
			QueuedWeight:    loadBalanceQueuedWeight,
			KVPressureAlpha: loadBalanceKVPressureAlpha,
			KVCriticalFree:  loadBalanceKVCriticalFree,
		},
		PrefixCache: types.PrefixCacheOverrides{
			StandardDeviationFactor: standardDeviationFactor,
		},
		Preble: types.PrebleOverrides{
			TargetGPU:      targetGPU,
			DecodingLength: decodingLength,
		},
		VTC: vtc.EnvOverrides(),
		AutoBlend: types.AutoBlendOverrides{
			LoadBalanceWeight:            autoBlendLoadBalanceWeight,
			LeastRequestWeight:           autoBlendLeastRequestWeight,
			PrefixCacheWeight:            autoBlendPrefixCacheWeight,
			PrefixCacheLoadBalanceWeight: autoBlendPrefixCacheLoadBalanceWeight,
		},
		PD: pdDefaults,
	}
}

// ResolveRoutingOverrides turns this request's model config profile into its
// resolved routing overrides and parks them on the routing context, so every
// strategy and gate on the routing path reads the same values with a single
// resolve per request. It is a no-op when the profile sets no applicable knob:
// the read sites then read the process defaults directly.
func ResolveRoutingOverrides(routingCtx *types.RoutingContext) {
	if routingCtx == nil {
		return
	}
	var cfg *types.RoutingConfig
	if profile := routingCtx.ConfigProfile; profile != nil {
		cfg = profile.Routing
	}
	routingCtx.SetRoutingOverrides(resolveRoutingOverrides(cfg))
}

// resolveRoutingOverrides applies a profile's routingConfig to a copy of the
// process defaults. It returns nil when the profile sets no applicable value,
// including the case where every value it sets was rejected, so a request
// without effective overrides reads the defaults directly.
func resolveRoutingOverrides(cfg *types.RoutingConfig) *types.RoutingOverrides {
	if cfg == nil {
		return nil
	}
	ov := *types.DefaultRoutingOverrides()
	applied := false
	set := func(didApply bool) {
		if didApply {
			applied = true
		}
	}

	if lb := cfg.LoadBalance; lb != nil {
		set(apply(&ov.LoadBalance.ImbalanceFactor, "loadBalance.imbalanceFactor", lb.ImbalanceFactor, positive[float64]))
		set(apply(&ov.LoadBalance.ImbalanceMinGap, "loadBalance.imbalanceMinGap", lb.ImbalanceMinGap, positive[int]))
		set(apply(&ov.LoadBalance.QueuedWeight, "loadBalance.queuedWeight", lb.QueuedWeight, nonNegative[float64]))
		set(apply(&ov.LoadBalance.KVPressureAlpha, "loadBalance.kvPressureAlpha", lb.KVPressureAlpha, nonNegative[float64]))
		set(apply(&ov.LoadBalance.KVCriticalFree, "loadBalance.kvCriticalFree", lb.KVCriticalFree, inRange(0.0, 1.0)))
	}
	if pc := cfg.PrefixCache; pc != nil {
		set(apply(&ov.PrefixCache.StandardDeviationFactor, "prefixCache.standardDeviationFactor", pc.StandardDeviationFactor, positive[int]))
	}
	if pb := cfg.Preble; pb != nil {
		set(apply(&ov.Preble.TargetGPU, "preble.targetGPU", pb.TargetGPU, knownPrebleGPU))
		set(apply(&ov.Preble.DecodingLength, "preble.decodingLength", pb.DecodingLength, positive[int]))
	}
	if v := cfg.VTC; v != nil {
		set(apply(&ov.VTC.MaxPodLoad, "vtc.maxPodLoad", v.MaxPodLoad, positive[float64]))
		set(apply(&ov.VTC.FairnessWeight, "vtc.fairnessWeight", v.FairnessWeight, nonNegative[float64]))
		set(apply(&ov.VTC.UtilizationWeight, "vtc.utilizationWeight", v.UtilizationWeight, nonNegative[float64]))
		set(apply(&ov.VTC.InputTokenWeight, "vtc.inputTokenWeight", v.InputTokenWeight, positive[float64]))
		set(apply(&ov.VTC.OutputTokenWeight, "vtc.outputTokenWeight", v.OutputTokenWeight, positive[float64]))
		set(apply(&ov.VTC.TokenTracker.WindowSize, "vtc.tokenTrackerWindowSize", v.TokenTrackerWindowSize, positive[int]))
		set(apply(&ov.VTC.TokenTracker.TimeUnit, "vtc.tokenTrackerTimeUnit", v.TokenTrackerTimeUnit, knownVTCTimeUnit))
		set(apply(&ov.VTC.TokenTracker.MinTokens, "vtc.tokenTrackerMinTokens", v.TokenTrackerMinTokens, positive[float64]))
		set(apply(&ov.VTC.TokenTracker.MaxTokens, "vtc.tokenTrackerMaxTokens", v.TokenTrackerMaxTokens, positive[float64]))
	}
	if ab := cfg.AutoBlend; ab != nil {
		set(apply(&ov.AutoBlend.LoadBalanceWeight, "autoBlend.loadBalanceWeight", ab.LoadBalanceWeight, inRange(0, maxWeightCoefficient)))
		set(apply(&ov.AutoBlend.LeastRequestWeight, "autoBlend.leastRequestWeight", ab.LeastRequestWeight, inRange(0, maxWeightCoefficient)))
		set(apply(&ov.AutoBlend.PrefixCacheWeight, "autoBlend.prefixCacheWeight", ab.PrefixCacheWeight, inRange(1, maxWeightCoefficient)))
		set(apply(&ov.AutoBlend.PrefixCacheLoadBalanceWeight, "autoBlend.prefixCacheLoadBalanceWeight", ab.PrefixCacheLoadBalanceWeight, inRange(0, maxWeightCoefficient)))
	}
	set(applyAny(&ov.PD.PromptLengthBucketing, cfg.PromptLengthBucketing))
	if p := cfg.PD; p != nil {
		set(applySeconds(&ov.PD.Abort.Timeout, "pd.decodeAbortTimeout", p.DecodeAbortTimeout, nonNegative[int]))
		set(applySeconds(&ov.PD.Abort.RetryDelay, "pd.decodeAbortRetryDelay", p.DecodeAbortRetryDelay, nonNegative[int]))
		set(applySeconds(&ov.PD.PrefillRequestTimeout, "pd.prefillRequestTimeout", p.PrefillRequestTimeout, positive[int]))
		set(apply(&ov.PD.Spreads.PrefillLoadImbalanceMinSpread, "pd.prefillLoadImbalanceMinSpread", p.PrefillLoadImbalanceMinSpread, positive[int32]))
		set(apply(&ov.PD.Spreads.DecodeLoadImbalanceMinSpread, "pd.decodeLoadImbalanceMinSpread", p.DecodeLoadImbalanceMinSpread, positive[float64]))
		set(apply(&ov.PD.Spreads.DecodeThroughputImbalanceMinSpread, "pd.decodeThroughputImbalanceMinSpread", p.DecodeThroughputImbalanceMinSpread, positive[float64]))
		set(apply(&ov.PD.Spreads.DecodeScoreRatioThreshold, "pd.decodeScoreRatioThreshold", p.DecodeScoreRatioThreshold, positive[float64]))
		set(apply(&ov.PD.DecodeLB.WeightRunning, "pd.decodeLBWeightRunning", p.DecodeLBWeightRunning, positive[float64]))
		set(apply(&ov.PD.DecodeLB.WeightThroughput, "pd.decodeLBWeightThroughput", p.DecodeLBWeightThroughput, positive[float64]))
		set(apply(&ov.PD.HybridCacheLoadFactor, "pd.hybridCacheLoadFactor", p.HybridCacheLoadFactor, inRange(0.0, 1.0)))
		set(apply(&ov.PD.MinMatchPct, "pd.minMatchPct", p.MinMatchPct, inRange(0.0, 100.0)))
		set(apply(&ov.PD.TokenLoad.KVWeight, "pd.tokenLoadKVWeight", p.TokenLoadKVWeight, positive[float64]))
		set(apply(&ov.PD.TokenLoad.RequestCost, "pd.tokenLoadRequestCost", p.TokenLoadRequestCost, positive[float64]))
		set(applySeconds(&ov.PD.TokenLoad.TTL, "pd.tokenLoadTTLSeconds", p.TokenLoadTTLSeconds, nonNegative[int]))
		set(applySeconds(&ov.PD.TokenLoad.SessionTTL, "pd.tokenLoadSessionTTLSeconds", p.TokenLoadSessionTTLSeconds, nonNegative[int]))
	}

	if !applied {
		return nil
	}
	return &ov
}

// apply copies *v into *dst when v is set and passes ok, and returns whether it
// did. A value the environment would reject is dropped with a warning instead
// of landing on the routing path.
func apply[T any](dst *T, knob string, v *T, ok func(T) bool) bool {
	if v == nil {
		return false
	}
	if !ok(*v) {
		warnDroppedKnob(knob, *v)
		return false
	}
	*dst = *v
	return true
}

// applyAny is apply for knobs with no invalid value, such as the booleans.
func applyAny[T any](dst *T, v *T) bool {
	if v == nil {
		return false
	}
	*dst = *v
	return true
}

// applySeconds is apply for a knob expressed in seconds, which the resolved
// overrides carry as a duration.
func applySeconds(dst *time.Duration, knob string, v *int, ok func(int) bool) bool {
	if v == nil {
		return false
	}
	if !ok(*v) {
		warnDroppedKnob(knob, *v)
		return false
	}
	*dst = time.Duration(*v) * time.Second
	return true
}

// knownVTCTimeUnit accepts the bucket sizes the VTC token tracker knows. An
// unknown name would otherwise be normalized to minutes behind the profile's
// back, which is not what a profile that misspelled "seconds" asked for.
func knownVTCTimeUnit(v string) bool {
	return vtc.TimeUnitName(v) == v
}

// The predicates mirror the environment loaders: utils.LoadEnvInt and
// utils.LoadEnvFloat reject non-positive values, so a profile value they would
// reject must not reach the routing path either. NaN fails every comparison
// and is rejected the same way.
func positive[T int | int32 | float64](v T) bool { return v > 0 }
func nonNegative[T int | float64](v T) bool      { return v >= 0 }
func inRange[T int | float64](lo, hi T) func(T) bool {
	return func(v T) bool { return v >= lo && v <= hi }
}

// knownPrebleGPU accepts the GPU names the preble cost model knows. An unknown
// name would otherwise make every scored request log a warning and silently
// assume V100.
func knownPrebleGPU(v string) bool {
	return v == "A6000" || v == "V100"
}

// droppedKnobWarnings deduplicates the validation warnings: the resolver runs
// once per request, so a repeated profile value would otherwise repeat the same
// line for every request of the model. Keys come from model config values,
// which the deployment sets, not from clients.
var droppedKnobWarnings sync.Map

// warnDroppedKnob reports a profile value the matching AIBRIX_* variable would
// reject, once per distinct value.
func warnDroppedKnob(knob string, value any) {
	key := fmt.Sprintf("%s=%v", knob, value)
	if _, loaded := droppedKnobWarnings.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	klog.Warningf("ignoring invalid routingConfig value for %s (%v): the matching AIBRIX_* variable rejects it, so the process default stays in effect", knob, value)
}
