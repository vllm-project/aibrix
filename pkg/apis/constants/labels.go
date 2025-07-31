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

package constants

// Label keys used by the Aibrix system.
// The format `resource.aibrix.ai/attribute` is the standard.

const (
	// ModelNameLabel is the label for identifying the model name
	// Example: "model.aibrix.ai/name": "deepseek-llm-7b-chat"
	ModelNameLabel = "model.aibrix.ai/name"

	// ModelEngineLabel is the label for identifying the inference engine
	// Example: "model.aibrix.ai/engine": "vllm"
	ModelEngineLabel = "model.aibrix.ai/engine"

	// ModelMetricPortLabel is the label for specifying the metrics port
	// Example: "model.aibrix.ai/metric-port": "8000"
	ModelMetricPortLabel = "model.aibrix.ai/metric-port"

	// ModelPortLabel is the label for specifying the service port
	// Example: "model.aibrix.ai/port": "8080"
	ModelPortLabel = "model.aibrix.ai/port"
)

// GetModelName retrieves the model name from pod labels
func GetModelName(labels map[string]string) string {
	if model, ok := labels[ModelNameLabel]; ok {
		return model
	}
	return ""
}

// GetInferenceEngine retrieves the inference engine from pod labels
func GetInferenceEngine(labels map[string]string) string {
	if engine, ok := labels[ModelEngineLabel]; ok {
		return engine
	}
	return ""
}
