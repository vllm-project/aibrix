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

	corev1 "k8s.io/api/core/v1"
)

type PodAutoscalerCreateRequest struct {
	Name            string                 `json:"name" binding:"required"`
	Namespace       string                 `json:"namespace" binding:"required"`
	ScaleTargetRef  corev1.ObjectReference `json:"scaleTargetRef" binding:"required"`
	MinReplicas     *int32                 `json:"minReplicas,omitempty"`
	MaxReplicas     int32                  `json:"maxReplicas" binding:"required"`
	ScalingStrategy string                 `json:"scalingStrategy" binding:"required"`
}

type PodAutoscalerUpdateRequest struct {
	MinReplicas     *int32 `json:"minReplicas,omitempty"`
	MaxReplicas     *int32 `json:"maxReplicas,omitempty"`
	ScalingStrategy string `json:"scalingStrategy,omitempty"`
}

type PodAutoscalerListItem struct {
	Name            string    `json:"name"`
	Namespace       string    `json:"namespace"`
	ScalingStrategy string    `json:"scalingStrategy"`
	MinReplicas     *int32    `json:"minReplicas,omitempty"`
	MaxReplicas     int32     `json:"maxReplicas"`
	DesiredScale    int32     `json:"desiredScale"`
	ActualScale     int32     `json:"actualScale"`
	CreatedAt       time.Time `json:"createdAt"`
}

type PodAutoscalerListResponse struct {
	Items      []PodAutoscalerListItem `json:"items"`
	Pagination Pagination              `json:"pagination"`
}

type PodAutoscalerDetailResponse struct {
	Name      string                    `json:"name"`
	Namespace string                    `json:"namespace"`
	Status    PodAutoscalerDetailStatus `json:"status"`
	CreatedAt time.Time                 `json:"createdAt"`
}

type PodAutoscalerDetailStatus struct {
	DesiredScale int32 `json:"desiredScale"`
	ActualScale  int32 `json:"actualScale"`
}
