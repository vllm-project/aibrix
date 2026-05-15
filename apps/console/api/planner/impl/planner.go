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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"k8s.io/klog/v2"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Planner is an asynchronous implementation of plannerapi.Planner.
// Enqueue records the job in memory, returns a placeholder batch in
// "pending" status, and lets workers run Provision, wait for the
// resource to reach Running, then CreateBatch.
type Planner struct {
	bc   plannerclient.BatchClient
	prov provisioner.Provisioner

	queue pendingQueue

	baseCtx    context.Context
	baseCancel context.CancelFunc
	wg         sync.WaitGroup // tracks live workers; Close waits on this

	mu         sync.RWMutex          // guards jobs and jobByBatch
	jobs       map[string]*queuedJob // JobID -> state
	jobByBatch map[string]string     // batch.ID -> JobID (for ListJobs tagging)

	// provPollInterval is how often waitForProvisionReady polls the RM.
	// Same-package tests override for fast assertions.
	provPollInterval time.Duration
}

// jobState is the planner-side lifecycle. Before submission, statusFor maps
// it to "pending" or "provisioning". After submission, status comes from
// MDS.
//
//	Pending      → buffered in the queue, no worker yet.
//	Provisioning → worker holds the job; Provision + CreateBatch in flight.
//	Submitted    → CreateBatch returned; MDS owns the lifecycle from here.
//	Failed       → Provision or CreateBatch errored.
//	Canceled     → user-canceled. A cancel landing mid-Provision or
//	               mid-CreateBatch is honored at the post-CreateBatch
//	               checkpoint: state stays Canceled, CancelBatch is
//	               forwarded to MDS, and the resource is released.
type jobState int

const (
	jobStatePending jobState = iota
	jobStateProvisioning
	jobStateSubmitted
	jobStateFailed
	jobStateCanceled
)

type queuedJob struct {
	req         *plannerapi.EnqueueRequest
	state       jobState
	provisionID string // populated once Provision returns accepted
	batchID     string // populated when state == jobStateSubmitted
	err         error  // populated when state == jobStateFailed
	enqueuedAt  time.Time
	failedAt    time.Time // populated when state == jobStateFailed
	canceledAt  time.Time // populated when state == jobStateCanceled
}

// terminalTime returns the timestamp at which the job transitioned into a
// terminal state, or the zero value if it isn't terminal. Caller holds the
// lock guarding queuedJob.state.
func terminalTime(j *queuedJob) time.Time {
	switch j.state {
	case jobStateFailed:
		return j.failedAt
	case jobStateCanceled:
		return j.canceledAt
	}
	return time.Time{}
}

// DefaultWorkerCount sizes the worker pool.
const DefaultWorkerCount = 8

// defaultProvPollInterval matches the cadence used by Provisioner-level
// integration tests when waiting for "running" status.
const defaultProvPollInterval = 5 * time.Second

// provReadyTimeout caps how long a single worker will wait for a
// provision to reach Running. Beyond this, the job is marked Failed and
// the resource is released.
const provReadyTimeout = 2 * time.Minute

// NewPlanner constructs a Planner and starts workerCount background
// workers. workerCount < 1 is floored to 1.
func NewPlanner(bc plannerclient.BatchClient, prov provisioner.Provisioner, workerCount int) *Planner {
	if workerCount < 1 {
		workerCount = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Planner{
		bc:               bc,
		prov:             prov,
		queue:            newFIFOPendingQueue(queueCapacity),
		baseCtx:          ctx,
		baseCancel:       cancel,
		jobs:             make(map[string]*queuedJob),
		jobByBatch:       make(map[string]string),
		provPollInterval: defaultProvPollInterval,
	}
	q.wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go q.run()
	}
	klog.Infof("[planner] started worker pool size=%d capacity=%d", workerCount, queueCapacity)
	return q
}

var _ plannerapi.Planner = (*Planner)(nil)

// Close cancels in-flight work and waits for workers to exit.
func (q *Planner) Close() error {
	q.queue.Close()
	q.baseCancel()
	q.wg.Wait()
	return nil
}

func (q *Planner) run() {
	defer q.wg.Done()
	for {
		jobID, err := q.queue.Pop(q.baseCtx)
		if err != nil {
			return
		}
		q.process(jobID)
	}
}

