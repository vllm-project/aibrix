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

func setupKVCacheTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
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

	r.GET("/api/v1/kvcaches", h.ListKVCaches)
	r.POST("/api/v1/kvcaches", h.CreateKVCache)
	r.GET("/api/v1/kvcaches/:namespace/:name", h.GetKVCache)
	r.PUT("/api/v1/kvcaches/:namespace/:name", h.UpdateKVCache)
	r.DELETE("/api/v1/kvcaches/:namespace/:name", h.DeleteKVCache)

	return r, fakeClient
}

func newTestKVCache(name, namespace string, creationTime time.Time) *orchestrationv1alpha1.KVCache {
	return &orchestrationv1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: orchestrationv1alpha1.KVCacheSpec{
			Mode: "distributed",
		},
		Status: orchestrationv1alpha1.KVCacheStatus{
			ReadyReplicas: 0,
		},
	}
}

func TestListKVCaches_Empty(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.KVCacheListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListKVCaches_WithItems(t *testing.T) {
	now := time.Now()
	k1 := newTestKVCache("kv-1", "default", now.Add(-2*time.Hour))
	k2 := newTestKVCache("kv-2", "default", now.Add(-1*time.Hour))

	r, _ := setupKVCacheTestRouter(k1, k2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.KVCacheListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	assert.Equal(t, "kv-2", resp.Items[0].Name)
	assert.Equal(t, "kv-1", resp.Items[1].Name)
}

func TestCreateKVCache_Success(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	body := types.KVCacheCreateRequest{
		Name:      "new-kv",
		Namespace: "default",
		Mode:      "distributed",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/kvcaches", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp types.KVCacheDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-kv", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "distributed", resp.Mode)
}

func TestCreateKVCache_ValidationError(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	body := map[string]string{"mode": "distributed"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/kvcaches", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetKVCache_Found(t *testing.T) {
	kv := newTestKVCache("my-kv", "default", time.Now())
	r, _ := setupKVCacheTestRouter(kv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches/default/my-kv", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.KVCacheDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-kv", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "distributed", resp.Mode)
}

func TestGetKVCache_NotFound(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeleteKVCache_Success(t *testing.T) {
	kv := newTestKVCache("to-delete", "default", time.Now())
	r, _ := setupKVCacheTestRouter(kv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/kvcaches/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteKVCache_NotFound(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/kvcaches/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdateKVCache_Success(t *testing.T) {
	kv := newTestKVCache("my-kv", "default", time.Now())
	r, _ := setupKVCacheTestRouter(kv)

	body := types.KVCacheUpdateRequest{
		Mode: "local",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/kvcaches/default/my-kv", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp types.KVCacheDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-kv", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.Equal(t, "local", resp.Mode)
}

func TestUpdateKVCache_NotFound(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	body := types.KVCacheUpdateRequest{
		Mode: "local",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/kvcaches/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListKVCaches_FilterByNamespace(t *testing.T) {
	now := time.Now()
	k1 := newTestKVCache("kv-1", "ns-a", now)
	k2 := newTestKVCache("kv-2", "ns-b", now)

	r, _ := setupKVCacheTestRouter(k1, k2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.KVCacheListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "kv-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}
