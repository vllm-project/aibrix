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

package gateway

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/cache"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// warnIfReplicaInflightBelowRPS logs when requestsInflight is set tighter than
// requestsPerSecondPerReplica. The two are independent limits set by the user on purpose
// (e.g. "at most 3 concurrent, and also no more than 5 rps"), so this only surfaces the
// resulting RPS ceiling being practically unreachable -- it must never silently raise the
// concurrency cap the user configured.
func warnIfReplicaInflightBelowRPS(routingCtx *types.RoutingContext, inflight int64, replicaRPS float64) {
	if inflight <= 0 || replicaRPS <= 0 || float64(inflight) >= replicaRPS {
		return
	}
	klog.InfoS("requestsInflight below requestsPerSecondPerReplica; replica RPS ceiling may be unreachable",
		"requestID", routingCtx.RequestID, "model", routingCtx.Model,
		"requestsInflight", inflight, "replicaRPS", replicaRPS)
}

// maxRateWindowSeconds bounds the window rpsToLimitWindow will derive for sub-1 rps values,
// so a near-zero rps doesn't produce an unbounded Redis key TTL / bucket lifetime.
const maxRateWindowSeconds = 3600

// rpsRoundingEpsilon absorbs floating-point representation error (e.g. 0.2*10 evaluates to
// 1.9999999999999998, and 1/0.5 can land a hair under 2) so a rate that is really an exact
// integer, or an exact reciprocal, isn't pushed down a bucket by float noise. It's small
// enough that no genuinely fractional rps (e.g. 1.6, or 0.18) is affected.
const rpsRoundingEpsilon = 1e-9

// rpsToLimitWindow converts a (possibly fractional) requests-per-second rate into a
// (limit, windowSeconds) pair suitable for the fixed-window rate limiter: limit requests
// are allowed per windowSeconds-second window. Both branches round towards a lower delivered
// rate, never a higher one, so the derived pair never admits more than the configured rps.
//   - rps >= 1 uses a 1-second window with limit = floor(rps): e.g. 1.6 -> 1 req/s, not 2.
//   - 0 < rps < 1 is expressed as "1 request every N seconds", i.e. limit = 1 over a
//     windowSeconds-second window, with windowSeconds = ceil(1/rps) so the delivered rate is
//     never faster than what was configured (e.g. 0.18 -> every 6s, not every 5s).
//   - rps <= 0 disables the limit (limit = 0, windowSeconds = 0).
func rpsToLimitWindow(rps float64) (limit int64, windowSeconds int64) {
	if rps <= 0 {
		return 0, 0
	}
	if rps >= 1 {
		return int64(math.Floor(rps + rpsRoundingEpsilon)), 1
	}
	windowSeconds = int64(math.Ceil(1/rps - rpsRoundingEpsilon))
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	if windowSeconds > maxRateWindowSeconds {
		windowSeconds = maxRateWindowSeconds
	}
	return 1, windowSeconds
}

// modelConfigFromCache returns the model config c holds for model when c
// implements cache.ModelConfigCache, and "" otherwise.
func modelConfigFromCache(c cache.Cache, model string) string {
	if provider, ok := c.(cache.ModelConfigCache); ok {
		if cfg, found := provider.ModelConfig(model); found {
			return cfg
		}
	}
	return ""
}

