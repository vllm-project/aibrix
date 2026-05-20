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

// Package pd contains the scoring types used by the PD (prefill-decode)
// disaggregated-inference router. The scoring logic is split across three
// files:
//
//   - prefill_scorer.go  — PrefillScorePolicy / PrefillScorer interfaces and
//     their built-in implementations (prefix_cache, least_request).
//   - decode_scorer.go   — DecodeScorePolicy / DecodeScorer interfaces and
//     their built-in implementations (load_balancing, least_request), plus the
//     policy registry used by AIBRIX_DECODE_SCORE_POLICY.
//   - trackers.go        — PrefillRequestTracker and PendingDecodeTracker,
//     which bridge the gap between pod selection and actual request start.
package pd

import (
	"fmt"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// PrefillScorePolicyPrefixCache selects the prefix-cache scoring policy,
	// which routes prefill requests to pods that already hold matching KV-cache
	// blocks, weighted by the pod's current running-request count.
	PrefillScorePolicyPrefixCache = "prefix_cache"

	// PrefillScorePolicyLeastRequest selects the least-request scoring policy,
	// which routes prefill requests purely by the lowest running-request count
	// without consulting the prefix cache.
	PrefillScorePolicyLeastRequest = "least_request"
)

// PrefillScorer is a request-scoped scorer created by PrefillScorePolicy. Prepare
// for a single request. Each call to Prepare returns a fresh instance; no state
// is shared across concurrent requests.
type PrefillScorer interface {
	// ScorePod returns a score for pod (lower is better). reqCnt is the pod's
	// current running-request count; maxRequestCount is the maximum across all
	// candidate pods and is used for normalization. Implementations may use pod
	// for logging or metadata lookups (e.g. prefix_cache); implementations that
	// score purely by count (e.g. least_request) may ignore it.
	ScorePod(pod *v1.Pod, reqCnt, maxRequestCount float64) float64

	// PrefixHashes returns the token-prefix hashes used to warm the prefix-cache
	// index after a pod is selected. Returns nil when the policy does not use
	// the prefix cache (e.g. least_request).
	PrefixHashes() []uint64
}

// PrefillScorePolicy is the stateless factory for per-request PrefillScorers.
// The policy itself holds only immutable config (e.g. tokenizer and cache index
// handles). All per-request state is captured inside the PrefillScorer returned
// by Prepare, so the policy is safe to share across concurrent goroutines.
//
// To add a new prefill scoring strategy: implement this interface and register
// it in NewPDRouter (pd_disaggregation.go) by handling the new policy name in
// the AIBRIX_PREFILL_SCORE_POLICY switch statement.
type PrefillScorePolicy interface {
	// Prepare is called once per request. pods and readyPodsMap represent the
	// same candidate set; readyPodsMap is provided for O(1) name lookups.
	// Returns an error only when scoring cannot proceed at all (e.g. tokenization
	// failure); in that case the router falls back to skipping the request.
	Prepare(routingCtx *types.RoutingContext, pods []*v1.Pod, readyPodsMap map[string]struct{}) (PrefillScorer, error)

	// Name returns the policy identifier used in log lines and metrics.
	Name() string
}

// prefixCachePrefillPolicy scores prefill pods by prefix-cache hit percentage
// combined with their current running-request count:
//
//	score = (100 - matchPercent) * 0.1 + reqCnt / maxReqCnt
//
// A pod with a 100 % cache match contributes 0.0 from the cache term, so its
// final score is determined solely by load. A pod with no match contributes
// 10.0 from the cache term, making it significantly less preferred.
//
// The policy is stateless: tok and prefixCacheIndexer are read-only handles
// shared across all requests. Obtain an instance via NewPrefixCachePrefillPolicy.
type prefixCachePrefillPolicy struct {
	tok                tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable
}

