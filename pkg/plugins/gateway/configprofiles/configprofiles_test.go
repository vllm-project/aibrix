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

package configprofiles

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func int64Ptr(v int64) *int64 {
	return &v
}

func intPtr(v int) *int {
	return &v
}

const randomRoutingStrategy = "random"

func TestParseModelConfig(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "empty",
			json: "",
		},
		{
			name: "single profile",
			json: `{"profiles":{"default":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}}`,
		},
		{
			name: "multiple profiles with defaultProfile",
			json: `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096}},"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}}`,
		},
		{
			name: "with routingConfig",
			json: `{"profiles":{"default":{"routingStrategy":"pd","routingConfig":{"key":"value"}}}}`,
		},
		{
			name: "with auto profile hints in routingConfig",
			json: `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"random"},"large":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":8192}}}}`,
		},
		{
			name:    "invalid json",
			json:    `{`,
			wantErr: true,
		},
		{
			name:    "no profiles",
			json:    `{}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseModelConfig(tt.json)
			if tt.wantErr {
				if err == nil || cfg != nil {
					t.Errorf("ParseModelConfig() expected error, got cfg=%v err=%v", cfg, err)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseModelConfig() err=%v", err)
				return
			}
			if tt.json != "" && cfg == nil {
				t.Errorf("ParseModelConfig() expected config for non-empty input")
			}
		})
	}
}

func TestResolveConfigForRequestAutoProfileRoutingConfigHints(t *testing.T) {
	configJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"random"},
			"large-input":{"routingStrategy":"pd","routingConfig":{"promptTokensGte":8192}},
			"offline-generation":{"routingStrategy":"throughput","routingConfig":{"maxTokensGte":2048}},
			"combined":{"routingStrategy":"least-request","routingConfig":{"promptTokensGte":8192,"maxTokensGte":2048}},
			"broad-short-input":{"routingStrategy":"least-request","routingConfig":{"promptTokensLt":2048}},
			"narrow-short-input":{"routingStrategy":"least-latency","routingConfig":{"promptTokensLt":512}},
			"broad-short-output":{"routingStrategy":"least-request","routingConfig":{"maxTokensLt":4096}},
			"narrow-short-output":{"routingStrategy":"random","routingConfig":{"maxTokensLt":1024}}
		}
	}`
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod1",
			Namespace:   "default",
			Annotations: map[string]string{constants.ModelAnnoConfig: configJSON},
		},
	}

	tests := []struct {
		name          string
		headerProfile string
		features      RequestFeatures
		wantProfile   string
		wantName      string
	}{
		{
			name:          "concrete profile bypasses auto selection",
			headerProfile: "offline-generation",
			features:      RequestFeatures{PromptTokens: intPtr(9000)},
			wantProfile:   "throughput",
			wantName:      "offline-generation",
		},
		{
			name:          "auto selects prompt token profile",
			headerProfile: "auto",
			features:      RequestFeatures{PromptTokens: intPtr(9000)},
			wantProfile:   "pd",
			wantName:      "large-input",
		},
		{
			name:          "auto selects max token profile",
			headerProfile: "auto",
			features:      RequestFeatures{MaxTokens: int64Ptr(2048)},
			wantProfile:   "throughput",
			wantName:      "offline-generation",
		},
		{
			name:          "auto selects more specific matching profile",
			headerProfile: "auto",
			features:      RequestFeatures{PromptTokens: intPtr(9000), MaxTokens: int64Ptr(2048)},
			wantProfile:   "least-request",
			wantName:      "combined",
		},
		{
			name:          "auto falls back when no profile hints match",
			headerProfile: "auto",
			features:      RequestFeatures{PromptTokens: intPtr(2048)},
			wantProfile:   randomRoutingStrategy,
			wantName:      "default",
		},
		{
			name:          "auto selects narrower prompt token upper bound",
			headerProfile: "auto",
			features:      RequestFeatures{PromptTokens: intPtr(100)},
			wantProfile:   "least-latency",
			wantName:      "narrow-short-input",
		},
		{
			name:          "auto selects narrower max token upper bound",
			headerProfile: "auto",
			features:      RequestFeatures{MaxTokens: int64Ptr(512)},
			wantProfile:   randomRoutingStrategy,
			wantName:      "narrow-short-output",
		},
		{
			name:          "auto does not match prompt token hint when prompt tokens are unknown",
			headerProfile: "auto",
			features:      RequestFeatures{},
			wantProfile:   randomRoutingStrategy,
			wantName:      "default",
		},
		{
			name:          "case insensitive auto value",
			headerProfile: " AUTO ",
			features:      RequestFeatures{PromptTokens: intPtr(9000)},
			wantProfile:   "pd",
			wantName:      "large-input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, name, locked := ResolveConfigForRequest([]*v1.Pod{pod}, tt.headerProfile, tt.features)
			if profile == nil {
				t.Fatal("ResolveConfigForRequest() profile = nil")
			}
			if profile.RoutingStrategy != tt.wantProfile {
				t.Errorf("ResolveConfigForRequest().RoutingStrategy = %s, want %s", profile.RoutingStrategy, tt.wantProfile)
			}
			if name != tt.wantName {
				t.Errorf("ResolveConfigForRequest() name = %q, want %q", name, tt.wantName)
			}
			if locked != "" {
				t.Errorf("ResolveConfigForRequest() locked = %q, want empty", locked)
			}
		})
	}
}

