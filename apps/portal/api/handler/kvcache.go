package handler

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (h *Handler) ListKVCaches(c *gin.Context) {
	pagination := parsePagination(c)
	ns := namespaceFilter(c)

	var list orchestrationv1alpha1.KVCacheList
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}

	if err := h.client.List(context.TODO(), &list, opts...); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to list kv caches", err.Error())
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

	items := make([]types.KVCacheListItem, 0, end-start)
	for _, kv := range list.Items[start:end] {
		items = append(items, types.KVCacheListItem{
			Name:          kv.Name,
			Namespace:     kv.Namespace,
			Mode:          kv.Spec.Mode,
			ReadyReplicas: kv.Status.ReadyReplicas,
			CreatedAt:     kv.CreationTimestamp.Time,
		})
	}

	c.JSON(http.StatusOK, types.KVCacheListResponse{
		Items: items,
		Pagination: types.Pagination{
			Page:     pagination.Page,
			PageSize: pagination.PageSize,
			Total:    total,
		},
	})
}

func (h *Handler) CreateKVCache(c *gin.Context) {
	var req types.KVCacheCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	kv := &orchestrationv1alpha1.KVCache{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "aibrix-portal",
			},
		},
		Spec: orchestrationv1alpha1.KVCacheSpec{
			Mode: req.Mode,
		},
	}

	if err := h.client.Create(context.TODO(), kv); err != nil {
		if apierrors.IsAlreadyExists(err) {
			respondError(c, http.StatusConflict, fmt.Sprintf("kv cache %s/%s already exists", req.Namespace, req.Name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to create kv cache", err.Error())
		return
	}

	c.JSON(http.StatusCreated, kvCacheToDetail(kv))
}

func (h *Handler) GetKVCache(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var kv orchestrationv1alpha1.KVCache
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &kv); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("kv cache %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get kv cache", err.Error())
		return
	}

	c.JSON(http.StatusOK, kvCacheToDetail(&kv))
}

func (h *Handler) UpdateKVCache(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var req types.KVCacheUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	var kv orchestrationv1alpha1.KVCache
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &kv); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("kv cache %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get kv cache", err.Error())
		return
	}

	if req.Mode != "" {
		kv.Spec.Mode = req.Mode
	}

	if err := h.client.Update(context.TODO(), &kv); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update kv cache", err.Error())
		return
	}

	c.JSON(http.StatusOK, kvCacheToDetail(&kv))
}

func (h *Handler) DeleteKVCache(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	var kv orchestrationv1alpha1.KVCache
	if err := h.client.Get(context.TODO(), client.ObjectKey{Namespace: ns, Name: name}, &kv); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("kv cache %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to get kv cache", err.Error())
		return
	}

	if err := h.client.Delete(context.TODO(), &kv); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to delete kv cache", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func kvCacheToDetail(kv *orchestrationv1alpha1.KVCache) types.KVCacheDetailResponse {
	return types.KVCacheDetailResponse{
		Name:      kv.Name,
		Namespace: kv.Namespace,
		Mode:      kv.Spec.Mode,
		Status: types.KVCacheDetailStatus{
			ReadyReplicas: kv.Status.ReadyReplicas,
		},
		CreatedAt: kv.CreationTimestamp.Time,
	}
}
