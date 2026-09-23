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

import "time"

// PDRuntimeKnobs carries the prefill/decode (PD) routing knobs a model config
// profile sets for one request. It is the per-request counterpart of the
// AIBRIX_* variables the PD path reads: the environment stays the default, and
// a profile that sets a knob overrides it for its own requests only.
//
// Every field is a pointer so that "unset" is distinguishable from a knob
// whose meaningful value is zero, and every accessor is nil-receiver-safe, so a
// read site can apply an override without a nil check of its own:
//
//	wRun := routingCtx.PDKnobs().DecodeLBWeightRunningOrDefault(envWeight)
//
// A nil *PDRuntimeKnobs, or a nil field, means the read site keeps the default
// it passed, which is the value the gateway read from the environment. The
// knobs are resolved from the profile's routingConfig by the PD algorithm
// (effectivePDKnobs) and parked on the request's PDLegState, so the parts of
// the PD path that outlive the routing context - the async prefill goroutine
// and the decode abort it may start - read the same values.
type PDRuntimeKnobs struct {
	// DecodeAbortTimeoutSeconds overrides AIBRIX_DECODE_ABORT_TIMEOUT: the
	// per-attempt deadline of the decode /abort_request POST, in seconds.
	// 0 disables decode aborts for the request.
	DecodeAbortTimeoutSeconds *int
	// DecodeAbortRetryDelaySeconds overrides AIBRIX_DECODE_ABORT_RETRY_DELAY:
	// the wait before the single repeated abort, in seconds. 0 sends one
	// attempt only.
	DecodeAbortRetryDelaySeconds *int
	// PrefillRequestTimeoutSeconds overrides AIBRIX_PREFILL_REQUEST_TIMEOUT:
	// the deadline of the prefill HTTP call, in seconds.
	PrefillRequestTimeoutSeconds *int
	// PrefillLoadImbalanceMinSpread overrides
	// AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD: the outstanding-prefill count
	// spread above which the least-loaded prefill pod is picked directly.
	PrefillLoadImbalanceMinSpread *int32
	// DecodeLoadImbalanceMinSpread overrides
	// AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD.
	DecodeLoadImbalanceMinSpread *float64
	// DecodeThroughputImbalanceMinSpread overrides
	// AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD.
	DecodeThroughputImbalanceMinSpread *float64
	// DecodeScoreRatioThreshold overrides
	// AIBRIX_DECODE_SCORE_RATIO_THRESHOLD: the max/min drain-rate score ratio
	// above which the slowest decode pod is avoided.
	DecodeScoreRatioThreshold *float64
	// PromptLengthBucketing overrides AIBRIX_PROMPT_LENGTH_BUCKETING.
	PromptLengthBucketing *bool
	// DecodeLBWeightRunning overrides AIBRIX_DECODE_LB_WEIGHT_RUNNING.
	DecodeLBWeightRunning *float64
	// DecodeLBWeightThroughput overrides
	// AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT.
	DecodeLBWeightThroughput *float64
	// HybridCacheLoadFactor overrides AIBRIX_HYBRID_CACHE_LOAD_FACTOR.
	HybridCacheLoadFactor *float64
	// MinMatchPct overrides AIBRIX_MIN_MATCH_PCT, the minimum prefix-match
	// percentage shared by the prefix_cache and hybrid_cache_load policies.
	MinMatchPct *float64
	// TokenLoadKVWeight overrides AIBRIX_TOKEN_LOAD_KV_WEIGHT: the weight of
	// resident KV tokens in the token-load priority.
	TokenLoadKVWeight *float64
	// TokenLoadRequestCost overrides AIBRIX_TOKEN_LOAD_REQUEST_COST: the fixed
	// per-request cost added to every token-load charge.
	TokenLoadRequestCost *float64
	// TokenLoadTTLSeconds overrides AIBRIX_TOKEN_LOAD_TTL_SECONDS: the maximum
	// age of the request's charge. 0 disables the sweep for it.
	TokenLoadTTLSeconds *int
	// TokenLoadSessionTTLSeconds overrides
	// AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS: how long a session's last prompt
	// size is remembered. 0 disables the session-delta rule for the request.
	TokenLoadSessionTTLSeconds *int
	// TokenLoadMaxSessions overrides AIBRIX_TOKEN_LOAD_MAX_SESSIONS.
	TokenLoadMaxSessions *int
}

// DecodeAbortTimeoutOrDefault returns the profile's decode abort deadline, or
// def (normally the environment-derived default) when the request sets none.
func (k *PDRuntimeKnobs) DecodeAbortTimeoutOrDefault(def time.Duration) time.Duration {
	if k == nil || k.DecodeAbortTimeoutSeconds == nil {
		return def
	}
	return time.Duration(*k.DecodeAbortTimeoutSeconds) * time.Second
}

// DecodeAbortRetryDelayOrDefault returns the profile's abort retry delay, or
// def (normally the environment-derived default) when the request sets none.
func (k *PDRuntimeKnobs) DecodeAbortRetryDelayOrDefault(def time.Duration) time.Duration {
	if k == nil || k.DecodeAbortRetryDelaySeconds == nil {
		return def
	}
	return time.Duration(*k.DecodeAbortRetryDelaySeconds) * time.Second
}

