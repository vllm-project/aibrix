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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test for ValidateArtifactURL function
func TestValidateArtifactURL(t *testing.T) {
	// Case 1: Valid s3 URL
	t.Run("valid s3 URL", func(t *testing.T) {
		err := ValidateArtifactURL("s3://bucket/path")
		assert.NoError(t, err)
	})

	// Case 2: Valid gcs URL
	t.Run("valid gcs URL", func(t *testing.T) {
		err := ValidateArtifactURL("gcs://bucket/path")
		assert.NoError(t, err)
	})

	// Case 3: Valid huggingface URL
	t.Run("valid huggingface URL", func(t *testing.T) {
		err := ValidateArtifactURL("huggingface://path/to/model")
		assert.NoError(t, err)
	})

	// Case 4: Invalid scheme
	t.Run("invalid scheme", func(t *testing.T) {
		err := ValidateArtifactURL("ftp://bucket/path")
		assert.EqualError(t, err, "unsupported schema")
	})
}
