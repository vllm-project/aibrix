/*
Copyright 2025 The Aibrix Team.

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

package catalog

import (
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// ============================================================================
// Resource Types
// ============================================================================

// Resource
type Resource struct {
	RegionResource

	// Provider is the cloud provider.
	Provider types.ResourceProvisionType `json:"provider"`
}

// RegionResource contains region-level resource information.
type RegionResource struct {
	// Region is the region identifier, provider-specific.
	Region *types.RegionSpec `json:"region"`

	Overview []RegionResourceItem `json:"overview"`

	Breakdown map[string][]RegionResourceItem `json:"breakdown,omitempty"`
}

// RegionResourceItem represents a single resource item statistics.
type RegionResourceItem struct {
	// NextLevel provides next-level resource statistics.
	NextLevel []RegionResourceItem `json:"nextLevel,omitempty"`

	// Key is the resource key (e.g., accelerator type, instance type).
	// Example:
	//   - "accelerator_type": "a100"
	//   - "instance_type": "p4d.24xlarge"
	Key string `json:"key"`

	// Value is the resource value.
	Value string `json:"value"`

	// Stat contains detailed resource statistics.
	Stat ResourceStat `json:"stat"`
}

// ResourceStat contains detailed resource allocation statistics.
type ResourceStat struct {
	// OnDemand contains on-demand resource statistics.
	OnDemand *ResourceStatItem `json:"onDemand,omitempty"`

	// Spot contains spot resource statistics.
	Spot *ResourceStatItem `json:"spot,omitempty"`

	// Scheduled contains scheduled resource statistics.
	Scheduled *ScheduledResourceStatItem `json:"scheduled,omitempty"`
}

type ResourceStatItem struct {
	// Allocated resources.
	Allocated ResourceItem `json:"allocated"`

	// Supply resources.
	Supply ResourceItem `json:"supply"`

	// Allocatable resources.
	Allocatable ResourceItem `json:"allocatable"`
}

type ScheduledResourceStatItem struct {
	// Allocated resources.
	Allocated ScheduledResourceItem `json:"allocated"`

	// Supply resources.
	Supply ScheduledResourceItem `json:"supply"`

	// Allocatable resources.
	Allocatable ScheduledResourceItem `json:"allocatable"`
}

// ResourceItem maps resource names to quantities (node -> resource -> quantity).
type ResourceItem map[string]map[string]string

// ScheduledResourceItem maps timestamps to ResourceItem.
type ScheduledResourceItem map[string]ResourceItem

// ============================================================================
// Pricing Types
// ============================================================================

// ResourcePricing contains pricing information for a region.
type ResourcePricing struct {
	// Region is the region identifier, provider-specific.
	Region types.RegionSpec `json:"region"`

	// Items contains pricing items for each resource type.
	Items map[string]ResourcePricingItem `json:"items"`
}

type ResourcePricingItem struct {
	// OnDemandPrice is the hourly on-demand price in USD.
	OnDemandPrice *float64 `json:"onDemandPrice,omitempty"`

	// SpotPrice is the hourly spot price in USD.
	SpotPrice *float64 `json:"spotPrice,omitempty"`

	// ScheduledPrice is the hourly scheduled price in USD.
	ScheduledPrice *float64 `json:"scheduledPrice,omitempty"`
}

// ============================================================================
// List Options
// ============================================================================

// ResourceListOptions contains options for listing resources.
type ResourceListOptions struct {
	// Region is the region identifier, provider-specific.
	Region types.RegionSpec `json:"region"`

	// Filter contains the resource filters.
	Filter *ListResourceFilter `json:"filter,omitempty"`
}

// ListResourceFilter contains filters for listing resources.
type ListResourceFilter struct {
	// ResourceSelector selects resources.
	ResourceSelectors []types.Selector `json:"resourceSelectors"`
}
