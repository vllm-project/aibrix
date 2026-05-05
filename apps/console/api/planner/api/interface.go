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

import "context"

// Planner is the Console BFF -> planner boundary. It carries two
// groups of methods:
//
//  1. Job lifecycle (Enqueue / GetJob / ListJobs) — the write path and
//     the merged read view.
//  2. Planner-side telemetry (GetQueueStats / GetProvisionResourceStats)
//     — both return information the planner *itself* owns (queue
//     depth, demand-side resource accounting), intended to power the
//     Console / ops dashboard. They are not generic gateways into
//     other systems' data; if Console wants something the planner
//     does not own (for example RM pool supply, or MDS batch state
//     for a single batch), it queries that system directly.
//
// Console talks to one Planner client; whether the implementation
// reads queue stats from a TaskStore query or computes provision
// stats from PlannerTask aggregates is an implementation detail, not
// part of the public surface.
//
// # Job lifecycle
//
// CreateJob in the Console BFF should stop calling MDS synchronously.
// It builds an EnqueueRequest, calls Enqueue, and returns a queued
// view to the UI. A planner worker submits the task to MDS later.
// GetJob / ListJobs return the merged planner + MDS view; cancellation
// flows through MDS directly (the OpenAI Batches API exposes /cancel)
// and the planner observes the resulting terminal state on its
// read-time overlay rather than driving cancellation itself.
//
// # Telemetry (Planner-owned, ops-dashboard oriented)
//
// GetQueueStats reports queue depth and worker activity over the
// planner's own task store: how many PlannerTasks are currently
// queued, claimed, or in retryable_failure, plus the age of the
// oldest queued row. It is the canonical source for "is the planner
// keeping up?" — pure planner-internal data, no external lookup.
//
// GetProvisionResourceStats reports the planner's demand-side view
// of the RM-provisioned resource pool: how many slots planner tasks
// have already requested (Total) and how many of those requests
// currently hold an RM provision (InUse). Pool-supply data (how
// many slots the RM actually has) is not part of this view — for
// that, Console queries RM's catalog directly. Counts are broken
// down at three levels (pool total, per-cluster, per-accelerator-
// type within cluster) so the UI can render the full breakdown or
// roll up to the level it cares about.
type Planner interface {
	// Job lifecycle.
	Enqueue(ctx context.Context, req *EnqueueRequest) (*EnqueueResult, error)
	GetJob(ctx context.Context, jobID string) (*JobView, error)
	ListJobs(ctx context.Context, req *ListJobsRequest) (*ListJobsResponse, error)

	// Planner-side telemetry. Both methods return information the
	// planner itself owns and are intended to power the Console / ops
	// dashboard; they do not gateway into RM or MDS data.
	GetQueueStats(ctx context.Context, req *GetQueueStatsRequest) (*QueueStatsView, error)
	GetProvisionResourceStats(ctx context.Context, req *GetProvisionResourceStatsRequest) (*ProvisionResourceStatsView, error)
}
