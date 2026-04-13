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

// Package pd holds PD (prefill–decode disaggregation) routing helpers used by the gateway PD algorithm.
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

// DecodePolicyName identifies a decode scoring policy (AIBRIX_DECODE_SCORE_POLICY).
type DecodePolicyName string

const (
	DecodePolicyLoadBalancing DecodePolicyName = "load_balancing"
	DecodePolicyLeastRequest  DecodePolicyName = "least_request"
)

// ScorePolicy* are untyped string constants for use in env-var defaults and config parsing
// (e.g. utils.LoadEnv("AIBRIX_DECODE_SCORE_POLICY", ScorePolicyLoadBalancing)).
// For in-code comparisons prefer the typed DecodePolicyName constants.
const (
	ScorePolicyLoadBalancing = string(DecodePolicyLoadBalancing)
	ScorePolicyLeastRequest  = string(DecodePolicyLeastRequest)
)

// Env tunables for load_balancing numerator terms: score = (wRun*normRun + wThru*normThru) / normFreeGPU.
// Defaults preserve the historical equal weighting of running-request count vs inverse-throughput terms.
var (
	decodeLBWeightRunningReq   = utils.LoadEnvFloat("AIBRIX_DECODE_LB_WEIGHT_RUNNING", 1.0)
	decodeLBWeightThroughput   = utils.LoadEnvFloat("AIBRIX_DECODE_LB_WEIGHT_THROUGHPUT", 1.0)
	decodePolicyRegistryMu     sync.RWMutex
	decodePolicyRegistryCustom = map[string]func() DecodeScorePolicy{}
)

// DecodePodInput bundles per-pod metrics and batch maxima for one decode scoring pass.
// Invariants (enforced by the PD router when collecting metrics): MaxRequestCount, MaxThroughput,
// and MaxFreeGPUUsage are positive (typically ≥ 1 for maxima from loadImbalanceSelectDecodePod); free GPU
// per pod is floored before scoring so division stays stable.
type DecodePodInput struct {
	RunningReqs     float64
	Throughput      float64
	FreeGPUPercent  float64
	MaxRequestCount float64
	MaxThroughput   float64
	MaxFreeGPUUsage float64
}

// RolesetDecodePick is the winning decode pod for one roleset after comparing scores (lower is better).
type RolesetDecodePick struct {
	Pod   *v1.Pod
	Score float64
}

// DecodeScoreRun is the outcome of one decode scoring pass.
//
// Semantics:
//   - Empty PerRoleset with Err == nil means no decode pod produced a score (e.g. empty input list).
//   - Err non-nil means the pass failed in a way callers should surface (currently unused; reserved).
//   - FallbackUsed indicates an invalid primary-policy score was replaced by load_balancing for that pod.
//   - MaxScore is the maximum raw score over all pods evaluated in the pass (for finalPDScore normalization).
type DecodeScoreRun struct {
	PerRoleset   map[string]RolesetDecodePick
	MaxScore     float64
	Err          error
	FallbackUsed bool
	Policy       DecodePolicyName
}

// DecodeScorePolicy is the stateless scoring strategy for decode pod selection among pods
// that passed load-imbalance fast paths. Wire implementations via AIBRIX_DECODE_SCORE_POLICY or RegisterDecodePolicy.
type DecodeScorePolicy interface {
	// Name returns the canonical policy id (for logs and metrics).
	Name() DecodePolicyName
	// Describe returns a short human-readable summary for observability (dashboards, support).
	Describe() string
	// ScoreDecodePod returns a score for one decode pod; lower is better.
	ScoreDecodePod(routingCtx *types.RoutingContext, pod *v1.Pod, in DecodePodInput) float64
}

// LoadBalancingDecodePolicy combines normalized running request count, inverse throughput, and free GPU headroom.
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

// LeastRequestDecodePolicy scores only by running decode request count (including pending decode)
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

// built-in factories (mutable only via RegisterDecodePolicy for extra names).
var decodePolicyFactories = map[string]func() DecodeScorePolicy{
	string(DecodePolicyLoadBalancing): func() DecodeScorePolicy { return LoadBalancingDecodePolicy{} },
	string(DecodePolicyLeastRequest):  func() DecodeScorePolicy { return LeastRequestDecodePolicy{} },
}

// RegisterDecodePolicy adds or overrides a named decode policy factory (e.g. from tests or plugins).
// Must be called before ResolveDecodePolicy for that name to ensure the policy is visible; concurrent calls are safe.
func RegisterDecodePolicy(name string, factory func() DecodeScorePolicy) {
	if factory == nil {
		return
	}
	decodePolicyRegistryMu.Lock()
	defer decodePolicyRegistryMu.Unlock()
	decodePolicyRegistryCustom[strings.ToLower(strings.TrimSpace(name))] = factory
}

// ResolveDecodePolicy returns the policy for env value raw, its canonical name, and unknown true if raw was not registered.
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

// ValidDecodePolicyNames returns built-in policy ids plus dynamically registered custom ids.
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

// InvalidDecodeScore reports whether a score cannot be used for routing.
// Only NaN is treated as invalid so behavior matches historical scoring (e.g. +Inf when free-GPU term is zero).
func InvalidDecodeScore(s float64) bool {
	return math.IsNaN(s)
}
