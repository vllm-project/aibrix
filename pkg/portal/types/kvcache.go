package types

import "time"

type KVCacheCreateRequest struct {
	Name      string `json:"name" binding:"required"`
	Namespace string `json:"namespace" binding:"required"`
	Mode      string `json:"mode,omitempty"`
}

type KVCacheUpdateRequest struct {
	Mode string `json:"mode,omitempty"`
}

type KVCacheListItem struct {
	Name          string    `json:"name"`
	Namespace     string    `json:"namespace"`
	Mode          string    `json:"mode"`
	ReadyReplicas int32     `json:"readyReplicas"`
	CreatedAt     time.Time `json:"createdAt"`
}

type KVCacheListResponse struct {
	Items      []KVCacheListItem `json:"items"`
	Pagination Pagination        `json:"pagination"`
}

type KVCacheDetailResponse struct {
	Name      string              `json:"name"`
	Namespace string              `json:"namespace"`
	Mode      string              `json:"mode"`
	Status    KVCacheDetailStatus `json:"status"`
	CreatedAt time.Time           `json:"createdAt"`
}

type KVCacheDetailStatus struct {
	ReadyReplicas int32 `json:"readyReplicas"`
}
