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

import (
	"encoding/json"
	"fmt"
	"time"
)

type ResourceProvision struct {
	Spec ResourceProvisionSpec `json:"spec"`

	// IdempotencyKey is a unique identifier to ensure idempotency of the resource provision
	IdempotencyKey string `json:"idempotencyKey"`
}

type ResourceProvisionSpec struct {
	Credential ResourceCredential `json:"credential"`

	// Groups is a list of resource group requirements. Each group describes the per-replica
	// resource shape (e.g., GPUs per replica, CPU cores) plus the number of replicas.
	// Provisioner-specific options (NUMA, affinity, placement) go under kubernetes/aws/lambda.
	//
	// Typical use cases:
	// - Distributed inference with multiple identical inference replicas.
	// - Batch inference with N identical replicas.
	// - Heterogeneous multi-group placement (e.g., trainers + parameter servers).
	//
	// Example (Kubernetes with topology):
	//   groups:
	//     - replicas: 4
	//       gpusPerReplica: 8
	//       kubernetes:
	//         replicaAffinity:
	//           policies: [host]
	//     - gpusPerReplica: 2
	//       kubernetes:
	//         replicaAffinity:
	//           policies: [numa]
	//         numaConfig:
	//           numaRequired: true
	//
	// Example (AWS):
	//   groups:
	//     - replicas: 1
	//       gpusPerReplica: 8
	//       acceleratorPreference:
	//         preferredTypes: ["p5.48xlarge"]
	Groups *[]ResourceGroupSpec `json:"groups,omitempty"`

	// TimeWindow defines the time window for resource provisioning.
	// Mainly used for scheduled resource provisioning.
	TimeWindow *TimeWindow `json:"timeWindow,omitempty"`
}

// ResourceGroupSpec describes the per-replica resource shape plus the number of replicas.
// It contains common fields applicable to all provisioners, plus provisioner-specific options.
//
// Structure:
//   - Common fields: All provisioners support these (AWS, LambdaCloud, K8s)
//   - Provisioner-specific options: Use oneOf based on provisioner type
//
// Example:
//
//	groups:
//	  - gpusPerReplica: 8
//	    replicas: 4
//	    kubernetes:
//	      numaConfig:
//	        numaRequired: true
//	      replicaAffinity:
//	        policies: [host]
type ResourceGroupSpec struct {
	// ===== Common fields (all provisioners) =====

	// GpusPerReplica is the number of accelerators required per replica.
	GpusPerReplica int `json:"gpusPerReplica"`

	// Replicas is the number of replicas for this resource group.
	// For P/D disaggregation, you can define a prefill group (replicas=3) and a decode group (replicas=1).
	// Default: 1
	Replicas *int `json:"replicas,omitempty"`

	// CpuCoresPerReplica is the number of CPU cores required per replica.
	CpuCoresPerReplica *int `json:"cpuCoresPerReplica,omitempty"`

	// AcceleratorPreference is a soft constraint describing preferred accelerator types, features,
	// capabilities, or weights for provisioning.
	AcceleratorPreference *AcceleratorPreference `json:"acceleratorPreference,omitempty"`

	// Network defines network bandwidth and RDMA constraints.
	Network *GroupSpecNetwork `json:"network,omitempty"`

	// Storage defines storage system accessibility requirements (e.g., Ceph, HDFS).
	Storage *[]string `json:"storage,omitempty"`

	// GroupRole indicates the logical role of this group (e.g., "prefill", "decode").
	GroupRole *string `json:"groupRole,omitempty"`

	// ===== Provisioner-specific options =====
	// Only one of these should be set based on the provisioner type.
	// The provisioner will use its corresponding options; others are ignored.

	// Kubernetes contains Kubernetes-specific options (NUMA, affinity, placement).
	Kubernetes *KubernetesGroupOptions `json:"kubernetes,omitempty"`

	// AWS contains AWS-specific options.
	AWS *AWSGroupOptions `json:"aws,omitempty"`

	// LambdaCloud contains LambdaCloud Cloud-specific options.
	LambdaCloud *LambdaCloudGroupOptions `json:"lambdaCloud,omitempty"`
}

