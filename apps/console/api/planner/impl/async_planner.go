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

// Scheduler is an asynchronous Planner. Enqueue records the job in memory,
// returns a placeholder batch in "pending" status, and lets workers run
// Provision + CreateBatch in the background.
type Scheduler struct {
	bc   plannerclient.BatchClient
	prov provisioner.Provisioner

	submit chan string // buffered FIFO of pending JobIDs

	baseCtx    context.Context
	baseCancel context.CancelFunc
	wg         sync.WaitGroup // tracks live workers; Close waits on this

	mu         sync.RWMutex          // guards jobs and jobByBatch
	jobs       map[string]*queuedJob // JobID -> state
	jobByBatch map[string]string     // batch.ID -> JobID (for ListJobs tagging)
}

// jobState is the planner-side lifecycle. Before submission, statusFor maps
// it to "pending" or "provisioning". After submission, status comes from
// MDS.
//
//	Pending      → buffered in submit channel, no worker yet.
//	Provisioning → worker holds the job; Provision + CreateBatch in flight.
//	Submitted    → CreateBatch returned; MDS owns the lifecycle from here.
//	Failed       → Provision or CreateBatch errored.
//	Canceled     → user-canceled while Pending or Provisioning. See Cancel
//	               for the MVP cancellation-race gap.
type jobState int

const (
	jobStatePending jobState = iota
	jobStateProvisioning
	jobStateSubmitted
	jobStateFailed
	jobStateCanceled
)

type queuedJob struct {
	req        *plannerapi.EnqueueRequest
	state      jobState
	batchID    string // populated when state == jobStateSubmitted
	err        error  // populated when state == jobStateFailed
	enqueuedAt time.Time
}

// queueCapacity caps the submit channel. When full, Enqueue blocks on the
// caller's context.
const queueCapacity = 256

// DefaultWorkerCount sizes the worker pool.
const DefaultWorkerCount = 4

// NewScheduler constructs an asynchronous Scheduler Planner and starts
// workerCount background workers. workerCount < 1 is floored to 1.
func NewScheduler(bc plannerclient.BatchClient, prov provisioner.Provisioner, workerCount int) *Scheduler {
	if workerCount < 1 {
		workerCount = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Scheduler{
		bc:         bc,
		prov:       prov,
		submit:     make(chan string, queueCapacity),
		baseCtx:    ctx,
		baseCancel: cancel,
		jobs:       make(map[string]*queuedJob),
		jobByBatch: make(map[string]string),
	}
	q.wg.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go q.run()
	}
	klog.Infof("[planner.scheduler] started worker pool size=%d capacity=%d", workerCount, queueCapacity)
	return q
}

var _ plannerapi.Planner = (*Scheduler)(nil)

// Close cancels in-flight work and waits for workers to exit.
func (q *Scheduler) Close() error {
	q.baseCancel()
	q.wg.Wait()
	return nil
}

func (q *Scheduler) run() {
	defer q.wg.Done()
	for {
		select {
		case <-q.baseCtx.Done():
			return
		case jobID := <-q.submit:
			q.process(jobID)
		}
	}
}

func (q *Scheduler) process(jobID string) {
	// Check-and-flip Pending -> Provisioning under the same lock.
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return
	}
	if job.state != jobStatePending {
		// Cancel raced ahead; drop without provisioning.
		state := job.state
		q.mu.Unlock()
		klog.Infof("[planner.scheduler] skip job_id=%q state=%d", jobID, state)
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

	aibrix := plannerclient.AIBrixExtraBody{
		JobID: req.JobID,
		PlannerDecision: &struct {
			ProvisionID               string `json:"provision_id,omitempty"`
			ProvisionResourceDeadline int64  `json:"provision_resource_deadline,omitempty"`
			ResourceDetails           []struct {
				ResourceType    string `json:"resource_type"`
				EndpointCluster string `json:"endpoint_cluster,omitempty"`
				GPUType         string `json:"gpu_type,omitempty"`
				WorkerNum       int    `json:"worker_num,omitempty"`
			} `json:"resource_details,omitempty"`
		}{
			ProvisionID: provResult.ProvisionID,
		},
		ModelTemplate: req.ModelTemplate,
	}

	klog.Infof("[planner.scheduler] submit job_id=%q provision_id=%q model_template=%v",
		req.JobID, provResult.ProvisionID, req.ModelTemplate)

	batch, err := q.bc.CreateBatch(q.baseCtx, req.BatchParams, aibrix)
	if err != nil {
		// Best-effort release; surface the original CreateBatch error.
		if relErr := q.prov.Release(q.baseCtx, provResult.ProvisionID); relErr != nil {
			klog.Warningf("[planner.scheduler] release after CreateBatch failure job_id=%q provision_id=%q: %v",
				jobID, provResult.ProvisionID, relErr)
		}
		q.markFailed(jobID, err)
		return
	}

	// Record batch.ID and mark the job submitted.
	q.mu.Lock()
	if job, ok := q.jobs[jobID]; ok {
		job.state = jobStateSubmitted
		job.batchID = batch.ID
		q.jobByBatch[batch.ID] = jobID
	}
	q.mu.Unlock()
}

