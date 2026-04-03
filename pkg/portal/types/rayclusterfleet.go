package types

import "time"

type RayClusterFleetCreateRequest struct {
	Name             string               `json:"name" binding:"required"`
	Namespace        string               `json:"namespace" binding:"required"`
	Replicas         *int32               `json:"replicas,omitempty"`
	RayVersion       string               `json:"rayVersion,omitempty"`
	HeadGroupSpec    *RayHeadGroupSpec    `json:"headGroupSpec,omitempty"`
	WorkerGroupSpecs []RayWorkerGroupSpec `json:"workerGroupSpecs,omitempty"`
}

type RayClusterFleetUpdateRequest struct {
	Replicas         *int32               `json:"replicas,omitempty"`
	WorkerGroupSpecs []RayWorkerGroupSpec `json:"workerGroupSpecs,omitempty"`
}

type RayHeadGroupSpec struct {
	Image  string `json:"image,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type RayWorkerGroupSpec struct {
	GroupName string `json:"groupName"`
	Replicas  int32  `json:"replicas"`
	Image     string `json:"image,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	GPU       int32  `json:"gpu,omitempty"`
}

type RayClusterFleetListItem struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	Replicas          int32     `json:"replicas"`
	ReadyReplicas     int32     `json:"readyReplicas"`
	AvailableReplicas int32     `json:"availableReplicas"`
	CreatedAt         time.Time `json:"createdAt"`
}

type RayClusterFleetListResponse struct {
	Items      []RayClusterFleetListItem `json:"items"`
	Pagination Pagination                `json:"pagination"`
}

type RayClusterFleetDetailResponse struct {
	Name      string                      `json:"name"`
	Namespace string                      `json:"namespace"`
	Replicas  int32                       `json:"replicas"`
	Status    RayClusterFleetDetailStatus `json:"status"`
	CreatedAt time.Time                   `json:"createdAt"`
}

type RayClusterFleetDetailStatus struct {
	Replicas            int32 `json:"replicas"`
	ReadyReplicas       int32 `json:"readyReplicas"`
	UpdatedReplicas     int32 `json:"updatedReplicas"`
	AvailableReplicas   int32 `json:"availableReplicas"`
	UnavailableReplicas int32 `json:"unavailableReplicas"`
}
