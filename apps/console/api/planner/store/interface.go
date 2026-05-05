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

// Package store defines the planner-internal durable state and
// worker-coordination boundary. It is intentionally factored into three
// orthogonal sub-interfaces so each consumer depends only on what it
// actually uses:
//
//   - TaskQueue      - queue coordination (queued <-> claimed,
//     crash recovery). Consumer: Worker dispatcher
//     and SchedulerFunc.
//   - TaskLifecycle  - post-claim state transitions
//     (claimed -> submitted / retryable / terminal,
//     submitted -> superseded). Consumer: Worker
//     task goroutines and the provision-expiry
//     sweeper.
//   - TaskRepository - read-only queries by task/job ID. Consumer:
//     Planner.GetJob / ListJobs.
//
// The current MVP can be backed by an in-memory implementation.
// For process restart / crash recovery and multi-worker coordination,
// we expect a durable store implementation (for example Postgres/MySQL)

package store

import (
	"context"
	"errors"
	"time"

	"github.com/vllm-project/aibrix/apps/console/api/planner/task"
)

// ErrTaskAlreadyTerminal indicates a state transition (Ack / Nack /
// Fail / Supersede) targeted a task that has already reached a
// terminal state (terminal_failure, superseded, or any post-submit
// MDS-driven terminal — for example MDS finished the batch before the
// provision-expiry sweeper saw it). Callers may treat this as a
// no-op success — the task is settled — but the sentinel is exposed
// so the provision-expiry sweeper can distinguish "raced with
// MDS-side completion" from real errors and skip without alerting.
var ErrTaskAlreadyTerminal = errors.New("planner/store: task already terminal")

// TaskQueue is the worker-coordination layer: enqueue, dequeue, peek
// (for policy input), and crash recovery. It does not know about
// Ack/Nack/Fail or about provisions; those live on TaskLifecycle.
//
// Two dequeue paths are supported via DequeueRequest:
//
//   - TaskIDs == nil : FCFS convenience path. The queue applies its
//     default ordering (typically `available_at` ascending) and claims
//     up to Limit candidates in a single round-trip. Workers that don't
//     need a custom policy use this directly.
//   - TaskIDs != nil : policy-aware path. A SchedulerFunc peeks at
//     candidates without claiming them, applies its ranking policy,
//     and asks the queue to claim just the IDs it selected. This
//     supports priority, score-based, fair-share, and resource-aware
//     policies without coupling them to the queue.
type TaskQueue interface {
	// Enqueue inserts the first PlannerTask of a JobID's attempt
	// chain. The queue sets Attempt = 1 regardless of any value the
	// caller passed in. Subsequent attempts (continuations after a
	// provision expiry) go through TaskLifecycle.Supersede, never
	// through Enqueue.
	Enqueue(ctx context.Context, t *task.PlannerTask) error

	// Dequeue atomically transitions tasks from queued to claimed
	// and returns them. When req.TaskIDs is nil, up to req.Limit
	// tasks are picked by the queue's default ordering (FCFS);
	// otherwise only the listed IDs are claimed, silently skipping
	// any that are no longer claimable.
	Dequeue(ctx context.Context, req *DequeueRequest) ([]*task.PlannerTask, error)

	// Peek returns claimable tasks without transitioning them. Used
	// by SchedulerFunc implementations to compute a ranking before
	// committing a selection via Dequeue.
	Peek(ctx context.Context, req *PeekRequest) ([]*task.PlannerTask, error)

	// Recover is called once on Worker startup. It resets every
	// PlannerTask currently in PlannerTaskStateClaimed back to
	// PlannerTaskStateQueued so any tasks the prior process was
	// holding when it crashed are re-claimable on the next Dequeue
	// call. Returns the number of rows transitioned for telemetry.
	Recover(ctx context.Context) (int, error)
}

// TaskLifecycle is the post-claim state-machine layer. Ack/Nack/Fail
// validate that the task is still in claimed and return
// ErrTaskAlreadyTerminal otherwise. Supersede and
// ListExpiringProvisions drive the provision-expiry sweeper.
type TaskLifecycle interface {
	Ack(ctx context.Context, req *AckRequest) error
	Nack(ctx context.Context, req *NackRequest) error
	Fail(ctx context.Context, req *FailRequest) error

	// ListExpiringProvisions returns submitted tasks whose
	// ProvisionExpiresAt is at or before `before`, capped at `limit`.
	// Used by the provision-expiry sweeper to find submitted attempts
	// whose RM provision is about to expire so it can create a
	// continuation via Supersede. Tasks without a persisted
	// ProvisionExpiresAt are not returned.
	ListExpiringProvisions(ctx context.Context, before time.Time, limit int) ([]*task.PlannerTask, error)

	// Supersede atomically supersedes a submitted task and inserts
	// a continuation: the prior task transitions to
	// PlannerTaskStateSuperseded (BatchID preserved for audit) and
	// a new PlannerTask is inserted in PlannerTaskStateQueued with
	// the same JobID and Attempt = priorTask.Attempt + 1. Used by
	// the provision-expiry sweeper.
	//
	// Does not require a lease (the prior task is no longer leased
	// after Ack). Returns ErrJobNotFound if SupersededTaskID does
	// not exist; ErrTaskAlreadyTerminal if the prior task is not in
	// submitted (raced with MDS-side completion or a concurrent
	// CancelTask).
	Supersede(ctx context.Context, req *EnqueueContinuationRequest) error
}

// TaskRepository is the read-only query layer used by planner read
// surfaces (Planner.GetJob / ListJobs) and audit/debug tools.
type TaskRepository interface {
	// GetByJobID returns the *latest* PlannerTask for a JobID - the
	// row with the highest Attempt. Most consumers (Planner.GetJob,
	// the worker on lease) want this. Use ListTasksByJobID for the
	// full chain of attempts.
	GetByJobID(ctx context.Context, jobID string) (*task.PlannerTask, error)

	// ListTasksByJobID returns every PlannerTask for a JobID,
	// ordered by Attempt ascending. Used by audit/debug surfaces
	// and by Planner.GetJob when it needs to surface the
	// prior-attempts history alongside the latest attempt's
	// BatchView.
	ListTasksByJobID(ctx context.Context, jobID string) ([]*task.PlannerTask, error)

	GetByTaskID(ctx context.Context, taskID string) (*task.PlannerTask, error)
}

// TaskStore is the composite interface a production implementation
// satisfies. New code should depend on the narrowest sub-interface
// (TaskQueue / TaskLifecycle / TaskRepository) it needs.
type TaskStore interface {
	TaskQueue
	TaskLifecycle
	TaskRepository
}
