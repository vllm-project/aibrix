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

import (
	"encoding/json"
	"testing"
	"time"
)

// generateExpectedJSON is a helper function to generate the expected JSON
func generateExpectedJSON(modelNames []string) []byte {
	response := ModelListResponse{
		Object: "list",
		Data:   []ModelInfo{},
	}
	for _, model := range modelNames {
		modelInfo := ModelInfo{
			ID:      model,
			Created: time.Now().Unix(),
			Object:  "model",
			OwnedBy: "aibrix",
		}
		response.Data = append(response.Data, modelInfo)
	}
	jsonBytes, _ := json.Marshal(response)
	return jsonBytes
}

func TestBuildModelsResponse(t *testing.T) {
	testCases := []struct {
		name           string
		modelNames     []string
		expectedLength int
		expectedJSON   string
	}{
		{
			name:           "Single model",
			modelNames:     []string{"model1"},
			expectedLength: 1,
			expectedJSON:   `{"object":"list","data":[{"id":"model1","created":0,"object":"model","owned_by":"aibrix"}]}`,
		},
		{
			name:           "Multiple models",
			modelNames:     []string{"model1", "model2", "model3"},
			expectedLength: 3,
			expectedJSON:   `{"object":"list","data":[{"id":"model1","created":0,"object":"model","owned_by":"aibrix"},{"id":"model2","created":0,"object":"model","owned_by":"aibrix"},{"id":"model3","created":0,"object":"model","owned_by":"aibrix"}]}`,
		},
		{
			name:           "Empty list",
			modelNames:     []string{},
			expectedLength: 0,
			expectedJSON:   `{"object":"list","data":[]}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			response := BuildModelsResponse(tc.modelNames)
			jsonBytes, err := json.Marshal(response)
			if err != nil {
				t.Fatalf("Error marshaling response: %v", err)
			}

			if response.Object != "list" {
				t.Errorf("For %s, expected 'object' field to be 'list', but got '%s'", tc.name, response.Object)
			}

			if len(response.Data) != tc.expectedLength {
				t.Errorf("For %s, expected 'data' slice to have length %d, but got %d", tc.name, tc.expectedLength, len(response.Data))
			}

			for i, model := range tc.modelNames {
				modelInfo := response.Data[i]
				if modelInfo.ID != model {
					t.Errorf("For %s, expected 'id' field of ModelInfo at index %d to be '%s', but got '%s'", tc.name, i, model, modelInfo.ID)
				}
				if modelInfo.Object != "model" {
					t.Errorf("For %s, expected 'object' field of ModelInfo at index %d to be 'model', but got '%s'", tc.name, i, modelInfo.Object)
				}
				if modelInfo.OwnedBy != "aibrix" {
					t.Errorf("For %s, expected 'owned_by' field of ModelInfo at index %d to be 'aibrix', but got '%s'", tc.name, i, modelInfo.OwnedBy)
				}
			}

			actualJSON := string(jsonBytes)
			if actualJSON != tc.expectedJSON {
				t.Errorf("For %s, expected JSON: %s, but got: %s", tc.name, tc.expectedJSON, actualJSON)
			}
		})
	}
}
