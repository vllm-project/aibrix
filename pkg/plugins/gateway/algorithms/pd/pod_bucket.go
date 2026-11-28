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
	"math"

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
)

// PromptLenBucketBounds returns the prompt-length range a pod's profile
// declares, with [0, math.MaxInt32] as the default: a pod that declares none is
// not bucket-scoped. A non-positive minimum or maximum falls back to its
// default, mirroring the pod-side tolerance of a partially filled range.
func PromptLenBucketBounds(cfg *types.RoutingConfig) (int, int) {
	minLength, maxLength := 0, math.MaxInt32
	if cfg == nil {
		return minLength, maxLength
	}
	if v := cfg.PromptLenBucketMinLength; v != nil && *v > 0 {
		minLength = *v
	}
	if v := cfg.PromptLenBucketMaxLength; v != nil && *v > 0 {
		maxLength = *v
	}
	return minLength, maxLength
}

// IsPromptLengthInBucket reports whether cfg covers promptLength. A nil or
// unbounded config matches any length; an inverted range (min > max) matches
// none.
func IsPromptLengthInBucket(cfg *types.RoutingConfig, promptLength int) bool {
	minLength, maxLength := PromptLenBucketBounds(cfg)
	if minLength > maxLength {
		return false
	}
	if minLength == 0 && maxLength == math.MaxInt32 {
		return true
	}
	return promptLength >= minLength && promptLength <= maxLength
}

// IsCombinedRoutingConfig reports whether cfg marks a pod as a combined
// prefill+decode spillover replica.
func IsCombinedRoutingConfig(cfg *types.RoutingConfig) bool {
	return cfg != nil && cfg.Combined != nil && *cfg.Combined
}

// ResolvePodRoutingConfig resolves and parses pod's RoutingConfig from
// routingCtx.ModelConfig or pod's model.aibrix.ai/config annotation.
// When cache is non-nil, parsed configs are memoized per distinct pod config
// annotation within a single routing pass to avoid repeated JSON unmarshaling
// across large mixed or bucketed fleets.
func ResolvePodRoutingConfig(routingCtx *types.RoutingContext, pod *v1.Pod, cache *map[string]*types.RoutingConfig) *types.RoutingConfig {
	if routingCtx == nil || pod == nil {
		return nil
	}
	var anno string
	if cache != nil {
		anno = pod.Annotations[constants.ModelAnnoConfig]
		if *cache != nil {
			if cfg, ok := (*cache)[anno]; ok {
				return cfg
			}
		}
	}
	var cfg *types.RoutingConfig
	if profile := configprofiles.ResolveProfileFrom(routingCtx.ModelConfig, pod, routingCtx.ReqConfigProfile); profile != nil {
		cfg = configprofiles.ParseRoutingConfig(profile.RoutingConfig)
	}
	if cache != nil {
		if *cache == nil {
			*cache = make(map[string]*types.RoutingConfig, 4)
		}
		(*cache)[anno] = cfg
	}
	return cfg
}

// PodPromptLenBucketBounds returns the prompt-length bucket bounds declared by
// pod's resolved config profile, defaulting to [0, math.MaxInt32].
func PodPromptLenBucketBounds(routingCtx *types.RoutingContext, pod *v1.Pod) (int, int) {
	return PromptLenBucketBounds(ResolvePodRoutingConfig(routingCtx, pod, nil))
}

// IsPodSuitableForPromptLength reports whether pod's resolved config profile
// covers promptLength.
func IsPodSuitableForPromptLength(routingCtx *types.RoutingContext, pod *v1.Pod, promptLength int) bool {
	return IsPromptLengthInBucket(ResolvePodRoutingConfig(routingCtx, pod, nil), promptLength)
}

// IsCombinedPod reports whether pod's resolved config profile marks it as a
// combined spillover pod.
func IsCombinedPod(routingCtx *types.RoutingContext, pod *v1.Pod) bool {
	return IsCombinedRoutingConfig(ResolvePodRoutingConfig(routingCtx, pod, nil))
}
