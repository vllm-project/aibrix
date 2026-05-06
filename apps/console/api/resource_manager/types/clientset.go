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

package types

import "k8s.io/client-go/kubernetes"

type ResourceClientset struct {
	Provider   ResourceProvisionType `json:"provider"`
	Credential *ResourceCredential   `json:"credential,omitempty"`

	AWS         *AWSClientset         `json:"aws,omitempty"`
	LambdaCloud *LambdaCloudClientset `json:"lambdaCloud,omitempty"`
	Kubernetes  *KubernetesClientset  `json:"kubernetes,omitempty"`
}

type AWSClientset struct{}

type LambdaCloudClientset struct{}

type KubernetesClientset struct {
	RegionClients []KubernetesRegionClient `json:"-"`
}

type KubernetesRegionClient struct {
	Region    RegionSpec           `json:"region"`
	Clientset kubernetes.Interface `json:"-"`
}

func (k *KubernetesClientset) ListRegions() []RegionSpec {
	if k == nil || len(k.RegionClients) == 0 {
		return nil
	}

	regions := make([]RegionSpec, 0, len(k.RegionClients))
	for _, regionClient := range k.RegionClients {
		regions = append(regions, regionClient.Region)
	}
	return regions
}

func (k *KubernetesClientset) Resolve(filter *RegionSpec) []KubernetesRegionClient {
	if k == nil || len(k.RegionClients) == 0 {
		return nil
	}
	if filter == nil || filter.Kubernetes == nil {
		return k.RegionClients
	}

	matched := make([]KubernetesRegionClient, 0, len(k.RegionClients))
	for _, regionClient := range k.RegionClients {
		if !matchesKubernetesRegion(regionClient.Region.Kubernetes, filter.Kubernetes) {
			continue
		}
		matched = append(matched, regionClient)
	}
	return matched
}

func (k *KubernetesClientset) Primary() *KubernetesRegionClient {
	if k == nil || len(k.RegionClients) == 0 {
		return nil
	}
	return &k.RegionClients[0]
}

func matchesKubernetesRegion(candidate, filter *KubernetesRegion) bool {
	if filter == nil {
		return true
	}
	if candidate == nil {
		return false
	}
	if filter.Context != "" && candidate.Context != filter.Context {
		return false
	}
	if filter.Cluster != "" && candidate.Cluster != filter.Cluster {
		return false
	}
	if filter.Namespace != "" && candidate.Namespace != filter.Namespace {
		return false
	}
	return true
}
