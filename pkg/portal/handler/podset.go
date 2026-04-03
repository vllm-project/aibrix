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

func (h *Handler) ListPodSets(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list orchestrationv1alpha1.PodSetList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(context.TODO(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list pod sets", err.Error())
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

	items := make([]types.PodSetListItem, 0, end-start)
	for _, ps := range list.Items[start:end] {
		items = append(items, types.PodSetListItem{
			Name:         ps.Name,
			Namespace:    ps.Namespace,
			PodGroupSize: ps.Spec.PodGroupSize,
			ReadyPods:    ps.Status.ReadyPods,
			TotalPods:    ps.Status.TotalPods,
			Phase:        string(ps.Status.Phase),
			CreatedAt:    ps.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.PodSetListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreatePodSet(c *gin.Context) {
	var req types.PodSetCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	ps := &orchestrationv1alpha1.PodSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: orchestrationv1alpha1.PodSetSpec{
			PodGroupSize: req.PodGroupSize,
			Stateful:     req.Stateful,
		},
	}

	if err := h.client.Create(context.TODO(), ps); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("pod set %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create pod set", err.Error())
		return
	}

	c.JSON(http.StatusCreated, podSetToDetail(ps))
}

func (h *Handler) GetPodSet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var ps orchestrationv1alpha1.PodSet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &ps); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod set %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod set", err.Error())
		return
	}

	c.JSON(http.StatusOK, podSetToDetail(&ps))
}

func (h *Handler) UpdatePodSet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.PodSetUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var ps orchestrationv1alpha1.PodSet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &ps); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod set %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod set", err.Error())
		return
	}

	if req.PodGroupSize != nil {
		ps.Spec.PodGroupSize = *req.PodGroupSize
	}

	if err := h.client.Update(context.TODO(), &ps); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update pod set", err.Error())
		return
	}

	c.JSON(http.StatusOK, podSetToDetail(&ps))
}

func (h *Handler) DeletePodSet(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var ps orchestrationv1alpha1.PodSet
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &ps); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod set %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod set", err.Error())
		return
	}

	if err := h.client.Delete(context.TODO(), &ps); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete pod set", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func podSetToDetail(ps *orchestrationv1alpha1.PodSet) types.PodSetDetailResponse {
	return types.PodSetDetailResponse{
		Name:         ps.Name,
		Namespace:    ps.Namespace,
		PodGroupSize: ps.Spec.PodGroupSize,
		Stateful:     ps.Spec.Stateful,
		Status: types.PodSetDetailStatus{
			ReadyPods: ps.Status.ReadyPods,
			TotalPods: ps.Status.TotalPods,
			Phase:     string(ps.Status.Phase),
		},
		CreatedAt: ps.CreationTimestamp.Time,
	}
}
