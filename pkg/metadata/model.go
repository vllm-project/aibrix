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

package metadata

// ModelInfo represents the information about a single model
type ModelInfo struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// ModelListResponse represents the overall response structure
type ModelListResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// BuildModelsResponse converts a list of model names to the target response type
func BuildModelsResponse(modelNames []string) ModelListResponse {
	response := ModelListResponse{
		Object: "list",
		Data:   []ModelInfo{},
	}

	// Iterate over the model names and create ModelInfo objects
	// ModelCard information is reserved at this moment. It is subject to change to be more aligned with OpenAI/vLLM API.
	for _, model := range modelNames {
		modelInfo := ModelInfo{
			ID:      model,
			Created: 0,
			Object:  "model",
			OwnedBy: "aibrix", // TODO: get model ownership from tenants annotations in future
		}
		response.Data = append(response.Data, modelInfo)
	}

	return response
}
