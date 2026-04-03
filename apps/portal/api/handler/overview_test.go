package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupOverviewRouter(objects ...client.Object) *gin.Engine {
	gin.SetMode(gin.TestMode)

	scheme := runtime.NewScheme()
	_ = modelv1alpha1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	_ = autoscalingv1alpha1.AddToScheme(scheme)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	fakeClient := builder.Build()

	h := New(fakeClient)
	r := gin.New()
	r.GET("/api/v1/overview", h.GetOverview)

	return r
}

func TestGetOverview_Empty(t *testing.T) {
	r := setupOverviewRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/overview", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, 0, resp.ModelAdapters.Total)
	assert.Equal(t, 0, resp.ModelAdapters.Ready)
	assert.Equal(t, 0, resp.RayClusterFleets.Total)
	assert.Equal(t, 0, resp.StormServices.Total)
	assert.Equal(t, 0, resp.PodAutoscalers.Total)
	assert.Equal(t, 0, resp.KVCaches.Total)
	assert.Equal(t, 0, resp.PodSets.Total)
}

func TestGetOverview_WithResources(t *testing.T) {
	baseModel := "base-model"
	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "adapter-1",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel:   &baseModel,
			ArtifactURL: "s3://bucket/adapter",
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "vllm"},
			},
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Phase:         modelv1alpha1.ModelAdapterRunning,
			ReadyReplicas: 1,
		},
	}

	r := setupOverviewRouter(adapter)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/overview", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp types.OverviewResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Equal(t, 1, resp.ModelAdapters.Total)
	assert.Equal(t, 1, resp.ModelAdapters.Ready)
	assert.Equal(t, 0, resp.ModelAdapters.NotReady)

	// Other types should still be 0
	assert.Equal(t, 0, resp.RayClusterFleets.Total)
	assert.Equal(t, 0, resp.PodAutoscalers.Total)
}
