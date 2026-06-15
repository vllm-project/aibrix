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

package impl

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openai/openai-go/v3"
	"k8s.io/klog/v2"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	pu "github.com/vllm-project/aibrix/apps/console/api/planner/utils"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"github.com/vllm-project/aibrix/apps/console/api/utils"
)

const (
	defaultWorkerCount      = 10
	defaultPlanningInterval = 60 * time.Second
	defaultListJobsLimit    = 20
)

// Planner is an asynchronous implementation of plannerapi.Planner.
// Enqueue records the job in memory, returns a placeholder batch in
// "pending" status, and lets workers run Provision, wait for the
// resource to reach Running, then CreateBatch.
type Planner struct {
	bc    plannerclient.BatchClient
	prov  provisioner.Provisioner
	store store.Store

	isRunning atomic.Bool

	backend plannerBackend // per-provisioner decision-making

	// policy makes planning decisions
	policy PlanningPolicy[*queuedJob]

	// Queues for different job states
	pendingQueue pu.PriorityQueue[*queuedJob]
	runningQueue pu.PriorityQueue[*queuedJob]

	// planning worker
	planningLoop *planningLoop

	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu   sync.RWMutex          // guards jobs
	jobs map[string]*queuedJob // JobID -> state
}

// PlannerConfig holds configuration for creating a Planner.
type PlannerConfig struct {
	BatchClient            plannerclient.BatchClient
	Provisioner            provisioner.Provisioner
	Store                  store.Store
	PolicyType             PlanningPolicyType
	WorkerCount            int           // concurrent job processing, default 10
	PlanningInterval       time.Duration // planning loop interval, default 60s
	MaxConcurrentProvision int           // max concurrent provisioning jobs, default 1
}

// DefaultPlannerConfig returns a PlannerConfig with default values.
func DefaultPlannerConfig() PlannerConfig {
	return PlannerConfig{
		WorkerCount:            defaultWorkerCount,
		PlanningInterval:       defaultPlanningInterval,
		PolicyType:             PlanningPolicyTypeSimple,
		MaxConcurrentProvision: DefaultPolicyConfig().MaxConcurrentProvisioning,
	}
}

// NewPlanner constructs a Planner with the given configuration.
// A nil Store disables persistence (used by tests).
func NewPlanner(cfg PlannerConfig) *Planner {
	// Apply defaults
	if cfg.WorkerCount < 1 {
		cfg.WorkerCount = DefaultPlannerConfig().WorkerCount
	}
	if cfg.PlanningInterval <= 0 {
		cfg.PlanningInterval = DefaultPlannerConfig().PlanningInterval
	}
	if cfg.MaxConcurrentProvision < 1 {
		cfg.MaxConcurrentProvision = DefaultPlannerConfig().MaxConcurrentProvision
	}

	if cfg.Provisioner == nil {
		klog.Errorf("[planner] missing provisioner")
		return nil
	}

	policyCfg := PolicyConfig{MaxConcurrentProvisioning: cfg.MaxConcurrentProvision}
	planningPolicy, err := newPlanningPolicy[*queuedJob](cfg.Provisioner.Type(), cfg.PolicyType, policyCfg)
	if err != nil {
		klog.Errorf("[planner] create policy %q failed: %v", cfg.PolicyType, err)
		return nil
	}

	q := &Planner{
		bc:           cfg.BatchClient,
		prov:         cfg.Provisioner,
		store:        cfg.Store,
		backend:      newPlannerBackend(cfg.Provisioner),
		policy:       planningPolicy,
		pendingQueue: pu.NewPriorityQueue[*queuedJob](),
		runningQueue: pu.NewPriorityQueue[*queuedJob](),
		jobs:         make(map[string]*queuedJob),
		isRunning:    atomic.Bool{},
	}

	// Planning loop
	q.planningLoop = newPlanningLoop(q, cfg.PlanningInterval, cfg.WorkerCount)

	klog.Infof("[planner] created planning loop interval=%v worker_pool_size=%d",
		cfg.PlanningInterval, cfg.WorkerCount)
	return q
}

var _ plannerapi.Planner = (*Planner)(nil)