type GroupSpecNetwork struct {
	// MaxHops is the maximum network hops (soft constraints).
	MaxHops *int `json:"maxHops,omitempty"`

	// MinBandwidthGbps is the minimum network bandwidth requirement (Gbps). Leave empty if not required.
	MinBandwidthGbps *float32 `json:"minBandwidthGbps,omitempty"`

	// Rdma is the RDMA network requirement type.
	// - none: No RDMA required
	// - any: RDMA required, any implementation
	// - infiniband: Must be InfiniBand
	// - roce: Must be RoCE
	// - iwarp: Must be iWARP
	// - efa: Must be EFA
	Rdma *GroupSpecNetworkRdma `json:"rdma,omitempty"`
}

// AcceleratorPreference is a soft constraint describing preferred accelerator types, features,
// capabilities, or weights for provisioning. The provisioner will try to satisfy preferences
// but may downgrade if not possible. Suitable for expressing preferred types, bandwidth,
// features, sorting weights, etc. Only describes per-accelerator features.
//
// Example fields:
//
//	preferredTypes: ["NVIDIA A100", "NVIDIA H100"]
//	preferHighBandwidth: true
//	minMemoryGB: 40
//	weight: 10
type AcceleratorPreference struct {
	// Advanced contains advanced parameters; only advanced users should fill this.
	Advanced *AcceleratorPreferenceAdvanced `json:"advanced,omitempty"`

	// MinBandwidthGBps prefers accelerators with memory bandwidth >= this value.
	MinBandwidthGBps *float32 `json:"minBandwidthGBps,omitempty"`

	// MinMemoryGB prefers accelerators with memory >= this value.
	MinMemoryGB *float32 `json:"minMemoryGB,omitempty"`

	// PrecisionSupport defines precision/data type support requirements.
	// - Required: precision types that must all be supported (hard requirement).
	// - Preferred: precision types that are nice to have (more support = higher priority).
	// Used to distinguish model inference/training requirements for INT8, BF16, FP16, etc.
	PrecisionSupport *AcceleratorPreferencePrecisionSupport `json:"precisionSupport,omitempty"`

	// PreferHighBandwidth indicates whether to prefer high-bandwidth memory.
	PreferHighBandwidth *bool `json:"preferHighBandwidth,omitempty"`

	// PreferredTypes is a list of preferred accelerator types (e.g., ["NVIDIA A100", "NVIDIA H100"]), ordered by priority.
	PreferredTypes *[]string `json:"preferredTypes,omitempty"`

	// Weight is the preference weight; higher values indicate higher priority.
	Weight *int `json:"weight,omitempty"`
}

type AcceleratorPreferenceAdvanced struct {
	// PcieGen is the PCIe generation requirement (e.g., Gen4, Gen5).
	PcieGen *string `json:"pcieGen,omitempty"`

	// PcieLanes is the number of PCIe lanes required.
	PcieLanes *int `json:"pcieLanes,omitempty"`

	// VendorSpecificFeatures contains vendor-specific advanced parameters.
	VendorSpecificFeatures *map[string]interface{} `json:"vendorSpecificFeatures,omitempty"`
}

type AcceleratorPreferencePrecisionSupport struct {
	// Preferred is the list of precision types that are nice to have (e.g., FP16; more support = higher priority).
	Preferred *[]AcceleratorPreferencePrecisionType `json:"preferred,omitempty"`

	// Required is the list of precision types that must all be supported (e.g., INT8, BF16).
	Required *[]AcceleratorPreferencePrecisionType `json:"required,omitempty"`
}