func (q *Planner) process(jobID string) {
	// Atomic check-and-flip Pending → Provisioning.
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return
	}
	// Example: a pending job is canceled before a worker picks it up, so the
	// worker later observes a non-pending state here and skips provisioning.
	if job.state != jobStatePending {
		state := job.state
		q.mu.Unlock()
		klog.Infof("[planner] invalid state before provisioning job_id=%q state=%d", jobID, state)
		return
	}
	job.state = jobStateProvisioning
	req := job.req
	q.mu.Unlock()

	provReq := &rmtypes.ResourceProvision{
		Spec: rmtypes.ResourceProvisionSpec{
			Credential: rmtypes.ResourceCredential{Provider: q.prov.Type()},
		},
		IdempotencyKey: req.JobID,
	}
	provResult, err := q.prov.Provision(q.baseCtx, provReq)
	if err != nil {
		q.markFailed(jobID, errors.Join(plannerapi.ErrInsufficientResources, err))
		return
	}
	q.mu.Lock()
	q.jobs[jobID].provisionID = provResult.ProvisionID
	q.mu.Unlock()

	// Provision returns when the request is accepted, not when the resource
	// is ready. Wait for Running before submitting to MDS, which rejects
	// batches that point to not-yet-ready provisions.
	if err := q.waitForProvisionReady(provResult.ProvisionID); err != nil {
		q.releaseAfter(jobID, provResult.ProvisionID, "wait failure")
		q.markFailed(jobID, errors.Join(plannerapi.ErrInsufficientResources, err))
		return
	}
	klog.Infof("[planner] provision ready job_id=%q provision_id=%q provider=%q",
		jobID, provResult.ProvisionID, q.prov.Type())

	aibrix := plannerclient.AIBrixExtraBody{
		JobID: req.JobID,
		PlannerDecision: &plannerclient.PlannerDecision{
			ProvisionID: provResult.ProvisionID,
		},
		ModelTemplate: req.ModelTemplate,
	}

	klog.Infof("[planner] submit job_id=%q provision_id=%q model_template=%v",
		req.JobID, provResult.ProvisionID, req.ModelTemplate)

	batch, err := q.bc.CreateBatch(q.baseCtx, req.BatchParams, aibrix)
	if err != nil {
		q.releaseAfter(jobID, provResult.ProvisionID, "CreateBatch failure")
		q.markFailed(jobID, err)
		return
	}

	// Cancel may have raced in during Provision or CreateBatch. Record the
	// batch.ID either way so ListJobs can tag it; only flip to Submitted if
	// no cancel landed. On race, forward CancelBatch to MDS and release the
	// provisioned resource.
	canceled := false
	q.mu.Lock()
	if job, ok := q.jobs[jobID]; ok {
		job.batchID = batch.ID
		q.jobByBatch[batch.ID] = jobID
		if job.state == jobStateCanceled {
			canceled = true
		} else {
			job.state = jobStateSubmitted
		}
	}
	q.mu.Unlock()

	if !canceled {
		return
	}
	klog.Infof("[planner] cancel raced submit; forwarding to MDS job_id=%q batch_id=%q", jobID, batch.ID)
	if _, err := q.bc.CancelBatch(q.baseCtx, batch.ID); err != nil {
		klog.Warningf("[planner] race cancel forward failed job_id=%q batch_id=%q: %v", jobID, batch.ID, err)
	}
	q.releaseAfter(jobID, provResult.ProvisionID, "cancel-race")
}

