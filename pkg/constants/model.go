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

package constants

// Label keys used by the Aibrix system.
// The format `resource.aibrix.ai/attribute` is the standard.

const (
	// ModelLabelName is the label for identifying the model name
	// Example: "model.aibrix.ai/name": "deepseek-llm-7b-chat"
	ModelLabelName = "model.aibrix.ai/name"

	// ModelLabelEngine is the label for identifying the inference engine
	// Example: "model.aibrix.ai/engine": "vllm"
	ModelLabelEngine = "model.aibrix.ai/engine"

	// ModelLabelMetricPort is the label for specifying the metrics port
	// Example: "model.aibrix.ai/metric-port": "8000"
	ModelLabelMetricPort = "model.aibrix.ai/metric-port"

	// ModelLabelPort is the label for specifying the service port
	// Example: "model.aibrix.ai/port": "8080"
	ModelLabelPort = "model.aibrix.ai/port"
)