// TimeWindow defines scheduling time window.
// Supports one-time windows (startTime/endTime), long-running services (endTime optional),
// All times are interpreted according to timezone, defaulting to UTC if not specified.
//
// Example usage:
// ```yaml
// timeWindow:
//
//	startTime: "2025-04-23T02:00:00Z"
//	endTime:   "2025-04-23T04:00:00Z"
//
// # Long-running online service
// timeWindow:
//
//	startTime: "2025-04-23T00:00:00Z"
//
// ```
type TimeWindow struct {
	// StartTime is the start time when the task/service can be scheduled (ISO 8601 format).
	// For periodic tasks, this is the first scheduling start point.
	StartTime time.Time `json:"startTime"`

	// EndTime is the end time when the task/service can be scheduled (ISO 8601 format).
	// For long-running services, this can be omitted, meaning "until actively released".
	EndTime *time.Time `json:"endTime,omitempty"`

	// Timezone optionally overrides workload.timezone for this timeWindow (IANA/Olson format).
	Timezone *string `json:"timezone,omitempty"`

	// MaxDuration is the maximum continuous duration (hours), indicating the longest continuous period needed.
	// Used to limit the maximum resource allocation duration.
	// If not specified, no upper limit is set; the entire window may be allocated.
	MaxDuration *int `json:"maxDuration,omitempty"`

	// MinDuration is the minimum continuous duration (hours), indicating the shortest continuous period needed.
	// If not specified, defaults to the entire window length.
	MinDuration *int `json:"minDuration,omitempty"`
}

// ============================================================================
// Provisioner-Specific Group Options
// ============================================================================

// KubernetesGroupOptions contains Kubernetes-specific topology options.
type KubernetesGroupOptions struct {
	// NumaConfig contains NUMA-related configuration.
	NumaConfig *NUMAConfig `json:"numaConfig,omitempty"`

	// ReplicaAffinity constrains how resources within this group should be co-located.
	// Policies: numa (strongest) > host > tor > minipod > bigpod (weakest).
	ReplicaAffinity *AffinityPolicies `json:"replicaAffinity,omitempty"`

	// GroupAffinity constrains how this group should be co-located relative to other groups.
	GroupAffinity *AffinityPolicies `json:"groupAffinity,omitempty"`

	RegionAffinity *KubernetesRegionAffinity `json:"regionAffinity,omitempty"`

	// TopologyConstraint is optional. It can be used to apply specific topology constraints.
	TopologyConstraint *[]Selector `json:"topologyConstraint,omitempty"`
}

// AWSGroupOptions contains AWS-specific options.
type AWSGroupOptions struct {
	RegionAffinity *AWSRegionAffinity `json:"regionAffinity,omitempty"`
}

// LambdaCloudGroupOptions contains Lambda Cloud-specific options.
type LambdaCloudGroupOptions struct {
	RegionAffinity *LambdaCloudRegionAffinity `json:"regionAffinity,omitempty"`
}

// NUMAConfig contains NUMA-related configuration for this group. Used for fine-grained constraints
// such as node count, local memory, CPU pinning, etc.
type NUMAConfig struct {
	// CpuPinning indicates whether CPU pinning is required.
	// Note: Only applicable to Kubernetes provisioner.
	CpuPinning *bool `json:"cpuPinning,omitempty"`

	// NumaAware indicates whether NUMA topology awareness is required.
	NumaAware *bool `json:"numaAware,omitempty"`

	// NumaLocalMemoryGB is the local memory requirement per NUMA node (GB).
	NumaLocalMemoryGB *float32 `json:"numaLocalMemoryGB,omitempty"`

	// NumaNodeCount is the number of NUMA nodes required.
	NumaNodeCount *int `json:"numaNodeCount,omitempty"`

	// NumaOptimizedInterconnect indicates whether NUMA-optimized interconnect is required.
	NumaOptimizedInterconnect *bool `json:"numaOptimizedInterconnect,omitempty"`

	// NumaRequired indicates whether NUMA architecture support is mandatory.
	NumaRequired *bool `json:"numaRequired,omitempty"`
}

// AffinityPolicies is an affinity policy object supporting ordered fallback.
// The policies list is ordered; the scheduler tries policies in order, preferring earlier ones.
// Unlisted affinity levels are not accepted. Example:
//
//	policies: [host, tor]
//
// Policies can be empty or contain only the lowest level (e.g., bigpod) to indicate "no higher affinity required".
//
// Note: Only applicable to Kubernetes provisioner; other provisioners may ignore this field.
type AffinityPolicies struct {
	// Policies is an ordered list of affinity policies. **Callers MUST order from strongest to weakest.**
	// - numa: Same NUMA node (strongest)
	// - host: All resources on the same physical machine
	// - tor: Same ToR/rack switch
	// - minipod: Same minipod switch
	// - bigpod: Same bigpod switch (weakest)
	// The scheduler tries policies in order, preferring earlier ones. Unlisted levels are not accepted.
	//
	// **Ordering requirement: strongest to weakest.**
	Policies []AffinityPolicy `json:"policies"`
}

