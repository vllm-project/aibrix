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
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
)

// Test for validateModelAdapter function
func TestValidateModelAdapter(t *testing.T) {
	// Case 1: All valid fields
	t.Run("valid input", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			Spec: modelv1alpha1.ModelAdapterSpec{
				ArtifactURL: "s3://bucket/path/to/artifact",
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				Replicas: ptr.To(int32(1)),
			},
		}

		err := validateModelAdapter(instance)
		assert.NoError(t, err)
	})

	// Case 2: Missing ArtifactURL
	t.Run("missing ArtifactURL", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			Spec: modelv1alpha1.ModelAdapterSpec{
				ArtifactURL: "",
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
		}

		err := validateModelAdapter(instance)
		assert.EqualError(t, err, "artifactURL is required")
	})

	// Case 3: Invalid ArtifactURL format
	t.Run("invalid ArtifactURL", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			Spec: modelv1alpha1.ModelAdapterSpec{
				ArtifactURL: "ftp://bucket/path/to/artifact",
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
			},
		}

		err := validateModelAdapter(instance)
		assert.EqualError(t, err, "artifactURL must start with one of the following schemes: s3://, gcs://, huggingface://")
	})

	// Case 4: Missing PodSelector
	t.Run("missing PodSelector", func(t *testing.T) {
		instance := &modelv1alpha1.ModelAdapter{
			Spec: modelv1alpha1.ModelAdapterSpec{
				ArtifactURL: "s3://bucket/path/to/artifact",
				PodSelector: nil,
			},
		}

		err := validateModelAdapter(instance)
		assert.EqualError(t, err, "podSelector is required")
	})

	// Case 5: Invalid Replicas
	t.Run("invalid Replicas", func(t *testing.T) {
		replicas := int32(0)
		instance := &modelv1alpha1.ModelAdapter{
			Spec: modelv1alpha1.ModelAdapterSpec{
				ArtifactURL: "s3://bucket/path/to/artifact",
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "test"},
				},
				Replicas: &replicas,
			},
		}

		err := validateModelAdapter(instance)
		assert.EqualError(t, err, "replicas must be greater than 0")
	})
}

// Test for validateArtifactURL function
func TestValidateArtifactURL(t *testing.T) {
	// Case 1: Valid s3 URL
	t.Run("valid s3 URL", func(t *testing.T) {
		err := validateArtifactURL("s3://bucket/path")
		assert.NoError(t, err)
	})

	// Case 2: Valid gcs URL
	t.Run("valid gcs URL", func(t *testing.T) {
		err := validateArtifactURL("gcs://bucket/path")
		assert.NoError(t, err)
	})

	// Case 3: Valid huggingface URL
	t.Run("valid huggingface URL", func(t *testing.T) {
		err := validateArtifactURL("huggingface://path/to/model")
		assert.NoError(t, err)
	})

	// Case 4: Invalid scheme
	t.Run("invalid scheme", func(t *testing.T) {
		err := validateArtifactURL("ftp://bucket/path")
		assert.EqualError(t, err, "artifactURL must start with one of the following schemes: s3://, gcs://, huggingface://")
	})
}

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
