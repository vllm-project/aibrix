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

	"github.com/openai/openai-go/v3"
)

// Planner is the Console BFF -> planner boundary.
//
// Console.CreateJob builds an EnqueueRequest and calls Enqueue, which
// returns the live batch view. ListJobs / GetJob are the read seam:
// Console always goes through the Planner so that future queued
// implementations can overlay planner-side state (queued / claimed /
// retryable_failure) onto the MDS batch view without changing the
// handler.
//
// Cancellation flows through MDS directly (the OpenAI Batches API
// exposes /cancel) and is intentionally not part of this interface;
// the Planner observes the resulting MDS terminal state at read time.
type Planner interface {
	Enqueue(ctx context.Context, req *EnqueueRequest) (*EnqueueResult, error)
	GetJob(ctx context.Context, jobID string) (*openai.Batch, error)
	ListJobs(ctx context.Context, req *ListJobsRequest) (*ListJobsResponse, error)
}
