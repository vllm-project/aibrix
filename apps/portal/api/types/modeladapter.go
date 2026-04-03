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