// applyConfigProfile resolves the model config and applies the selected profile
// plus the model-wide locked routing strategy onto routingCtx.ConfigProfile.
// The config is routingCtx.ModelConfig when discovery supplied one for the
// model (the config field of a local-mode endpoints file), and otherwise the
// pod annotation (model.aibrix.ai/config).
//   - The profile is selected by the config-profile header, falling back to
//     defaultProfile (or "default") in the JSON.
//   - config-profile: auto evaluates request-local hints in profile routingConfig
//     and resolves to a concrete profile before routing strategy derivation.
//   - authoritativeRoutingPolicy clears the client routing controls before profile
//     resolution, so the existing default-profile and routing-precedence behavior
//     applies as if the client had not sent those headers.
//   - lockedRoutingStrategy (top-level) is applied even when no profile resolves, so a
//     model-wide lock cannot be bypassed by selecting a profile or sending a header.
//   - The profile's requestsPerSecondPerReplica, if set, always takes precedence over the
//     resolved profile's requestsPerSecond: the effective limit becomes
//     requestsPerSecondPerReplica times the model's current routable replica count, and the
//     routing strategy is forced to least-request so traffic is balanced evenly enough
//     across replicas for that per-replica figure to hold in aggregate.
//   - The profile's requestsInflight, if set, is a per-replica concurrency cap, and (like
//     requestsPerSecondPerReplica) forces the routing strategy to least-request so the
//     per-pod cap actually gets enforced -- selectTargetPod only runs, and thus only applies
//     the cap, when a routing strategy resolves to something other than RouterNotSet.
//     requestsInflight and requestsPerSecondPerReplica are independent, user-configured
//     limits: when inflight is set below the resolved replica RPS, it is left as configured
//     (only logged via warnIfReplicaInflightBelowRPS) rather than raised, since silently
//     loosening the concurrency cap the user set would defeat its purpose. Neither
//     requestsPerSecondPerReplica nor requestsInflight has an env-var form: both are
//     configured directly in the profile.
func applyConfigProfile(routingCtx *types.RoutingContext, pods []*v1.Pod) {
	applyConfigProfileWithRoutableCount(routingCtx, pods, nil)
}

// applyConfigProfileFromPodList behaves like applyConfigProfile while taking
// requestsPerSecondPerReplica's replica count from pods' cached Routable view
// when pods is a *utils.PodArray. ResolveModelConfigFrom still inspects
// pods.All().
func applyConfigProfileFromPodList(routingCtx *types.RoutingContext, pods types.PodList) {
	if pods == nil {
		applyConfigProfileWithRoutableCount(routingCtx, nil, nil)
		return
	}
	applyConfigProfileWithRoutableCount(routingCtx, pods.All(), func() int {
		return countRoutablePodsFromList(pods)
	})
}

func applyConfigProfileWithRoutableCount(routingCtx *types.RoutingContext, pods []*v1.Pod, routableCount func() int) {
	if routingCtx == nil {
		return
	}
	cfg := configprofiles.ResolveModelConfigFrom(routingCtx.ModelConfig, pods)
	if cfg == nil {
		return
	}
	if cfg.AuthoritativeRoutingPolicy {
		routingCtx.ReqConfigProfile = ""
		delete(routingCtx.ReqHeaders, HeaderRoutingStrategy)
		delete(routingCtx.ReqHeaders, HeaderExternalFilter)
	}

	reqConfigProfile := routingCtx.ReqConfigProfile
	var features configprofiles.RequestFeatures
	if strings.EqualFold(strings.TrimSpace(reqConfigProfile), "auto") {
		features = buildConfigProfileRequestFeatures(routingCtx)
	}
	profile, profileName, locked := cfg.ResolveForRequest(reqConfigProfile, features)

	var replicaRPS float64
	var inflight int64
	if profile != nil {
		replicaRPS = profile.RequestsPerSecondPerReplica
		inflight = profile.RequestsInflight
	}
	if profile == nil && locked == "" && !cfg.AuthoritativeRoutingPolicy && replicaRPS <= 0 && inflight <= 0 {
		return
	}

	if strings.EqualFold(strings.TrimSpace(reqConfigProfile), "auto") && profileName != "" {
		routingCtx.ReqConfigProfile = profileName
		if routingCtx.RespHeaders == nil {
			routingCtx.RespHeaders = make(map[string]string)
		}
		routingCtx.RespHeaders[HeaderAIBrixConfigProfile] = profileName
	}
	cp := &types.ResolvedConfigProfile{
		LockedRoutingStrategy:      locked,
		AuthoritativeRoutingPolicy: cfg.AuthoritativeRoutingPolicy,
	}
	if profile != nil {
		cp.ProfileName = profileName
		cp.RoutingStrategy = profile.RoutingStrategy
		cp.RoutingConfig = profile.RoutingConfig
		cp.Routing = configprofiles.ParseRoutingConfig(profile.RoutingConfig)
		cp.RequestsPerSecond = profile.RequestsPerSecond
		cp.TTFTThresholdS = profile.TTFTThresholdS
	}
	routingCtx.ConfigProfile = cp

	if replicaRPS > 0 {
		var replicas int64
		if routableCount != nil {
			replicas = int64(routableCount())
		} else {
			replicas = int64(utils.CountRoutablePods(pods))
		}
		limit, windowSeconds := rpsToLimitWindow(replicaRPS * float64(replicas))
		routingCtx.ConfigProfile.RequestsPerSecond = limit
		routingCtx.ConfigProfile.RateWindowSeconds = windowSeconds
		routingCtx.ConfigProfile.RoutingStrategy = string(routing.RouterLeastRequest)
		klog.V(4).InfoS("applied requestsPerSecondPerReplica to config profile", "requestID", routingCtx.RequestID, "model", routingCtx.Model,
			"replicaRPS", replicaRPS, "replicas", replicas, "limit", limit, "windowSeconds", windowSeconds)
	}

	if inflight > 0 {
		warnIfReplicaInflightBelowRPS(routingCtx, inflight, replicaRPS)
		routingCtx.ConfigProfile.RequestsInflight = inflight
		routingCtx.ConfigProfile.RoutingStrategy = string(routing.RouterLeastRequest)
		klog.V(4).InfoS("applied requestsInflight to config profile", "requestID", routingCtx.RequestID, "model", routingCtx.Model, "requestsInflight", inflight)
	}
}

