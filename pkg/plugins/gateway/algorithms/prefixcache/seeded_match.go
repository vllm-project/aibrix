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

package prefixcache

import (
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	prefixMatchLookup           = "lookup"
	prefixMatchReuse            = "reuse"
	prefixMatchIdentityFallback = "fallback_identity"
	prefixMatchCoverageFallback = "fallback_uncovered"
)

func recordPrefixMatchOperation(model, result string) {
	if metrics := getPrefixCacheMetrics(); metrics != nil {
		metrics.prefixCacheMatchOperations.WithLabelValues(model, result).Inc()
	}
}

func (p prefixCacheRouter) matchPrefix(ctx *types.RoutingContext, pods []*v1.Pod, tokens []byte, remember bool) (map[string]int, []uint64) {
	return matchPrefix(ctx, pods, p.prefixCacheIndexer, ctx.Model, -1, remember, func(ready map[string]struct{}) (map[string]int, []uint64) {
		return p.prefixCacheIndexer.MatchPrefix(tokens, ctx.Model, ready)
	})
}

func (k *kvSyncPrefixCacheRouter) matchPrefix(ctx *types.RoutingContext, pods []*v1.Pod, tokens []byte, model string, loraID int64, remember bool) (map[string]int, []uint64) {
	return matchPrefix(ctx, pods, k.syncIndexer, model, loraID, remember, func(ready map[string]struct{}) (map[string]int, []uint64) {
		return k.syncIndexer.MatchPrefix(model, loraID, tokens, ready)
	})
}

// matchPrefix reuses seeding's raw result for covered subsets in the same index
// partition. It filters out matches outside pods: standalone routing uses the
// size of the matched set, and must not see holders the gate already removed.
// remember is true only for seeding; other callers never extend the snapshot.
// Both indexers must leave the ready-set input unchanged, and each pod's raw
// match must be independent of the other pods in that set. Retaining ready and
// projecting matches onto subsets relies on those contracts.
func matchPrefix(ctx *types.RoutingContext, pods []*v1.Pod, index any, model string, loraID int64, remember bool,
	read func(map[string]struct{}) (map[string]int, []uint64),
) (map[string]int, []uint64) {
	result := prefixMatchLookup
	if snapshot := ctx.SeededPrefixMatch(); snapshot != nil {
		result = prefixMatchIdentityFallback
		if snapshot.Index == index && snapshot.Model == model && snapshot.LoRAID == loraID {
			covered := true
			matches := make(map[string]int, min(len(pods), len(snapshot.Matches)))
			for _, pod := range pods {
				key := utils.GeneratePodKey(pod.Namespace, pod.Name)
				if _, ok := snapshot.Pods[key]; !ok {
					covered = false
					break
				}
				if percent, ok := snapshot.Matches[key]; ok {
					matches[key] = percent
				}
			}
			if covered {
				recordPrefixMatchOperation(ctx.Model, prefixMatchReuse)
				return matches, snapshot.Hashes[:len(snapshot.Hashes):len(snapshot.Hashes)]
			}
			result = prefixMatchCoverageFallback
		}
		klog.V(4).InfoS("seeded prefix match fallback", "request_id", ctx.RequestID, "model", ctx.Model, "reason", result, "pods", len(pods))
	}

	ready := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		ready[utils.GeneratePodKey(pod.Namespace, pod.Name)] = struct{}{}
	}
	matches, hashes := read(ready)
	recordPrefixMatchOperation(ctx.Model, result)
	// Consumers must not overwrite hash elements. Restrict capacity so appending
	// to a returned slice cannot modify the backing array retained by seeding.
	hashes = hashes[:len(hashes):len(hashes)]
	if remember {
		ctx.SetSeededPrefixMatch(&types.SeededPrefixMatch{
			Index: index, Model: model, LoRAID: loraID,
			Pods: ready, Matches: matches, Hashes: hashes,
		})
	}
	return matches, hashes
}
