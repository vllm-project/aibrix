package portal

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/vllm-project/aibrix/apps/portal/api/handler"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewRouter(c client.Client) *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	h := handler.New(c)

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	api := r.Group("/api/v1")
	{
		api.GET("/overview", h.GetOverview)

		// ModelAdapter
		api.GET("/modeladapters", h.ListModelAdapters)
		api.POST("/modeladapters", h.CreateModelAdapter)
		api.GET("/modeladapters/:namespace/:name", h.GetModelAdapter)
		api.PUT("/modeladapters/:namespace/:name", h.UpdateModelAdapter)
		api.DELETE("/modeladapters/:namespace/:name", h.DeleteModelAdapter)

		// RayClusterFleet
		api.GET("/rayclusterfleets", h.ListRayClusterFleets)
		api.POST("/rayclusterfleets", h.CreateRayClusterFleet)
		api.GET("/rayclusterfleets/:namespace/:name", h.GetRayClusterFleet)
		api.PUT("/rayclusterfleets/:namespace/:name", h.UpdateRayClusterFleet)
		api.DELETE("/rayclusterfleets/:namespace/:name", h.DeleteRayClusterFleet)

		// StormService
		api.GET("/stormservices", h.ListStormServices)
		api.POST("/stormservices", h.CreateStormService)
		api.GET("/stormservices/:namespace/:name", h.GetStormService)
		api.PUT("/stormservices/:namespace/:name", h.UpdateStormService)
		api.DELETE("/stormservices/:namespace/:name", h.DeleteStormService)

		// PodAutoscaler
		api.GET("/podautoscalers", h.ListPodAutoscalers)
		api.POST("/podautoscalers", h.CreatePodAutoscaler)
		api.GET("/podautoscalers/:namespace/:name", h.GetPodAutoscaler)
		api.PUT("/podautoscalers/:namespace/:name", h.UpdatePodAutoscaler)
		api.DELETE("/podautoscalers/:namespace/:name", h.DeletePodAutoscaler)

		// KVCache
		api.GET("/kvcaches", h.ListKVCaches)
		api.POST("/kvcaches", h.CreateKVCache)
		api.GET("/kvcaches/:namespace/:name", h.GetKVCache)
		api.PUT("/kvcaches/:namespace/:name", h.UpdateKVCache)
		api.DELETE("/kvcaches/:namespace/:name", h.DeleteKVCache)

		// PodSet
		api.GET("/podsets", h.ListPodSets)
		api.POST("/podsets", h.CreatePodSet)
		api.GET("/podsets/:namespace/:name", h.GetPodSet)
		api.PUT("/podsets/:namespace/:name", h.UpdatePodSet)
		api.DELETE("/podsets/:namespace/:name", h.DeletePodSet)
	}

	return r
}
