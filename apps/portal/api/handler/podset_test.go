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

func setupPodSetTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
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

	r.GET("/api/v1/podsets", h.ListPodSets)
	r.POST("/api/v1/podsets", h.CreatePodSet)
	r.GET("/api/v1/podsets/:namespace/:name", h.GetPodSet)
	r.PUT("/api/v1/podsets/:namespace/:name", h.UpdatePodSet)
	r.DELETE("/api/v1/podsets/:namespace/:name", h.DeletePodSet)

	return r, fakeClient
}

func newTestPodSet(name, namespace string, creationTime time.Time) *orchestrationv1alpha1.PodSet {
	return &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: 2,
			Stateful:     false,
		},
		Status: orchestrationv1alpha1.PodSetStatus{
			ReadyPods: 0,
			TotalPods: 2,
			Phase:     orchestrationv1alpha1.PodSetPhasePending,
		},
	}
}

func TestListPodSets_Empty(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodSetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListPodSets_WithItems(t *testing.T) {
	now := time.Now()
	p1 := newTestPodSet("podset-1", "default", now.Add(-2*time.Hour))
	p2 := newTestPodSet("podset-2", "default", now.Add(-1*time.Hour))

	r, _ := setupPodSetTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodSetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	assert.Equal(t, "podset-2", resp.Items[0].Name)
	assert.Equal(t, "podset-1", resp.Items[1].Name)
}

func TestCreatePodSet_Success(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	body := types.PodSetCreateRequest{
		Name:         "new-podset",
		Namespace:    "default",
		PodGroupSize: 4,
		Stateful:     true,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podsets", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp types.PodSetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-podset", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, int32(4), resp.PodGroupSize)
	assert.True(t, resp.Stateful)
}

func TestCreatePodSet_ValidationError(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podsets", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetPodSet_Found(t *testing.T) {
	ps := newTestPodSet("my-podset", "default", time.Now())
	r, _ := setupPodSetTestRouter(ps)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets/default/my-podset", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodSetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-podset", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, int32(2), resp.PodGroupSize)
}

func TestGetPodSet_NotFound(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeletePodSet_Success(t *testing.T) {
	ps := newTestPodSet("to-delete", "default", time.Now())
	r, _ := setupPodSetTestRouter(ps)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podsets/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeletePodSet_NotFound(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podsets/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdatePodSet_Success(t *testing.T) {
	ps := newTestPodSet("my-podset", "default", time.Now())
	r, _ := setupPodSetTestRouter(ps)

	newPodGroupSize := int32(8)
	body := types.PodSetUpdateRequest{
		PodGroupSize: &newPodGroupSize,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/podsets/default/my-podset", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp types.PodSetDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-podset", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, int32(8), resp.PodGroupSize)
}

func TestUpdatePodSet_NotFound(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	newPodGroupSize := int32(8)
	body := types.PodSetUpdateRequest{
		PodGroupSize: &newPodGroupSize,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/podsets/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListPodSets_FilterByNamespace(t *testing.T) {
	now := time.Now()
	p1 := newTestPodSet("podset-1", "ns-a", now)
	p2 := newTestPodSet("podset-2", "ns-b", now)

	r, _ := setupPodSetTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodSetListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "podset-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}
