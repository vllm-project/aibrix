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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type ErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type Pagination struct {
	Page     int `json:"page"`
	PageSize int `json:"pageSize"`
	Total    int `json:"total"`
}

type PaginationParams struct {
	Page     int
	PageSize int
}

func DefaultPagination() PaginationParams {
	return PaginationParams{Page: 1, PageSize: 20}
}

type ResourceStatus struct {
	Phase      string             `json:"phase"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type OverviewCount struct {
	Total    int `json:"total"`
	Ready    int `json:"ready"`
	NotReady int `json:"notReady"`
}

type OverviewResponse struct {
	ModelAdapters    OverviewCount `json:"modelAdapters"`
	RayClusterFleets OverviewCount `json:"rayClusterFleets"`
	StormServices    OverviewCount `json:"stormServices"`
	PodAutoscalers   OverviewCount `json:"podAutoscalers"`
	KVCaches         OverviewCount `json:"kvCaches"`
	PodSets          OverviewCount `json:"podSets"`
}
