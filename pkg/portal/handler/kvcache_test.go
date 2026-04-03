package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/portal/types"
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

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.KVCacheListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 0 {
		t.Errorf("expected total 0, got %d", resp.Pagination.Total)
	}
}

func TestListKVCaches_WithItems(t *testing.T) {
	now := time.Now()
	k1 := newTestKVCache("kv-1", "default", now.Add(-2*time.Hour))
	k2 := newTestKVCache("kv-2", "default", now.Add(-1*time.Hour))

	r, _ := setupKVCacheTestRouter(k1, k2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.KVCacheListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	if resp.Items[0].Name != "kv-2" {
		t.Errorf("expected first item to be kv-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "kv-1" {
		t.Errorf("expected second item to be kv-1, got %s", resp.Items[1].Name)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.KVCacheDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-kv" {
		t.Errorf("expected name new-kv, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Mode != "distributed" {
		t.Errorf("expected mode distributed, got %s", resp.Mode)
	}
}

func TestCreateKVCache_ValidationError(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	body := map[string]string{"mode": "distributed"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/kvcaches", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if resp.Error.Code != http.StatusBadRequest {
		t.Errorf("expected error code 400, got %d", resp.Error.Code)
	}
}

func TestGetKVCache_Found(t *testing.T) {
	kv := newTestKVCache("my-kv", "default", time.Now())
	r, _ := setupKVCacheTestRouter(kv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches/default/my-kv", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.KVCacheDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-kv" {
		t.Errorf("expected name my-kv, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Mode != "distributed" {
		t.Errorf("expected mode distributed, got %s", resp.Mode)
	}
}

func TestGetKVCache_NotFound(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/kvcaches/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if resp.Error.Code != http.StatusNotFound {
		t.Errorf("expected error code 404, got %d", resp.Error.Code)
	}
}

func TestDeleteKVCache_Success(t *testing.T) {
	kv := newTestKVCache("to-delete", "default", time.Now())
	r, _ := setupKVCacheTestRouter(kv)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/kvcaches/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteKVCache_NotFound(t *testing.T) {
	r, _ := setupKVCacheTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/kvcaches/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if resp.Error.Code != http.StatusNotFound {
		t.Errorf("expected error code 404, got %d", resp.Error.Code)
	}
}
