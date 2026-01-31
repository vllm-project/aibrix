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

package modeladapter

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/config"
)

// Test for equalStringSlices function
func TestEqualStringSlices(t *testing.T) {
	// Case 1: Equal slices
	t.Run("equal slices", func(t *testing.T) {
		a := []string{"one", "two", "three"}
		b := []string{"two", "one", "three"}
		assert.True(t, equalStringSlices(a, b))
	})

	// Case 2: Unequal slices - different lengths
	t.Run("unequal slices with different lengths", func(t *testing.T) {
		a := []string{"one", "two"}
		b := []string{"one", "two", "three"}
		assert.False(t, equalStringSlices(a, b))
	})

	// Case 3: Unequal slices - same lengths, different content
	t.Run("unequal slices with same lengths", func(t *testing.T) {
		a := []string{"one", "two", "three"}
		b := []string{"one", "two", "four"}
		assert.False(t, equalStringSlices(a, b))
	})
}

func TestExtractHuggingFacePath(t *testing.T) {
	tests := []struct {
		name        string
		artifactURL string
		expected    string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Valid HuggingFace URL",
			artifactURL: "huggingface://xxx/yyy",
			expected:    "xxx/yyy",
			expectError: false,
		},
		{
			name:        "Empty path in HuggingFace URL",
			artifactURL: "huggingface://",
			expectError: true,
		},
		{
			name:        "Invalid protocol (S3)",
			artifactURL: "s3://mybucket/mykey",
			expectError: true,
		},
		{
			name:        "Invalid protocol (GCS)",
			artifactURL: "gcs://mybucket/mykey",
			expectError: true,
		},
		{
			name:        "Invalid URL format",
			artifactURL: ":invalid-url",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractHuggingFacePath(tt.artifactURL)

			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if err.Error() != tt.errorMsg && tt.errorMsg != "" {
					t.Errorf("expected error message %q but got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result != tt.expected {
					t.Errorf("expected %q but got %q", tt.expected, result)
				}
			}
		})
	}
}

func TestDetectRuntimeSidecar(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name:     "Nil pod",
			pod:      nil,
			expected: false,
		},
		{
			name: "Pod with runtime sidecar",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main-container"},
						{Name: "aibrix-runtime"},
					},
				},
			},
			expected: true,
		},
		{
			name: "Pod without runtime sidecar",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main-container"},
						{Name: "other-sidecar"},
					},
				},
			},
			expected: false,
		},
		{
			name: "Pod with only main container",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main-container"},
					},
				},
			},
			expected: false,
		},
		{
			name: "Pod with empty containers",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := DetectRuntimeSidecar(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestBuildURLs(t *testing.T) {
	tests := []struct {
		name         string
		podIP        string
		config       config.RuntimeConfig
		useSidecar   bool
		engineType   string
		expectedURLs URLConfig
		expectError  bool
	}{
		{
			name:  "Debug mode enabled",
			podIP: "192.168.1.1",
			config: config.RuntimeConfig{
				DebugMode:            true,
				EnableRuntimeSidecar: false,
			},
			useSidecar: false,
			engineType: VLLMEngine,
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "localhost", DefaultDebugInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, ModelListVLLMAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, LoadLoraAdapterVLLMAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, UnloadLoraAdapterVLLMAPIPath),
			},
			expectError: false,
		},
		{
			name:  "Runtime sidecar enabled with useSidecar true",
			podIP: "192.168.1.2",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: true,
			},
			useSidecar: true,
			engineType: VLLMEngine,
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.2", DefaultRuntimeAPIPort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, ModelListRuntimeAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, LoadLoraRuntimeAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, UnloadLoraRuntimeAPIPath),
			},
			expectError: false,
		},
		{
			name:  "Default mode with useSidecar false",
			podIP: "192.168.1.3",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: false,
			},
			useSidecar: false,
			engineType: "",
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.3", DefaultInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, ModelListVLLMAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, LoadLoraAdapterVLLMAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, UnloadLoraAdapterVLLMAPIPath),
			},
			expectError: false,
		},
		{
			name:  "VLLM engine with useSidecar false",
			podIP: "192.168.1.4",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: false,
			},
			useSidecar: false,
			engineType: VLLMEngine,
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.4", DefaultInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.4", DefaultInferenceEnginePort, ModelListVLLMAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.4", DefaultInferenceEnginePort, LoadLoraAdapterVLLMAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.4", DefaultInferenceEnginePort, UnloadLoraAdapterVLLMAPIPath),
			},
			expectError: false,
		},
		{
			name:  "SGLang engine with useSidecar false",
			podIP: "192.168.1.5",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: false,
			},
			useSidecar: false,
			engineType: SGLangEngine,
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.5", DefaultInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.5", DefaultInferenceEnginePort, ModelListSGLangAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.5", DefaultInferenceEnginePort, LoadLoraAdapterSGLangAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.5", DefaultInferenceEnginePort, UnloadLoraAdapterSGLangAPIPath),
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urls := BuildURLs(tt.podIP, tt.config, tt.useSidecar, tt.engineType)

			if tt.expectError {
				t.Fatalf("Expected error but got none")
			} else {
				if urls != tt.expectedURLs {
					t.Errorf("Expected URLs %+v but got %+v", tt.expectedURLs, urls)
				}
			}
		})
	}
}
