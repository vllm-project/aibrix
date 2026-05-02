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

// Package catalog provides instance type discovery, pricing information, and resource management.
// Each cloud provider has a unified interface for querying instance types,
// accelerators, costs, and resource availability.
// Instance and Resource are treated as logically equivalent concepts.
package catalog

import (
	"context"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Catalog provides resource discovery and pricing information.
type Catalog interface {
	// Provider returns the cloud provider this catalog is for.
	Provider() types.ResourceProvisionType

	// ResourceCatalog provides resource discovery.
	ResourceCatalog

	// PricingCatalog provides pricing information.
	PricingCatalog
}

// ResourceCatalog provides resource discovery.
type ResourceCatalog interface {
	// ListRegions lists available regions for the catalog.
	ListRegions(ctx context.Context) ([]types.RegionSpec, error)

	// ListInstanceTypes lists available instance types for the catalog.
	ListInstanceTypes(ctx context.Context) ([]types.InstanceTypeSpec, error)

	// ListResources lists available resources matching the options.
	ListResources(ctx context.Context, opts *ResourceListOptions) ([]Resource, error)

	// ListResourcePredictions lists resource predictions for the options.
	ListResourcePredictions(ctx context.Context, opts *ResourceListOptions) (map[string]Resource, error)
}

// PricingCatalog provides pricing information.
type PricingCatalog interface {
	// ListPricing returns pricing information for instance types.
	ListPricing(ctx context.Context, opts *ResourceListOptions) ([]ResourcePricing, error)

	// ListPricingPredictions lists pricing predictions for the options.
	ListPricingPredictions(ctx context.Context, opts *ResourceListOptions) (map[string]ResourcePricing, error)
}
