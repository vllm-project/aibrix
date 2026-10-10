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
	"reflect"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/catalog"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

var (
	regionA = types.RegionSpec{Kubernetes: &types.KubernetesRegion{
		Context: "ctx-a", Cluster: "https://a.example.com", Namespace: "default",
	}}
	regionB = types.RegionSpec{Kubernetes: &types.KubernetesRegion{
		Context: "ctx-b", Cluster: "https://b.example.com", Namespace: "team-b",
	}}
)

// newTestCatalog returns a catalog over two fake clusters:
//   - regionA: a GPU node, a CPU node, and a node that is not ready.
//   - regionB: a node with no instance type label and an AMD GPU without a product label.
func newTestCatalog() *k8sCatalog {
	gpuNode := testNode("gpu-node", true,
		map[string]string{K8sInstanceTypeLabel: "g5.2xlarge", "nvidia.com/gpu.product": "NVIDIA-H20"},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("32Gi"),
			"nvidia.com/gpu":      resource.MustParse("2"),
			"hugepages-1Gi":       resource.MustParse("2Gi"),
		},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("7500m"),
			corev1.ResourceMemory: resource.MustParse("30Gi"),
			"nvidia.com/gpu":      resource.MustParse("2"),
			"hugepages-1Gi":       resource.MustParse("2Gi"),
		})
	cpuNode := testNode("cpu-node", true,
		map[string]string{K8sBetaInstanceTypeLabel: "m5.large"},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1900m"),
			corev1.ResourceMemory: resource.MustParse("7Gi"),
		})
	notReadyNode := testNode("not-ready-node", false,
		map[string]string{K8sInstanceTypeLabel: "p4d.24xlarge"},
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("96")},
		corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("96")})
	plainNode := testNode("plain-node", true, nil,
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			"amd.com/gpu":         resource.MustParse("1"),
		},
		corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			"amd.com/gpu":         resource.MustParse("1"),
		})

	return &k8sCatalog{clientset: &kubernetesClientset{RegionClients: []kubernetesRegionClient{
		{Region: regionA, Clientset: fake.NewSimpleClientset(gpuNode, cpuNode, notReadyNode)},
		{Region: regionB, Clientset: fake.NewSimpleClientset(plainNode)},
	}}}
}

func testNode(name string, ready bool, labels map[string]string, capacity, allocatable corev1.ResourceList) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity:    capacity,
			Allocatable: allocatable,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
		},
	}
}

func TestK8sCatalogListRegions(t *testing.T) {
	regions, err := newTestCatalog().ListRegions(context.Background())
	if err != nil {
		t.Fatalf("ListRegions failed: %v", err)
	}
	if want := []types.RegionSpec{regionA, regionB}; !reflect.DeepEqual(regions, want) {
		t.Errorf("ListRegions = %v, want %v", regions, want)
	}
}

