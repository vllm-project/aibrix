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

// Package scheduler defines the planner scheduling-policy boundary
// (SchedulerFunc + ScheduleRequest) and is the home for concrete
// policy implementations.
//
// Concrete policies (FCFS, priority, fair-share, RM-aware, ...) land
// in sibling files — fcfs.go, priority.go, fair_share.go — within this
// same package. A package-per-policy split is intentionally NOT used
// at MVP: the policy count is small, related policies share unexported
// helpers, and Go's default convention is to wait until the second
// non-trivial implementation before splitting. Triggers for upgrading
// to one sub-package per policy are documented in the planner README.
//
// The Worker accepts a SchedulerFunc at construction time; whichever
// function is injected determines the policy. Wiring changes amount to
// one assignment in main(). A nil SchedulerFunc means "use the
// FCFS-equivalent path on TaskQueue.Dequeue(TaskIDs=nil) directly", so
// callers that don't care about ranking can leave it unset.
package scheduler

import (
	"context"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/planner/store"
)

// ScheduleRequest is the standard input to a SchedulerFunc. The worker
// passes itself in via WorkerID, the batch size as Limit, and the
// current time as Now (so policies stay deterministic for tests).
type ScheduleRequest struct {
	WorkerID string    `json:"worker_id"`
	Limit    int       `json:"limit"`
	Now      time.Time `json:"now"`
}

// SchedulerFunc is the convention for plugging a scheduling policy
// into the Worker. Given a TaskStore and a ScheduleRequest, it returns
// the TaskIDs to claim in preferred order. The Worker hands the result
// to TaskQueue.Dequeue(TaskIDs=...) for atomic acquisition.
//
// SchedulerFunc is the function-shaped equivalent of a Scheduler
// interface; if a real plug-in taxonomy is needed later (multiple
// policies in the same binary, exposed by name), the function type
// wraps cleanly inside a one-method interface (see http.HandlerFunc /
// http.Handler for the same pattern in the standard library). Keep it
// as a function type until that need is concrete.
type SchedulerFunc func(ctx context.Context, ts store.TaskStore, req *ScheduleRequest) ([]string, error)
