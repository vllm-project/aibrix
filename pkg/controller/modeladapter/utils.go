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

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// ExtractPodNames takes a list of Pods and returns a list of their names.
func ExtractPodNames(pods []*corev1.Pod) []string {
	podNames := make([]string, len(pods))
	for i, pod := range pods {
		podNames[i] = pod.Name
	}
	return podNames
}

// ExtractPodIPs takes a list of Pods and returns a list of their ips.
func ExtractPodIPs(pods []*corev1.Pod) []string {
	podIPs := make([]string, len(pods))
	for i, pod := range pods {
		podIPs[i] = pod.Status.PodIP
	}
	return podIPs
}

func getCandidatePods(backendPods []corev1.Pod, podsWithModelAdapter []*corev1.Pod) []*corev1.Pod {
	// Step 1: Create a set of pod names from podsWithModelAdapter
	modelAdapterPodNames := make(map[string]bool)
	for _, pod := range podsWithModelAdapter {
		modelAdapterPodNames[pod.Name] = true
	}

	// Step 2: Iterate through backendPods and find pods that are not in podsWithModelAdapter
	var candidatePods []*corev1.Pod
	for _, pod := range backendPods {
		// Create a copy of the loop variable to avoid exporting the same pointer
		podCopy := pod.DeepCopy()
		if !modelAdapterPodNames[pod.Name] {
			// Pod is not in podsWithModelAdapter, add to candidatePods
			candidatePods = append(candidatePods, podCopy)
		}
	}

	return candidatePods
}

// Difference returns the elements in `a` that are not in `b`
func Difference(a, b []string) []string {
	// Create a set (map) to store elements of b for quick lookup
	bSet := make(map[string]struct{}, len(b))
	for _, item := range b {
		bSet[item] = struct{}{}
	}

	// Iterate over a and keep only elements not in bSet
	var diff []string
	for _, item := range a {
		if _, found := bSet[item]; !found {
			diff = append(diff, item)
		}
	}

	return diff
}

// NewCondition creates a new replicaset condition.
func NewCondition(condType string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	}
}
