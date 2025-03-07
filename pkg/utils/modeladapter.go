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
	"fmt"
	"strings"
)

var AllowedSchemas = []string{"s3://", "gcs://", "huggingface://", "hf://", "/"}

// ValidateArtifactURL checks if the ArtifactURL has a valid schema (s3://, gcs://, huggingface://, https://, /)
func ValidateArtifactURL(artifactURL string) error {
	for _, schema := range AllowedSchemas {
		if strings.HasPrefix(artifactURL, schema) {
			return nil
		}
	}
	return fmt.Errorf("unsupported schema")
}
