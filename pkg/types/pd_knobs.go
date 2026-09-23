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

import "time"

// PDOverrides carries one request's resolved prefill/decode (PD) routing
// knobs. Like RoutingOverrides it holds concrete values only: the process
// defaults with the profile's overrides applied, resolved once per request at
// the profile boundary. The PD path reads the same values from the request's
// PD leg (PDLegState.PDOverrides), which parks a copy when the PD route starts
// because the async prefill and decode-abort paths outlive the pooled routing
// context.
//
// AIBRIX_TOKEN_LOAD_MAX_SESSIONS has no field here on purpose: it gates the
// shared session table of the token-load tracker, which every model of a
// gateway process shares, so a per-request cap would not be a real per-model
// limit. It keeps its environment-only semantics.
type PDOverrides struct {
	Abort     PDAbortOverrides
	Spreads   PDSpreadOverrides
	DecodeLB  PDDecodeLBOverrides
	TokenLoad PDTokenLoadOverrides
	// HybridCacheLoadFactor overrides AIBRIX_HYBRID_CACHE_LOAD_FACTOR: the
	// fraction of the hybrid prefix-cache/load score the cache term contributes.
	HybridCacheLoadFactor float64
	// MinMatchPct overrides AIBRIX_MIN_MATCH_PCT: the minimum prefix-match
	// percentage (0 to 100) a candidate needs to score as a cache hit.
	MinMatchPct float64
	// PromptLengthBucketing overrides AIBRIX_PROMPT_LENGTH_BUCKETING for
	// requests routed with this profile.
	PromptLengthBucketing bool
	// PrefillRequestTimeout overrides AIBRIX_PREFILL_REQUEST_TIMEOUT: the
	// deadline of this request's prefill HTTP call.
	PrefillRequestTimeout time.Duration
}

// PDAbortOverrides mirrors AIBRIX_DECODE_ABORT_TIMEOUT and
// AIBRIX_DECODE_ABORT_RETRY_DELAY. Zero is a meaningful value for both: it
// disables decode aborts, and it reduces the abort to a single attempt.
type PDAbortOverrides struct {
	// Timeout is the deadline of the decode abort of a failed prefill.
	Timeout time.Duration
	// RetryDelay is the delay between the two abort attempts.
	RetryDelay time.Duration
}

// PDSpreadOverrides mirrors the four load-imbalance thresholds of the prefill
// and decode fast paths.
type PDSpreadOverrides struct {
	// PrefillLoadImbalanceMinSpread overrides
	// AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD.
	PrefillLoadImbalanceMinSpread int32
	// DecodeLoadImbalanceMinSpread overrides
	// AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD.
	DecodeLoadImbalanceMinSpread float64
	// DecodeThroughputImbalanceMinSpread overrides
	// AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD.
	DecodeThroughputImbalanceMinSpread float64
	// DecodeScoreRatioThreshold overrides
	// AIBRIX_DECODE_SCORE_RATIO_THRESHOLD: the max/min drain-rate score ratio
	// above which the decode fast path treats a replica as a hotspot.
	DecodeScoreRatioThreshold float64
}

// PDDecodeLBOverrides mirrors AIBRIX_DECODE_LB_WEIGHT_RUNNING and
// AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT, the weights of the decode score terms.
type PDDecodeLBOverrides struct {
	WeightRunning    float64
	WeightThroughput float64
}

// PDTokenLoadOverrides mirrors the AIBRIX_TOKEN_LOAD_* knobs of the
// token-load prefill policy. Zero is meaningful for both TTLs: it disables,
// respectively, the sweep of the request's charge and the session-delta rule.
type PDTokenLoadOverrides struct {
	// KVWeight is the weight of the KV-token cost term.
	KVWeight float64
	// RequestCost is the fixed cost charged per request, in tokens.
	RequestCost float64
	// TTL bounds how long a request's charge stays in the tracker.
	TTL time.Duration
	// SessionTTL bounds how long a session's last prompt is remembered.
	SessionTTL time.Duration
}