// waitForProvisionReady polls the RM until the provision reaches Running
// or Failed, the timeout elapses, or the scheduler is shutting down.
// Provisioner.Provision returns when the request is accepted, not when the
// resource is ready; Planner must wait for Running before invoking CreateBatch.
func (q *Planner) waitForProvisionReady(provisionID string) error {
	filter := &rmtypes.ListOptions{ProvisionIDs: &[]string{provisionID}}
	deadline := time.Now().Add(provReadyTimeout)
	for {
		results, err := q.prov.List(q.baseCtx, filter)
		switch {
		case err != nil:
			klog.Warningf("[planner] poll provision_id=%q: %v", provisionID, err)
		case len(results) == 0:
			return fmt.Errorf("provision %q not found", provisionID)
		default:
			switch results[0].Status {
			case rmtypes.ProvisionStatusRunning:
				return nil
			case rmtypes.ProvisionStatusFailed:
				return fmt.Errorf("provision failed: %s", results[0].ErrorMessage)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("provision %q did not reach Running within %v", provisionID, provReadyTimeout)
		}
		select {
		case <-q.baseCtx.Done():
			return q.baseCtx.Err()
		case <-time.After(q.provPollInterval):
		}
	}
}

// releaseAfter performs a best-effort RM release and logs failures. The
// reason string ("wait failure", "CreateBatch failure", "cancel-race",
// "cancel submitted") appears in the log line so each call site is
// self-identifying.
func (q *Planner) releaseAfter(jobID, provisionID, reason string) {
	if err := q.prov.Release(q.baseCtx, provisionID); err != nil {
		klog.Warningf("[planner] release after %s job_id=%q provision_id=%q: %v",
			reason, jobID, provisionID, err)
	}
}

func (q *Planner) markFailed(jobID string, err error) {
	q.mu.Lock()
	job := q.jobs[jobID]
	job.state = jobStateFailed
	job.err = err
	job.failedAt = time.Now()
	q.mu.Unlock()
	klog.Warningf("[planner] job_id=%q failed: %v", jobID, err)
}

// Enqueue records the job, pushes it onto the queue, and returns
// a placeholder batch in "pending" status.
func (q *Planner) Enqueue(ctx context.Context, req *plannerapi.EnqueueRequest) (*plannerapi.Job, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", plannerapi.ErrInvalidJob)
	}
	if req.JobID == "" {
		return nil, fmt.Errorf("%w: missing job_id", plannerapi.ErrInvalidJob)
	}
	if req.BatchParams.InputFileID == "" {
		return nil, fmt.Errorf("%w: missing input_file_id", plannerapi.ErrInvalidJob)
	}
	if req.BatchParams.Endpoint == "" {
		return nil, fmt.Errorf("%w: missing endpoint", plannerapi.ErrInvalidJob)
	}
	if q.prov == nil {
		return nil, fmt.Errorf("%w: missing provisioner", plannerapi.ErrInsufficientResources)
	}
	if err := q.baseCtx.Err(); err != nil {
		return nil, fmt.Errorf("planner closed: %w", err)
	}

	now := time.Now()
	q.mu.Lock()
	if _, exists := q.jobs[req.JobID]; exists {
		q.mu.Unlock()
		return nil, fmt.Errorf("%w: duplicate job_id %q", plannerapi.ErrInvalidJob, req.JobID)
	}
	q.jobs[req.JobID] = &queuedJob{
		req:        req,
		state:      jobStatePending,
		enqueuedAt: now,
	}
	q.mu.Unlock()

	if err := q.queue.Push(ctx, req.JobID); err != nil {
		q.rollbackEnqueue(req.JobID)
		if errors.Is(err, errQueueClosed) {
			// Planner shutting down while the queue was full; roll back the orphaned insert.
			return nil, fmt.Errorf("planner closed: %w", q.baseCtx.Err())
		}
		// Caller gave up while the queue was full; the bookkeeping insert is orphaned.
		return nil, err
	}

	klog.Infof("[planner] enqueue job_id=%q", req.JobID)
	return &plannerapi.Job{
		JobID: req.JobID,
		Batch: placeholderBatch(req, statusFor(jobStatePending), now, time.Time{}),
	}, nil
}

// GetJob resolves the JobID. Submitted jobs forward to MDS; others return
// a placeholder batch with status derived from jobState.
func (q *Planner) GetJob(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	q.mu.RLock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.RUnlock()
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	state := job.state
	batchID := job.batchID
	req := job.req
	enqueuedAt := job.enqueuedAt
	terminalAt := terminalTime(job)
	q.mu.RUnlock()

	if state == jobStateSubmitted {
		klog.Infof("[planner] get_job job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.GetBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	return &plannerapi.Job{
		JobID: jobID,
		Batch: placeholderBatch(req, statusFor(state), enqueuedAt, terminalAt),
	}, nil
}

// Cancel marks a pending/provisioning job canceled, or forwards cancel to
// MDS for a submitted job. A cancel that lands mid-Provision or
// mid-CreateBatch is honored at the worker's post-CreateBatch checkpoint
// (which forwards CancelBatch and releases the resource).
func (q *Planner) Cancel(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	state := job.state
	batchID := job.batchID
	provisionID := job.provisionID
	req := job.req
	enqueuedAt := job.enqueuedAt
	var terminalAt time.Time
	if state == jobStatePending || state == jobStateProvisioning {
		now := time.Now()
		job.state = jobStateCanceled
		job.canceledAt = now
		terminalAt = now
	} else {
		terminalAt = terminalTime(job)
	}
	q.mu.Unlock()

	switch state {
	case jobStatePending, jobStateProvisioning:
		klog.Infof("[planner] cancel pre-submit job_id=%q state=%d", jobID, state)
		return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, openai.BatchStatusCancelled, enqueuedAt, terminalAt)}, nil
	case jobStateSubmitted:
		klog.Infof("[planner] cancel submitted job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.CancelBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		q.releaseAfter(jobID, provisionID, "cancel submitted")
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	// Already terminal (failed/canceled) — return current view, no double-cancel side effects.
	return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, statusFor(state), enqueuedAt, terminalAt)}, nil
}

