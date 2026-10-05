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

package lambdacloud

import (
	"context"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// lambdaCatalog implements catalog.Catalog for Lambda Cloud against the live
// API. It is the home of the accelerator -> instance-type mapping the
// provisioner reuses at launch time.
type lambdaCatalog struct {
	client *Client
}

// newCatalog builds a Lambda catalog. A configured API key is required;
// selecting Lambda without one is an error rather than a silent degradation.
func newCatalog(cfg *Config) (*lambdaCatalog, error) {
	if cfg == nil || cfg.APIKey == "" {
		return nil, &types.CatalogError{Message: "lambdaCloud catalog selected but LAMBDA_CLOUD_API_KEY is not set"}
	}
	return &lambdaCatalog{client: NewClient(cfg)}, nil
}

// Provider returns the cloud provider this catalog is for.
func (c *lambdaCatalog) Provider() types.ResourceProvisionType {
	return types.ResourceProvisionTypeLambdaCloud
}

// ListRegions lists Lambda regions that currently have any capacity.
func (c *lambdaCatalog) ListRegions(ctx context.Context) ([]types.RegionSpec, error) {
	entries, err := c.client.ListInstanceTypes(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var regions []types.RegionSpec
	for _, entry := range entries {
		for _, r := range entry.RegionsWithCapacityAvailable {
			if !seen[r.Name] {
				seen[r.Name] = true
				regions = append(regions, types.RegionSpec{LambdaCloud: &types.LambdaCloudRegion{Region: r.Name}})
			}
		}
	}
	return regions, nil
}

// ListInstanceTypes lists Lambda instance type names, optionally restricted to
// those with capacity in the given region.
func (c *lambdaCatalog) ListInstanceTypes(ctx context.Context, region *types.RegionSpec) ([]types.InstanceTypeSpec, error) {
	entries, err := c.client.ListInstanceTypes(ctx)
	if err != nil {
		return nil, err
	}
	wantRegion := ""
	if region != nil && region.LambdaCloud != nil {
		wantRegion = region.LambdaCloud.Region
	}
	specs := make([]types.InstanceTypeSpec, 0, len(entries))
	for name, entry := range entries {
		if wantRegion != "" && !regionHasCapacity(entry.RegionsWithCapacityAvailable, wantRegion) {
			continue
		}
		specs = append(specs, types.InstanceTypeSpec{InstanceType: name})
	}
	return specs, nil
}

// ListPricing returns hourly on-demand pricing per region from the live API.
func (c *lambdaCatalog) ListPricing(ctx context.Context, _ *catalog.ResourceListOptions) ([]catalog.ResourcePricing, error) {
	entries, err := c.client.ListInstanceTypes(ctx)
	if err != nil {
		return nil, err
	}
	byRegion := map[string]map[string]catalog.ResourcePricingItem{}
	for name, entry := range entries {
		price := float64(entry.InstanceType.PriceCentsPerHour) / 100.0
		for _, r := range entry.RegionsWithCapacityAvailable {
			if byRegion[r.Name] == nil {
				byRegion[r.Name] = map[string]catalog.ResourcePricingItem{}
			}
			byRegion[r.Name][name] = catalog.ResourcePricingItem{OnDemandPrice: &price}
		}
	}
	pricing := make([]catalog.ResourcePricing, 0, len(byRegion))
	for region, items := range byRegion {
		pricing = append(pricing, catalog.ResourcePricing{
			Region: types.RegionSpec{LambdaCloud: &types.LambdaCloudRegion{Region: region}},
			Items:  items,
		})
	}
	return pricing, nil
}

// ListResources is not modeled for Lambda: the public API exposes only boolean
// per-region capacity, not the allocated/allocatable/supply breakdown that
// catalog.Resource represents.
func (c *lambdaCatalog) ListResources(_ context.Context, _ *catalog.ResourceListOptions) ([]catalog.Resource, error) {
	return []catalog.Resource{}, nil
}

// ListResourcePredictions is not supported for Lambda.
func (c *lambdaCatalog) ListResourcePredictions(
	_ context.Context, _ *catalog.ResourceListOptions,
) (map[string]catalog.Resource, error) {
	return map[string]catalog.Resource{}, nil
}

// ListPricingPredictions is not supported for Lambda (on-demand pricing only).
func (c *lambdaCatalog) ListPricingPredictions(
	_ context.Context, _ *catalog.ResourceListOptions,
) (map[string]catalog.ResourcePricing, error) {
	return map[string]catalog.ResourcePricing{}, nil
}

func regionHasCapacity(available []Region, region string) bool {
	for _, r := range available {
		if r.Name == region {
			return true
		}
	}
	return false
}
