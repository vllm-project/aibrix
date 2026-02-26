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
	"math"
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
	RoutingStrategy          string `json:"routingStrategy"`
	PromptLenBucketMinLength int    `json:"promptLenBucketMinLength"`
	PromptLenBucketMaxLength int    `json:"promptLenBucketMaxLength"`
	Combined                 bool   `json:"combined"`
}

// ModelConfigProfiles is the root JSON structure from model.aibrix.ai/config.
type ModelConfigProfiles struct {
	DefaultProfile string                        `json:"defaultProfile"`
	Profiles       map[string]ModelConfigProfile `json:"profiles"`
}

// GetProfile returns the profile for the given name, or the default profile.
// Falls back to defaultProfile/"default" when the requested profile does not exist.
// Returns nil only if no default profile exists.
func (c *ModelConfigProfiles) GetProfile(name string) *ModelConfigProfile {
	if name != "" {
		if p, ok := c.Profiles[name]; ok {
			return &p
		}
	}
	// Fall back to default
	if name = c.DefaultProfile; name == "" {
		name = DefaultProfileName
	}
	if p, ok := c.Profiles[name]; ok {
		return &p
	}
	return nil
}

// ResolveProfile resolves the model config from pods (annotation),
// then returns the profile selected by headerProfile (from config-profile).
// configMapGetter can be nil; it is checked first when provided.
func ResolveProfile(pods []*v1.Pod, headerProfile string) *ModelConfigProfile {
	for _, pod := range pods {
		if p := ResolveProfileFromPod(pod, headerProfile); p != nil {
			return p
		}
	}
	return nil
}

// ResolveProfileFromPod resolves the model config from a single pod annotation and returns the selected profile.
func ResolveProfileFromPod(pod *v1.Pod, headerProfile string) *ModelConfigProfile {
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
	if headerProfile == "" {
		return cfg.GetProfile("")
	}
	return cfg.GetProfile(headerProfile)
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
	// Default prompt bounds when not provided: min=0, max=MaxInt32
	for name, p := range cfg.Profiles {
		if p.PromptLenBucketMinLength < 0 {
			p.PromptLenBucketMinLength = 0
		}
		if p.PromptLenBucketMaxLength == 0 {
			p.PromptLenBucketMaxLength = math.MaxInt32
		}
		cfg.Profiles[name] = p
	}
	return &cfg, nil
}
