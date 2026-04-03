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

import "time"

type PodSetCreateRequest struct {
	Name         string `json:"name" binding:"required"`
	Namespace    string `json:"namespace" binding:"required"`
	PodGroupSize int32  `json:"podGroupSize" binding:"required"`
	Stateful     bool   `json:"stateful,omitempty"`
}

type PodSetUpdateRequest struct {
	PodGroupSize *int32 `json:"podGroupSize,omitempty"`
}

type PodSetListItem struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	PodGroupSize int32     `json:"podGroupSize"`
	ReadyPods    int32     `json:"readyPods"`
	TotalPods    int32     `json:"totalPods"`
	Phase        string    `json:"phase"`
	CreatedAt    time.Time `json:"createdAt"`
}

type PodSetListResponse struct {
	Items      []PodSetListItem `json:"items"`
	Pagination Pagination       `json:"pagination"`
}

type PodSetDetailResponse struct {
	Name         string             `json:"name"`
	Namespace    string             `json:"namespace"`
	PodGroupSize int32              `json:"podGroupSize"`
	Stateful     bool               `json:"stateful"`
	Status       PodSetDetailStatus `json:"status"`
	CreatedAt    time.Time          `json:"createdAt"`
}

type PodSetDetailStatus struct {
	ReadyPods int32  `json:"readyPods"`
	TotalPods int32  `json:"totalPods"`
	Phase     string `json:"phase"`
}