func TestResolveConfigForRequestConcreteProfileFallbackName(t *testing.T) {
	configJSON := `{
		"defaultProfile":"default",
		"profiles":{
			"default":{"routingStrategy":"random"},
			"pd":{"routingStrategy":"pd"}
		}
	}`
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{constants.ModelAnnoConfig: configJSON}}}

	profile, name, _ := ResolveConfigForRequest([]*v1.Pod{pod}, "missing", RequestFeatures{})
	if profile == nil {
		t.Fatal("ResolveConfigForRequest() profile = nil")
	}
	if profile.RoutingStrategy != randomRoutingStrategy {
		t.Errorf("ResolveConfigForRequest().RoutingStrategy = %s, want %s", profile.RoutingStrategy, randomRoutingStrategy)
	}
	if name != "default" {
		t.Errorf("ResolveConfigForRequest() name = %q, want default", name)
	}
}

func TestResolveConfigForRequestAutoFallbacks(t *testing.T) {
	tests := []struct {
		name        string
		configJSON  string
		wantProfile string
		wantName    string
	}{
		{
			name:        "missing auto selection hints use default profile",
			configJSON:  `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd"}}}`,
			wantProfile: randomRoutingStrategy,
			wantName:    "default",
		},
		{
			name:        "invalid routingConfig hint is ignored",
			configJSON:  `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd","routingConfig":"invalid"}}}`,
			wantProfile: randomRoutingStrategy,
			wantName:    "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{constants.ModelAnnoConfig: tt.configJSON}}}
			profile, name, _ := ResolveConfigForRequest([]*v1.Pod{pod}, "auto", RequestFeatures{PromptTokens: intPtr(2)})
			if profile == nil {
				t.Fatal("ResolveConfigForRequest() profile = nil")
			}
			if profile.RoutingStrategy != tt.wantProfile {
				t.Errorf("ResolveConfigForRequest().RoutingStrategy = %s, want %s", profile.RoutingStrategy, tt.wantProfile)
			}
			if name != tt.wantName {
				t.Errorf("ResolveConfigForRequest() name = %q, want %q", name, tt.wantName)
			}
		})
	}
}

func TestGetProfile(t *testing.T) {
	json := `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096}},"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}}`

	cfg, err := ParseModelConfig(json)
	if err != nil || cfg == nil {
		t.Fatalf("ParseModelConfig failed: %v", err)
	}

	if p := cfg.GetProfile("pd"); p == nil || p.RoutingStrategy != "pd" {
		t.Errorf("GetProfile(pd) = %v, want routingStrategy=pd", p)
	}
	if p := cfg.GetProfile(""); p == nil || p.RoutingStrategy != "pd" {
		t.Errorf("GetProfile(\"\") should use defaultProfile, got %v", p)
	}
	if p := cfg.GetProfile("default"); p == nil || p.RoutingStrategy != "random" {
		t.Errorf("GetProfile(default) = %v", p)
	}
	// nonexistent profile falls back to defaultProfile
	if p := cfg.GetProfile("nonexistent"); p == nil || p.RoutingStrategy != "pd" {
		t.Errorf("GetProfile(nonexistent) should fall back to default, got %v", p)
	}
}

func TestGetProfile_NoDefault(t *testing.T) {
	// No defaultProfile set; falls back to "default"
	json := `{"profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd"}}}`

	cfg, err := ParseModelConfig(json)
	if err != nil || cfg == nil {
		t.Fatalf("ParseModelConfig failed: %v", err)
	}

	// Empty/unknown name should use "default" (implied default)
	if p := cfg.GetProfile(""); p == nil || p.RoutingStrategy != "random" {
		t.Errorf("GetProfile(\"\") with no defaultProfile should use \"default\", got %v", p)
	}
}

func TestResolveProfileFromPod(t *testing.T) {
	configJSON := `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMaxLength":2048}}}}`

	podWithAnno := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod1",
			Namespace:   "default",
			Annotations: map[string]string{constants.ModelAnnoConfig: configJSON},
		},
	}
	podNoAnno := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default"},
	}

	tests := []struct {
		name          string
		pod           *v1.Pod
		headerProfile string
		wantProfile   string
	}{
		{"nil pod", nil, "", ""},
		{"pod without anno", podNoAnno, "", ""},
		{"pod with anno, no header", podWithAnno, "", "pd"},
		{"pod with anno, header pd", podWithAnno, "pd", "pd"},
		{"pod with anno, header default", podWithAnno, "default", "random"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ResolveProfileFromPod(tt.pod, tt.headerProfile)
			if tt.wantProfile == "" {
				if p != nil {
					t.Errorf("ResolveProfileFromPod() = %v, want nil", p)
				}
				return
			}
			if p == nil {
				t.Errorf("ResolveProfileFromPod() = nil, want profile with routingStrategy=%s", tt.wantProfile)
				return
			}
			if p.RoutingStrategy != tt.wantProfile {
				t.Errorf("ResolveProfileFromPod().RoutingStrategy = %s, want %s", p.RoutingStrategy, tt.wantProfile)
			}
		})
	}
}

