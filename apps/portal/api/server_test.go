package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupServerRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)

	scheme := runtime.NewScheme()
	_ = modelv1alpha1.AddToScheme(scheme)
	_ = orchestrationv1alpha1.AddToScheme(scheme)
	_ = autoscalingv1alpha1.AddToScheme(scheme)

	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewRouter(fc)
}

func TestHealthz(t *testing.T) {
	r := setupServerRouter()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/healthz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %s", body["status"])
	}
}

func TestAPIRoutes_Registered(t *testing.T) {
	r := setupServerRouter()

	endpoints := []string{
		"/api/v1/overview",
		"/api/v1/modeladapters",
		"/api/v1/rayclusterfleets",
		"/api/v1/stormservices",
		"/api/v1/podautoscalers",
		"/api/v1/kvcaches",
		"/api/v1/podsets",
	}

	for _, ep := range endpoints {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", ep, nil)
		r.ServeHTTP(w, req)

		if w.Code == http.StatusNotFound {
			t.Errorf("endpoint %s returned 404, expected route to be registered", ep)
		}
	}
}