func (q *Planner) Start(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if q.isRunning.Load() {
		return nil
	}

	q.isRunning.Store(true)

	ctx, cancel := context.WithCancel(ctx)
	q.baseCtx = ctx
	q.baseCancel = cancel
	q.planningLoop.Start(ctx)
	return nil
}

// Close cancels in-flight work and waits for all workers to exit.
func (q *Planner) Close() error {
	if q.baseCancel != nil {
		q.baseCancel()
	}
	if q.planningLoop != nil {
		q.planningLoop.Stop()
	}
	q.isRunning.Store(false)
	return nil
}

// triggerPlanning triggers an immediate planning cycle (non-blocking).
func (q *Planner) triggerPlanning() {
	if q.planningLoop != nil {
		q.planningLoop.Trigger()
	}
}

// releaseAfter performs a best-effort RM release and logs failures. The
// reason string ("wait failure", "CreateBatch failure", "cancel-race",
// "cancel submitted") appears in the log line so each call site is
// self-identifying.
func (q *Planner) releaseAfter(jobID, provisionID, reason string) {
	if q.prov == nil || provisionID == "" {
		return
	}
	if err := q.prov.Release(q.baseCtx, provisionID); err != nil {
		klog.Warningf("[planner] release after %s job_id=%q provision_id=%q: %v",
			reason, jobID, provisionID, err)
	} else {
		klog.Infof("[planner] release %s job_id=%q provision_id=%q", reason, jobID, provisionID)
	}
}

func (q *Planner) markFailed(job *queuedJob, status plannerapi.JobStatus, err error) {
	job.mu.Lock()
	if job.status == plannerapi.JobStatusCancelling {
		status = plannerapi.JobStatusCancelled
	}
	jobID := job.req.JobID
	provisionID := job.provisionID
	batchID := job.batchID
	job.status = status
	job.errMsg = err.Error()
	job.provisionID = ""
	job.batchID = ""
	now := time.Now().UTC()
	switch status {
	case plannerapi.JobStatusResourceFailed:
		job.resourceFailedAt = now
	case plannerapi.JobStatusSubmitFailed:
		job.submitFailedAt = now
	case plannerapi.JobStatusCancelled:
		job.canceledAt = now
	}
	job.mu.Unlock()

	q.persist(job)

	if provisionID != "" {
		q.releaseAfter(jobID, provisionID, err.Error())
	}
	if batchID != "" {
		klog.Infof("[planner] cancel submitted job_id=%q batch_id=%q", jobID, batchID)
		_, err := q.bc.CancelBatch(q.baseCtx, batchID)
		if err != nil {
			klog.Warningf("[planner] CancelBatch failed for job_id=%q: %v", jobID, err)
		}
	}

	klog.Warningf("[planner] job_id=%q status=%s: %v", jobID, status, err)
}

// persist writes the in-memory queuedJob snapshot to the store.
func (q *Planner) persist(job *queuedJob) {
	if q.store == nil {
		return
	}
	job.mu.RLock()
	jobID := job.req.JobID
	rec := jobToModel(job)
	job.mu.RUnlock()

	if err := q.store.UpsertJob(q.baseCtx, rec); err != nil {
		klog.Warningf("[planner] persist job_id=%q: %v", jobID, err)
	}
}

// Recover replays non-terminal jobs from the store into the Planner's
// in-memory state. Must be called once at startup, after NewPlanner and
// before the gRPC server begins accepting requests. Safe to call with a
// nil store (no-op).
//
// Recover is supposed to be invoked once at startup. No locks are held.
func (q *Planner) Recover(ctx context.Context) error {
	if q.store == nil {
		return q.Start(ctx)
	}
	rows, err := q.store.ListNonTerminalJobs(ctx)
	if err != nil {
		return fmt.Errorf("list non-terminal jobs: %w", err)
	}
	recovered := make([]*queuedJob, 0, len(rows))
	for _, rec := range rows {
		recovered = append(recovered, modelToJob(rec))
	}

	for _, job := range recovered {
		if job.status.IsTerminal() {
			continue
		}

		q.jobs[job.req.JobID] = job
		// Re-enqueue the job onto the queue based on the status
		if job.status == plannerapi.JobStatusQueued || job.status == plannerapi.JobStatusPlanned {
			q.pendingQueue.Push(job, 0)
			job.queue = q.pendingQueue
		} else {
			q.runningQueue.Push(job, 0)
			job.queue = q.runningQueue
		}
	}

	klog.Infof("[planner] recovered %d non-terminal jobs (%d re-enqueued)", len(rows), q.pendingQueue.Len())

	// Start the planner
	if err := q.Start(ctx); err != nil {
		return err
	}

	// Trigger immediate planning if jobs were recovered
	if q.pendingQueue.Len() > 0 {
		q.triggerPlanning()
	}
	return nil
}

