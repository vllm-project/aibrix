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

package types

// RoutingConfig is the typed form of a model config profile's routingConfig.
// It is parsed once per request, when the model config profile is resolved
// (configprofiles.ParseRoutingConfig), and every consumer reads it through
// ResolvedConfigProfile.Routing: the auto-selection hints, the PD algorithm
// configuration and the per-request knobs all come from that single parse.
//
// Every field is a pointer so "unset" stays distinguishable from a meaningful
// zero. Values are validated when the routing overrides are resolved
// (routingalgorithms.ResolveRoutingOverrides) and not here, so a profile that
// sets a value the matching AIBRIX_* variable would reject keeps the
// environment default.
type RoutingConfig struct {
	// PromptTokensGte, PromptTokensLt, MaxTokensGte and MaxTokensLt are the
	// auto-selection hints of the "auto" config-profile header.
	PromptTokensGte *int   `json:"promptTokensGte,omitempty"`
	PromptTokensLt  *int   `json:"promptTokensLt,omitempty"`
	MaxTokensGte    *int64 `json:"maxTokensGte,omitempty"`
	MaxTokensLt     *int64 `json:"maxTokensLt,omitempty"`

	// PromptLenBucketMinLength, PromptLenBucketMaxLength and Combined configure
	// prompt-length bucketing of the PD prefill scoring.
	PromptLenBucketMinLength *int  `json:"promptLenBucketMinLength,omitempty"`
	PromptLenBucketMaxLength *int  `json:"promptLenBucketMaxLength,omitempty"`
	Combined                 *bool `json:"combined,omitempty"`
	// PromptLengthBucketing overrides AIBRIX_PROMPT_LENGTH_BUCKETING for
	// requests routed with this profile. Flat, next to the promptLenBucket*
	// fields it interacts with.
	PromptLengthBucketing *bool `json:"promptLengthBucketing,omitempty"`
	// BucketServe and BucketServeMode override AIBRIX_BUCKET_SERVE and
	// AIBRIX_BUCKET_SERVE_MODE for requests routed with this profile. Flat,
	// next to the bucketing switch whose candidates the plan re-orders.
	BucketServe     *bool   `json:"bucketServe,omitempty"`
	BucketServeMode *string `json:"bucketServeMode,omitempty"`
	// PrefillScorePolicy and DecodeScorePolicy select the PD scoring policies
	// for requests routed with this profile.
	PrefillScorePolicy string `json:"prefillScorePolicy,omitempty"`
	DecodeScorePolicy  string `json:"decodeScorePolicy,omitempty"`

	// LoadBalance, PrefixCache, Preble, VTC and AutoBlend mirror the knob
	// families of the matching routing strategies.
	LoadBalance *LoadBalanceProfileConfig `json:"loadBalance,omitempty"`
	PrefixCache *PrefixCacheProfileConfig `json:"prefixCache,omitempty"`
	Preble      *PrebleProfileConfig      `json:"preble,omitempty"`
	VTC         *VTCProfileConfig         `json:"vtc,omitempty"`
	AutoBlend   *AutoBlendProfileConfig   `json:"autoBlend,omitempty"`

	// PD carries the prefill/decode knobs in one flat object, so
	// "routingConfig.pd.decodeAbortTimeout" reads as one knob instead of a
	// four-level path.
	PD *PDProfileConfig `json:"pd,omitempty"`
}

// LoadBalanceProfileConfig mirrors the AIBRIX_LOAD_BALANCE_* knobs of the
// load-balance score and its imbalance gate.
type LoadBalanceProfileConfig struct {
	ImbalanceFactor *float64 `json:"imbalanceFactor,omitempty"`
	ImbalanceMinGap *int     `json:"imbalanceMinGap,omitempty"`
	QueuedWeight    *float64 `json:"queuedWeight,omitempty"`
	KVPressureAlpha *float64 `json:"kvPressureAlpha,omitempty"`
	KVCriticalFree  *float64 `json:"kvCriticalFree,omitempty"`
}

// PrefixCacheProfileConfig mirrors AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR.
type PrefixCacheProfileConfig struct {
	StandardDeviationFactor *int `json:"standardDeviationFactor,omitempty"`
}

// PrebleProfileConfig mirrors the prefix-cache-preble cost-model knobs that are
// read per request.
type PrebleProfileConfig struct {
	TargetGPU      *string `json:"targetGPU,omitempty"`
	DecodingLength *int    `json:"decodingLength,omitempty"`
}

