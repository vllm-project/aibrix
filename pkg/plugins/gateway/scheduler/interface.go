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

package scheduler

import (
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
)

// Decision is the output from the scheduler,
// containing the job to be processed.
type Decision struct {
	Job *SchedulingJob // The job to be processed.
	Err error          // Any error encountered during scheduling.
}

// Scheduler defines the interface for a session-aware request scheduler.
// It is designed to be pluggable into the AIBrix Gateway.
type Scheduler interface {
	// SubmitJob adds a new job to the scheduler's queue and blocks until a
	// decision is made. The decision contains the job itself,
	// empowering the caller to proceed.
	// Plus, one job is one request.
	SubmitJob(ctx *types.RoutingContext, sessionID string) (*Decision, error)

	// FinalizeJob updates the session state after a job has been completed.
	FinalizeJob(sessionID string, inheritedCST, executionTime, waitTime time.Duration)

	// Stop gracefully shuts down the scheduler's background processing loop.
	Stop()

	// TODO: Add a GetStats() method for monitoring in the future.
	// GetSchedulerStats() *SchedulerStats
}
