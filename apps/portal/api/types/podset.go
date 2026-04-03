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
