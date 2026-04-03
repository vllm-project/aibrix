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

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.PodSetListResponse
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

func TestListPodSets_WithItems(t *testing.T) {
	now := time.Now()
	p1 := newTestPodSet("podset-1", "default", now.Add(-2*time.Hour))
	p2 := newTestPodSet("podset-2", "default", now.Add(-1*time.Hour))

	r, _ := setupPodSetTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.PodSetListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	if resp.Items[0].Name != "podset-2" {
		t.Errorf("expected first item to be podset-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "podset-1" {
		t.Errorf("expected second item to be podset-1, got %s", resp.Items[1].Name)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.PodSetDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-podset" {
		t.Errorf("expected name new-podset, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.PodGroupSize != 4 {
		t.Errorf("expected podGroupSize 4, got %d", resp.PodGroupSize)
	}
	if resp.Stateful != true {
		t.Errorf("expected stateful true, got %v", resp.Stateful)
	}
}

func TestCreatePodSet_ValidationError(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podsets", bytes.NewBuffer(jsonBody))
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

func TestGetPodSet_Found(t *testing.T) {
	ps := newTestPodSet("my-podset", "default", time.Now())
	r, _ := setupPodSetTestRouter(ps)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets/default/my-podset", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.PodSetDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-podset" {
		t.Errorf("expected name my-podset, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.PodGroupSize != 2 {
		t.Errorf("expected podGroupSize 2, got %d", resp.PodGroupSize)
	}
}

func TestGetPodSet_NotFound(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podsets/default/nonexistent", nil)
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

func TestDeletePodSet_Success(t *testing.T) {
	ps := newTestPodSet("to-delete", "default", time.Now())
	r, _ := setupPodSetTestRouter(ps)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podsets/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeletePodSet_NotFound(t *testing.T) {
	r, _ := setupPodSetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podsets/default/nonexistent", nil)
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
