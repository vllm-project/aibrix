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

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.RayClusterFleetListResponse
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

func TestListRayClusterFleets_WithItems(t *testing.T) {
	now := time.Now()
	f1 := newTestRayClusterFleet("fleet-1", "default", now.Add(-2*time.Hour))
	f2 := newTestRayClusterFleet("fleet-2", "default", now.Add(-1*time.Hour))

	r, _ := setupRayClusterFleetTestRouter(f1, f2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.RayClusterFleetListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	if resp.Items[0].Name != "fleet-2" {
		t.Errorf("expected first item to be fleet-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "fleet-1" {
		t.Errorf("expected second item to be fleet-1, got %s", resp.Items[1].Name)
	}
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

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.RayClusterFleetDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-fleet" {
		t.Errorf("expected name new-fleet, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Replicas != 3 {
		t.Errorf("expected replicas 3, got %d", resp.Replicas)
	}
}

func TestCreateRayClusterFleet_ValidationError(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/rayclusterfleets", bytes.NewBuffer(jsonBody))
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

func TestGetRayClusterFleet_Found(t *testing.T) {
	fleet := newTestRayClusterFleet("my-fleet", "default", time.Now())
	r, _ := setupRayClusterFleetTestRouter(fleet)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets/default/my-fleet", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.RayClusterFleetDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-fleet" {
		t.Errorf("expected name my-fleet, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
}

func TestGetRayClusterFleet_NotFound(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/rayclusterfleets/default/nonexistent", nil)
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

func TestDeleteRayClusterFleet_Success(t *testing.T) {
	fleet := newTestRayClusterFleet("to-delete", "default", time.Now())
	r, _ := setupRayClusterFleetTestRouter(fleet)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/rayclusterfleets/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteRayClusterFleet_NotFound(t *testing.T) {
	r, _ := setupRayClusterFleetTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/rayclusterfleets/default/nonexistent", nil)
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