// Enqueue records the job, pushes it onto the queue, and returns
// a placeholder batch in "pending" status.
func (q *Planner) Enqueue(ctx context.Context, req *plannerapi.EnqueueRequest) (*plannerapi.Job, error) {
	// Validate the request
	if err := validateEnqueueRequest(req); err != nil {
		return nil, fmt.Errorf("%w: %v", plannerapi.ErrInvalidJob, err)
	}
	// Backend may impose additional pre-flight checks. Default is a no-op.
	if err := q.backend.ValidateRequest(req); err != nil {
		return nil, err
	}
	if err := q.baseCtx.Err(); err != nil {
		return nil, fmt.Errorf("planner closed: %w", err)
	}

	now := time.Now().UTC()
	q.mu.Lock()
	if _, exists := q.jobs[req.JobID]; exists {
		q.mu.Unlock()
		return nil, fmt.Errorf("%w: duplicate job_id %q", plannerapi.ErrInvalidJob, req.JobID)
	}
	completionWindow, _ := time.ParseDuration(string(req.BatchParams.CompletionWindow))
	job := &queuedJob{
		req:        req,
		status:     plannerapi.JobStatusQueued,
		queuedAt:   now,
		expiresAt:  now.Add(completionWindow),
		pqPriority: 0,
		queue:      q.pendingQueue,
	}
	q.jobs[req.JobID] = job
	q.mu.Unlock()

	// Add to pending queue
	q.pendingQueue.Push(job, 0)

	// Persist to store
	q.persist(job)

	// Trigger immediate planning cycle
	q.triggerPlanning()

	reqJson, err := json.Marshal(req)
	if err != nil {
		reqJson = []byte("null")
	}

	klog.Infof("[planner] enqueue job_id=%q %s", req.JobID, reqJson)
	return &plannerapi.Job{
		JobID: req.JobID,
		Batch: placeholderBatch(req, plannerapi.JobStatusQueued.ToBatchStatus(), now, time.Time{}),
		State: &plannerapi.JobState{
			QueuedAt: now,
		},
	}, nil
}

// GetJob resolves the JobID.
func (q *Planner) GetJob(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	q.mu.RLock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.RUnlock()
		return q.getJobFromStore(ctx, jobID)
	}
	q.mu.RUnlock()

	job.mu.RLock()
	status := job.status
	req := job.req
	queuedAt := job.queuedAt
	terminalAt := terminalTime(job)
	// The running worker will poll MDS and update the batch if it changes.
	batch := job.batch
	state := jobStateSnapshot(job)
	job.mu.RUnlock()

	return &plannerapi.Job{
		JobID: jobID,
		Batch: getBatchOrPlaceholder(batch, req, status, queuedAt, terminalAt),
		State: state,
	}, nil
}

