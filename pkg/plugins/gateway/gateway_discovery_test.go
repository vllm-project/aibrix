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

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	discoverytest "github.com/vllm-project/aibrix/test/utils/discovery"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInstallationDiscoveryRoutingAndModels(t *testing.T) {
	const namespace = "workloads"
	const annotatedModel = "vendor/Exact-Model.v1"
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "label", Namespace: namespace,
			Labels:      map[string]string{constants.ModelLabelName: "label-wins"},
			Annotations: map[string]string{constants.ModelLabelName: "ignored-annotation"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "annotation", Namespace: namespace,
			Annotations: map[string]string{constants.ModelLabelName: annotatedModel}}},
	}
	for i := range pods {
		pods[i].Status = corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		foreign := *pods[i].DeepCopy()
		foreign.Namespace = "other-workloads"
		foreign.Status.PodIP = "10.0.0.99"
		pods = append(pods, foreign)
	}
	pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign-only", Namespace: "other-workloads",
		Annotations: map[string]string{constants.ModelLabelName: "foreign/model"}}})
	adapters := []modelv1alpha1.ModelAdapter{
		{ObjectMeta: metav1.ObjectMeta{Name: "local-adapter", Namespace: namespace},
			Status: modelv1alpha1.ModelAdapterStatus{Instances: []string{"label"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "foreign-adapter", Namespace: "other-workloads"},
			Status: modelv1alpha1.ModelAdapterStatus{Instances: []string{"label"}}},
	}
	config := discoverytest.NewServer(t, namespace, pods, adapters, nil)
	stop := make(chan struct{})
	defer close(stop)
	st := cache.InitWithOptions(config, stop, cache.InitOptions{InstallationNamespace: namespace})
	defer st.Close()
	server := &Server{cache: st}
	routing.Init()
	expected := []string{"label-wins", annotatedModel, "local-adapter"}
	require.ElementsMatch(t, expected, st.ListModels())
	for _, model := range expected {
		pool, err := st.ListPodsByModel(model)
		require.NoError(t, err)
		require.Equal(t, 1, pool.Len())
		require.Equal(t, namespace, pool.All()[0].Namespace)
		route := types.NewRoutingContext(context.Background(), routing.RouterRandom, model, "", "test", "")
		address, err := server.selectTargetPod(context.Background(), route, pool, "")
		require.NoError(t, err)
		require.Contains(t, address, "10.0.0.1:")
		route.Delete()
	}
	for _, model := range []string{"foreign/model", "foreign-adapter", "ignored-annotation", "vendor-Exact-Model.v1"} {
		_, err := st.ListPodsByModel(model)
		require.Error(t, err)
	}
	handler := modelListHTTPHandler(server.handleListModels)
	for _, path := range []string{"/v1/models", "/v1/models/", "/v1/models?limit=10", "/v1/models/?limit=10"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, response.Code)
			var body struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			var names []string
			for _, model := range body.Data {
				names = append(names, model.ID)
			}
			require.ElementsMatch(t, expected, names)
		})
	}
	for _, path := range []string{"/v1/models/unknown", "/v1/models-extra"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, response.Code)
	}
	for _, path := range []string{"/v1/models", "/v1/models/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, http.StatusMethodNotAllowed, response.Code)
	}
}