// ProvisionResult contains the result of a provisioning operation.
// It follows the Resource pattern with cluster/dc identification and resource details.
//
// Structure:
//   - Common fields: ProvisionID, Status, timestamps, error info
//   - Provisioner-specific details: Use oneOf based on provider type
//
// Example - AWS provisioner result:
//
//	provisionResult:
//	  provisionId: "i-1234567890abcdef0"
//	  status: "running"
//	  aws:
//	    instances:
//	      - instanceId: "i-1234567890abcdef0"
//	        instanceType: "p5.48xlarge"
//	        state: "running"
//	        region: "us-east-1"
type ProvisionResult struct {
	// ProvisionID is the unique identifier for this provision.
	// Set by the provisioner.
	ProvisionID string `json:"provisionId"`

	// IdempotencyKey is the idempotency key from the original request.
	// Used to prevent duplicate provisions and enable request deduplication.
	// Set by the planner.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`

	// Status is the current provision status.
	Status ProvisionStatus `json:"status"`

	// Region is the JSON string representation of the region (xxxRegion).
	// Used for filtering provisions by region.
	Region string `json:"region,omitempty"`

	// ErrorMessage contains error details if provisioning failed.
	ErrorMessage string `json:"errorMessage,omitempty"`

	// CreatedAt is when the provision was created.
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is when the provision was last updated.
	UpdatedAt time.Time `json:"updatedAt"`

	// ===== Provisioner-specific details =====
	// Only one of these should be set based on the provider type.

	// Kubernetes contains Kubernetes-specific provision details.
	// Set when provider is "kubernetes".
	Kubernetes *KubernetesProvisionDetail `json:"kubernetes,omitempty"`

	// AWS contains AWS-specific provision details.
	// Set when provider is "aws".
	AWS *AWSProvisionDetail `json:"aws,omitempty"`

	// LambdaCloud contains Lambda Cloud-specific provision details.
	// Set when provider is "lambdaCloud".
	LambdaCloud *LambdaCloudProvisionDetail `json:"lambdaCloud,omitempty"`
}

func (pr *ProvisionResult) ToProvisionRecord() (*ProvisionRecord, error) {
	payload, err := json.Marshal(pr)
	if err != nil {
		return nil, fmt.Errorf("marshal provision result: %w", err)
	}
	return &ProvisionRecord{
		ProvisionID: pr.ProvisionID,
		Status:      string(pr.Status),
		Region:      pr.Region,
		Payload:     payload,
		CreatedAt:   pr.CreatedAt,
		UpdatedAt:   pr.UpdatedAt,
		Deleted:     false,
	}, nil
}

// ============================================================================
// Provisioner-Specific Result Details
// ============================================================================

// KubernetesProvisionDetail contains Kubernetes-specific provision result details.
type KubernetesProvisionDetail struct {
	// Kubernetes does not allocate any resources. Keep this field empty.
}

// AWSProvisionDetail contains AWS-specific provision result details.
type AWSProvisionDetail struct {
	// GroupResults contains allocation details for each group.
	GroupResults []AWSGroupResult `json:"groupResults,omitempty"`

	// Region is the AWS region.
	Region AWSRegion `json:"region"`
}

// AWSGroupResult contains allocation details for a single AWS group.
type AWSGroupResult struct {
	// GroupRole is the role of this group (if specified in request).
	GroupRole *string `json:"groupRole,omitempty"`

	// Instances contains details of provisioned EC2 instances for this group.
	Instances []AWSInstanceDetail `json:"instances,omitempty"`

	// Replicas is the number of replicas provisioned.
	Replicas int `json:"replicas"`

	// AcceleratorType is the GPU type allocated (e.g., "A100", "H100").
	AcceleratorType *string `json:"acceleratorType,omitempty"`

	// AcceleratorCount is the number of GPUs per instance.
	AcceleratorCount *int `json:"acceleratorCount,omitempty"`
}