func buildConfigProfileRequestFeatures(routingCtx *types.RoutingContext) configprofiles.RequestFeatures {
	if routingCtx == nil {
		return configprofiles.RequestFeatures{}
	}
	features := configprofiles.RequestFeatures{
		PromptTokens: estimatePromptTokens(routingCtx.Message),
		MaxTokens:    maxTokensFromRequestBody(routingCtx.ReqBody),
	}
	return features
}

func estimatePromptTokens(message string) *int {
	tokens, err := utils.TokenizeInputText(message)
	if err != nil {
		klog.V(4).InfoS("failed to estimate prompt tokens for config profile selection", "err", err)
		return nil
	}
	tokenCount := len(tokens)
	return &tokenCount
}

func maxTokensFromRequestBody(body []byte) *int64 {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var req map[string]json.RawMessage
	if err := sonic.Unmarshal(body, &req); err != nil {
		return nil
	}
	if v, ok := int64Field(req, "max_tokens"); ok {
		return &v
	}
	if v, ok := int64Field(req, "max_completion_tokens"); ok {
		return &v
	}
	return nil
}

func int64Field(req map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := req[key]
	// JSON null is treated as absent: unmarshalling null into int64 is a no-op
	// without an error, so it would otherwise read as 0.
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var v int64
	if err := sonic.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	return v, true
}

var defaultRoutingStrategy, defaultRoutingStrategyEnabled = utils.LookupEnv(EnvRoutingAlgorithm)

// deriveRoutingStrategyFromContext retrieves routing strategy with the following
// precedence (highest first):
//  1. lockedRoutingStrategy pinned model-wide in the model config
//  2. routing-strategy request header
//  3. routingStrategy from the resolved config profile
//  4. ROUTING_ALGORITHM environment variable
func deriveRoutingStrategyFromContext(routingCtx *types.RoutingContext) (string, bool) {
	if routingCtx == nil {
		return defaultRoutingStrategy, defaultRoutingStrategyEnabled
	}

	// Check locked routing strategy from model config (top-level, model-wide).
	if cp := routingCtx.ConfigProfile; cp != nil {
		if s := strings.TrimSpace(cp.LockedRoutingStrategy); s != "" {
			return s, true
		}
	}
	// Check request headers (case-insensitive key match)
	for k, v := range routingCtx.ReqHeaders {
		if strings.EqualFold(k, HeaderRoutingStrategy) {
			if s := strings.TrimSpace(v); s != "" {
				return s, true
			}
			break
		}
	}
	// Fallback to resolved profile on routing context
	if cp := routingCtx.ConfigProfile; cp != nil {
		if s := strings.TrimSpace(cp.RoutingStrategy); s != "" {
			return s, true
		}
	}
	// Fallback to environment default
	return defaultRoutingStrategy, defaultRoutingStrategyEnabled
}
