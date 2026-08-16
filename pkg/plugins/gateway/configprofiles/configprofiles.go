/*
Copyright 2025 The Aibrix Team.

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

// Package configprofiles parses the model.aibrix.ai/config annotation (or ConfigMap)
// and supports multiple named profiles selectable at runtime via config-profile header.
// See docs/source/designs/model-config-profiles.rst for the design.
package configprofiles

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

const (
	// DefaultProfileName is used when defaultProfile is not set in the JSON.
	DefaultProfileName = "default"
)

// ModelConfigProfile holds gateway options for a single profile.
type ModelConfigProfile struct {
	RoutingStrategy   string          `json:"routingStrategy"`
	RoutingConfig     json.RawMessage `json:"routingConfig,omitempty"`
	RequestsPerSecond int64           `json:"requestsPerSecond,omitempty"`
}

// autoProfileRoutingConfig holds request-local profile selection hints embedded
// in each profile's routingConfig.
type autoProfileRoutingConfig struct {
	PromptTokensGte *int   `json:"promptTokensGte,omitempty"`
	PromptTokensLt  *int   `json:"promptTokensLt,omitempty"`
	MaxTokensGte    *int64 `json:"maxTokensGte,omitempty"`
	MaxTokensLt     *int64 `json:"maxTokensLt,omitempty"`
}

// RequestFeatures are the request attributes used by automatic profile selection.
type RequestFeatures struct {
	PromptTokens *int
	MaxTokens    *int64
}

// ModelConfigProfiles is the root JSON structure from model.aibrix.ai/config.
type ModelConfigProfiles struct {
	// LockedRoutingStrategy, when set, pins the routing strategy model-wide.
	// It takes precedence over the routing-strategy request header, the per-profile
	// routingStrategy and the ROUTING_ALGORITHM environment variable.
	LockedRoutingStrategy string                        `json:"lockedRoutingStrategy,omitempty"`
	DefaultProfile        string                        `json:"defaultProfile"`
	Profiles              map[string]ModelConfigProfile `json:"profiles"`
}

// GetProfile returns the profile for the given name, or the default profile.
// Falls back to defaultProfile/"default" when the requested profile does not exist.
// Returns nil only if no default profile exists.
func (c *ModelConfigProfiles) GetProfile(name string) *ModelConfigProfile {
	if name != "" {
		if p := c.GetProfileExact(name); p != nil {
			return p
		}
	}
	// Fall back to default
	if name = c.DefaultProfile; name == "" {
		name = DefaultProfileName
	}
	return c.GetProfileExact(name)
}

// GetProfileExact returns the named profile without falling back.
func (c *ModelConfigProfiles) GetProfileExact(name string) *ModelConfigProfile {
	if p, ok := c.Profiles[name]; ok {
		return &p
	}
	return nil
}

// DefaultProfileOrName returns defaultProfile, or "default" when it is not set.
func (c *ModelConfigProfiles) DefaultProfileOrName() string {
	if c.DefaultProfile != "" {
		return c.DefaultProfile
	}
	return DefaultProfileName
}

// ResolveAutoProfileName resolves config-profile: auto to a concrete profile name.
func (c *ModelConfigProfiles) ResolveAutoProfileName(features RequestFeatures) string {
	if c == nil {
		return DefaultProfileName
	}
	if name, ok := c.bestAutoProfileName(features); ok {
		return name
	}
	return c.DefaultProfileOrName()
}

func (c *ModelConfigProfiles) bestAutoProfileName(features RequestFeatures) (string, bool) {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	var bestName string
	var best autoProfileRoutingConfig
	bestSet := false
	for _, name := range names {
		profile := c.Profiles[name]
		criteria, ok := parseAutoProfileRoutingConfig(profile.RoutingConfig)
		if !ok || !criteria.matches(features) {
			continue
		}
		if !bestSet || criteria.moreSpecificThan(best) {
			bestName = name
			best = criteria
			bestSet = true
		}
	}
	return bestName, bestSet
}

func parseAutoProfileRoutingConfig(raw json.RawMessage) (autoProfileRoutingConfig, bool) {
	if len(raw) == 0 {
		return autoProfileRoutingConfig{}, false
	}
	var cfg autoProfileRoutingConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return autoProfileRoutingConfig{}, false
	}
	return cfg, cfg.hasAutoSelectionHints()
}

func (c autoProfileRoutingConfig) hasAutoSelectionHints() bool {
	return c.PromptTokensGte != nil || c.PromptTokensLt != nil || c.MaxTokensGte != nil || c.MaxTokensLt != nil
}

func (c autoProfileRoutingConfig) matches(features RequestFeatures) bool {
	if c.PromptTokensGte != nil || c.PromptTokensLt != nil {
		if features.PromptTokens == nil {
			return false
		}
		if c.PromptTokensGte != nil && *features.PromptTokens < *c.PromptTokensGte {
			return false
		}
		if c.PromptTokensLt != nil && *features.PromptTokens >= *c.PromptTokensLt {
			return false
		}
	}
	if c.MaxTokensGte != nil {
		if features.MaxTokens == nil || *features.MaxTokens < *c.MaxTokensGte {
			return false
		}
	}
	if c.MaxTokensLt != nil {
		if features.MaxTokens == nil || *features.MaxTokens >= *c.MaxTokensLt {
			return false
		}
	}
	return true
}

func (c autoProfileRoutingConfig) moreSpecificThan(other autoProfileRoutingConfig) bool {
	if c.hintCount() != other.hintCount() {
		return c.hintCount() > other.hintCount()
	}
	if c.promptTokensGteValue() != other.promptTokensGteValue() {
		return c.promptTokensGteValue() > other.promptTokensGteValue()
	}
	if c.promptTokensLtValue() != other.promptTokensLtValue() {
		return c.promptTokensLtValue() < other.promptTokensLtValue()
	}
	if c.maxTokensGteValue() != other.maxTokensGteValue() {
		return c.maxTokensGteValue() > other.maxTokensGteValue()
	}
	if c.maxTokensLtValue() != other.maxTokensLtValue() {
		return c.maxTokensLtValue() < other.maxTokensLtValue()
	}
	return false
}

func (c autoProfileRoutingConfig) hintCount() int {
	count := 0
	if c.PromptTokensGte != nil {
		count++
	}
	if c.PromptTokensLt != nil {
		count++
	}
	if c.MaxTokensGte != nil {
		count++
	}
	if c.MaxTokensLt != nil {
		count++
	}
	return count
}

func (c autoProfileRoutingConfig) promptTokensGteValue() int {
	if c.PromptTokensGte == nil {
		return 0
	}
	return *c.PromptTokensGte
}

func (c autoProfileRoutingConfig) promptTokensLtValue() int {
	if c.PromptTokensLt == nil {
		return int(^uint(0) >> 1)
	}
	return *c.PromptTokensLt
}

func (c autoProfileRoutingConfig) maxTokensGteValue() int64 {
	if c.MaxTokensGte == nil {
		return 0
	}
	return *c.MaxTokensGte
}

func (c autoProfileRoutingConfig) maxTokensLtValue() int64 {
	if c.MaxTokensLt == nil {
		return int64(^uint64(0) >> 1)
	}
	return *c.MaxTokensLt
}

// ResolveProfileFromPod resolves the model config from a single pod annotation and
// returns the selected profile. The profile is selected by headerProfile; an empty
// headerProfile falls back to the default profile. Returns nil when the pod has no
// config annotation, the config is invalid, or no selectable profile exists.
func ResolveProfileFromPod(pod *v1.Pod, headerProfile string) *ModelConfigProfile {
	cfg := parseConfigFromPod(pod)
	if cfg == nil {
		return nil
	}
	return cfg.GetProfile(headerProfile)
}

// ResolveConfig resolves the model config from the first pod carrying a
// model.aibrix.ai/config annotation. It returns the profile selected by
// headerProfile (nil when no selectable profile exists) together with the
// model-wide locked routing strategy ("" when unset). The locked strategy applies
// even when no profile resolves.
func ResolveConfig(pods []*v1.Pod, headerProfile string) (*ModelConfigProfile, string) {
	profile, _, locked := ResolveConfigForRequest(pods, headerProfile, RequestFeatures{})
	return profile, locked
}

// ResolveConfigForRequest resolves the model config from the first pod carrying a
// model.aibrix.ai/config annotation. If headerProfile is "auto", request-local
// hints in each profile's routingConfig are evaluated and the returned
// profileName is the concrete profile selected for this request.
func ResolveConfigForRequest(pods []*v1.Pod, headerProfile string, features RequestFeatures) (*ModelConfigProfile, string, string) {
	for _, pod := range pods {
		cfg := parseConfigFromPod(pod)
		if cfg == nil {
			continue
		}
		profileName := strings.TrimSpace(headerProfile)
		if strings.EqualFold(profileName, "auto") {
			selectedName := cfg.ResolveAutoProfileName(features)
			if profile := cfg.GetProfileExact(selectedName); profile != nil {
				return profile, selectedName, cfg.LockedRoutingStrategy
			}
			fallbackName := cfg.DefaultProfileOrName()
			klog.Warningf("auto profile selection referenced missing profile %q; falling back to %q", selectedName, fallbackName)
			return cfg.GetProfileExact(fallbackName), fallbackName, cfg.LockedRoutingStrategy
		}
		if profile := cfg.GetProfileExact(profileName); profile != nil {
			return profile, profileName, cfg.LockedRoutingStrategy
		}
		fallbackName := cfg.DefaultProfileOrName()
		return cfg.GetProfileExact(fallbackName), fallbackName, cfg.LockedRoutingStrategy
	}
	return nil, "", ""
}

// parseConfigFromPod parses the model config from a single pod annotation.
// Returns nil when the pod is nil, has no config annotation, or the config is invalid.
func parseConfigFromPod(pod *v1.Pod) *ModelConfigProfiles {
	if pod == nil {
		return nil
	}
	anno := pod.Annotations[constants.ModelAnnoConfig]
	if anno == "" {
		return nil
	}
	cfg, err := ParseModelConfig(anno)
	if err != nil {
		klog.V(4).InfoS("failed to parse model config from pod annotation", "pod", pod.Name, "err", err)
		return nil
	}
	return cfg
}

// ParseModelConfig parses the JSON from annotation data.
// Returns nil if jsonStr is empty or invalid.
func ParseModelConfig(jsonStr string) (*ModelConfigProfiles, error) {
	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" {
		return nil, nil
	}
	var cfg ModelConfigProfiles
	if err := json.Unmarshal([]byte(jsonStr), &cfg); err != nil {
		return nil, fmt.Errorf("parse model config: %w", err)
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("model config has no profiles")
	}
	return &cfg, nil
}