// AWSInstanceDetail contains details about an EC2 instance.
type AWSInstanceDetail struct {
	// InstanceId is the EC2 instance ID.
	InstanceId string `json:"instanceId"`

	// InstanceType is the EC2 instance type.
	InstanceType string `json:"instanceType"`

	// State is the instance state.
	State string `json:"state"`

	// PrivateIp is the private IP address.
	PrivateIp *string `json:"privateIp,omitempty"`

	// PublicIp is the public IP address.
	PublicIp *string `json:"publicIp,omitempty"`

	// AvailabilityZone is the availability zone.
	AvailabilityZone *string `json:"availabilityZone,omitempty"`

	// ImageId is the AMI ID.
	ImageId *string `json:"imageId,omitempty"`

	// LaunchTime is the launch time.
	LaunchTime *time.Time `json:"launchTime,omitempty"`

	// Tags are the instance tags.
	Tags map[string]string `json:"tags,omitempty"`
}

// LambdaCloudProvisionDetail contains Lambda Cloud-specific provision result details.
type LambdaCloudProvisionDetail struct {
	// GroupResults contains allocation details for each group.
	GroupResults []LambdaCloudGroupResult `json:"groupResults,omitempty"`

	// Region is the Lambda Cloud region.
	Region LambdaCloudRegion `json:"region"`
}

// LambdaCloudGroupResult contains allocation details for a single Lambda Cloud group.
type LambdaCloudGroupResult struct {
	// GroupRole is the role of this group (if specified in request).
	GroupRole *string `json:"groupRole,omitempty"`

	// Instances contains details of provisioned Lambda Cloud instances for this group.
	Instances []LambdaCloudInstanceDetail `json:"instances,omitempty"`

	// Replicas is the number of replicas provisioned.
	Replicas int `json:"replicas"`

	// AcceleratorType is the GPU type allocated (e.g., "A100", "H100").
	AcceleratorType *string `json:"acceleratorType,omitempty"`

	// AcceleratorCount is the number of GPUs per instance.
	AcceleratorCount *int `json:"acceleratorCount,omitempty"`
}

// LambdaCloudInstanceDetail contains details about a Lambda Cloud instance.
type LambdaCloudInstanceDetail struct {
	// InstanceId is the Lambda Cloud instance ID.
	InstanceId string `json:"instanceId"`

	// InstanceType is the instance type name.
	InstanceType string `json:"instanceType"`

	// Status is the instance status.
	Status string `json:"status"`

	// PrivateIp is the private IP address.
	PrivateIp *string `json:"privateIp,omitempty"`

	// PublicIp is the public IP address.
	PublicIp *string `json:"publicIp,omitempty"`
}

type InstanceTypeSpec struct {
	InstanceType string `json:"instanceType"`
	// TODO: Add more instance type fields as needed.
}

// ProvisionRecord represents the result of a provision result stored in the store.
type ProvisionRecord struct {
	ProvisionID string    `json:"provisionId"`
	Status      string    `json:"status"`
	Region      string    `json:"region,omitempty"`
	Payload     []byte    `json:"payload,omitempty"` // JSON-serialized result
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Deleted     bool      `json:"deleted,omitempty"`
}

func (pr *ProvisionRecord) MarkDeleted() {
	pr.Deleted = true
	pr.UpdatedAt = time.Now()
}

func (pr *ProvisionRecord) UpdateStatus(status ProvisionStatus) {
	pr.Status = string(status)
	pr.UpdatedAt = time.Now()
}

func (pr *ProvisionRecord) ToProvisionResult() (*ProvisionResult, error) {
	payload := pr.Payload
	if payload == nil {
		return nil, fmt.Errorf("provision payload is nil")
	}

	var result ProvisionResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("unmarshal provision payload: %w", err)
	}
	result.Status = ProvisionStatus(pr.Status)
	result.UpdatedAt = pr.UpdatedAt
	return &result, nil
}
