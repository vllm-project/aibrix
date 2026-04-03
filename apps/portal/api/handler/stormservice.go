package handler

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/apps/portal/api/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

	if err := h.client.List(c.Request.Context(), &list, opts...); err != nil {
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

	// Convert request roles to RoleSpec
	roles := make([]orchestrationv1alpha1.RoleSpec, 0, len(req.Roles))
	for _, r := range req.Roles {
		replicas := r.Replicas
		resources := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{},
			Limits:   corev1.ResourceList{},
		}
		if r.CPU != "" {
			resources.Requests[corev1.ResourceCPU] = resource.MustParse(r.CPU)
			resources.Limits[corev1.ResourceCPU] = resource.MustParse(r.CPU)
		}
		if r.Memory != "" {
			resources.Requests[corev1.ResourceMemory] = resource.MustParse(r.Memory)
			resources.Limits[corev1.ResourceMemory] = resource.MustParse(r.Memory)
		}
		if r.GPU > 0 {
			resources.Requests[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse(fmt.Sprintf("%d", r.GPU))
			resources.Limits[corev1.ResourceName("nvidia.com/gpu")] = resource.MustParse(fmt.Sprintf("%d", r.GPU))
		}

		roles = append(roles, orchestrationv1alpha1.RoleSpec{
			Name:     r.Name,
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:      r.Name,
							Image:     r.Image,
							Resources: resources,
						},
					},
				},
			},
		})
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
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: roles,
				},
			},
		},
	}

	if err := h.client.Create(c.Request.Context(), svc); err != nil {
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
	if err := h.client.Get(c.Request.Context(), client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
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
	if err := h.client.Get(c.Request.Context(), client.ObjectKey{Namespace: ns, Name: name}, &svc); err != nil {
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

	if err := h.client.Update(c.Request.Context(), &svc); err != nil {
		respondError(c, http.StatusInternalServerError, "failed to update storm service", err.Error())
		return
	}

	c.JSON(http.StatusOK, stormServiceToDetail(&svc))
}

func (h *Handler) DeleteStormService(c *gin.Context) {
	ns := c.Param("namespace")
	name := c.Param("name")

	obj := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
	if err := h.client.Delete(c.Request.Context(), obj); err != nil {
		if apierrors.IsNotFound(err) {
			respondError(c, http.StatusNotFound, fmt.Sprintf("storm service %s/%s not found", ns, name), err.Error())
			return
		}
		respondError(c, http.StatusInternalServerError, "failed to delete storm service", err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

func stormServiceToDetail(svc *orchestrationv1alpha1.StormService) types.StormServiceDetailResponse {
	rolesCount := 0
	if svc.Spec.Template.Spec != nil {
		rolesCount = len(svc.Spec.Template.Spec.Roles)
	}

	return types.StormServiceDetailResponse{
		Name:       svc.Name,
		Namespace:  svc.Namespace,
		Stateful:   svc.Spec.Stateful,
		RolesCount: rolesCount,
		Status: types.StormServiceDetailStatus{
			Replicas:      svc.Status.Replicas,
			ReadyReplicas: svc.Status.ReadyReplicas,
		},
		CreatedAt: svc.CreationTimestamp.Time,
	}
}
