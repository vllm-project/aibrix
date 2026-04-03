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

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
	gin.SetMode(gin.TestMode)

	scheme := runtime.NewScheme()
	_ = modelv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	fakeClient := builder.Build()

	h := New(fakeClient)
	r := gin.New()

	r.GET("/api/v1/modeladapters", h.ListModelAdapters)
	r.POST("/api/v1/modeladapters", h.CreateModelAdapter)
	r.GET("/api/v1/modeladapters/:namespace/:name", h.GetModelAdapter)
	r.PUT("/api/v1/modeladapters/:namespace/:name", h.UpdateModelAdapter)
	r.DELETE("/api/v1/modeladapters/:namespace/:name", h.DeleteModelAdapter)

	return r, fakeClient
}

func newTestModelAdapter(name, namespace string, creationTime time.Time) *modelv1alpha1.ModelAdapter {
	baseModel := "base-model"
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel:   &baseModel,
			ArtifactURL: "s3://bucket/adapter",
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "vllm"},
			},
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Phase:         modelv1alpha1.ModelAdapterPending,
			ReadyReplicas: 0,
		},
	}
}

func TestListModelAdapters_Empty(t *testing.T) {
	r, _ := setupTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListModelAdapters_WithItems(t *testing.T) {
	now := time.Now()
	a1 := newTestModelAdapter("adapter-1", "default", now.Add(-2*time.Hour))
	a2 := newTestModelAdapter("adapter-2", "default", now.Add(-1*time.Hour))

	r, _ := setupTestRouter(a1, a2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	// Should be sorted by creation time descending (adapter-2 first)
	assert.Equal(t, "adapter-2", resp.Items[0].Name)
	assert.Equal(t, "adapter-1", resp.Items[1].Name)
}

func TestListModelAdapters_FilterByNamespace(t *testing.T) {
	now := time.Now()
	a1 := newTestModelAdapter("adapter-1", "ns-a", now)
	a2 := newTestModelAdapter("adapter-2", "ns-b", now)

	r, _ := setupTestRouter(a1, a2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "adapter-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}

func TestCreateModelAdapter_Success(t *testing.T) {
	r, _ := setupTestRouter()

	body := types.ModelAdapterCreateRequest{
		Name:        "new-adapter",
		Namespace:   "default",
		BaseModel:   "llama-7b",
		ArtifactURL: "s3://bucket/new-adapter",
		PodSelector: map[string]string{"app": "vllm"},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/modeladapters", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)

	var resp types.ModelAdapterDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-adapter", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "s3://bucket/new-adapter", resp.Spec.ArtifactURL)
	assert.Equal(t, "llama-7b", resp.Spec.BaseModel)
}

func TestCreateModelAdapter_ValidationError(t *testing.T) {
	r, _ := setupTestRouter()

	// Missing required fields
	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/modeladapters", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetModelAdapter_Found(t *testing.T) {
	adapter := newTestModelAdapter("my-adapter", "default", time.Now())
	r, _ := setupTestRouter(adapter)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters/default/my-adapter", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-adapter", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "base-model", resp.Spec.BaseModel)
	assert.Equal(t, "vllm", resp.Spec.PodSelector["app"])
}

func TestGetModelAdapter_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeleteModelAdapter_Success(t *testing.T) {
	adapter := newTestModelAdapter("to-delete", "default", time.Now())
	r, _ := setupTestRouter(adapter)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/modeladapters/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteModelAdapter_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/modeladapters/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdateModelAdapter_Success(t *testing.T) {
	adapter := newTestModelAdapter("my-adapter", "default", time.Now())
	r, _ := setupTestRouter(adapter)

	body := types.ModelAdapterUpdateRequest{
		ArtifactURL: "s3://bucket/updated-adapter",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/modeladapters/default/my-adapter", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-adapter", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "s3://bucket/updated-adapter", resp.Spec.ArtifactURL)
}

func TestUpdateModelAdapter_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	body := types.ModelAdapterUpdateRequest{
		ArtifactURL: "s3://bucket/updated-adapter",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/modeladapters/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListModelAdapters_Pagination(t *testing.T) {
	now := time.Now()
	a1 := newTestModelAdapter("adapter-1", "default", now.Add(-3*time.Hour))
	a2 := newTestModelAdapter("adapter-2", "default", now.Add(-2*time.Hour))
	a3 := newTestModelAdapter("adapter-3", "default", now.Add(-1*time.Hour))

	r, _ := setupTestRouter(a1, a2, a3)

	// Page 1 with pageSize 2
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters?page=1&pageSize=2", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.ModelAdapterListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 3, resp.Pagination.Total)
	assert.Equal(t, 1, resp.Pagination.Page)
	assert.Equal(t, 2, resp.Pagination.PageSize)

	// Page 2 with pageSize 2
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/modeladapters?page=2&pageSize=2", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp2 types.ModelAdapterListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp2))

	require.Len(t, resp2.Items, 1)
	assert.Equal(t, 3, resp2.Pagination.Total)
}
