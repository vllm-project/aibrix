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
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupStormServiceTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
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

	r.GET("/api/v1/stormservices", h.ListStormServices)
	r.POST("/api/v1/stormservices", h.CreateStormService)
	r.GET("/api/v1/stormservices/:namespace/:name", h.GetStormService)
	r.PUT("/api/v1/stormservices/:namespace/:name", h.UpdateStormService)
	r.DELETE("/api/v1/stormservices/:namespace/:name", h.DeleteStormService)

	return r, fakeClient
}

func newTestStormService(name, namespace string, creationTime time.Time) *orchestrationv1alpha1.StormService {
	replicas := int32(1)
	return &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: &replicas,
			Stateful: false,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
		},
		Status: orchestrationv1alpha1.StormServiceStatus{
			Replicas:      1,
			ReadyReplicas: 0,
		},
	}
}

func TestListStormServices_Empty(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.StormServiceListResponse
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

func TestListStormServices_WithItems(t *testing.T) {
	now := time.Now()
	s1 := newTestStormService("svc-1", "default", now.Add(-2*time.Hour))
	s2 := newTestStormService("svc-2", "default", now.Add(-1*time.Hour))

	r, _ := setupStormServiceTestRouter(s1, s2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.StormServiceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	if resp.Items[0].Name != "svc-2" {
		t.Errorf("expected first item to be svc-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "svc-1" {
		t.Errorf("expected second item to be svc-1, got %s", resp.Items[1].Name)
	}
}

func TestCreateStormService_Success(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	replicas := int32(2)
	body := types.StormServiceCreateRequest{
		Name:      "new-svc",
		Namespace: "default",
		Replicas:  &replicas,
		Stateful:  true,
		Roles: []types.StormRoleSpec{
			{Name: "worker", Replicas: 1, Image: "worker:latest"},
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/stormservices", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.StormServiceDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-svc" {
		t.Errorf("expected name new-svc, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
	if resp.Stateful != true {
		t.Errorf("expected stateful true, got %v", resp.Stateful)
	}
}

func TestCreateStormService_ValidationError(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/stormservices", bytes.NewBuffer(jsonBody))
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

func TestGetStormService_Found(t *testing.T) {
	svc := newTestStormService("my-svc", "default", time.Now())
	r, _ := setupStormServiceTestRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices/default/my-svc", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.StormServiceDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-svc" {
		t.Errorf("expected name my-svc, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
}

func TestGetStormService_NotFound(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices/default/nonexistent", nil)
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

func TestDeleteStormService_Success(t *testing.T) {
	svc := newTestStormService("to-delete", "default", time.Now())
	r, _ := setupStormServiceTestRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/stormservices/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteStormService_NotFound(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/stormservices/default/nonexistent", nil)
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
