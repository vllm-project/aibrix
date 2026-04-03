package types

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ModelAdapterCreateRequest struct {
	Name        string            `json:"name" binding:"required"`
	Namespace   string            `json:"namespace" binding:"required"`
	BaseModel   string            `json:"baseModel,omitempty"`
	ArtifactURL string            `json:"artifactURL" binding:"required"`
	PodSelector map[string]string `json:"podSelector" binding:"required"`
	Replicas    *int32            `json:"replicas,omitempty"`
}

type ModelAdapterUpdateRequest struct {
	ArtifactURL string            `json:"artifactURL,omitempty"`
	PodSelector map[string]string `json:"podSelector,omitempty"`
	Replicas    *int32            `json:"replicas,omitempty"`
}

type ModelAdapterListItem struct {
	Name            string    `json:"name"`
	Namespace       string    `json:"namespace"`
	BaseModel       string    `json:"baseModel,omitempty"`
	ArtifactURL     string    `json:"artifactURL"`
	Phase           string    `json:"phase"`
	ReadyReplicas   int32     `json:"readyReplicas"`
	DesiredReplicas int32     `json:"desiredReplicas"`
	CreatedAt       time.Time `json:"createdAt"`
}

type ModelAdapterListResponse struct {
	Items      []ModelAdapterListItem `json:"items"`
	Pagination Pagination             `json:"pagination"`
}

type ModelAdapterDetailResponse struct {
	Name      string                   `json:"name"`
	Namespace string                   `json:"namespace"`
	Spec      ModelAdapterDetailSpec   `json:"spec"`
	Status    ModelAdapterDetailStatus `json:"status"`
	CreatedAt time.Time                `json:"createdAt"`
}

type ModelAdapterDetailSpec struct {
	BaseModel   string            `json:"baseModel,omitempty"`
	ArtifactURL string            `json:"artifactURL"`
	PodSelector map[string]string `json:"podSelector,omitempty"`
	Replicas    *int32            `json:"replicas,omitempty"`
}

type ModelAdapterDetailStatus struct {
	Phase           string             `json:"phase"`
	ReadyReplicas   int32              `json:"readyReplicas"`
	DesiredReplicas int32              `json:"desiredReplicas"`
	Instances       []string           `json:"instances,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}
