package handler

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (h *Handler) ListModelAdapters(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list modelv1alpha1.ModelAdapterList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(c.Request.Context(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list model adapters", err.Error())
		return
	}

	// Sort by creation time descending
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

	items := make([]types.ModelAdapterListItem, 0, end-start)
	for _, adapter := range list.Items[start:end] {
		baseModel := ""
		if adapter.Spec.BaseModel != nil {
			baseModel = *adapter.Spec.BaseModel
		}
		items = append(items, types.ModelAdapterListItem{
			Name:            adapter.Name,
			Namespace:       adapter.Namespace,
			BaseModel:       baseModel,
			ArtifactURL:     adapter.Spec.ArtifactURL,
			Phase:           string(adapter.Status.Phase),
			ReadyReplicas:   adapter.Status.ReadyReplicas,
			DesiredReplicas: adapter.Status.DesiredReplicas,
			CreatedAt:       adapter.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.ModelAdapterListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreateModelAdapter(c *gin.Context) {
	var req types.ModelAdapterCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	adapter := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			ArtifactURL: req.ArtifactURL,
			PodSelector: &metav1.LabelSelector{
				MatchLabels: req.PodSelector,
			},
			Replicas: req.Replicas,
		},
	}

	if req.BaseModel != "" {
		adapter.Spec.BaseModel = &req.BaseModel
	}

	if err := h.client.Create(c.Request.Context(), adapter); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("model adapter %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create model adapter", err.Error())
		return
	}

	c.JSON(http.StatusCreated, modelAdapterToDetail(adapter))
}

func (h *Handler) GetModelAdapter(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var adapter modelv1alpha1.ModelAdapter
	if err := h.client.Get(c.Request.Context(), client.ObjectKey{Namespace: ns, Name: name}, &adapter); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("model adapter %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get model adapter", err.Error())
		return
	}

	c.JSON(http.StatusOK, modelAdapterToDetail(&adapter))
}

func (h *Handler) UpdateModelAdapter(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.ModelAdapterUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var adapter modelv1alpha1.ModelAdapter
	if err := h.client.Get(c.Request.Context(), client.ObjectKey{Namespace: ns, Name: name}, &adapter); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("model adapter %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get model adapter", err.Error())
		return
	}

	if req.ArtifactURL != "" {
		adapter.Spec.ArtifactURL = req.ArtifactURL
	}
	if req.PodSelector != nil {
		adapter.Spec.PodSelector = &metav1.LabelSelector{
			MatchLabels: req.PodSelector,
		}
	}
	if req.Replicas != nil {
		adapter.Spec.Replicas = req.Replicas
	}

	if err := h.client.Update(c.Request.Context(), &adapter); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update model adapter", err.Error())
		return
	}

	c.JSON(http.StatusOK, modelAdapterToDetail(&adapter))
}

func (h *Handler) DeleteModelAdapter(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	obj := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := h.client.Delete(c.Request.Context(), obj); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("model adapter %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to delete model adapter", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func modelAdapterToDetail(adapter *modelv1alpha1.ModelAdapter) types.ModelAdapterDetailResponse {
	baseModel := ""
	if adapter.Spec.BaseModel != nil {
		baseModel = *adapter.Spec.BaseModel
	}

	var podSelector map[string]string
	if adapter.Spec.PodSelector != nil {
		podSelector = adapter.Spec.PodSelector.MatchLabels
	}

	return types.ModelAdapterDetailResponse{
		Name:      adapter.Name,
		Namespace: adapter.Namespace,
		Spec: types.ModelAdapterDetailSpec{
			BaseModel:   baseModel,
			ArtifactURL: adapter.Spec.ArtifactURL,
			PodSelector: podSelector,
			Replicas:    adapter.Spec.Replicas,
		},
		Status: types.ModelAdapterDetailStatus{
			Phase:           string(adapter.Status.Phase),
			ReadyReplicas:   adapter.Status.ReadyReplicas,
			DesiredReplicas: adapter.Status.DesiredReplicas,
			Instances:       adapter.Status.Instances,
			Conditions:      adapter.Status.Conditions,
		},
		CreatedAt: adapter.CreationTimestamp.Time,
	}
}
