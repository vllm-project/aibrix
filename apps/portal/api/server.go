/*
Copyright 2024 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package portal

import (
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/vllm-project/aibrix/apps/portal/api/handler"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StaticFS holds the embedded frontend dist files. Set from main.go.
var StaticFS fs.FS

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

	// Serve embedded frontend (SPA fallback)
	if StaticFS != nil {
		fileServer := http.FileServer(http.FS(StaticFS))
		r.NoRoute(func(c *gin.Context) {
			// Try to serve the exact file first
			path := c.Request.URL.Path
			if f, err := StaticFS.Open(path[1:]); err == nil {
				err = f.Close()
				if err != nil {
					panic(err)
				}
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
			// SPA fallback: serve index.html for all non-API routes
			c.Request.URL.Path = "/"
			fileServer.ServeHTTP(c.Writer, c.Request)
		})
	}

	return r
}
