package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.ModelAdapterListResponse
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

func TestListModelAdapters_WithItems(t *testing.T) {
	now := time.Now()
	a1 := newTestModelAdapter("adapter-1", "default", now.Add(-2*time.Hour))
	a2 := newTestModelAdapter("adapter-2", "default", now.Add(-1*time.Hour))

	r, _ := setupTestRouter(a1, a2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.ModelAdapterListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	// Should be sorted by creation time descending (adapter-2 first)
	if resp.Items[0].Name != "adapter-2" {
		t.Errorf("expected first item to be adapter-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "adapter-1" {
		t.Errorf("expected second item to be adapter-1, got %s", resp.Items[1].Name)
	}
}

func TestListModelAdapters_FilterByNamespace(t *testing.T) {
	now := time.Now()
	a1 := newTestModelAdapter("adapter-1", "ns-a", now)
	a2 := newTestModelAdapter("adapter-2", "ns-b", now)

	r, _ := setupTestRouter(a1, a2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.ModelAdapterListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(resp.Items))
	}
	if resp.Items[0].Name != "adapter-1" {
		t.Errorf("expected adapter-1, got %s", resp.Items[0].Name)
	}
	if resp.Items[0].Namespace != "ns-a" {
		t.Errorf("expected namespace ns-a, got %s", resp.Items[0].Namespace)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.ModelAdapterDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-adapter" {
		t.Errorf("expected name new-adapter, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Spec.ArtifactURL != "s3://bucket/new-adapter" {
		t.Errorf("expected artifactURL s3://bucket/new-adapter, got %s", resp.Spec.ArtifactURL)
	}
	if resp.Spec.BaseModel != "llama-7b" {
		t.Errorf("expected baseModel llama-7b, got %s", resp.Spec.BaseModel)
	}
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

func TestGetModelAdapter_Found(t *testing.T) {
	adapter := newTestModelAdapter("my-adapter", "default", time.Now())
	r, _ := setupTestRouter(adapter)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters/default/my-adapter", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.ModelAdapterDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-adapter" {
		t.Errorf("expected name my-adapter, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Spec.BaseModel != "base-model" {
		t.Errorf("expected baseModel base-model, got %s", resp.Spec.BaseModel)
	}
	if resp.Spec.PodSelector["app"] != "vllm" {
		t.Errorf("expected podSelector app=vllm, got %v", resp.Spec.PodSelector)
	}
}

func TestGetModelAdapter_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/modeladapters/default/nonexistent", nil)
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

func TestDeleteModelAdapter_Success(t *testing.T) {
	adapter := newTestModelAdapter("to-delete", "default", time.Now())
	r, _ := setupTestRouter(adapter)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/modeladapters/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteModelAdapter_NotFound(t *testing.T) {
	r, _ := setupTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/modeladapters/default/nonexistent", nil)
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
