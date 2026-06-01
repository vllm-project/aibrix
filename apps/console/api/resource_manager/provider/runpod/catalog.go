/*
Copyright 2026 The Aibrix Team.

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

package runpod

import (
	"context"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// runpodCatalog implements catalog.Catalog for RunPod.
//
// RunPod's REST API (rest.runpod.io/v1) exposes no GPU-type / pricing / region
// catalog endpoint — gpuTypeIds are an enum within the pod schema. Rather than
// ship a hand-maintained static table, the catalog reports nothing; callers
// pass the RunPod gpuTypeId directly via acceleratorPreference.preferredTypes.
type runpodCatalog struct{}

func newCatalog() (*runpodCatalog, error) {
	return &runpodCatalog{}, nil
}

// Provider returns the cloud provider this catalog is for.
func (c *runpodCatalog) Provider() types.ResourceProvisionType {
	return types.ResourceProvisionTypeRunPod
}

// ListRegions is not exposed by the RunPod REST API.
func (c *runpodCatalog) ListRegions(_ context.Context) ([]types.RegionSpec, error) {
	return []types.RegionSpec{}, nil
}

// ListInstanceTypes is not exposed by the RunPod REST API.
func (c *runpodCatalog) ListInstanceTypes(_ context.Context, _ *types.RegionSpec) ([]types.InstanceTypeSpec, error) {
	return []types.InstanceTypeSpec{}, nil
}

// ListPricing is not exposed by the RunPod REST API.
func (c *runpodCatalog) ListPricing(_ context.Context, _ *catalog.ResourceListOptions) ([]catalog.ResourcePricing, error) {
	return []catalog.ResourcePricing{}, nil
}

// ListResources is not exposed by the RunPod REST API.
func (c *runpodCatalog) ListResources(_ context.Context, _ *catalog.ResourceListOptions) ([]catalog.Resource, error) {
	return []catalog.Resource{}, nil
}

// ListResourcePredictions is not supported for RunPod.
func (c *runpodCatalog) ListResourcePredictions(
	_ context.Context, _ *catalog.ResourceListOptions,
) (map[string]catalog.Resource, error) {
	return map[string]catalog.Resource{}, nil
}

// ListPricingPredictions is not supported for RunPod.
func (c *runpodCatalog) ListPricingPredictions(
	_ context.Context, _ *catalog.ResourceListOptions,
) (map[string]catalog.ResourcePricing, error) {
	return map[string]catalog.ResourcePricing{}, nil
}
