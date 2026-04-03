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

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodAutoscalerListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListPodAutoscalers_WithItems(t *testing.T) {
	now := time.Now()
	p1 := newTestPodAutoscaler("pa-1", "default", now.Add(-2*time.Hour))
	p2 := newTestPodAutoscaler("pa-2", "default", now.Add(-1*time.Hour))

	r, _ := setupPodAutoscalerTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodAutoscalerListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	assert.Equal(t, "pa-2", resp.Items[0].Name)
	assert.Equal(t, "pa-1", resp.Items[1].Name)
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

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp types.PodAutoscalerDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-pa", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestCreatePodAutoscaler_ValidationError(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/podautoscalers", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetPodAutoscaler_Found(t *testing.T) {
	pa := newTestPodAutoscaler("my-pa", "default", time.Now())
	r, _ := setupPodAutoscalerTestRouter(pa)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers/default/my-pa", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodAutoscalerDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-pa", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestGetPodAutoscaler_NotFound(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeletePodAutoscaler_Success(t *testing.T) {
	pa := newTestPodAutoscaler("to-delete", "default", time.Now())
	r, _ := setupPodAutoscalerTestRouter(pa)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podautoscalers/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeletePodAutoscaler_NotFound(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/podautoscalers/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdatePodAutoscaler_Success(t *testing.T) {
	pa := newTestPodAutoscaler("my-pa", "default", time.Now())
	r, _ := setupPodAutoscalerTestRouter(pa)

	newMaxReplicas := int32(20)
	body := types.PodAutoscalerUpdateRequest{
		MaxReplicas: &newMaxReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/podautoscalers/default/my-pa", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp types.PodAutoscalerDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-pa", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestUpdatePodAutoscaler_NotFound(t *testing.T) {
	r, _ := setupPodAutoscalerTestRouter()

	newMaxReplicas := int32(20)
	body := types.PodAutoscalerUpdateRequest{
		MaxReplicas: &newMaxReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/podautoscalers/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListPodAutoscalers_FilterByNamespace(t *testing.T) {
	now := time.Now()
	p1 := newTestPodAutoscaler("pa-1", "ns-a", now)
	p2 := newTestPodAutoscaler("pa-2", "ns-b", now)

	r, _ := setupPodAutoscalerTestRouter(p1, p2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/podautoscalers?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.PodAutoscalerListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "pa-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}
