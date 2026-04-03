package types

import "time"

type StormServiceCreateRequest struct {
	Name      string          `json:"name" binding:"required"`
	Namespace string          `json:"namespace" binding:"required"`
	Replicas  *int32          `json:"replicas,omitempty"`
	Stateful  bool            `json:"stateful,omitempty"`
	Roles     []StormRoleSpec `json:"roles" binding:"required"`
}

type StormServiceUpdateRequest struct {
	Replicas *int32          `json:"replicas,omitempty"`
	Roles    []StormRoleSpec `json:"roles,omitempty"`
}

type StormRoleSpec struct {
	Name     string `json:"name" binding:"required"`
	Replicas int32  `json:"replicas"`
	Image    string `json:"image" binding:"required"`
	CPU      string `json:"cpu,omitempty"`
	Memory   string `json:"memory,omitempty"`
	GPU      int32  `json:"gpu,omitempty"`
}

type StormServiceListItem struct {
	Name          string    `json:"name"`
	Namespace     string    `json:"namespace"`
	Replicas      int32     `json:"replicas"`
	ReadyReplicas int32     `json:"readyReplicas"`
	RolesCount    int       `json:"rolesCount"`
	Stateful      bool      `json:"stateful"`
	CreatedAt     time.Time `json:"createdAt"`
}

type StormServiceListResponse struct {
	Items      []StormServiceListItem `json:"items"`
	Pagination Pagination             `json:"pagination"`
}

type StormServiceDetailResponse struct {
	Name       string                   `json:"name"`
	Namespace  string                   `json:"namespace"`
	Stateful   bool                     `json:"stateful"`
	RolesCount int                      `json:"rolesCount"`
	Status     StormServiceDetailStatus `json:"status"`
	CreatedAt  time.Time                `json:"createdAt"`
}

type StormServiceDetailStatus struct {
	Replicas      int32 `json:"replicas"`
	ReadyReplicas int32 `json:"readyReplicas"`
}
