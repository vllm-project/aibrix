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
	"strings"

	"github.com/vllm-project/aibrix/pkg/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

func StringInSlice(slice []string, str string) bool {
	for _, v := range slice {
		if v == str {
			return true
		}
	}
	return false
}

// RemoveInstanceFromList removes a string from a slice of strings
func RemoveInstanceFromList(slice []string, strToRemove string) []string {
	var result []string
	for _, s := range slice {
		if s != strToRemove {
			result = append(result, s)
		}
	}
	return result
}

// NewCondition creates a new condition.
func NewCondition(condType string, status metav1.ConditionStatus, reason, msg string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	}
}

// DetectRuntimeSidecar checks if a pod has the aibrix-runtime sidecar container
func DetectRuntimeSidecar(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	// Check all containers for the runtime sidecar
	for _, container := range pod.Spec.Containers {
		if container.Name == "aibrix-runtime" {
			return true
		}
	}

	return false
}

// BuildURLs constructs the API URLs for model adapter operations
// useSidecar parameter determines whether to use runtime sidecar API or direct engine API
func BuildURLs(podIP string, config config.RuntimeConfig, useSidecar bool) URLConfig {
	var host string
	if config.DebugMode {
		host = fmt.Sprintf("http://%s:%s", "localhost", DefaultDebugInferenceEnginePort)
	} else if useSidecar {
		host = fmt.Sprintf("http://%s:%s", podIP, DefaultRuntimeAPIPort)
	} else {
		host = fmt.Sprintf("http://%s:%s", podIP, DefaultInferenceEnginePort)
	}

	apiPath := ModelListPath
	loadPath := LoadLoraAdapterPath
	unloadPath := UnloadLoraAdapterPath
	if useSidecar {
		apiPath = ModelListRuntimeAPIPath
		loadPath = LoadLoraRuntimeAPIPath
		unloadPath = UnloadLoraRuntimeAPIPath
	}

	return URLConfig{
		BaseURL:          host,
		ListModelsURL:    fmt.Sprintf("%s%s", host, apiPath),
		LoadAdapterURL:   fmt.Sprintf("%s%s", host, loadPath),
		UnloadAdapterURL: fmt.Sprintf("%s%s", host, unloadPath),
	}
}
