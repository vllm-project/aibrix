/*
Copyright 2026 The Aibrix Team.

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

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHandleListModelsMode(t *testing.T) {
	cache.InitForTest()
	store := cache.NewForTest()
	ready := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "default"},
		Status: v1.PodStatus{
			PodIP:      "10.0.0.1",
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
	unready := ready.DeepCopy()
	unready.Name = "unready"
	unready.Status.Conditions[0].Status = v1.ConditionFalse
	cache.InitWithPods(store, []*v1.Pod{ready}, "ready-model")
	cache.InitWithPods(store, []*v1.Pod{unready}, "unready-model")

	for _, tc := range []struct {
		name string
		mode ModelListMode
		want []string
	}{
		{name: "default", want: []string{"ready-model", "unready-model"}},
		{name: "ready pods", mode: ModelListReadyPods, want: []string{"ready-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServerWithOptions(nil, nil, nil, ServerOptions{
				Cache:         store,
				ModelListMode: tc.mode,
			})
			response := httptest.NewRecorder()
			server.handleListModels(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, "application/json", response.Header().Get("Content-Type"))
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, "list", body.Object)
			ids := make([]string, len(body.Data))
			for i, model := range body.Data {
				ids[i] = model.ID
			}
			assert.ElementsMatch(t, tc.want, ids)
		})
	}
}

func TestHandleListModelsReadyPodsEmpty(t *testing.T) {
	server := NewServerWithOptions(nil, nil, nil, ServerOptions{
		Cache:         cache.NewForTest(),
		ModelListMode: ModelListReadyPods,
	})
	response := httptest.NewRecorder()
	server.handleListModels(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	require.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{"object":"list","data":[]}`, response.Body.String())
}
