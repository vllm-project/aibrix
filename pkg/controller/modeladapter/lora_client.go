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

package modeladapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/metrics"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	VLLMEngine   string = "vllm"
	SGLangEngine string = "sglang"

	ModelListRuntimeAPIPath  = "/v1/models"
	LoadLoraRuntimeAPIPath   = "/v1/lora_adapter/load"
	UnloadLoraRuntimeAPIPath = "/v1/lora_adapter/unload"

	ModelListVLLMAPIPath         = "/v1/models"
	LoadLoraAdapterVLLMAPIPath   = "/v1/load_lora_adapter"
	UnloadLoraAdapterVLLMAPIPath = "/v1/unload_lora_adapter"

	ModelListSGLangAPIPath         = "/v1/models"
	LoadLoraAdapterSGLangAPIPath   = "/load_lora_adapter"
	UnloadLoraAdapterSGLangAPIPath = "/unload_lora_adapter"
)

func NewLoraClient(runtimeConfig config.RuntimeConfig) *loraClient {
	return &loraClient{
		runtimeConfig: runtimeConfig,
		httpClient: &http.Client{
			Timeout: time.Duration(HTTPTimeoutSeconds) * time.Second,
		},
	}
}

type loraClient struct {
	runtimeConfig config.RuntimeConfig
	httpClient    *http.Client
}

// LoadAdapter loads the loras in inference engines
func (c *loraClient) LoadAdapter(instance *modelv1alpha1.ModelAdapter, targetPod *corev1.Pod) (loaded bool, exists bool, err error) {
	// Determine whether to use runtime sidecar:
	// - If global flag is disabled, always use direct engine API
	// - If global flag is enabled, detect if pod has sidecar container
	useSidecar := c.runtimeConfig.EnableRuntimeSidecar && DetectRuntimeSidecar(targetPod)
	if useSidecar {
		klog.V(4).InfoS("Using runtime sidecar API for adapter loading", "pod", targetPod.Name, "adapter", instance.Name)
	} else {
		klog.V(4).InfoS("Using direct engine API for adapter loading", "pod", targetPod.Name, "adapter", instance.Name)
	}

	urls := BuildURLs(targetPod.Status.PodIP, c.runtimeConfig, useSidecar, metrics.GetEngineType(*targetPod))

	models, err := c.getModels(urls.ListModelsURL, instance)
	if err != nil {
		return false, false, err
	}
	if models[instance.Name] {
		return false, true, nil
	}

	err = c.loadAdapterCall(urls.LoadAdapterURL, instance, useSidecar)
	if err != nil {
		return false, false, err
	}
	return true, false, nil
}

// UnloadAdapter unloads the loras in inference engines
func (c *loraClient) UnloadAdapter(instance *modelv1alpha1.ModelAdapter, targetPod *corev1.Pod) (*http.Response, error) {
	// Determine whether to use runtime sidecar:
	// - If global flag is disabled, always use direct engine API
	// - If global flag is enabled, detect if pod has sidecar container
	useSidecar := c.runtimeConfig.EnableRuntimeSidecar && DetectRuntimeSidecar(targetPod)
	if useSidecar {
		klog.V(4).InfoS("Using runtime sidecar API for adapter unload", "pod", targetPod.Name, "adapter", instance.Name)
	} else {
		klog.V(4).InfoS("Using direct engine API for adapter unload", "pod", targetPod.Name, "adapter", instance.Name)
	}

	// Build payload using helper function
	payloadBytes, err := buildUnloadPayload(instance, useSidecar)
	if err != nil {
		return nil, err
	}

	urls := BuildURLs(targetPod.Status.PodIP, c.runtimeConfig, useSidecar, metrics.GetEngineType(*targetPod))
	req, err := http.NewRequest("POST", urls.UnloadAdapterURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	resp, err := c.httpClient.Do(req)
	return resp, err
}

func (c *loraClient) getModels(url string, instance *modelv1alpha1.ModelAdapter) (map[string]bool, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Check if "api-key" exists in the map and set the Authorization header accordingly
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get models: %s", body)
	}

	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	models := make(map[string]bool)
	for _, item := range response.Data {
		models[item.ID] = true
	}

	return models, nil
}

// Separate method to load the LoRA adapter
func (c *loraClient) loadAdapterCall(url string, instance *modelv1alpha1.ModelAdapter, useSidecar bool) error {
	var payloadBytes []byte
	var err error

	// Build payload based on runtime mode
	if useSidecar {
		// Runtime path - send original URL for artifact delegation
		payload := map[string]interface{}{
			"lora_name":    instance.Name,
			"artifact_url": instance.Spec.ArtifactURL, // Send original URL unchanged
		}

		// Add credentials secret reference if provided
		if instance.Spec.CredentialsSecretRef != nil {
			payload["credentials_secret"] = instance.Spec.CredentialsSecretRef.Name
		}

		// Add additional config if provided
		if instance.Spec.AdditionalConfig != nil {
			payload["additional_config"] = instance.Spec.AdditionalConfig
		}

		payloadBytes, err = json.Marshal(payload)
		if err != nil {
			return err
		}

		klog.V(4).InfoS("Using runtime path for artifact delegation",
			"ModelAdapter", klog.KObj(instance),
			"artifactURL", instance.Spec.ArtifactURL)

	} else {
		// Direct path - transform URL for engine (existing logic)
		artifactURL := instance.Spec.ArtifactURL

		// Transform huggingface:// URLs to paths
		if strings.HasPrefix(instance.Spec.ArtifactURL, "huggingface://") {
			var err error
			artifactURL, err = extractHuggingFacePath(instance.Spec.ArtifactURL)
			if err != nil {
				klog.ErrorS(err, "Invalid artifact URL", "artifactURL", artifactURL)
				return err
			}
		}
		// TODO: Add support for other URL transformations if needed

		payload := map[string]string{
			"lora_name": instance.Name,
			"lora_path": artifactURL,
		}

		payloadBytes, err = json.Marshal(payload)
		if err != nil {
			return err
		}

		klog.V(4).InfoS("Using direct path to engine",
			"ModelAdapter", klog.KObj(instance),
			"transformedURL", artifactURL)
	}

	// Send HTTP request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Check if "api-key" exists in the map and set the Authorization header accordingly
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to load LoRA adapter: %s", body)
	}

	klog.InfoS("Successfully loaded LoRA adapter",
		"ModelAdapter", klog.KObj(instance),
		"url", url)

	return nil
}

// buildUnloadPayload creates the unload request payload based on runtime mode
func buildUnloadPayload(instance *modelv1alpha1.ModelAdapter, useSidecar bool) ([]byte, error) {
	var payloadBytes []byte
	var err error

	if useSidecar {
		// Runtime path - include cleanup flag
		payload := map[string]interface{}{
			"lora_name":     instance.Name,
			"cleanup_local": true, // Clean up local artifacts on unload
		}
		payloadBytes, err = json.Marshal(payload)
	} else {
		// Direct path - simple unload
		payload := map[string]string{
			"lora_name": instance.Name,
		}
		payloadBytes, err = json.Marshal(payload)
	}

	return payloadBytes, err
}
