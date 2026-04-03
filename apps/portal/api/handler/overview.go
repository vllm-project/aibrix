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

package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
)

func (h *Handler) GetOverview(c *gin.Context) {
	var resp types.OverviewResponse

	ctx := c.Request.Context()

	// ModelAdapters
	var adapterList modelv1alpha1.ModelAdapterList
	if err := h.client.List(ctx, &adapterList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list model adapters", err.Error())
		return
	}
	resp.ModelAdapters.Total = len(adapterList.Items)
	for _, a := range adapterList.Items {
		if a.Status.Phase == modelv1alpha1.ModelAdapterRunning {
			resp.ModelAdapters.Ready++
		}
	}
	resp.ModelAdapters.NotReady = resp.ModelAdapters.Total - resp.ModelAdapters.Ready

	// RayClusterFleets
	var fleetList orchestrationv1alpha1.RayClusterFleetList
	if err := h.client.List(ctx, &fleetList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list ray cluster fleets", err.Error())
		return
	}
	resp.RayClusterFleets.Total = len(fleetList.Items)
	for _, f := range fleetList.Items {
		var replicas int32
		if f.Spec.Replicas != nil {
			replicas = *f.Spec.Replicas
		}
		if f.Status.ReadyReplicas == replicas && replicas > 0 {
			resp.RayClusterFleets.Ready++
		}
	}
	resp.RayClusterFleets.NotReady = resp.RayClusterFleets.Total - resp.RayClusterFleets.Ready

	// StormServices
	var stormList orchestrationv1alpha1.StormServiceList
	if err := h.client.List(ctx, &stormList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list storm services", err.Error())
		return
	}
	resp.StormServices.Total = len(stormList.Items)
	for _, s := range stormList.Items {
		var replicas int32
		if s.Spec.Replicas != nil {
			replicas = *s.Spec.Replicas
		}
		if s.Status.ReadyReplicas == replicas && replicas > 0 {
			resp.StormServices.Ready++
		}
	}
	resp.StormServices.NotReady = resp.StormServices.Total - resp.StormServices.Ready

	// PodAutoscalers
	var paList autoscalingv1alpha1.PodAutoscalerList
	if err := h.client.List(ctx, &paList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list pod autoscalers", err.Error())
		return
	}
	resp.PodAutoscalers.Total = len(paList.Items)
	resp.PodAutoscalers.Ready = len(paList.Items) // all considered active/ready
	resp.PodAutoscalers.NotReady = 0

	// KVCaches
	var kvList orchestrationv1alpha1.KVCacheList
	if err := h.client.List(ctx, &kvList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list kv caches", err.Error())
		return
	}
	resp.KVCaches.Total = len(kvList.Items)
	for _, kv := range kvList.Items {
		if kv.Status.ReadyReplicas > 0 {
			resp.KVCaches.Ready++
		}
	}
	resp.KVCaches.NotReady = resp.KVCaches.Total - resp.KVCaches.Ready

	// PodSets
	var psList orchestrationv1alpha1.PodSetList
	if err := h.client.List(ctx, &psList); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list pod sets", err.Error())
		return
	}
	resp.PodSets.Total = len(psList.Items)
	for _, ps := range psList.Items {
		if ps.Status.ReadyPods == ps.Status.TotalPods && ps.Status.TotalPods > 0 {
			resp.PodSets.Ready++
		}
	}
	resp.PodSets.NotReady = resp.PodSets.Total - resp.PodSets.Ready

	c.JSON(http.StatusOK, resp)
}
