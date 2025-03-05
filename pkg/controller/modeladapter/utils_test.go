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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

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

// Test for getEnvKey function
func TestGetEnvKey(t *testing.T) {
	// Case 1: Environment variable exists
	t.Run("environment variable exists", func(t *testing.T) {
		err := os.Setenv("TEST_ENV", "test_value")
		assert.NoError(t, err)
		value, exists := getEnvKey("TEST_ENV")
		assert.True(t, exists)
		assert.Equal(t, "test_value", value)
		err = os.Unsetenv("TEST_ENV")
		assert.NoError(t, err)
	})

	// Case 2: Environment variable does not exist
	t.Run("environment variable does not exist", func(t *testing.T) {
		value, exists := getEnvKey("NON_EXISTENT_ENV")
		assert.False(t, exists)
		assert.Equal(t, "", value)
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

func TestBuildURLs(t *testing.T) {
	tests := []struct {
		name         string
		podIP        string
		config       config.RuntimeConfig
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
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "localhost", DefaultDebugInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, ModelListPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, LoadLoraAdapterPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "localhost", DefaultDebugInferenceEnginePort, UnloadLoraAdapterPath),
			},
			expectError: false,
		},
		{
			name:  "Runtime sidecar enabled",
			podIP: "192.168.1.2",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: true,
			},
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.2", DefaultRuntimeAPIPort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, ModelListRuntimeAPIPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, LoadLoraRuntimeAPIPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.2", DefaultRuntimeAPIPort, UnloadLoraRuntimeAPIPath),
			},
			expectError: false,
		},
		{
			name:  "Default mode",
			podIP: "192.168.1.3",
			config: config.RuntimeConfig{
				DebugMode:            false,
				EnableRuntimeSidecar: false,
			},
			expectedURLs: URLConfig{
				BaseURL:          fmt.Sprintf("http://%s:%s", "192.168.1.3", DefaultInferenceEnginePort),
				ListModelsURL:    fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, ModelListPath),
				LoadAdapterURL:   fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, LoadLoraAdapterPath),
				UnloadAdapterURL: fmt.Sprintf("http://%s:%s%s", "192.168.1.3", DefaultInferenceEnginePort, UnloadLoraAdapterPath),
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			urls := BuildURLs(tt.podIP, tt.config)

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
