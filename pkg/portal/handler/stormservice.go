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

func (h *Handler) ListStormServices(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list orchestrationv1alpha1.StormServiceList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(context.TODO(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list storm services", err.Error())
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

	items := make([]types.StormServiceListItem, 0, end-start)
	for _, svc := range list.Items[start:end] {
		var replicas int32
		if svc.Spec.Replicas != nil {
			replicas = *svc.Spec.Replicas
		}
		rolesCount := 0
		if svc.Spec.Template.Spec != nil {
			rolesCount = len(svc.Spec.Template.Spec.Roles)
		}
		items = append(items, types.StormServiceListItem{
			Name:          svc.Name,
			Namespace:     svc.Namespace,
			Replicas:      replicas,
			ReadyReplicas: svc.Status.ReadyReplicas,
			RolesCount:    rolesCount,
			Stateful:      svc.Spec.Stateful,
			CreatedAt:     svc.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.StormServiceListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreateStormService(c *gin.Context) {
	var req types.StormServiceCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	svc := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: req.Replicas,
			Stateful: req.Stateful,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": req.Name,
				},
			},
		},
	}

	if err := h.client.Create(context.TODO(), svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("storm service %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create storm service", err.Error())
		return
	}

	c.JSON(http.StatusCreated, stormServiceToDetail(svc))
}

func (h *Handler) GetStormService(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var svc orchestrationv1alpha1.StormService
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("storm service %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get storm service", err.Error())
		return
	}

	c.JSON(http.StatusOK, stormServiceToDetail(&svc))
}

func (h *Handler) UpdateStormService(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.StormServiceUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var svc orchestrationv1alpha1.StormService
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("storm service %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get storm service", err.Error())
		return
	}

	if req.Replicas != nil {
		svc.Spec.Replicas = req.Replicas
	}

	if err := h.client.Update(context.TODO(), &svc); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update storm service", err.Error())
		return
	}

	c.JSON(http.StatusOK, stormServiceToDetail(&svc))
}

func (h *Handler) DeleteStormService(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var svc orchestrationv1alpha1.StormService
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("storm service %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get storm service", err.Error())
		return
	}

	if err := h.client.Delete(context.TODO(), &svc); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete storm service", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func stormServiceToDetail(svc *orchestrationv1alpha1.StormService) types.StormServiceDetailResponse {
	return types.StormServiceDetailResponse{
		Name:      svc.Name,
		Namespace: svc.Namespace,
		Stateful:  svc.Spec.Stateful,
		Status: types.StormServiceDetailStatus{
			Replicas:      svc.Status.Replicas,
			ReadyReplicas: svc.Status.ReadyReplicas,
		},
		CreatedAt: svc.CreationTimestamp.Time,
	}
}
