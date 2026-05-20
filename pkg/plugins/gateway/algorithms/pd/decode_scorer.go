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

package pd

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type DecodePolicyName string

const (
	// DecodePolicyLoadBalancing routes to the pod with the best balance of
	// running-request count, generation throughput, and free GPU headroom.
	DecodePolicyLoadBalancing DecodePolicyName = "load_balancing"

	// DecodePolicyLeastRequest routes to the pod with the fewest active decode
	// requests (including pending requests not yet reflected in metrics).
	DecodePolicyLeastRequest DecodePolicyName = "least_request"
)

const (
	ScorePolicyLoadBalancing = string(DecodePolicyLoadBalancing)
	ScorePolicyLeastRequest  = string(DecodePolicyLeastRequest)
)

// Weights for the load_balancing score numerator:
//
//	score = (wRun*normRunningReqs + wThru*normInverseThroughput) / normFreeGPU
//
// Configurable via AIBRIX_DECODE_LB_WEIGHT_RUNNING and AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT.
// Default equal weighting (1.0 / 1.0) preserves historical behaviour.
var (
	decodeLBWeightRunningReq   = utils.LoadEnvFloat("AIBRIX_DECODE_LB_WEIGHT_RUNNING", 1.0)
	decodeLBWeightThroughput   = utils.LoadEnvFloat("AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT", 1.0)
	decodePolicyRegistryMu     sync.RWMutex
	decodePolicyRegistryCustom = map[string]func() DecodeScorePolicy{}
)

// DecodePodInput bundles the per-pod metrics and per-batch maxima needed for
// one decode scoring pass. Maxima are enforced to be positive by the PD router
// (typically floored at 1.0) so that normalisation denominators are never zero.
type DecodePodInput struct {
	RunningReqs     float64 // active decode requests on this pod (incl. pending)
	Throughput      float64 // AvgGenerationThroughputToksPerS for the model
	FreeGPUPercent  float64 // 100 - GPUCacheUsagePerc*100, floored at 0.1
	MaxRequestCount float64 // max RunningReqs across the candidate decode pods
	MaxThroughput   float64 // max Throughput across the candidate decode pods
	MaxFreeGPUUsage float64 // max FreeGPUPercent across the candidate decode pods
}

// RolesetDecodePick is the winning decode pod for one roleset after comparing
// scores within that roleset (lower score is better).
type RolesetDecodePick struct {
	Pod   *v1.Pod
	Score float64
}

// DecodeScoreRun is the outcome of a single call to scoreDecodePods.
//
//   - PerRoleset empty with Err == nil: no decode pod produced a usable score
//     (e.g. the input list was empty or every score was NaN after fallback).
//   - Err non-nil: the pass failed in a way the router should surface to the caller.
//   - FallbackUsed true: at least one pod's primary-policy score was invalid (NaN)
//     and was replaced by load_balancing for that pod.
//   - MaxScore is the largest raw score produced in the pass, used by finalPDScore
//     to normalise decode scores before combining them with prefill scores.
type DecodeScoreRun struct {
	PerRoleset   map[string]RolesetDecodePick
	MaxScore     float64
	Err          error
	FallbackUsed bool
	Policy       DecodePolicyName
}

// DecodeScorePolicy is the stateless scoring strategy for decode pod selection.
// Implementations are selected via AIBRIX_DECODE_SCORE_POLICY or registered
// dynamically with RegisterDecodePolicy.
//
// To add a new decode scoring strategy: implement this interface and call
// RegisterDecodePolicy before NewPDRouter is invoked.
type DecodeScorePolicy interface {
	// Name returns the canonical policy identifier used in log lines and metrics.
	Name() DecodePolicyName
	// Describe returns a short human-readable summary for observability (e.g. startup logs).
	Describe() string
	// ScoreDecodePod returns a score for one decode pod; lower is better.
	ScoreDecodePod(routingCtx *types.RoutingContext, pod *v1.Pod, in DecodePodInput) float64
}

// LoadBalancingDecodePolicy scores decode pods by combining three normalised
// metrics: running-request count (higher → worse), generation throughput
// (lower → worse, expressed as inverse), and free GPU headroom (higher → better):
//
//	score = (wRun*normRunningReqs + wThru*(1 - normThroughput)) / normFreeGPU
//
// Weights are set by AIBRIX_DECODE_LB_WEIGHT_RUNNING and
// AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT (default 1.0 each).
type LoadBalancingDecodePolicy struct{}

func (LoadBalancingDecodePolicy) Name() DecodePolicyName { return DecodePolicyLoadBalancing }

func (LoadBalancingDecodePolicy) Describe() string {
	return fmt.Sprintf("load_balancing: (wRun*normRun + wThru*normThru) / normFreeGPU; wRun=%g wThru=%g (AIBRIX_DECODE_LB_WEIGHT_*)",
		decodeLBWeightRunningReq, decodeLBWeightThroughput)
}

