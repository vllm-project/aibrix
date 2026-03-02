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
	"math"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
)

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
			json: `{"profiles":{"default":{"routingStrategy":"pd","promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}`,
		},
		{
			name: "multiple profiles with defaultProfile",
			json: `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random","promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096},"pd":{"routingStrategy":"pd","promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}`,
		},
		{
			name: "with combined field",
			json: `{"profiles":{"default":{"routingStrategy":"pd","promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048,"combined":true}}}`,
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

func TestParseModelConfig_DefaultValues(t *testing.T) {
	// promptLenBucketMinLength negative → normalized to 0
	// promptLenBucketMaxLength 0 or omitted → MaxInt32
	json := `{"profiles":{"p1":{"routingStrategy":"pd","promptLenBucketMinLength":-5,"promptLenBucketMaxLength":0},"p2":{"routingStrategy":"random"}}}`

	cfg, err := ParseModelConfig(json)
	if err != nil || cfg == nil {
		t.Fatalf("ParseModelConfig failed: %v", err)
	}

	p1 := cfg.GetProfile("p1")
	if p1 == nil {
		t.Fatal("GetProfile(p1) = nil")
	}
	if p1.PromptLenBucketMinLength != 0 {
		t.Errorf("promptLenBucketMinLength -5 should be normalized to 0, got %d", p1.PromptLenBucketMinLength)
	}
	if p1.PromptLenBucketMaxLength != math.MaxInt32 {
		t.Errorf("promptLenBucketMaxLength 0 should become MaxInt32, got %d", p1.PromptLenBucketMaxLength)
	}

	p2 := cfg.GetProfile("p2")
	if p2 == nil {
		t.Fatal("GetProfile(p2) = nil")
	}
	if p2.PromptLenBucketMaxLength != math.MaxInt32 {
		t.Errorf("omitted promptLenBucketMaxLength should become MaxInt32, got %d", p2.PromptLenBucketMaxLength)
	}
}

func TestGetProfile(t *testing.T) {
	json := `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random","promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096},"pd":{"routingStrategy":"pd","promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}`

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
	configJSON := `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random"},"pd":{"routingStrategy":"pd","promptLenBucketMaxLength":2048}}}`

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

func TestResolveProfile(t *testing.T) {
	configJSON := `{"defaultProfile":"pd","profiles":{"default":{"routingStrategy":"random","promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096},"pd":{"routingStrategy":"pd","promptLenBucketMinLength":0,"promptLenBucketMaxLength":2048}}}`

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
		pods          []*v1.Pod
		headerProfile string
		wantProfile   string
	}{
		{"no pods", nil, "", ""},
		{"pods without anno", []*v1.Pod{podNoAnno}, "", ""},
		{"pods with anno, no header", []*v1.Pod{podWithAnno}, "", "pd"},
		{"pods with anno, header pd", []*v1.Pod{podWithAnno}, "pd", "pd"},
		{"pods with anno, header default", []*v1.Pod{podWithAnno}, "default", "random"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ResolveProfile(tt.pods, tt.headerProfile)
			if tt.wantProfile == "" {
				if p != nil {
					t.Errorf("ResolveProfile() = %v, want nil", p)
				}
				return
			}
			if p == nil {
				t.Errorf("ResolveProfile() = nil, want profile with routingStrategy=%s", tt.wantProfile)
				return
			}
			if p.RoutingStrategy != tt.wantProfile {
				t.Errorf("ResolveProfile().RoutingStrategy = %s, want %s", p.RoutingStrategy, tt.wantProfile)
			}
		})
	}
}
