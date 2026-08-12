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

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

func TestK8sCatalog(t *testing.T) {
	t.Skip("skip for now")

	// Check if ~/.kube/config exists
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("failed to get home directory: %v", err)
	}
	kubeconfig := filepath.Join(home, ".kube", "config")
	if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
		t.Skipf("kubeconfig not found at %s", kubeconfig)
	}

	ctx := context.Background()
	cat, err := newCatalog()
	if err != nil {
		t.Fatalf("failed to create catalog: %v", err)
	}

	// Test ListRegions
	t.Run("ListRegions", func(t *testing.T) {
		regions, err := cat.ListRegions(ctx)
		if err != nil {
			t.Fatalf("ListRegions failed: %v", err)
		}
		log.Printf("=== ListRegions ===")
		log.Printf("Found %d region(s)", len(regions))
		for i, r := range regions {
			log.Printf("  [%d] %s", i+1, formatRegion(r))
		}
	})

	// Test ListInstanceTypes
	t.Run("ListInstanceTypes", func(t *testing.T) {
		instanceTypes, err := cat.ListInstanceTypes(ctx, nil)
		if err != nil {
			t.Fatalf("ListInstanceTypes failed: %v", err)
		}
		log.Printf("\n=== ListInstanceTypes ===")
		log.Printf("Found %d instance type(s)", len(instanceTypes))
		for i, it := range instanceTypes {
			log.Printf("  [%d] %s", i+1, it.InstanceType)
		}
	})

	// Test ListResources
	t.Run("ListResources", func(t *testing.T) {
		resources, err := cat.ListResources(ctx, &catalog.ResourceListOptions{})
		if err != nil {
			t.Fatalf("ListResources failed: %v", err)
		}
		log.Printf("\n=== ListResources ===")
		log.Printf("Found %d resource(s)", len(resources))
		for i, res := range resources {
			log.Printf("\n--- Resource %d ---", i+1)
			log.Printf("Provider: %s", res.Provider)
			if res.Region != nil {
				log.Printf("Region: %s", formatRegion(*res.Region))
			}
			log.Printf("Overview items: %d", len(res.Overview))
			for j, item := range res.Overview {
				log.Printf("\n  Overview[%d]: key=%s, value=%s", j, item.Key, item.Value)
				if item.Stat.OnDemand != nil {
					log.Printf("    OnDemand:")
					printResourceItem("    ", "Allocated", item.Stat.OnDemand.Allocated)
					printResourceItem("    ", "Supply", item.Stat.OnDemand.Supply)
					printResourceItem("    ", "Allocatable", item.Stat.OnDemand.Allocatable)
				}
			}
		}
	})

	// Test ListPricing
	t.Run("ListPricing", func(t *testing.T) {
		pricing, err := cat.ListPricing(ctx, &catalog.ResourceListOptions{})
		if err != nil {
			t.Fatalf("ListPricing failed: %v", err)
		}
		log.Printf("\n=== ListPricing ===")
		log.Printf("Found %d pricing entry(ies)", len(pricing))
		for i, p := range pricing {
			log.Printf("\n--- Pricing %d ---", i+1)
			log.Printf("Region: %s", formatRegion(p.Region))
			for name, item := range p.Items {
				log.Printf("  %s: onDemand=%s, spot=%s", name,
					formatFloat(item.OnDemandPrice), formatFloat(item.SpotPrice))
			}
		}
	})
}

func formatRegion(r types.RegionSpec) string {
	if r.Kubernetes != nil {
		return r.Kubernetes.String()
	}
	data, _ := json.Marshal(r)
	return string(data)
}

func printResourceItem(prefix, name string, item catalog.ResourceItem) {
	if len(item) == 0 {
		return
	}
	log.Printf("%s%s:", prefix, name)
	for resourceType, names := range item {
		log.Printf("%s  %s:", prefix, resourceType)
		for resourceName, quantity := range names {
			log.Printf("%s    %s: %s", prefix, resourceName, quantity)
		}
	}
}

func formatFloat(f *float64) string {
	if f == nil {
		return "nil"
	}
	return fmt.Sprintf("%.2f", *f)
}