func TestK8sCatalogListInstanceTypes(t *testing.T) {
	tests := []struct {
		name   string
		region *types.RegionSpec
		want   []string
	}{
		{
			name: "all regions",
			want: []string{"g5.2xlarge", K8sDefaultInstanceType, "m5.large"},
		},
		{
			name:   "one region",
			region: &regionA,
			want:   []string{"g5.2xlarge", "m5.large"},
		},
		{
			name:   "unknown region",
			region: &types.RegionSpec{Kubernetes: &types.KubernetesRegion{Context: "ctx-unknown"}},
			want:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instanceTypes, err := newTestCatalog().ListInstanceTypes(context.Background(), tt.region)
			if err != nil {
				t.Fatalf("ListInstanceTypes failed: %v", err)
			}
			got := make([]string, 0, len(instanceTypes))
			for _, it := range instanceTypes {
				got = append(got, it.InstanceType)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ListInstanceTypes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestK8sCatalogListResources(t *testing.T) {
	resources, err := newTestCatalog().ListResources(context.Background(), &catalog.ResourceListOptions{})
	if err != nil {
		t.Fatalf("ListResources failed: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("ListResources returned %d resources, want one per region (2)", len(resources))
	}

	wantStats := map[string]catalog.ResourceStatItem{
		"gpu-node": {
			Supply: catalog.ResourceItem{
				"cpu":      {"cpu": "8000"},
				"memory":   {"memory": "34359738368"},
				"gpu":      {"NVIDIA-H20": "2"},
				"hugepage": {"hugepages-1Gi": "2147483648"},
			},
			Allocatable: catalog.ResourceItem{
				"cpu":      {"cpu": "7500"},
				"memory":   {"memory": "32212254720"},
				"gpu":      {"NVIDIA-H20": "2"},
				"hugepage": {"hugepages-1Gi": "2147483648"},
			},
			Allocated: catalog.ResourceItem{
				"cpu":      {"cpu": "500"},
				"memory":   {"memory": "2147483648"},
				"gpu":      {"NVIDIA-H20": "0"},
				"hugepage": {"hugepages-1Gi": "0"},
			},
		},
		"cpu-node": {
			Supply:      catalog.ResourceItem{"cpu": {"cpu": "2000"}, "memory": {"memory": "8589934592"}},
			Allocatable: catalog.ResourceItem{"cpu": {"cpu": "1900"}, "memory": {"memory": "7516192768"}},
			Allocated:   catalog.ResourceItem{"cpu": {"cpu": "100"}, "memory": {"memory": "1073741824"}},
		},
		"plain-node": {
			Supply:      catalog.ResourceItem{"cpu": {"cpu": "4000"}, "memory": {"memory": "17179869184"}, "gpu": {"gpu": "1"}},
			Allocatable: catalog.ResourceItem{"cpu": {"cpu": "4000"}, "memory": {"memory": "17179869184"}, "gpu": {"gpu": "1"}},
			Allocated:   catalog.ResourceItem{"cpu": {"cpu": "0"}, "memory": {"memory": "0"}, "gpu": {"gpu": "0"}},
		},
	}
	wantNodes := map[string][]string{
		regionA.Kubernetes.Context: {"cpu-node", "gpu-node"},
		regionB.Kubernetes.Context: {"plain-node"},
	}

	for _, res := range resources {
		if res.Provider != types.ResourceProvisionTypeKubernetes {
			t.Errorf("resource provider = %q, want %q", res.Provider, types.ResourceProvisionTypeKubernetes)
		}
		if res.Region == nil || res.Region.Kubernetes == nil {
			t.Fatalf("resource has no Kubernetes region: %+v", res.Region)
		}
		regionContext := res.Region.Kubernetes.Context

		var nodes []string
		for _, item := range res.Overview {
			nodes = append(nodes, item.Value)
			if item.Key != "node" {
				t.Errorf("%s: overview key = %q, want %q", item.Value, item.Key, "node")
			}
			if item.Stat.OnDemand == nil {
				t.Errorf("%s: no on-demand stats", item.Value)
				continue
			}
			if got, want := *item.Stat.OnDemand, wantStats[item.Value]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s: on-demand stats = %+v, want %+v", item.Value, got, want)
			}
		}
		sort.Strings(nodes)
		if !reflect.DeepEqual(nodes, wantNodes[regionContext]) {
			t.Errorf("region %s: nodes = %v, want %v", regionContext, nodes, wantNodes[regionContext])
		}
	}
}

func TestK8sCatalogListResourcesFiltersByRegion(t *testing.T) {
	resources, err := newTestCatalog().ListResources(context.Background(), &catalog.ResourceListOptions{Region: regionB})
	if err != nil {
		t.Fatalf("ListResources failed: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("ListResources returned %d resources, want 1", len(resources))
	}
	if got := resources[0].Region; got == nil || !reflect.DeepEqual(*got, regionB) {
		t.Errorf("resource region = %v, want %v", got, regionB)
	}
	if overview := resources[0].Overview; len(overview) != 1 || overview[0].Value != "plain-node" {
		t.Errorf("overview = %+v, want only plain-node", overview)
	}
}

func TestK8sCatalogListPricing(t *testing.T) {
	pricing, err := newTestCatalog().ListPricing(context.Background(), &catalog.ResourceListOptions{Region: regionA})
	if err != nil {
		t.Fatalf("ListPricing failed: %v", err)
	}
	if len(pricing) != 1 {
		t.Fatalf("ListPricing returned %d entries, want 1", len(pricing))
	}
	if !reflect.DeepEqual(pricing[0].Region, regionA) {
		t.Errorf("pricing region = %v, want %v", pricing[0].Region, regionA)
	}
	for _, name := range []string{"cpu", "memory", "node"} {
		item, ok := pricing[0].Items[name]
		if !ok || item.OnDemandPrice == nil {
			t.Errorf("pricing item %q has no on-demand price", name)
		}
	}
}
