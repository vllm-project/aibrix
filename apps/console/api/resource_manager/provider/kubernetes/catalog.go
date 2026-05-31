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
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

const (
	K8sInstanceTypeLabel     = "node.kubernetes.io/instance-type"
	K8sBetaInstanceTypeLabel = "beta.kubernetes.io/instance-type"
	K8sDefaultInstanceType   = "kubernetes-node"
	K8sResourceGPU           = "gpu"
)

// k8sCatalog implements catalog.Catalog for Kubernetes.
type k8sCatalog struct {
	mu        sync.Mutex
	clientset *kubernetesClientset
}

// newCatalog creates a new Kubernetes catalog.
func newCatalog() (catalog.Catalog, error) {
	return &k8sCatalog{}, nil
}

// Provider returns the provider type.
func (c *k8sCatalog) Provider() types.ResourceProvisionType {
	return types.ResourceProvisionTypeKubernetes
}

func (c *k8sCatalog) defaultClientset() (*kubernetesClientset, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.clientset != nil {
		return c.clientset, nil
	}

	resourceClientset, err := newK8sClientset(&types.KubernetesCredential{})
	if err != nil {
		return nil, err
	}
	c.clientset = resourceClientset
	return c.clientset, nil
}

// ListRegions lists available regions for the catalog.
func (c *k8sCatalog) ListRegions(ctx context.Context) ([]types.RegionSpec, error) {
	k8sClientset, err := c.defaultClientset()
	if err != nil {
		return nil, err
	}
	return k8sClientset.listRegions(), nil
}

// ListInstanceTypes lists available instance types for the catalog.
func (c *k8sCatalog) ListInstanceTypes(ctx context.Context, region *types.RegionSpec) ([]types.InstanceTypeSpec, error) {
	k8sClientset, err := c.defaultClientset()
	if err != nil {
		return nil, err
	}

	regionClients := k8sClientset.resolve(region)
	if len(regionClients) == 0 {
		return []types.InstanceTypeSpec{}, nil
	}

	instanceTypeMap := make(map[string]bool)
	for _, regionClient := range regionClients {
		nodes, err := regionClient.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}

		for _, node := range nodes.Items {
			if !isNodeReady(node) {
				continue
			}

			if instanceType, ok := node.Labels[K8sInstanceTypeLabel]; ok && instanceType != "" {
				instanceTypeMap[instanceType] = true
			} else if instanceType, ok := node.Labels[K8sBetaInstanceTypeLabel]; ok && instanceType != "" {
				instanceTypeMap[instanceType] = true
			} else {
				instanceTypeMap[K8sDefaultInstanceType] = true
			}
		}
	}

	var instanceTypes []types.InstanceTypeSpec
	for it := range instanceTypeMap {
		instanceTypes = append(instanceTypes, types.InstanceTypeSpec{InstanceType: it})
	}

	return instanceTypes, nil
}

// ListResources lists available resources matching the options.
func (c *k8sCatalog) ListResources(ctx context.Context, opts *catalog.ResourceListOptions) ([]catalog.Resource, error) {
	k8sClientset, err := c.defaultClientset()
	if err != nil {
		return nil, err
	}

	var regionFilter *types.RegionSpec
	if opts != nil && opts.Region.Kubernetes != nil {
		regionFilter = &opts.Region
	}

	regionClients := k8sClientset.resolve(regionFilter)
	if len(regionClients) == 0 {
		return []catalog.Resource{}, nil
	}

	resources := make([]catalog.Resource, 0, len(regionClients))
	for _, regionClient := range regionClients {
		nodes, err := regionClient.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		resources = append(resources, c.computeNodeResources(nodes, regionClient.Region))
	}

	return resources, nil
}

// ListResourcePredictions lists resource predictions for the options.
func (c *k8sCatalog) ListResourcePredictions(ctx context.Context, opts *catalog.ResourceListOptions) (map[string]catalog.Resource, error) {
	return nil, types.ErrNotImplemented
}

