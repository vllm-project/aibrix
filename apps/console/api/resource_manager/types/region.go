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

// RegionSpec represents a region identifier with provider-specific fields.
// Different providers use different fields:
//   - AWS: Region (e.g., "us-east-1"), optionally Zone (e.g., "us-east-1a")
//   - Lambda Cloud: Region (e.g., "us-west-2")
//
// Example - AWS:
//
//	regionSpec:
//	  aws:
//	    region: "us-east-1"
//	    zone: "us-east-1a"
//
// Example - Lambda Cloud:
//
//	regionSpec:
//	  lambdaCloud:
//	    region: "us-west-2"
type RegionSpec struct {
	// AWS contains AWS-specific region information.
	AWS *AWSRegion `json:"aws,omitempty"`

	// LambdaCloud contains Lambda Cloud-specific region information.
	LambdaCloud *LambdaCloudRegion `json:"lambdaCloud,omitempty"`

	// Kubernetes contains Kubernetes-specific region information.
	Kubernetes *KubernetesRegion `json:"kubernetes,omitempty"`
}

// AWSRegion contains AWS-specific region information.
type AWSRegion struct {
	// Region is the AWS region (e.g., "us-east-1", "us-west-2").
	Region string `json:"region"`

	// Zone is the availability zone (e.g., "us-east-1a").
	Zone string `json:"zone,omitempty"`
}

// LambdaCloudRegion contains Lambda Cloud-specific region information.
type LambdaCloudRegion struct {
	// Region is the Lambda Cloud region (e.g., "us-west-2").
	Region string `json:"region"`
}

// KubernetesRegion contains Kubernetes-specific region information.
type KubernetesRegion struct {
	// Context is the kubeconfig context name.
	Context string `json:"context,omitempty"`

	// Cluster is the cluster name.
	Cluster string `json:"cluster,omitempty"`

	// Namespace is the Kubernetes namespace.
	Namespace string `json:"namespace,omitempty"`
}

// RegionAffinity contains constraint sets expressed as required/preferred/forbidden arrays
// for region fields.
// - Required: resources that must be scheduled (hard constraint, all must be satisfied)
// - Preferred: resources preferred for scheduling (soft constraint, ordered by priority)
// - Forbidden: resources that must not be scheduled
type RegionAffinity struct {
	// Forbidden is the list of resources that must not be scheduled.
	Forbidden *[]string `json:"forbidden,omitempty"`

	// Preferred is the list of resources preferred for scheduling (soft constraint, ordered by priority).
	Preferred *[]string `json:"preferred,omitempty"`

	// Required is the list of resources that must be scheduled (hard constraint, all must be satisfied).
	Required *[]string `json:"required,omitempty"`
}

type AWSRegionAffinity struct {
	Region *RegionAffinity `json:"region,omitempty"`
	Zone   *RegionAffinity `json:"zone,omitempty"`
}

type LambdaCloudRegionAffinity struct {
	Region *RegionAffinity `json:"region,omitempty"`
}

type KubernetesRegionAffinity struct {
	Context   *RegionAffinity `json:"context,omitempty"`
	Cluster   *RegionAffinity `json:"cluster,omitempty"`
	Namespace *RegionAffinity `json:"namespace,omitempty"`
}

func (r *RegionSpec) GetRegion() interface{} {
	if r == nil {
		return nil
	}
	if r.AWS != nil {
		return r.AWS
	}
	if r.LambdaCloud != nil {
		return r.LambdaCloud
	}
	if r.Kubernetes != nil {
		return r.Kubernetes
	}
	return nil
}

// NewAWSRegion creates a RegionSpec for AWS.
func NewAWSRegion(region string, zone ...string) *RegionSpec {
	spec := &RegionSpec{
		AWS: &AWSRegion{Region: region},
	}
	if len(zone) > 0 && zone[0] != "" {
		spec.AWS.Zone = zone[0]
	}
	return spec
}

// NewLambdaCloudRegion creates a RegionSpec for Lambda Cloud.
func NewLambdaCloudRegion(region string) *RegionSpec {
	return &RegionSpec{
		LambdaCloud: &LambdaCloudRegion{Region: region},
	}
}

// NewKubernetesRegion creates a RegionSpec for Kubernetes.
func NewKubernetesRegion(context string) *RegionSpec {
	return &RegionSpec{
		Kubernetes: &KubernetesRegion{Context: context},
	}
}