// PrefillRequestTimeoutOrDefault returns the profile's prefill deadline, or
// def (normally the environment-derived default) when the request sets none.
func (k *PDRuntimeKnobs) PrefillRequestTimeoutOrDefault(def time.Duration) time.Duration {
	if k == nil || k.PrefillRequestTimeoutSeconds == nil {
		return def
	}
	return time.Duration(*k.PrefillRequestTimeoutSeconds) * time.Second
}

// PrefillLoadImbalanceMinSpreadOrDefault returns the profile's prefill
// imbalance spread, or def when the request sets none.
func (k *PDRuntimeKnobs) PrefillLoadImbalanceMinSpreadOrDefault(def int32) int32 {
	if k == nil || k.PrefillLoadImbalanceMinSpread == nil {
		return def
	}
	return *k.PrefillLoadImbalanceMinSpread
}

// DecodeLoadImbalanceMinSpreadOrDefault returns the profile's decode
// request-count imbalance spread, or def when the request sets none.
func (k *PDRuntimeKnobs) DecodeLoadImbalanceMinSpreadOrDefault(def float64) float64 {
	if k == nil || k.DecodeLoadImbalanceMinSpread == nil {
		return def
	}
	return *k.DecodeLoadImbalanceMinSpread
}

// DecodeThroughputImbalanceMinSpreadOrDefault returns the profile's decode
// throughput imbalance spread, or def when the request sets none.
func (k *PDRuntimeKnobs) DecodeThroughputImbalanceMinSpreadOrDefault(def float64) float64 {
	if k == nil || k.DecodeThroughputImbalanceMinSpread == nil {
		return def
	}
	return *k.DecodeThroughputImbalanceMinSpread
}

// DecodeScoreRatioThresholdOrDefault returns the profile's drain-rate score
// ratio threshold, or def when the request sets none.
func (k *PDRuntimeKnobs) DecodeScoreRatioThresholdOrDefault(def float64) float64 {
	if k == nil || k.DecodeScoreRatioThreshold == nil {
		return def
	}
	return *k.DecodeScoreRatioThreshold
}

// PromptLengthBucketingOrDefault returns the profile's prompt-length
// bucketing switch, or def when the request sets none.
func (k *PDRuntimeKnobs) PromptLengthBucketingOrDefault(def bool) bool {
	if k == nil || k.PromptLengthBucketing == nil {
		return def
	}
	return *k.PromptLengthBucketing
}

// DecodeLBWeightRunningOrDefault returns the profile's running-request weight
// of the load_balancing decode score, or def when the request sets none.
func (k *PDRuntimeKnobs) DecodeLBWeightRunningOrDefault(def float64) float64 {
	if k == nil || k.DecodeLBWeightRunning == nil {
		return def
	}
	return *k.DecodeLBWeightRunning
}

// DecodeLBWeightThroughputOrDefault returns the profile's throughput weight of
// the load_balancing decode score, or def when the request sets none.
func (k *PDRuntimeKnobs) DecodeLBWeightThroughputOrDefault(def float64) float64 {
	if k == nil || k.DecodeLBWeightThroughput == nil {
		return def
	}
	return *k.DecodeLBWeightThroughput
}

// HybridCacheLoadFactorOrDefault returns the profile's hybrid_cache_load
// discount factor, or def when the request sets none.
func (k *PDRuntimeKnobs) HybridCacheLoadFactorOrDefault(def float64) float64 {
	if k == nil || k.HybridCacheLoadFactor == nil {
		return def
	}
	return *k.HybridCacheLoadFactor
}

// MinMatchPctOrDefault returns the profile's minimum prefix-match percentage,
// or def when the request sets none.
func (k *PDRuntimeKnobs) MinMatchPctOrDefault(def float64) float64 {
	if k == nil || k.MinMatchPct == nil {
		return def
	}
	return *k.MinMatchPct
}

// TokenLoadKVWeightOrDefault returns the profile's KV weight of the token-load
// priority, or def when the request sets none.
func (k *PDRuntimeKnobs) TokenLoadKVWeightOrDefault(def float64) float64 {
	if k == nil || k.TokenLoadKVWeight == nil {
		return def
	}
	return *k.TokenLoadKVWeight
}

// TokenLoadRequestCostOrDefault returns the profile's fixed per-request
// token-load cost, or def when the request sets none.
func (k *PDRuntimeKnobs) TokenLoadRequestCostOrDefault(def float64) float64 {
	if k == nil || k.TokenLoadRequestCost == nil {
		return def
	}
	return *k.TokenLoadRequestCost
}

// TokenLoadTTLOrDefault returns the profile's charge TTL, or def when the
// request sets none.
func (k *PDRuntimeKnobs) TokenLoadTTLOrDefault(def time.Duration) time.Duration {
	if k == nil || k.TokenLoadTTLSeconds == nil {
		return def
	}
	return time.Duration(*k.TokenLoadTTLSeconds) * time.Second
}

// TokenLoadSessionTTLOrDefault returns the profile's session TTL, or def when
// the request sets none.
func (k *PDRuntimeKnobs) TokenLoadSessionTTLOrDefault(def time.Duration) time.Duration {
	if k == nil || k.TokenLoadSessionTTLSeconds == nil {
		return def
	}
	return time.Duration(*k.TokenLoadSessionTTLSeconds) * time.Second
}

// TokenLoadMaxSessionsOrDefault returns the profile's session-table cap, or
// def when the request sets none.
func (k *PDRuntimeKnobs) TokenLoadMaxSessionsOrDefault(def int) int {
	if k == nil || k.TokenLoadMaxSessions == nil {
		return def
	}
	return *k.TokenLoadMaxSessions
}
