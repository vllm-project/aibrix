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
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
)

func stringPtr(s string) *string {
	return &s
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func int32Ptr(i int32) *int32 {
	return &i
}

func mapPtr(m map[string]string) *map[string]string {
	return &m
}

func validateModelAdapter(instance *modelv1alpha1.ModelAdapter) error {
	if instance.Spec.ArtifactURL == "" {
		return fmt.Errorf("artifactURL is required")
	}

	if instance.Spec.PodSelector == nil {
		return fmt.Errorf("podSelector is required")
	}

	if _, err := url.ParseRequestURI(instance.Spec.ArtifactURL); err != nil {
		return fmt.Errorf("artifactURL is not a valid URL: %v", err)
	}

	if err := validateArtifactURL(instance.Spec.ArtifactURL); err != nil {
		return err
	}

	if instance.Spec.Replicas != nil && *instance.Spec.Replicas <= 0 {
		return fmt.Errorf("replicas must be greater than 0")
	}

	return nil
}

// validateArtifactURL checks if the ArtifactURL has a valid schema (s3://, gcs://, huggingface://, https://)
func validateArtifactURL(artifactURL string) error {
	allowedSchemes := []string{"s3://", "gcs://", "huggingface://", "hf://"}
	for _, scheme := range allowedSchemes {
		if strings.HasPrefix(artifactURL, scheme) {
			return nil
		}
	}
	return fmt.Errorf("artifactURL must start with one of the following schemes: s3://, gcs://, huggingface://")
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aSet := make(map[string]struct{}, len(a))
	for _, item := range a {
		aSet[item] = struct{}{}
	}

	for _, item := range b {
		if _, exists := aSet[item]; !exists {
			return false
		}
	}

	return true
}

// getEnvKey retrieves the value of the environment variable named by the key.
// If the variable is present, the function returns the value and a boolean true.
// If the variable is not present, the function returns an empty string and a boolean false.
func getEnvKey(key string) (string, bool) {
	value, exists := os.LookupEnv(key)
	return value, exists
}

func extractHuggingFacePath(artifactURL string) (string, error) {
	parsedURL, err := url.Parse(artifactURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse URL: %w", err)
	}

	// Check if the scheme is "huggingface"
	if parsedURL.Scheme != "huggingface" {
		return "", errors.New("unsupported protocol, only huggingface:// is allowed")
	}

	// Extract the path part (xxx/yyy) and trim any leading slashes
	// TODO: replace url.Parse with something else.
	path := strings.TrimPrefix(parsedURL.Host, "/") + parsedURL.Path

	if path == "" {
		return "", errors.New("invalid huggingface path, path cannot be empty")
	}

	return path, nil
}

func stringInSlice(slice []string, str string) bool {
	for _, v := range slice {
		if v == str {
			return true
		}
	}
	return false
}