// getJobFromStore resolves a job that is no longer in the in-memory map
// (terminal/evicted jobs after a Planner restart) from the durable store.
func (q *Planner) getJobFromStore(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if q.store == nil {
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	rec, err := q.store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	j := modelToJob(rec)
	if j.batchID != "" {
		klog.Infof("[planner] get_job (store) job_id=%q batch_id=%q", jobID, j.batchID)
		batch, err := q.bc.GetBatch(ctx, j.batchID)
		if err != nil {
			return nil, err
		}
		return &plannerapi.Job{JobID: jobID, Batch: batch, State: jobStateSnapshot(j)}, nil
	}
	return &plannerapi.Job{
		JobID: jobID,
		Batch: getBatchOrPlaceholder(j.batch, j.req, j.status, j.queuedAt, terminalTime(j)),
		State: jobStateSnapshot(j),
	}, nil
}

// Cancel enqueues a cancel request for async processing.
// It returns immediately with the cancelling job state.
func (q *Planner) Cancel(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	q.mu.RLock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.RUnlock()
		// Job not found in memory, must be a terminal job in the store
		return q.getJobFromStore(ctx, jobID)
	}
	q.mu.RUnlock()

	now := time.Now().UTC()
	job.mu.Lock()
	status := job.status
	req := job.req
	queuedAt := job.queuedAt

	// Already terminal — return current view, no double-cancel side effects.
	if status.IsTerminal() {
		terminalAt := terminalTime(job)
		batch := job.batch
		state := jobStateSnapshot(job)
		job.mu.Unlock()
		return &plannerapi.Job{JobID: jobID, Batch: getBatchOrPlaceholder(batch, req, status, queuedAt, terminalAt), State: state}, nil
	}

	job.cancelRequestedAt = now
	job.status = plannerapi.JobStatusCancelling
	batch := job.batch
	state := jobStateSnapshot(job)
	job.mu.Unlock()

	q.persist(job)
	klog.Infof("[planner] cancelling in-progress job_id=%q prior_status=%s", jobID, status)

	// Return with status cancelled
	status = plannerapi.JobStatusCancelled
	if batch != nil {
		batchCopy := *batch
		batchCopy.Status = openai.BatchStatusCancelled
		batch = &batchCopy
	}
	return &plannerapi.Job{JobID: jobID, Batch: getBatchOrPlaceholder(batch, req, status, queuedAt, now), State: state}, nil
}

// ListJobs lists all jobs from the store with cursor-based pagination.
func (q *Planner) ListJobs(ctx context.Context, req *plannerapi.ListJobsRequest) (*plannerapi.ListJobsResponse, error) {
	if q.store == nil {
		return &plannerapi.ListJobsResponse{Data: nil, HasMore: false}, nil
	}

	limit := defaultListJobsLimit
	if req != nil && req.Limit > 0 {
		limit = req.Limit
	}

	after := ""
	if req != nil {
		after = req.After
	}

	rows, hasMore, err := q.store.ListAllJobs(ctx, after, limit)
	if err != nil {
		return nil, err
	}

	out := make([]*plannerapi.Job, 0, len(rows))
	for _, row := range rows {
		j := modelToJob(row)
		out = append(out, &plannerapi.Job{
			JobID: row.ID,
			Batch: getBatchOrPlaceholder(j.batch, j.req, j.status, j.queuedAt, terminalTime(j)),
			State: jobStateSnapshot(j),
		})
	}

	return &plannerapi.ListJobsResponse{Data: out, HasMore: hasMore}, nil
}

// placeholderBatch builds the batch view for jobs without an MDS batch.ID.
// terminalAt is the recorded transition time for failed/canceled states;
// a zero value leaves FailedAt/CancelledAt at zero.
func placeholderBatch(req *plannerapi.EnqueueRequest, st openai.BatchStatus, enqueuedAt, terminalAt time.Time) *openai.Batch {
	b := &openai.Batch{
		Object:           "batch",
		Status:           st,
		Endpoint:         string(req.BatchParams.Endpoint),
		InputFileID:      req.BatchParams.InputFileID,
		CompletionWindow: string(req.BatchParams.CompletionWindow),
		Model:            req.Model,
		CreatedAt:        utils.UnixOrZero(enqueuedAt),
	}
	if len(req.BatchParams.Metadata) > 0 {
		b.Metadata = map[string]string(req.BatchParams.Metadata)
	}
	if !terminalAt.IsZero() {
		switch st {
		case openai.BatchStatusFailed:
			b.FailedAt = terminalAt.Unix()
		case openai.BatchStatusExpired:
			b.ExpiredAt = terminalAt.Unix()
		case openai.BatchStatusCancelled:
			b.CancelledAt = terminalAt.Unix()
		}
	}
	return b
}

func getBatchOrPlaceholder(batch *openai.Batch, req *plannerapi.EnqueueRequest, status plannerapi.JobStatus, enqueuedAt, terminalAt time.Time) *openai.Batch {
	if batch == nil {
		batch = placeholderBatch(req, status.ToBatchStatus(), enqueuedAt, terminalAt)
	}
	return batch
}
