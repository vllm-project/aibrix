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

import (
	"context"
)

// Planner is the Console BFF -> planner boundary.
//
// Console.CreateJob builds an EnqueueRequest and calls Enqueue. All
// reads (GetJob, ListJobs, Cancel) take the Console-generated JobID
// and return a Job; the MDS batch.ID never crosses this boundary
// upward. The planner owns the JobID -> batch.ID translation
// (in-memory in Passthrough, durable in the queued planner).
type Planner interface {
	Enqueue(ctx context.Context, req *EnqueueRequest) (*Job, error)
	GetJob(ctx context.Context, jobID string) (*Job, error)
	ListJobs(ctx context.Context, req *ListJobsRequest) (*ListJobsResponse, error)
	Cancel(ctx context.Context, jobID string) (*Job, error)
}
