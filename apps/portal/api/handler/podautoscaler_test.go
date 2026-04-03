package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupPodAutoscalerTestRouter(objects ...client.Object) (*gin.Engine, client.Client) {
	gin.SetMode(gin.TestMode)

	scheme := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	fakeClient := builder.Build()

	h := New(fakeClient)
	r := gin.New()

	r.GET("/api/v1/podautoscalers", h.ListPodAutoscalers)
	r.POST("/api/v1/podautoscalers", h.CreatePodAutoscaler)
	r.GET("/api/v1/podautoscalers/:namespace/:name", h.GetPodAutoscaler)
	r.PUT("/api/v1/podautoscalers/:namespace/:name", h.UpdatePodAutoscaler)
	r.DELETE("/api/v1/podautoscalers/:namespace/:name", h.DeletePodAutoscaler)

	return r, fakeClient
}

func newTestPodAutoscaler(name, namespace string, creationTime time.Time) *autoscalingv1alpha1.PodAutoscaler {
	minReplicas := int32(1)
	return &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-deployment",
			},
			MinReplicas:     &minReplicas,
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.HPA,
		},
		Status: autoscalingv1alpha1.PodAutoscalerStatus{
			DesiredScale: 1,
			ActualScale:  1,
		},
	}
}

func TestListPodAutoscalers_Empty(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.PodAutoscalerListResponse
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

func TestListPodAutoscalers_WithItems(t *testing.T) {
	now := time.Now()
	p1 := newTestPodAutoscaler("pa-1", "default", now.Add(-2*time.Hour))
	p2 := newTestPodAutoscaler("pa-2", "default", now.Add(-1*time.Hour))

	r, _ := setupPodAutoscalerTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp types.PodAutoscalerListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(resp.Items))
	}
	if resp.Pagination.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Pagination.Total)
	}

	if resp.Items[0].Name != "pa-2" {
		t.Errorf("expected first item to be pa-2, got %s", resp.Items[0].Name)
	}
	if resp.Items[1].Name != "pa-1" {
		t.Errorf("expected second item to be pa-1, got %s", resp.Items[1].Name)
	}
}

func TestCreatePodAutoscaler_Success(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	minReplicas := int32(1)
	body := types.PodAutoscalerCreateRequest{
		Name:      "new-pa",
		Namespace: "default",
		ScaleTargetRef: corev1.ObjectReference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       "my-deployment",
		},
		MinReplicas:     &minReplicas,
		MaxReplicas:     10,
		ScalingStrategy: "HPA",
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podautoscalers", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.PodAutoscalerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "new-pa" {
		t.Errorf("expected name new-pa, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
}

func TestCreatePodAutoscaler_ValidationError(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podautoscalers", bytes.NewBuffer(jsonBody))
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

func TestGetPodAutoscaler_Found(t *testing.T) {
	pa := newTestPodAutoscaler("my-pa", "default", time.Now())
	r, _ := setupPodAutoscalerTestRouter(pa)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers/default/my-pa", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.PodAutoscalerDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Name != "my-pa" {
		t.Errorf("expected name my-pa, got %s", resp.Name)
	}
	if resp.Namespace != "default" {
		t.Errorf("expected namespace default, got %s", resp.Namespace)
	}
}

func TestGetPodAutoscaler_NotFound(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers/default/nonexistent", nil)
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

func TestDeletePodAutoscaler_Success(t *testing.T) {
	pa := newTestPodAutoscaler("to-delete", "default", time.Now())
	r, _ := setupPodAutoscalerTestRouter(pa)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podautoscalers/default/to-delete", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeletePodAutoscaler_NotFound(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podautoscalers/default/nonexistent", nil)
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
