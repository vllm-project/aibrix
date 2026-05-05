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

package plannerapi

import "time"

// =============================================================================
// Console / ops -> queue telemetry
// =============================================================================

// GetQueueStatsRequest queries queue depth and worker activity intended for
// Console or ops dashboards.
type GetQueueStatsRequest struct {
	QueueName string `json:"queue_name,omitempty"`
}

// QueueStatsView is the normalized read model for queue telemetry.
//
// Counts mirror the values of PlannerTaskState that are observable as
// "in flight" from an ops perspective: queued, claimed, retryable_failure.
// Terminal states (submitted, terminal_failure, cancelled) are not
// counted here - they live in the per-job read model (JobView).
type QueueStatsView struct {
	QueueName             string     `json:"queue_name,omitempty"`
	Bounded               bool       `json:"bounded"`
	MaxQueuedTasks        int        `json:"max_queued_tasks,omitempty"`
	CurrentQueuedTasks    int        `json:"current_queued_tasks,omitempty"`
	CurrentClaimedTasks   int        `json:"current_claimed_tasks,omitempty"`
	CurrentRetryableTasks int        `json:"current_retryable_tasks,omitempty"`
	OldestQueuedAt        *time.Time `json:"oldest_queued_at,omitempty"`
	SampledAt             time.Time  `json:"sampled_at"`
}

// =============================================================================
// Console / ops -> provision-resource stats
//
// "Provision" here mirrors apps/console/api/resource_manager vocabulary
// (ResourceProvisionType, Provisioner, ProvisionResult): the resources
// being counted are the ones the RM has provisioned. Everything in this
// section is planner demand-side accounting (how many slots have planner
// tasks asked for, how many do they currently hold) — pool supply is
// intentionally not included; for that, query RM's catalog.ListResources
// directly. Going through planner for supply data would only add a hop
// without adding signal.
// =============================================================================

// GetProvisionResourceStatsRequest scopes a provision-resource stats query.
//
// All fields are optional filters. An empty request returns the full view.
type GetProvisionResourceStatsRequest struct {
	// Cluster, if non-empty, limits the response to that cluster only.
	Cluster string `json:"cluster,omitempty"`
	// AcceleratorType, if non-empty, limits the response to that accelerator
	// SKU (e.g. "H100-SXM").
	AcceleratorType string `json:"accelerator_type,omitempty"`
}

// ResourceCounts is the planner's demand-side count breakdown. Every
// number is "what planner's books say": how many slots have been
// requested by live PlannerTasks, and how many of those requests
// actually hold an RM provision right now.
//
// Pool-supply concepts (how much capacity does the RM have, what is
// allocatable) are intentionally NOT in this view. Pool supply is
// owned by RM and is reachable via catalog.ListResources /
// catalog.ResourceStatItem (Supply / Allocated / Allocatable). Going
// through the planner for that would just smuggle RM data through an
// extra hop. Callers that want the planner-overlay view (which
// PlannerTasks are holding which slots) ask here; callers that want
// the supply view ask RM directly.
//
// ResourceCounts is reused at every aggregation level (pool total,
// per-cluster total, per-accelerator within cluster) on
// ProvisionResourceStatsView.
//
// Field semantics:
//
//   - Total : sum of resources requested across all PlannerTasks that
//     have already issued a resource request to RM, i.e. tasks in
//     PlannerTaskStateClaimed or PlannerTaskStateSubmitted. Tasks
//     still in PlannerTaskStateQueued have not issued a request yet
//     and are not counted here.
//   - InUse : the subset of Total where the RM provision is actively
//     held by the task. The difference (Total - InUse) is "request
//     issued but no provision yet" — typically the brief window a
//     claimed worker is waiting on RM.Provision, or a tail of failed
//     requests being retried.
//

type ResourceCounts struct {
	Total int `json:"total"`
	InUse int `json:"in_use"`
}

// AcceleratorResourceStats is one accelerator-type breakdown within a cluster.
type AcceleratorResourceStats struct {
	Type   string         `json:"type"`
	Counts ResourceCounts `json:"counts"`
}

// ClusterResourceStats is one cluster's breakdown of the provisioned pool.
type ClusterResourceStats struct {
	Cluster      string                     `json:"cluster"`
	Region       string                     `json:"region,omitempty"`
	Total        ResourceCounts             `json:"total"`
	Accelerators []AcceleratorResourceStats `json:"accelerators,omitempty"`
}

// ProvisionResourceStatsView is the planner's demand-side view: how
// many resources planner tasks have requested and how many of those
// requests are actively held, broken down by cluster and accelerator
// type. Pool-supply data (RM-side) is not included here; query
// catalog.ListResources for that.
//
// SampledAt records when the counts were measured. Implementations may
// serve cached values; freshness can be inferred from SampledAt.
type ProvisionResourceStatsView struct {
	SampledAt time.Time              `json:"sampled_at"`
	Total     ResourceCounts         `json:"total"`
	Clusters  []ClusterResourceStats `json:"clusters,omitempty"`
}