func (q *Scheduler) markFailed(jobID string, err error) {
	q.mu.Lock()
	if job, ok := q.jobs[jobID]; ok {
		job.state = jobStateFailed
		job.err = err
	}
	q.mu.Unlock()
	klog.Warningf("[planner.scheduler] job_id=%q failed: %v", jobID, err)
}

// Enqueue records the job, pushes it onto the worker channel, and returns
// a placeholder batch in "pending" status.
func (q *Scheduler) Enqueue(ctx context.Context, req *plannerapi.EnqueueRequest) (*plannerapi.Job, error) {
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

	q.mu.Lock()
	if _, exists := q.jobs[req.JobID]; exists {
		q.mu.Unlock()
		return nil, fmt.Errorf("%w: duplicate job_id %q", plannerapi.ErrInvalidJob, req.JobID)
	}
	q.jobs[req.JobID] = &queuedJob{
		req:        req,
		state:      jobStatePending,
		enqueuedAt: time.Now(),
	}
	q.mu.Unlock()

	select {
	case q.submit <- req.JobID:
	case <-ctx.Done():
		q.deleteJob(req.JobID)
		return nil, ctx.Err()
	case <-q.baseCtx.Done():
		q.deleteJob(req.JobID)
		return nil, fmt.Errorf("planner closed: %w", q.baseCtx.Err())
	}

	klog.Infof("[planner.scheduler] enqueue job_id=%q", req.JobID)
	return &plannerapi.Job{
		JobID: req.JobID,
		Batch: placeholderBatch(req, statusFor(jobStatePending), time.Now()),
	}, nil
}

// GetJob resolves the JobID. Submitted jobs forward to MDS; others return
// a placeholder batch with status derived from jobState.
func (q *Scheduler) GetJob(ctx context.Context, jobID string) (*plannerapi.Job, error) {
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
	q.mu.RUnlock()

	if state == jobStateSubmitted {
		klog.Infof("[planner.scheduler] get_job job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.GetBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	return &plannerapi.Job{
		JobID: jobID,
		Batch: placeholderBatch(req, statusFor(state), enqueuedAt),
	}, nil
}

// Cancel marks a pending/provisioning job canceled, or forwards cancel to
// MDS for a submitted job.
//
// Known gap: a cancel that lands mid-Provision or mid-CreateBatch can still
// lose the race and end up submitted.
func (q *Scheduler) Cancel(ctx context.Context, jobID string) (*plannerapi.Job, error) {
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
	req := job.req
	enqueuedAt := job.enqueuedAt
	if state == jobStatePending || state == jobStateProvisioning {
		job.state = jobStateCanceled
	}
	q.mu.Unlock()

	switch state {
	case jobStatePending, jobStateProvisioning:
		klog.Infof("[planner.scheduler] cancel pre-submit job_id=%q state=%d", jobID, state)
		return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, openai.BatchStatusCancelled, enqueuedAt)}, nil
	case jobStateSubmitted:
		klog.Infof("[planner.scheduler] cancel submitted job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.CancelBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	// Already terminal (failed/canceled) — return current view, no double-cancel side effects.
	return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, statusFor(state), enqueuedAt)}, nil
}

// ListJobs merges MDS batches with local not-yet-submitted jobs. Local jobs
// are shown only on the first page so the MDS cursor remains valid.
func (q *Scheduler) ListJobs(ctx context.Context, req *plannerapi.ListJobsRequest) (*plannerapi.ListJobsResponse, error) {
	listReq := &plannerclient.ListBatchesRequest{}
	if req != nil {
		listReq.Limit = req.Limit
		listReq.After = req.After
	}
	klog.Infof("[planner.scheduler] list_jobs limit=%d after=%q", listReq.Limit, listReq.After)
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
// first.
func (q *Scheduler) unsubmittedJobs() []*plannerapi.Job {
	q.mu.RLock()
	unsubmitted := make([]*queuedJob, 0)
	for _, j := range q.jobs {
		if j.state != jobStateSubmitted {
			unsubmitted = append(unsubmitted, j)
		}
	}
	q.mu.RUnlock()
	sort.Slice(unsubmitted, func(i, k int) bool {
		return unsubmitted[i].enqueuedAt.After(unsubmitted[k].enqueuedAt)
	})
	out := make([]*plannerapi.Job, 0, len(unsubmitted))
	for _, j := range unsubmitted {
		out = append(out, &plannerapi.Job{
			JobID: j.req.JobID,
			Batch: placeholderBatch(j.req, statusFor(j.state), j.enqueuedAt),
		})
	}
	return out
}

func (q *Scheduler) deleteJob(jobID string) {
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

// placeholderBatch builds the batch view for jobs that do not yet have an
// MDS batch.ID.
func placeholderBatch(req *plannerapi.EnqueueRequest, st openai.BatchStatus, enqueuedAt time.Time) *openai.Batch {
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
	switch st {
	case openai.BatchStatusFailed:
		b.FailedAt = time.Now().Unix()
	case openai.BatchStatusCancelled:
		b.CancelledAt = time.Now().Unix()
	}
	return b
}
