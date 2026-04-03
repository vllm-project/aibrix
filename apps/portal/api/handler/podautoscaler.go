package handler

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (h *Handler) ListPodAutoscalers(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list autoscalingv1alpha1.PodAutoscalerList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(context.TODO(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list pod autoscalers", err.Error())
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

	items := make([]types.PodAutoscalerListItem, 0, end-start)
	for _, pa := range list.Items[start:end] {
		items = append(items, types.PodAutoscalerListItem{
			Name:            pa.Name,
			Namespace:       pa.Namespace,
			ScalingStrategy: string(pa.Spec.ScalingStrategy),
			MinReplicas:     pa.Spec.MinReplicas,
			MaxReplicas:     pa.Spec.MaxReplicas,
			DesiredScale:    pa.Status.DesiredScale,
			ActualScale:     pa.Status.ActualScale,
			CreatedAt:       pa.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.PodAutoscalerListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreatePodAutoscaler(c *gin.Context) {
	var req types.PodAutoscalerCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  req.ScaleTargetRef,
			MinReplicas:     req.MinReplicas,
			MaxReplicas:     req.MaxReplicas,
			ScalingStrategy: autoscalingv1alpha1.ScalingStrategyType(req.ScalingStrategy),
		},
	}

	if err := h.client.Create(context.TODO(), pa); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("pod autoscaler %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create pod autoscaler", err.Error())
		return
	}

	c.JSON(http.StatusCreated, podAutoscalerToDetail(pa))
}

func (h *Handler) GetPodAutoscaler(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &pa); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod autoscaler %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod autoscaler", err.Error())
		return
	}

	c.JSON(http.StatusOK, podAutoscalerToDetail(&pa))
}

func (h *Handler) UpdatePodAutoscaler(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.PodAutoscalerUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &pa); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod autoscaler %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod autoscaler", err.Error())
		return
	}

	if req.MinReplicas != nil {
		pa.Spec.MinReplicas = req.MinReplicas
	}
	if req.MaxReplicas != nil {
		pa.Spec.MaxReplicas = *req.MaxReplicas
	}
	if req.ScalingStrategy != "" {
		pa.Spec.ScalingStrategy = autoscalingv1alpha1.ScalingStrategyType(req.ScalingStrategy)
	}

	if err := h.client.Update(context.TODO(), &pa); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update pod autoscaler", err.Error())
		return
	}

	c.JSON(http.StatusOK, podAutoscalerToDetail(&pa))
}

func (h *Handler) DeletePodAutoscaler(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &pa); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("pod autoscaler %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get pod autoscaler", err.Error())
		return
	}

	if err := h.client.Delete(context.TODO(), &pa); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete pod autoscaler", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func podAutoscalerToDetail(pa *autoscalingv1alpha1.PodAutoscaler) types.PodAutoscalerDetailResponse {
	return types.PodAutoscalerDetailResponse{
		Name:      pa.Name,
		Namespace: pa.Namespace,
		Status: types.PodAutoscalerDetailStatus{
			DesiredScale: pa.Status.DesiredScale,
			ActualScale:  pa.Status.ActualScale,
		},
		CreatedAt: pa.CreationTimestamp.Time,
	}
}
