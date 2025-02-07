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

package routingalgorithms

import (
	"context"
	"fmt"
	"math/rand"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	AIBRIX_PREFIX_CACHE_BLOCK_SIZE               = 16 // loadEnv("AIBRIX_PREFIX_CACHE_BLOCK_SIZE", 16)
	AIBRIX_PREFIX_CACHE_EVICTION_INTERVAL_MS     = 50
	AIBRIX_PROMPT_PREFIX_MATCH_THRESHOLD_PERCENT = 50
)

type prefixCacheRouter struct {
	cache *cache.Cache
}

func NewPrefixCacheRouter() Router {
	cache, err := cache.GetCache()
	if err != nil {
		panic(err)
	}

	return prefixCacheRouter{
		cache: cache,
	}
}

func (p prefixCacheRouter) Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error) {
	readyPods := utils.FilterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no pods to forward request")
	}
	if len(readyPods) == 1 {
		for _, pod := range pods {
			return getPodAddress(pod.Status.PodIP)
		}
	}

	tokens, err := utils.TokenizeInputText(message)
	if err != nil {
		return "", err
	}

	var targetPod *v1.Pod
	matchedTokens, unMatchedTokens, matchedPods := p.cache.MatchPrefix(tokens, model, readyPods)
	if float64(len(matchedTokens)/len(tokens)) > 0.5 {
		targetPod = matchedPods[rand.Intn(len(matchedPods))]
	} else {
		targetPod = readyPods[rand.Intn(len(readyPods))]
	}
	if len(unMatchedTokens) > 0 {
		p.cache.AddPrefixBlock(unMatchedTokens, model, targetPod.Name)
	}

	var matchedPodNames, readyPodNames []string
	for _, p := range matchedPods {
		matchedPodNames = append(matchedPodNames, p.Status.PodIP)
	}
	for _, p := range readyPods {
		readyPodNames = append(readyPodNames, p.Status.PodIP)
	}
	klog.V(5).InfoS("prefix cache route",
		"message", message,
		"tokens", tokens,
		"matched_tokens", matchedTokens,
		"unmatched_tokens", unMatchedTokens,
		"matched_pods", matchedPodNames,
		"ready_pods", readyPodNames,
		"target_pod", targetPod.Status.PodIP)

	return getPodAddress(targetPod.Status.PodIP)
}