// ListJobs merges MDS batches with local not-yet-submitted jobs. Local jobs
// are shown only on the first page so the MDS cursor remains valid.
func (q *Planner) ListJobs(ctx context.Context, req *plannerapi.ListJobsRequest) (*plannerapi.ListJobsResponse, error) {
	listReq := &plannerclient.ListBatchesRequest{}
	if req != nil {
		listReq.Limit = req.Limit
		listReq.After = req.After
	}
	resp, err := q.bc.ListBatches(ctx, listReq)
	if err != nil {
		return nil, err
	}

	out := make([]*plannerapi.Job, 0, len(resp.Data))
	if listReq.After == "" {
		out = append(out, q.unsubmittedJobs()...)
	}
	q.mu.RLock()
	for _, b := range resp.Data {
		out = append(out, &plannerapi.Job{JobID: q.jobByBatch[b.ID], Batch: b})
	}
	q.mu.RUnlock()
	return &plannerapi.ListJobsResponse{Data: out, HasMore: resp.HasMore}, nil
}

// unsubmittedJobs returns the non-submitted planner-tracked jobs, newest
// first. Mutable fields are snapshotted under the lock so the rendering
// loop doesn't race against concurrent state transitions.
func (q *Planner) unsubmittedJobs() []*plannerapi.Job {
	type snap struct {
		req        *plannerapi.EnqueueRequest
		state      jobState
		enqueuedAt time.Time
		terminalAt time.Time
	}
	q.mu.RLock()
	unsubmitted := make([]snap, 0)
	for _, job := range q.jobs {
		if job.state != jobStateSubmitted {
			unsubmitted = append(unsubmitted, snap{
				req:        job.req,
				state:      job.state,
				enqueuedAt: job.enqueuedAt,
				terminalAt: terminalTime(job),
			})
		}
	}
	q.mu.RUnlock()
	sort.Slice(unsubmitted, func(i, k int) bool {
		return unsubmitted[i].enqueuedAt.After(unsubmitted[k].enqueuedAt)
	})
	out := make([]*plannerapi.Job, 0, len(unsubmitted))
	for _, job := range unsubmitted {
		out = append(out, &plannerapi.Job{
			JobID: job.req.JobID,
			Batch: placeholderBatch(job.req, statusFor(job.state), job.enqueuedAt, job.terminalAt),
		})
	}
	return out
}

// rollbackEnqueue undoes the q.jobs insert from Enqueue when Enqueue fails.
// Not called on processing failures — markFailed keeps those entries in
// q.jobs so callers can observe them.
func (q *Planner) rollbackEnqueue(jobID string) {
	q.mu.Lock()
	delete(q.jobs, jobID)
	q.mu.Unlock()
}

// statusFor maps planner state to the status used on placeholder batches.
func statusFor(s jobState) openai.BatchStatus {
	switch s {
	case jobStatePending:
		return openai.BatchStatus("pending")
	case jobStateProvisioning:
		return openai.BatchStatus("provisioning")
	case jobStateFailed:
		return openai.BatchStatusFailed
	case jobStateCanceled:
		return openai.BatchStatusCancelled
	}
	return openai.BatchStatus("pending")
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
		CreatedAt:        enqueuedAt.Unix(),
	}
	if len(req.BatchParams.Metadata) > 0 {
		b.Metadata = map[string]string(req.BatchParams.Metadata)
	}
	if !terminalAt.IsZero() {
		switch st {
		case openai.BatchStatusFailed:
			b.FailedAt = terminalAt.Unix()
		case openai.BatchStatusCancelled:
			b.CancelledAt = terminalAt.Unix()
		}
	}
	return b
}
