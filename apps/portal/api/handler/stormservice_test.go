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

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.StormServiceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.Items)
	assert.Equal(t, 0, resp.Pagination.Total)
}

func TestListStormServices_WithItems(t *testing.T) {
	now := time.Now()
	s1 := newTestStormService("svc-1", "default", now.Add(-2*time.Hour))
	s2 := newTestStormService("svc-2", "default", now.Add(-1*time.Hour))

	r, _ := setupStormServiceTestRouter(s1, s2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.StormServiceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 2)
	assert.Equal(t, 2, resp.Pagination.Total)

	assert.Equal(t, "svc-2", resp.Items[0].Name)
	assert.Equal(t, "svc-1", resp.Items[1].Name)
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

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var resp types.StormServiceDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "new-svc", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
	assert.True(t, resp.Stateful)
	assert.Equal(t, 1, resp.RolesCount)

	// GET the created resource and verify roles are populated
	w2 := httptest.NewRecorder()
	getReq, _ := http.NewRequest("GET", "/api/v1/stormservices/default/new-svc", nil)
	r.ServeHTTP(w2, getReq)

	require.Equal(t, http.StatusOK, w2.Code)

	var getResp types.StormServiceDetailResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &getResp))

	assert.Equal(t, 1, getResp.RolesCount)
	assert.True(t, getResp.Stateful)
}

func TestCreateStormService_ValidationError(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	body := map[string]string{"name": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/stormservices", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusBadRequest, resp.Error.Code)
}

func TestGetStormService_Found(t *testing.T) {
	svc := newTestStormService("my-svc", "default", time.Now())
	r, _ := setupStormServiceTestRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices/default/my-svc", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp types.StormServiceDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-svc", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestGetStormService_NotFound(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestDeleteStormService_Success(t *testing.T) {
	svc := newTestStormService("to-delete", "default", time.Now())
	r, _ := setupStormServiceTestRouter(svc)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/stormservices/default/to-delete", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
}

func TestDeleteStormService_NotFound(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/api/v1/stormservices/default/nonexistent", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestUpdateStormService_Success(t *testing.T) {
	svc := newTestStormService("my-svc", "default", time.Now())
	r, _ := setupStormServiceTestRouter(svc)

	newReplicas := int32(5)
	body := types.StormServiceUpdateRequest{
		Replicas: &newReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/stormservices/default/my-svc", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp types.StormServiceDetailResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, "my-svc", resp.Name)
	assert.Equal(t, "default", resp.Namespace)
}

func TestUpdateStormService_NotFound(t *testing.T) {
	r, _ := setupStormServiceTestRouter()

	newReplicas := int32(5)
	body := types.StormServiceUpdateRequest{
		Replicas: &newReplicas,
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/stormservices/default/nonexistent", bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp types.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, http.StatusNotFound, resp.Error.Code)
}

func TestListStormServices_FilterByNamespace(t *testing.T) {
	now := time.Now()
	s1 := newTestStormService("svc-1", "ns-a", now)
	s2 := newTestStormService("svc-2", "ns-b", now)

	r, _ := setupStormServiceTestRouter(s1, s2)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/stormservices?namespace=ns-a", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.StormServiceListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.Items, 1)
	assert.Equal(t, "svc-1", resp.Items[0].Name)
	assert.Equal(t, "ns-a", resp.Items[0].Namespace)
}
