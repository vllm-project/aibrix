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
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupRayClusterFleetTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
	gin.SetMode(gin.TestMode)

	scheme := runtime.NewScheme()
	_ = orchestrationv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	fakeClient := builder.Build()

	h := New(fakeClient)
	r := gin.New()

	r.GET("/api/v1/rayclusterfleets", h.ListRayClusterFleets)
	r.POST("/api/v1/rayclusterfleets", h.CreateRayClusterFleet)
	r.GET("/api/v1/rayclusterfleets/:namespace/:name", h.GetRayClusterFleet)
	r.PUT("/api/v1/rayclusterfleets/:namespace/:name", h.UpdateRayClusterFleet)
	r.DELETE("/api/v1/rayclusterfleets/:namespace/:name", h.DeleteRayClusterFleet)

	return r, fakeClient
}

func newTestRayClusterFleet(name, namespace string, creationTime time.Time) *orchestrationv1alpha1.RayClusterFleet {
	replicas := int32(1)
	return &orchestrationv1alpha1.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: orchestrationv1alpha1.RayClusterFleetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
		Status: orchestrationv1alpha1.RayClusterFleetStatus{
			Replicas:      1,
			ReadyReplicas: 0,
		},
	}
}

func TestListRayClusterFleets_Empty(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.RayClusterFleetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListRayClusterFleets_WithItems(t *testing.T) {
	now := time.Now()
	f1 := newTestRayClusterFleet("fleet-1", "default", now.Add(-2*time.Hour))
	f2 := newTestRayClusterFleet("fleet-2", "default", now.Add(-1*time.Hour))

	r, _ := setupRayClusterFleetTestRouter(f1, f2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.RayClusterFleetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	assert.Equal(t, "fleet-2", resp.Items[0].Name)
	assert.Equal(t, "fleet-1", resp.Items[1].Name)
}

func TestCreateRayClusterFleet_Success(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	replicas := int32(3)
	body := types.RayClusterFleetCreateRequest{
		Name:      "new-fleet",
		Namespace: "default",
		Replicas:  &replicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/rayclusterfleets", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)

	var resp types.RayClusterFleetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-fleet", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, int32(3), resp.Replicas)
}

func TestCreateRayClusterFleet_ValidationError(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/rayclusterfleets", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetRayClusterFleet_Found(t *testing.T) {
	fleet := newTestRayClusterFleet("my-fleet", "default", time.Now())
	r, _ := setupRayClusterFleetTestRouter(fleet)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets/default/my-fleet", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.RayClusterFleetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-fleet", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestGetRayClusterFleet_NotFound(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeleteRayClusterFleet_Success(t *testing.T) {
	fleet := newTestRayClusterFleet("to-delete", "default", time.Now())
	r, _ := setupRayClusterFleetTestRouter(fleet)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/rayclusterfleets/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteRayClusterFleet_NotFound(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/rayclusterfleets/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdateRayClusterFleet_Success(t *testing.T) {
	fleet := newTestRayClusterFleet("my-fleet", "default", time.Now())
	r, _ := setupRayClusterFleetTestRouter(fleet)

	newReplicas := int32(5)
	body := types.RayClusterFleetUpdateRequest{
		Replicas: &newReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/rayclusterfleets/default/my-fleet", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.RayClusterFleetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-fleet", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, int32(5), resp.Replicas)
}

func TestUpdateRayClusterFleet_NotFound(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	newReplicas := int32(5)
	body := types.RayClusterFleetUpdateRequest{
		Replicas: &newReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/rayclusterfleets/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListRayClusterFleets_FilterByNamespace(t *testing.T) {
	now := time.Now()
	f1 := newTestRayClusterFleet("fleet-1", "ns-a", now)
	f2 := newTestRayClusterFleet("fleet-2", "ns-b", now)

	r, _ := setupRayClusterFleetTestRouter(f1, f2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.RayClusterFleetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "fleet-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}
