package modeladapter

import (
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"testing"

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
				Replicas: int32Ptr(1),
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
		os.Setenv("TEST_ENV", "test_value")
		value, exists := getEnvKey("TEST_ENV")
		assert.True(t, exists)
		assert.Equal(t, "test_value", value)
		os.Unsetenv("TEST_ENV")
	})

	// Case 2: Environment variable does not exist
	t.Run("environment variable does not exist", func(t *testing.T) {
		value, exists := getEnvKey("NON_EXISTENT_ENV")
		assert.False(t, exists)
		assert.Equal(t, "", value)
	})
}