// NewPrefixCachePrefillPolicy constructs a prefix_cache PrefillScorePolicy with
// the given tokenizer and shared prefix-hash table.
func NewPrefixCachePrefillPolicy(tok tokenizer.Tokenizer, prefixCacheIndexer *prefixcacheindexer.PrefixHashTable) PrefillScorePolicy {
	return &prefixCachePrefillPolicy{
		tok:                tok,
		prefixCacheIndexer: prefixCacheIndexer,
	}
}

// Prepare tokenizes routingCtx.Message, performs a prefix-cache lookup against
// the ready-pod set, and returns a prefixCacheScorer populated with the match
// percentages and prefix hashes for the request.
func (p *prefixCachePrefillPolicy) Prepare(routingCtx *types.RoutingContext, _ []*v1.Pod, readyPodsMap map[string]struct{}) (PrefillScorer, error) {
	tokens, err := p.tok.TokenizeInputText(routingCtx.Message)
	if err != nil {
		return nil, err
	}
	matchedPods, hashes := p.prefixCacheIndexer.MatchPrefix(tokens, routingCtx.Model, readyPodsMap)
	return &prefixCacheScorer{matchedPods: matchedPods, hashes: hashes}, nil
}

func (p *prefixCachePrefillPolicy) Name() string { return PrefillScorePolicyPrefixCache }

// prefixCacheScorer is the request-scoped scorer produced by PrefixCachePrefillPolicy.
// matchedPods maps pod name → prefix-match percentage (0–100); hashes are the
// token-prefix hashes to be added to the index once a pod is selected.
type prefixCacheScorer struct {
	matchedPods map[string]int
	hashes      []uint64
}

func (s *prefixCacheScorer) PrefixHashes() []uint64 { return s.hashes }

func (s *prefixCacheScorer) ScorePod(pod *v1.Pod, reqCnt, maxRequestCount float64) float64 {
	matchPct := float64(s.matchedPods[pod.Name])
	score := (100-matchPct)*.1 + reqCnt/maxRequestCount
	klog.V(4).InfoS("prefill_score", "pod_name", pod.Name,
		"policy", PrefillScorePolicyPrefixCache,
		"score", fmt.Sprintf("(100 - %f) * 0.1 + %f / %f", matchPct, reqCnt, maxRequestCount),
		"prefix_match_percent", matchPct,
		"running_reqs", reqCnt, "max_running_reqs", maxRequestCount)
	return score
}

// leastRequestPrefillPolicy scores prefill pods solely by their running-request
// count with no prefix-cache consultation. It is suitable when prefix-cache
// locality is not important or when a tokenizer is unavailable.
//
// The scorer returned by Prepare is a zero-size struct; no per-request
// allocation is needed. Obtain an instance via NewLeastRequestPrefillPolicy.
type leastRequestPrefillPolicy struct{}

// NewLeastRequestPrefillPolicy returns a least_request PrefillScorePolicy that
// routes to the pod with the fewest active prefill requests.
func NewLeastRequestPrefillPolicy() PrefillScorePolicy {
	return &leastRequestPrefillPolicy{}
}

// Prepare returns a leastRequestScorer; no tokenization or cache lookup is performed.
func (p *leastRequestPrefillPolicy) Prepare(_ *types.RoutingContext, _ []*v1.Pod, _ map[string]struct{}) (PrefillScorer, error) {
	return leastRequestScorer{}, nil
}

func (p *leastRequestPrefillPolicy) Name() string { return PrefillScorePolicyLeastRequest }

// leastRequestScorer is the request-scoped scorer produced by LeastRequestPrefillPolicy.
type leastRequestScorer struct{}

// PrefixHashes returns nil because this policy does not use the prefix cache.
func (s leastRequestScorer) PrefixHashes() []uint64 { return nil }

func (s leastRequestScorer) ScorePod(pod *v1.Pod, reqCnt, _ float64) float64 {
	klog.V(4).InfoS("prefill_score", "pod_name", pod.Name,
		"policy", PrefillScorePolicyLeastRequest,
		"running_reqs", reqCnt)
	return reqCnt
}
