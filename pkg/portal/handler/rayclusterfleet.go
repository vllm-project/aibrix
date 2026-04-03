package handler

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/portal/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (h *Handler) ListRayClusterFleets(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list orchestrationv1alpha1.RayClusterFleetList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(context.TODO(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list ray cluster fleets", err.Error())
		return
	}

	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[j].CreationTimestamp.Before(&list.Items[i].CreationTimestamp)
	})

	total := len(list.Items)
	start := (pagination.Page - 1) * pagination.PageSize
	end := start + pagination.PageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	items := make([]types.RayClusterFleetListItem, 0, end-start)
	for _, fleet := range list.Items[start:end] {
		var replicas int32
		if fleet.Spec.Replicas != nil {
			replicas = *fleet.Spec.Replicas
		}
		items = append(items, types.RayClusterFleetListItem{
			Name:              fleet.Name,
			Namespace:         fleet.Namespace,
			Replicas:          replicas,
			ReadyReplicas:     fleet.Status.ReadyReplicas,
			AvailableReplicas: fleet.Status.AvailableReplicas,
			CreatedAt:         fleet.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.RayClusterFleetListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreateRayClusterFleet(c *gin.Context) {
	var req types.RayClusterFleetCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	fleet := &orchestrationv1alpha1.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: orchestrationv1alpha1.RayClusterFleetSpec{
			Replicas: req.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": req.Name,
				},
			},
		},
	}

	if err := h.client.Create(context.TODO(), fleet); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("ray cluster fleet %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create ray cluster fleet", err.Error())
		return
	}

	c.JSON(http.StatusCreated, rayClusterFleetToDetail(fleet))
}

func (h *Handler) GetRayClusterFleet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var fleet orchestrationv1alpha1.RayClusterFleet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &fleet); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("ray cluster fleet %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get ray cluster fleet", err.Error())
		return
	}

	c.JSON(http.StatusOK, rayClusterFleetToDetail(&fleet))
}

func (h *Handler) UpdateRayClusterFleet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.RayClusterFleetUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var fleet orchestrationv1alpha1.RayClusterFleet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &fleet); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("ray cluster fleet %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get ray cluster fleet", err.Error())
		return
	}

	if req.Replicas != nil {
		fleet.Spec.Replicas = req.Replicas
	}

	if err := h.client.Update(context.TODO(), &fleet); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update ray cluster fleet", err.Error())
		return
	}

	c.JSON(http.StatusOK, rayClusterFleetToDetail(&fleet))
}

func (h *Handler) DeleteRayClusterFleet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var fleet orchestrationv1alpha1.RayClusterFleet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &fleet); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("ray cluster fleet %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get ray cluster fleet", err.Error())
		return
	}

	if err := h.client.Delete(context.TODO(), &fleet); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete ray cluster fleet", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func rayClusterFleetToDetail(fleet *orchestrationv1alpha1.RayClusterFleet) types.RayClusterFleetDetailResponse {
	var replicas int32
	if fleet.Spec.Replicas != nil {
		replicas = *fleet.Spec.Replicas
	}

	return types.RayClusterFleetDetailResponse{
		Name:      fleet.Name,
		Namespace: fleet.Namespace,
		Replicas:  replicas,
		Status: types.RayClusterFleetDetailStatus{
			Replicas:            fleet.Status.Replicas,
			ReadyReplicas:       fleet.Status.ReadyReplicas,
			UpdatedReplicas:     fleet.Status.UpdatedReplicas,
			AvailableReplicas:   fleet.Status.AvailableReplicas,
			UnavailableReplicas: fleet.Status.UnavailableReplicas,
		},
		CreatedAt: fleet.CreationTimestamp.Time,
	}
}