func TestParseModelConfigWithRoutingConfig(t *testing.T) {
	jsonStr := `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"pd","routingConfig":{"promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096,"combined":true,"prefillScorePolicy":"least_request","decodeScorePolicy":"least_request"}}}}`
	cfg, err := ParseModelConfig(jsonStr)
	if err != nil {
		t.Fatalf("ParseModelConfig() err=%v", err)
	}
	profile := cfg.GetProfile("default")
	if profile == nil {
		t.Fatal("GetProfile(default) = nil")
	}
	if profile.RoutingStrategy != "pd" {
		t.Errorf("RoutingStrategy = %s, want pd", profile.RoutingStrategy)
	}
	if profile.RoutingConfig == nil {
		t.Fatal("RoutingConfig = nil, want non-nil")
	}
	if !strings.Contains(string(profile.RoutingConfig), "promptLenBucketMaxLength") {
		t.Errorf("RoutingConfig should contain promptLenBucketMaxLength, got %s", string(profile.RoutingConfig))
	}
	if !strings.Contains(string(profile.RoutingConfig), "prefillScorePolicy") || !strings.Contains(string(profile.RoutingConfig), "decodeScorePolicy") {
		t.Errorf("RoutingConfig should carry PD score policy fields, got %s", string(profile.RoutingConfig))
	}
}

func TestParseModelConfigWithoutRoutingConfig(t *testing.T) {
	// Profiles without routingConfig still work
	jsonStr := `{"defaultProfile":"default","profiles":{"default":{"routingStrategy":"pd"}}}`
	cfg, err := ParseModelConfig(jsonStr)
	if err != nil {
		t.Fatalf("ParseModelConfig() err=%v", err)
	}
	profile := cfg.GetProfile("default")
	if profile == nil {
		t.Fatal("GetProfile(default) = nil")
	}
	if profile.RoutingStrategy != "pd" {
		t.Errorf("RoutingStrategy = %s, want pd", profile.RoutingStrategy)
	}
	if profile.RoutingConfig != nil {
		t.Errorf("RoutingConfig = %s, want nil", string(profile.RoutingConfig))
	}
}

func TestResolveConfig(t *testing.T) {
	podWithAnno := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod1",
			Namespace:   "default",
			Annotations: map[string]string{constants.ModelAnnoConfig: `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd"}}}`},
		},
	}
	podWithLock := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod2",
			Namespace:   "default",
			Annotations: map[string]string{constants.ModelAnnoConfig: `{"lockedRoutingStrategy":"pd","profiles":{"burst":{"routingStrategy":"random"}}}`},
		},
	}
	podWithBoth := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod3",
			Namespace:   "default",
			Annotations: map[string]string{constants.ModelAnnoConfig: `{"lockedRoutingStrategy":"pd","defaultProfile":"default","profiles":{"default":{"routingStrategy":"random"}}}`},
		},
	}
	podNoAnno := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod4", Namespace: "default"}}
	podInvalid := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod5", Namespace: "default", Annotations: map[string]string{constants.ModelAnnoConfig: `{`}}}

	tests := []struct {
		name          string
		pods          []*v1.Pod
		headerProfile string
		wantProfile   string
		wantLocked    string
	}{
		{"no pods", nil, "", "", ""},
		{"pod without annotation", []*v1.Pod{podNoAnno}, "", "", ""},
		{"profile only", []*v1.Pod{podWithAnno}, "", "pd", ""},
		{"profile selected by header", []*v1.Pod{podWithAnno}, "default", "random", ""},
		{"lock without resolvable profile", []*v1.Pod{podWithLock}, "", "", "pd"},
		{"profile and lock together", []*v1.Pod{podWithBoth}, "", "random", "pd"},
		{"invalid config skipped", []*v1.Pod{podInvalid, podWithAnno}, "", "pd", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, locked := ResolveConfig(tt.pods, tt.headerProfile)
			if tt.wantProfile == "" {
				if profile != nil {
					t.Errorf("ResolveConfig() profile = %v, want nil", profile)
				}
			} else {
				if profile == nil {
					t.Fatalf("ResolveConfig() profile = nil, want routingStrategy=%s", tt.wantProfile)
				}
				if profile.RoutingStrategy != tt.wantProfile {
					t.Errorf("ResolveConfig().profile.RoutingStrategy = %s, want %s", profile.RoutingStrategy, tt.wantProfile)
				}
			}
			if locked != tt.wantLocked {
				t.Errorf("ResolveConfig() locked = %q, want %q", locked, tt.wantLocked)
			}
		})
	}
}
