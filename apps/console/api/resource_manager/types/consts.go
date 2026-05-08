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

// ResourceProvisionType identifies which provisioner backend should be used.
//
// Examples:
//   - "kubernetes": provision resources in a Kubernetes cluster.
//   - "lambdaCloud": provision resources on Lambda Cloud.
//   - "aws": provision resources on AWS.
//
// Note: Not every provisioner supports every field in ResourceGroupSpec.
// Provider-specific limitations are documented on individual fields.
type ResourceProvisionType string

const (
	ResourceProvisionTypeKubernetes  ResourceProvisionType = "kubernetes"
	ResourceProvisionTypeLambdaCloud ResourceProvisionType = "lambdaCloud"
	ResourceProvisionTypeAWS         ResourceProvisionType = "aws"
)

// AcceleratorPreferencePrecisionType defines precision type.
type AcceleratorPreferencePrecisionType string

// Defines values for AcceleratorPreferencePrecisionType.
const (
	AcceleratorPreferencePrecisionTypeBF16 AcceleratorPreferencePrecisionType = "BF16"
	AcceleratorPreferencePrecisionTypeFP16 AcceleratorPreferencePrecisionType = "FP16"
	AcceleratorPreferencePrecisionTypeFP32 AcceleratorPreferencePrecisionType = "FP32"
	AcceleratorPreferencePrecisionTypeFP4  AcceleratorPreferencePrecisionType = "FP4"
	AcceleratorPreferencePrecisionTypeFP64 AcceleratorPreferencePrecisionType = "FP64"
	AcceleratorPreferencePrecisionTypeFP8  AcceleratorPreferencePrecisionType = "FP8"
	AcceleratorPreferencePrecisionTypeINT4 AcceleratorPreferencePrecisionType = "INT4"
	AcceleratorPreferencePrecisionTypeINT8 AcceleratorPreferencePrecisionType = "INT8"
	AcceleratorPreferencePrecisionTypeTF32 AcceleratorPreferencePrecisionType = "TF32"
)

// AffinityPolicy represents a single affinity policy level.
type AffinityPolicy string

const (
	// AffinityPolicyNuma is the strongest affinity - same NUMA node.
	AffinityPolicyNuma AffinityPolicy = "numa"
	// AffinityPolicyHost - all resources on the same physical machine.
	AffinityPolicyHost AffinityPolicy = "host"
	// AffinityPolicyTor - same ToR/rack switch.
	AffinityPolicyTor AffinityPolicy = "tor"
	// AffinityPolicyMinipod - same minipod switch.
	AffinityPolicyMinipod AffinityPolicy = "minipod"
	// AffinityPolicyBigpod is the weakest affinity - same bigpod switch.
	AffinityPolicyBigpod AffinityPolicy = "bigpod"
)

// GroupSpecNetworkRdma defines RDMA network type requirements.
type GroupSpecNetworkRdma string

// Defines values for GroupSpecNetworkRdma.
const (
	GroupSpecNetworkRdmaNone       GroupSpecNetworkRdma = "none"
	GroupSpecNetworkRdmaAny        GroupSpecNetworkRdma = "any"
	GroupSpecNetworkRdmaInfiniband GroupSpecNetworkRdma = "infiniband"
	GroupSpecNetworkRdmaRoce       GroupSpecNetworkRdma = "roce"
	GroupSpecNetworkRdmaIwarp      GroupSpecNetworkRdma = "iwarp"
	GroupSpecNetworkRdmaEfa        GroupSpecNetworkRdma = "efa"
)

// ProvisionStatus represents the current state of a resource provision.
type ProvisionStatus string

const (
	ProvisionStatusPending      ProvisionStatus = "pending"      // Provision request received, not yet processed
	ProvisionStatusProvisioning ProvisionStatus = "provisioning" // Resources are being allocated
	ProvisionStatusRunning      ProvisionStatus = "running"      // Resources are active and ready
	ProvisionStatusFailed       ProvisionStatus = "failed"       // Provisioning failed
	ProvisionStatusReleasing    ProvisionStatus = "releasing"    // Resources are being released
	ProvisionStatusReleased     ProvisionStatus = "released"     // Resources have been released
)