func (LoadBalancingDecodePolicy) ScoreDecodePod(routingCtx *types.RoutingContext, pod *v1.Pod, in DecodePodInput) float64 {
	normalizedRunningReqs := in.RunningReqs / in.MaxRequestCount
	normalizedThroughput := 1 - in.Throughput/in.MaxThroughput
	normalizedFreeGPUPercent := in.FreeGPUPercent / in.MaxFreeGPUUsage

	numer := decodeLBWeightRunningReq*normalizedRunningReqs + decodeLBWeightThroughput*normalizedThroughput
	decodeScore := numer / normalizedFreeGPUPercent

	klog.V(4).InfoS("decode_score", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
		"policy", DecodePolicyLoadBalancing, "decode_score", decodeScore,
		"score", fmt.Sprintf("(%g*%f + %g*%f) / %f", decodeLBWeightRunningReq, normalizedRunningReqs, decodeLBWeightThroughput, normalizedThroughput, normalizedFreeGPUPercent),
		"running_reqs", in.RunningReqs, "max_running_reqs", in.MaxRequestCount,
		"throughput", in.Throughput, "max_throughput", in.MaxThroughput,
		"free_gpu", in.FreeGPUPercent, "max_free_gpu_usage", in.MaxFreeGPUUsage)

	return decodeScore
}

// LeastRequestDecodePolicy scores decode pods solely by their running-request
// count (including pending decode requests not yet visible in metrics). It is
// the simplest policy and useful when GPU-memory headroom differences between
// pods are negligible or when throughput metrics are unavailable.
type LeastRequestDecodePolicy struct{}

func (LeastRequestDecodePolicy) Name() DecodePolicyName { return DecodePolicyLeastRequest }

func (LeastRequestDecodePolicy) Describe() string {
	return "least_request: raw running decode request count (including pending)"
}

func (LeastRequestDecodePolicy) ScoreDecodePod(routingCtx *types.RoutingContext, pod *v1.Pod, in DecodePodInput) float64 {
	klog.V(4).InfoS("decode_score", "request_id", routingCtx.RequestID, "pod_name", pod.Name,
		"policy", DecodePolicyLeastRequest, "running_reqs", in.RunningReqs)

	return in.RunningReqs
}

// decodePolicyFactories is the immutable registry of built-in decode scoring
// policies. Custom policies registered at runtime go into decodePolicyRegistryCustom
// so that the built-in map never needs a mutex.
var decodePolicyFactories = map[string]func() DecodeScorePolicy{
	string(DecodePolicyLoadBalancing): func() DecodeScorePolicy { return LoadBalancingDecodePolicy{} },
	string(DecodePolicyLeastRequest):  func() DecodeScorePolicy { return LeastRequestDecodePolicy{} },
}

// RegisterDecodePolicy registers a custom decode scoring policy factory under
// name (case-insensitive, trimmed). Calling this with a name that already
// exists replaces the previous factory. Nil factories are rejected with a
// warning. Must be called before ResolveDecodePolicy is invoked for the same
// name; safe for concurrent use.
func RegisterDecodePolicy(name string, factory func() DecodeScorePolicy) {
	if factory == nil {
		klog.Warningf("RegisterDecodePolicy ignored: nil factory for name %q", name)
		return
	}
	decodePolicyRegistryMu.Lock()
	defer decodePolicyRegistryMu.Unlock()
	decodePolicyRegistryCustom[strings.ToLower(strings.TrimSpace(name))] = factory
}

// ResolveDecodePolicy resolves raw (e.g. from AIBRIX_DECODE_SCORE_POLICY) to a
// DecodeScorePolicy. It checks the custom registry first, then the built-in
// factory map, both after lower-casing and trimming raw. An empty raw string
// resolves to load_balancing. Returns unknown=true when raw is not recognised,
// in which case policy is load_balancing and canonical is DecodePolicyLoadBalancing.
func ResolveDecodePolicy(raw string) (policy DecodeScorePolicy, canonical DecodePolicyName, unknown bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		key = string(DecodePolicyLoadBalancing)
	}
	decodePolicyRegistryMu.RLock()
	f, ok := decodePolicyRegistryCustom[key]
	decodePolicyRegistryMu.RUnlock()
	if ok {
		if f == nil {
			return LoadBalancingDecodePolicy{}, DecodePolicyLoadBalancing, true
		}
		p := f()
		if p == nil {
			return LoadBalancingDecodePolicy{}, DecodePolicyLoadBalancing, true
		}
		return p, p.Name(), false
	}
	f, ok = decodePolicyFactories[key]
	if !ok {
		return LoadBalancingDecodePolicy{}, DecodePolicyLoadBalancing, true
	}
	p := f()
	return p, p.Name(), false
}

// ValidDecodePolicyNames returns the sorted list of all recognised decode
// policy names, including both built-in and dynamically registered custom ones.
// Used in log/error messages to guide operators toward valid values.
func ValidDecodePolicyNames() []string {
	names := []string{string(DecodePolicyLoadBalancing), string(DecodePolicyLeastRequest)}
	decodePolicyRegistryMu.RLock()
	defer decodePolicyRegistryMu.RUnlock()
	for name := range decodePolicyRegistryCustom {
		if name != string(DecodePolicyLoadBalancing) && name != string(DecodePolicyLeastRequest) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// InvalidDecodeScore reports whether score s cannot be used for routing.
// Only NaN is treated as invalid; +Inf is allowed so that a pod with zero free
// GPU headroom (causing division by zero in load_balancing) is routed last
// rather than skipped, preserving historical behaviour.
func InvalidDecodeScore(s float64) bool {
	return math.IsNaN(s)
}