// computeNodeResources aggregates all node resources into a catalog.Resource.
// catalog.ResourceItem format is resource type -> resource name -> quantity.
// For CPU/Memory, resource name equals type (cpu->cpu in millicores, memory->memory in bytes).
// For hugepages, resource type is hugepage with resource name like hugepages-1Gi.
// For GPU, resource type is gpu with resource name from vendor key or product label (e.g. NVIDIA H20).
func (c *k8sCatalog) computeNodeResources(nodes *corev1.NodeList, region types.RegionSpec) catalog.Resource {
	overview := make([]catalog.RegionResourceItem, 0, len(nodes.Items))

	for _, node := range nodes.Items {
		if !isNodeReady(node) {
			continue
		}

		nodeName := node.Name
		nodeAllocatable := make(catalog.ResourceItem)
		nodeSupply := make(catalog.ResourceItem)
		nodeAllocated := make(catalog.ResourceItem)

		for resourceName, capacity := range node.Status.Capacity {
			resourceType, normalizedName := normalizeResource(resourceName, node.Labels)
			if resourceType == "" || normalizedName == "" {
				continue
			}

			capacityQty := quantityValue(resourceName, capacity)
			allocatableQty := int64(0)
			if allocatable, ok := node.Status.Allocatable[resourceName]; ok {
				allocatableQty = quantityValue(resourceName, allocatable)
			}

			setResourceQuantity(nodeSupply, resourceType, normalizedName, capacityQty)
			setResourceQuantity(nodeAllocatable, resourceType, normalizedName, allocatableQty)
			setResourceQuantity(nodeAllocated, resourceType, normalizedName, capacityQty-allocatableQty)
		}

		overview = append(overview, catalog.RegionResourceItem{
			Key:   "node",
			Value: nodeName,
			Stat: catalog.ResourceStat{
				OnDemand: &catalog.ResourceStatItem{
					Allocated:   nodeAllocated,
					Supply:      nodeSupply,
					Allocatable: nodeAllocatable,
				},
			},
		})
	}

	return catalog.Resource{
		Provider: types.ResourceProvisionTypeKubernetes,
		RegionResource: catalog.RegionResource{
			Region:   &region,
			Overview: overview,
		},
	}
}

func setResourceQuantity(item catalog.ResourceItem, resourceType, resourceName string, quantity int64) {
	if _, ok := item[resourceType]; !ok {
		item[resourceType] = make(map[string]string)
	}
	if existing, ok := item[resourceType][resourceName]; ok {
		existingValue, err := strconv.ParseInt(existing, 10, 64)
		if err == nil {
			quantity += existingValue
		}
	}
	item[resourceType][resourceName] = fmt.Sprintf("%d", quantity)
}

func quantityValue(resourceName corev1.ResourceName, quantity resource.Quantity) int64 {
	if resourceName == corev1.ResourceCPU {
		return quantity.MilliValue()
	}
	return quantity.Value()
}

func normalizeResource(resourceName corev1.ResourceName, labels map[string]string) (string, string) {
	name := resourceName.String()
	switch {
	case name == corev1.ResourceCPU.String():
		return corev1.ResourceCPU.String(), corev1.ResourceCPU.String()
	case name == corev1.ResourceMemory.String():
		return corev1.ResourceMemory.String(), corev1.ResourceMemory.String()
	case strings.HasPrefix(name, "hugepages-"):
		return "hugepage", name
	case strings.HasPrefix(name, "nvidia.com/") || strings.HasPrefix(name, "amd.com/"):
		if strings.HasPrefix(name, "nvidia.com/") {
			if product := labels["nvidia.com/gpu.product"]; product != "" {
				return K8sResourceGPU, product
			}
		}
		if strings.HasPrefix(name, "amd.com/") {
			if product := labels["amd.com/gpu.product"]; product != "" {
				return K8sResourceGPU, product
			}
		}
		parts := strings.SplitN(name, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return K8sResourceGPU, parts[1]
		}
		return K8sResourceGPU, name
	case strings.Contains(name, "/"):
		parts := strings.SplitN(name, "/", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}
	return name, name
}

// isNodeReady checks if a node is in Ready state.
func isNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ============================================================================
// PricingCatalog Implementation
// ============================================================================

// Fixed pricing for Kubernetes resources (USD per hour).
// These are placeholder values since Kubernetes doesn't have native pricing.
const (
	fixedCPUPricePerCore  = 0.00
	fixedMemoryPricePerGB = 0.00
	fixedNodePricePerHour = 0.00
)

// ListPricing returns pricing information for instance types.
func (c *k8sCatalog) ListPricing(ctx context.Context, opts *catalog.ResourceListOptions) ([]catalog.ResourcePricing, error) {
	region := types.RegionSpec{}
	if opts != nil {
		region = opts.Region
	}

	// Calculate prices
	cpuPrice := fixedCPUPricePerCore
	memoryPrice := fixedMemoryPricePerGB
	nodePrice := fixedNodePricePerHour

	pricing := catalog.ResourcePricing{
		Region: region,
		Items: map[string]catalog.ResourcePricingItem{
			"cpu": {
				OnDemandPrice: &cpuPrice,
			},
			"memory": {
				OnDemandPrice: &memoryPrice,
			},
			"node": {
				OnDemandPrice: &nodePrice,
			},
		},
	}

	return []catalog.ResourcePricing{pricing}, nil
}

// ListPricingPredictions lists pricing predictions for the options.
func (c *k8sCatalog) ListPricingPredictions(ctx context.Context, opts *catalog.ResourceListOptions) (map[string]catalog.ResourcePricing, error) {
	return nil, types.ErrNotImplemented
}
