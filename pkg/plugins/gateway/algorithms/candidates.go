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
	"math/rand/v2"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// The candidate stage (RFC #3188) bounds the per-request routing work by K instead of the number
// of ready pods. Every strategy and the load-imbalance gate read each pod's cross-gateway running
// count from Redis, so routing over N pods costs several N-pod Redis reads per request. With
// AIBRIX_ROUTING_CANDIDATE_K > 0, the gateway draws K pods at random, adds up to
// AIBRIX_ROUTING_CANDIDATE_PREFIX_M pods that the router seeds (for prefix-cache, the pods holding
// the longest cached prefix), and runs the gate and the router on that set only. Off by default;
// a model config profile can set both through routingConfig.candidate.
var (
	routingCandidateK       = utils.LoadEnvInt("AIBRIX_ROUTING_CANDIDATE_K", 0)
	routingCandidatePrefixM = utils.LoadEnvInt("AIBRIX_ROUTING_CANDIDATE_PREFIX_M", 16)
)

// candidateSafeStrategies are the strategies whose choice depends only on the pods they are
// given, so routing over a sample changes which pods compete but not how they are judged.
// Strategies that keep a global view (vtc's fairness, the queue router, session pins, external
// and throughput routers, and the exclusive pd and slo routers) always see every ready pod.
var candidateSafeStrategies = map[string]bool{
	string(RouterLoadBalance):            true,
	string(RouterPrefixCache):            true,
	string(RouterRandom):                 true,
	string(RouterPowerOfTwo):             true,
	string(least.RouterLeastRequest):     true,
	string(least.RouterLeastRequestTopK): true,
	string(least.RouterLeastKvCache):     true,
	string(least.RouterLeastGpuCache):    true,
	string(least.RouterLeastBusyTime):    true,
	string(least.RouterLeastLatency):     true,
	string(least.RouterUtil):             true,
}

// candidateStageApplies reports whether every strategy in the request's resolved strategy, the
// one Select actually built including any auto-blend, is candidate-safe.
func candidateStageApplies(ctx *types.RoutingContext) bool {
	resolved := ctx.ResolvedStrategy
	if resolved == "" {
		resolved = string(ctx.Algorithm)
	}
	cfg, err := ParseMultiRouterConfig(resolved)
	if err != nil || len(cfg.Items) == 0 {
		return false
	}
	for _, item := range cfg.Items {
		if !candidateSafeStrategies[item.Name] {
			return false
		}
	}
	return true
}

// SelectCandidates returns the pods the gate and router should consider for this request: all of
// readyPods when the candidate stage is off, does not apply to the strategy, or would not shrink
// the set; otherwise K pods drawn uniformly at random plus up to M pods the router seeds. It reads
// no per-pod state from Redis. router is the one Select returned for ctx.
func SelectCandidates(ctx *types.RoutingContext, router types.Router, readyPods []*v1.Pod) []*v1.Pod {
	knobs := ctx.RoutingOverrides().Candidate
	return selectCandidates(ctx, router, readyPods, knobs.K, knobs.PrefixM)
}

func selectCandidates(ctx *types.RoutingContext, router types.Router, readyPods []*v1.Pod, k, m int) []*v1.Pod {
	if k <= 0 || m < 0 || len(readyPods) <= k+m || !candidateStageApplies(ctx) {
		return readyPods
	}

	var seeds []*v1.Pod
	if seeder, ok := router.(types.CandidateSeeder); ok && m > 0 {
		seeds = seeder.SeedCandidates(ctx, &utils.PodArray{Pods: readyPods}, m)
	}

	candidates := make([]*v1.Pod, 0, k+len(seeds))
	picked := make(map[*v1.Pod]struct{}, k+len(seeds))
	for _, pod := range seeds {
		if _, dup := picked[pod]; !dup {
			picked[pod] = struct{}{}
			candidates = append(candidates, pod)
		}
	}
	// k+m < len(readyPods), so k random pods outside the seeds always exist. When the sample is a
	// small share of readyPods, rejection sampling keeps this O(k) instead of touching every pod.
	want := len(candidates) + k
	if 2*want > len(readyPods) {
		for _, i := range rand.Perm(len(readyPods)) {
			if len(candidates) == want {
				break
			}
			if _, dup := picked[readyPods[i]]; !dup {
				picked[readyPods[i]] = struct{}{}
				candidates = append(candidates, readyPods[i])
			}
		}
	} else {
		for len(candidates) < want {
			pod := readyPods[rand.IntN(len(readyPods))]
			if _, dup := picked[pod]; !dup {
				picked[pod] = struct{}{}
				candidates = append(candidates, pod)
			}
		}
	}

	if klog.V(5).Enabled() {
		klog.V(5).InfoS("routing candidate stage", "request_id", ctx.RequestID, "ready_pods", len(readyPods),
			"candidates", len(candidates), "seeded", len(seeds), "strategy", ctx.ResolvedStrategy)
	}
	return candidates
}