// VTCProfileConfig mirrors the vtc-basic score knobs that are read per request.
type VTCProfileConfig struct {
	MaxPodLoad        *float64 `json:"maxPodLoad,omitempty"`
	FairnessWeight    *float64 `json:"fairnessWeight,omitempty"`
	UtilizationWeight *float64 `json:"utilizationWeight,omitempty"`
	// InputTokenWeight and OutputTokenWeight override
	// AIBRIX_ROUTER_VTC_BASIC_INPUT_TOKEN_WEIGHT and
	// AIBRIX_ROUTER_VTC_BASIC_OUTPUT_TOKEN_WEIGHT: the weights the token
	// tracker applies to a request's input and output tokens.
	InputTokenWeight  *float64 `json:"inputTokenWeight,omitempty"`
	OutputTokenWeight *float64 `json:"outputTokenWeight,omitempty"`
	// TokenTrackerWindowSize, TokenTrackerTimeUnit, TokenTrackerMinTokens and
	// TokenTrackerMaxTokens override AIBRIX_ROUTER_VTC_TOKEN_TRACKER_*: the
	// sliding window the token counts are bucketed in and the floors the
	// tracker reports while the window holds little activity. Profiles that
	// set any of the four get their own tracker, weights included, so one
	// model's window and weights never mix with another's.
	TokenTrackerWindowSize *int     `json:"tokenTrackerWindowSize,omitempty"`
	TokenTrackerTimeUnit   *string  `json:"tokenTrackerTimeUnit,omitempty"`
	TokenTrackerMinTokens  *float64 `json:"tokenTrackerMinTokens,omitempty"`
	TokenTrackerMaxTokens  *float64 `json:"tokenTrackerMaxTokens,omitempty"`
}

// AutoBlendProfileConfig mirrors the AIBRIX_ROUTING_AUTO_BLEND_* weights.
type AutoBlendProfileConfig struct {
	LoadBalanceWeight            *int `json:"loadBalanceWeight,omitempty"`
	LeastRequestWeight           *int `json:"leastRequestWeight,omitempty"`
	PrefixCacheWeight            *int `json:"prefixCacheWeight,omitempty"`
	PrefixCacheLoadBalanceWeight *int `json:"prefixCacheLoadBalanceWeight,omitempty"`
}

// PDProfileConfig mirrors the prefill/decode knobs a profile may set under
// routingConfig.pd. Each field maps to one AIBRIX_* variable the PD path reads;
// types.PDOverrides documents the mapping. A knob the profile leaves unset, or
// sets to a value the matching environment variable would reject, keeps the
// environment default.
type PDProfileConfig struct {
	// DecodeAbortTimeout and DecodeAbortRetryDelay override
	// AIBRIX_DECODE_ABORT_TIMEOUT and AIBRIX_DECODE_ABORT_RETRY_DELAY. Zero is
	// meaningful for both: it disables decode aborts and reduces the abort to a
	// single attempt.
	DecodeAbortTimeout    *int `json:"decodeAbortTimeout,omitempty"`
	DecodeAbortRetryDelay *int `json:"decodeAbortRetryDelay,omitempty"`
	// PrefillRequestTimeout overrides AIBRIX_PREFILL_REQUEST_TIMEOUT.
	PrefillRequestTimeout *int `json:"prefillRequestTimeout,omitempty"`
	// PrefillLoadImbalanceMinSpread, DecodeLoadImbalanceMinSpread,
	// DecodeThroughputImbalanceMinSpread and DecodeScoreRatioThreshold mirror
	// the four load-imbalance thresholds of the prefill and decode fast paths.
	PrefillLoadImbalanceMinSpread      *int32   `json:"prefillLoadImbalanceMinSpread,omitempty"`
	DecodeLoadImbalanceMinSpread       *float64 `json:"decodeLoadImbalanceMinSpread,omitempty"`
	DecodeThroughputImbalanceMinSpread *float64 `json:"decodeThroughputImbalanceMinSpread,omitempty"`
	DecodeScoreRatioThreshold          *float64 `json:"decodeScoreRatioThreshold,omitempty"`
	// DecodeLBWeightRunning and DecodeLBWeightThroughput override
	// AIBRIX_DECODE_LB_WEIGHT_RUNNING and AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT.
	DecodeLBWeightRunning    *float64 `json:"decodeLBWeightRunning,omitempty"`
	DecodeLBWeightThroughput *float64 `json:"decodeLBWeightThroughput,omitempty"`
	// HybridCacheLoadFactor and MinMatchPct override
	// AIBRIX_HYBRID_CACHE_LOAD_FACTOR and AIBRIX_MIN_MATCH_PCT.
	HybridCacheLoadFactor *float64 `json:"hybridCacheLoadFactor,omitempty"`
	MinMatchPct           *float64 `json:"minMatchPct,omitempty"`
	// TokenLoadKVWeight, TokenLoadRequestCost, TokenLoadTTLSeconds and
	// TokenLoadSessionTTLSeconds override the AIBRIX_TOKEN_LOAD_* knobs. Zero is
	// meaningful for both TTLs: it disables, respectively, the sweep of the
	// request's charge and the session-delta rule.
	TokenLoadKVWeight          *float64 `json:"tokenLoadKVWeight,omitempty"`
	TokenLoadRequestCost       *float64 `json:"tokenLoadRequestCost,omitempty"`
	TokenLoadTTLSeconds        *int     `json:"tokenLoadTTLSeconds,omitempty"`
	TokenLoadSessionTTLSeconds *int     `json:"tokenLoadSessionTTLSeconds,omitempty"`
}
