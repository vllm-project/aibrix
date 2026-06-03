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
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"gorm.io/datatypes"
	"k8s.io/klog/v2"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
	"github.com/vllm-project/aibrix/apps/console/api/utils"
)

// Planner is an asynchronous implementation of plannerapi.Planner.
// Enqueue records the job in memory, returns a placeholder batch in
// "pending" status, and lets workers run Provision, wait for the
// resource to reach Running, then CreateBatch.
type Planner struct {
	bc    plannerclient.BatchClient
	prov  provisioner.Provisioner
	store store.Store

	backend plannerBackend // per-provisioner decision-making

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

// queuedJob is the Planner's in-memory snapshot of a Job.
type queuedJob struct {
	req                 *plannerapi.EnqueueRequest
	status              plannerapi.JobStatus
	provisionID         string
	batchID             string
	errMsg              string // populated when status is resource_failed / submit_failed
	queuedAt            time.Time
	resourcePreparingAt time.Time
	submittingAt        time.Time
	resourceFailedAt    time.Time
	submitFailedAt      time.Time
	cancelRequestedAt   time.Time
	canceledAt          time.Time
}

// terminalTime returns the timestamp at which the job transitioned into a
// terminal pre-submit state (resource_failed, submit_failed, cancelled)
func terminalTime(j *queuedJob) time.Time {
	switch j.status {
	case plannerapi.JobStatusResourceFailed:
		return j.resourceFailedAt
	case plannerapi.JobStatusSubmitFailed:
		return j.submitFailedAt
	case plannerapi.JobStatusCancelled:
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
// workers. workerCount < 1 is floored to 1. A nil store disables
// persistence (used by tests).
func NewPlanner(bc plannerclient.BatchClient, prov provisioner.Provisioner, st store.Store, workerCount int) *Planner {
	if workerCount < 1 {
		workerCount = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Planner{
		bc:               bc,
		prov:             prov,
		store:            st,
		backend:          newPlannerBackend(prov),
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
	// Atomic check-and-flip Queued → ResourcePreparing.
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return
	}
	// Example: a queued job is canceled before a worker picks it up, so the
	// worker later observes a non-queued status here and skips provisioning.
	if job.status != plannerapi.JobStatusQueued {
		status := job.status
		q.mu.Unlock()
		klog.Infof("[planner] invalid status before provisioning job_id=%q status=%s", jobID, status)
		return
	}
	job.status = plannerapi.JobStatusResourcePreparing
	job.resourcePreparingAt = time.Now()
	req := job.req
	q.mu.Unlock()
	q.persist(jobID)

	// Backend.Schedule decides what to provision (RM-spec shaping today;
	// future scheduling decisions live here too).
	spec, gpuType, gpusPerReplica, err := q.backend.Schedule(q.baseCtx, req)
	if err != nil {
		q.markFailed(jobID, plannerapi.JobStatusResourceFailed, errors.Join(plannerapi.ErrInvalidJob, err))
		return
	}

	provReq := &rmtypes.ResourceProvision{
		Spec:           spec,
		IdempotencyKey: req.JobID,
	}
	provResult, err := q.prov.Provision(q.baseCtx, provReq)
	if err != nil {
		q.markFailed(jobID, plannerapi.JobStatusResourceFailed, errors.Join(plannerapi.ErrInsufficientResources, err))
		return
	}
	q.mu.Lock()
	q.jobs[jobID].provisionID = provResult.ProvisionID
	q.mu.Unlock()
	if logger, ok := q.backend.(provisionResponseLogger); ok {
		logger.LogProvisionResponse(req.JobID, provResult, spec)
	}

	// Provision returns when the request is accepted, not when the resource
	// is ready. Wait for Running before submitting to MDS, which rejects
	// batches that point to not-yet-ready provisions.
	readyResult, err := q.waitForProvisionReady(provResult.ProvisionID)
	if err != nil {
		q.releaseAfter(jobID, provResult.ProvisionID, "wait failure")
		q.markFailed(jobID, plannerapi.JobStatusResourceFailed, errors.Join(plannerapi.ErrInsufficientResources, err))
		return
	}
	klog.Infof("[planner] provision ready job_id=%q provision_id=%q provider=%q",
		jobID, provResult.ProvisionID, q.prov.Type())

	q.mu.Lock()
	if job.status == plannerapi.JobStatusResourcePreparing {
		job.status = plannerapi.JobStatusSubmitting
		job.submittingAt = time.Now()
	}
	q.mu.Unlock()
	q.persist(jobID)

	aibrix := plannerclient.AIBrixExtraBody{
		JobID:              req.JobID,
		Runtime:            plannerclient.RuntimeForProvisionType(q.prov.Type()),
		ResourceAllocation: q.backend.BuildResourceAllocation(spec, readyResult, gpuType, gpusPerReplica),
		ModelTemplate:      req.ModelTemplate,
	}

	if mt := req.ModelTemplate; mt != nil {
		klog.Infof("[planner] submit job_id=%q provision_id=%q model_template=%s@%s spec=%s",
			req.JobID, provResult.ProvisionID, mt.Name, mt.Version, mt.Spec)
	} else {
		klog.Infof("[planner] submit job_id=%q provision_id=%q model_template=<none>",
			req.JobID, provResult.ProvisionID)
	}

	batch, err := q.bc.CreateBatch(q.baseCtx, req.BatchParams, aibrix)
	if err != nil {
		q.releaseAfter(jobID, provResult.ProvisionID, "CreateBatch failure")
		q.markFailed(jobID, plannerapi.JobStatusSubmitFailed, err)
		return
	}

	// Cancel may have raced in during Provision or CreateBatch. Record the
	// batch.ID either way so ListJobs can tag it; only mirror MDS status if
	// no cancel landed. On race, forward CancelBatch to MDS and release the
	// provisioned resource.
	canceled := false
	q.mu.Lock()
	if job, ok := q.jobs[jobID]; ok {
		job.batchID = batch.ID
		q.jobByBatch[batch.ID] = jobID
		if job.status == plannerapi.JobStatusCancelled {
			canceled = true
		} else {
			job.status = plannerapi.JobStatus(batch.Status)
		}
	}
	q.mu.Unlock()
	q.persist(jobID)

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
func (q *Planner) waitForProvisionReady(provisionID string) (*rmtypes.ProvisionResult, error) {
	filter := &rmtypes.ListOptions{ProvisionIDs: &[]string{provisionID}}
	deadline := time.Now().Add(provReadyTimeout)
	for {
		results, err := q.prov.List(q.baseCtx, filter)
		switch {
		case err != nil:
			klog.Warningf("[planner] poll provision_id=%q: %v", provisionID, err)
		case len(results) == 0:
			return nil, fmt.Errorf("provision %q not found", provisionID)
		default:
			switch results[0].Status {
			case rmtypes.ProvisionStatusRunning:
				return results[0], nil
			case rmtypes.ProvisionStatusFailed:
				return nil, fmt.Errorf("provision failed: %s", results[0].ErrorMessage)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("provision %q did not reach Running within %v", provisionID, provReadyTimeout)
		}
		select {
		case <-q.baseCtx.Done():
			return nil, q.baseCtx.Err()
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

func (q *Planner) markFailed(jobID string, status plannerapi.JobStatus, err error) {
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return
	}
	job.status = status
	job.errMsg = err.Error()
	now := time.Now()
	switch status {
	case plannerapi.JobStatusResourceFailed:
		job.resourceFailedAt = now
	case plannerapi.JobStatusSubmitFailed:
		job.submitFailedAt = now
	}
	q.mu.Unlock()
	q.persist(jobID)
	klog.Warningf("[planner] job_id=%q status=%s: %v", jobID, status, err)
}

// syncFromBatch reconciles the in-memory queuedJob and persisted row with
// the freshest MDS batch view fetched by lazy sync. Always persists (MDS
// counters/usage may have moved even when status hasn't); fires async
// Release when the new JobStatus transitions to terminal.
func (q *Planner) syncFromBatch(jobID string, batch *openai.Batch) {
	if batch == nil {
		return
	}
	newStatus := plannerapi.JobStatus(batch.Status)
	q.mu.Lock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.Unlock()
		return
	}
	statusChanged := job.status != newStatus
	if statusChanged {
		job.status = newStatus
	}
	provisionID := job.provisionID
	rec := jobToModel(job)
	q.mu.Unlock()

	mergeBatchIntoModel(rec, batch)
	if q.store != nil {
		if err := q.store.UpsertJob(q.baseCtx, rec); err != nil {
			klog.Warningf("[planner] sync persist job_id=%q: %v", jobID, err)
		}
	}
	if statusChanged && newStatus.IsTerminal() && provisionID != "" {
		go q.releaseAfter(jobID, provisionID, "post-submit terminal")
	}
}

// mergeBatchIntoModel overlays MDS-owned batch fields onto rec.
func mergeBatchIntoModel(rec *models.Job, b *openai.Batch) {
	rec.BatchCreatedAt = utils.UnixToTimePtr(b.CreatedAt)
	rec.OutputDataset = b.OutputFileID
	rec.ErrorDataset = b.ErrorFileID
	rec.InProgressAt = utils.UnixToTimePtr(b.InProgressAt)
	rec.ExpiresAt = utils.UnixToTimePtr(b.ExpiresAt)
	rec.FinalizingAt = utils.UnixToTimePtr(b.FinalizingAt)
	rec.CompletedAt = utils.UnixToTimePtr(b.CompletedAt)
	rec.FailedAt = utils.UnixToTimePtr(b.FailedAt)
	rec.ExpiredAt = utils.UnixToTimePtr(b.ExpiredAt)
	rec.CancellingAt = utils.UnixToTimePtr(b.CancellingAt)
	rec.CancelledAt = utils.UnixToTimePtr(b.CancelledAt)
	if b.JSON.RequestCounts.Valid() {
		if data, err := json.Marshal(b.RequestCounts); err == nil {
			rec.RequestCounts = datatypes.JSON(data)
		}
	}
	if b.JSON.Usage.Valid() {
		if data, err := json.Marshal(b.Usage); err == nil {
			rec.Usage = datatypes.JSON(data)
		}
	}
	if b.JSON.Errors.Valid() {
		if data, err := json.Marshal(b.Errors); err == nil {
			rec.ErrorMessage = string(data)
		}
	}
}

// persist writes the in-memory queuedJob snapshot for jobID to the store.
func (q *Planner) persist(jobID string) {
	if q.store == nil {
		return
	}
	q.mu.RLock()
	job, ok := q.jobs[jobID]
	if !ok {
		q.mu.RUnlock()
		return
	}
	rec := jobToModel(job)
	q.mu.RUnlock()
	if err := q.store.UpsertJob(q.baseCtx, rec); err != nil {
		klog.Warningf("[planner] persist job_id=%q: %v", jobID, err)
	}
}

// Recover replays non-terminal jobs from the store into the Planner's
// in-memory state. Must be called once at startup, after NewPlanner and
// before the gRPC server begins accepting requests. Safe to call with a
// nil store (no-op).
func (q *Planner) Recover(ctx context.Context) error {
	if q.store == nil {
		return nil
	}
	rows, err := q.store.ListNonTerminalJobs(ctx)
	if err != nil {
		return fmt.Errorf("list non-terminal jobs: %w", err)
	}
	// Do the heavy JSON unmarshalling outside the lock.
	recovered := make([]*queuedJob, 0, len(rows))
	for _, rec := range rows {
		recovered = append(recovered, modelToJob(rec))
	}
	var reEnqueue []string
	q.mu.Lock()
	for _, job := range recovered {
		q.jobs[job.req.JobID] = job
		if job.batchID != "" {
			q.jobByBatch[job.batchID] = job.req.JobID
		}
		if isPreSubmitStatus(job.status) {
			job.status = plannerapi.JobStatusQueued
			reEnqueue = append(reEnqueue, job.req.JobID)
		}
	}
	q.mu.Unlock()
	for _, id := range reEnqueue {
		if err := q.queue.Push(ctx, id); err != nil {
			klog.Warningf("[planner] recovery re-enqueue job_id=%q: %v", id, err)
		}
	}
	klog.Infof("[planner] recovered %d non-terminal jobs (%d re-enqueued)", len(rows), len(reEnqueue))
	return nil
}

func isPreSubmitStatus(s plannerapi.JobStatus) bool {
	switch s {
	case plannerapi.JobStatusQueued,
		plannerapi.JobStatusResourcePreparing,
		plannerapi.JobStatusSubmitting:
		return true
	}
	return false
}

// modelToJob is the inverse of jobToModel: hydrates a queuedJob from a persisted row.
func modelToJob(rec *models.Job) *queuedJob {
	req := &plannerapi.EnqueueRequest{
		JobID: rec.ID,
		Model: rec.Model,
		BatchParams: openai.BatchNewParams{
			InputFileID:      rec.InputDataset,
			Endpoint:         openai.BatchNewParamsEndpoint(rec.Endpoint),
			CompletionWindow: openai.BatchNewParamsCompletionWindow(rec.CompletionWindow),
		},
	}
	if rec.ModelTemplateName != "" {
		req.ModelTemplate = &plannerapi.ModelTemplateRef{
			Name:    rec.ModelTemplateName,
			Version: rec.ModelTemplateVersion,
		}
	}
	if len(rec.Metadata) > 0 {
		var m map[string]string
		if err := json.Unmarshal(rec.Metadata, &m); err == nil {
			req.BatchParams.Metadata = m
		}
	}
	return &queuedJob{
		req:                 req,
		status:              plannerapi.JobStatus(rec.Status),
		provisionID:         rec.ProvisionID,
		batchID:             rec.BatchID,
		errMsg:              rec.ErrorMessage,
		queuedAt:            utils.TimeOrZero(rec.QueuedAt),
		resourcePreparingAt: utils.TimeOrZero(rec.ResourcePreparingAt),
		submittingAt:        utils.TimeOrZero(rec.SubmittingAt),
		resourceFailedAt:    utils.TimeOrZero(rec.ResourceFailedAt),
		submitFailedAt:      utils.TimeOrZero(rec.SubmitFailedAt),
		cancelRequestedAt:   utils.TimeOrZero(rec.CancelRequestedAt),
		canceledAt:          utils.TimeOrZero(rec.CancelledAt),
	}
}

// jobToModel projects a queuedJob into the storage row.
func jobToModel(j *queuedJob) *models.Job {
	rec := &models.Job{
		ID:                  j.req.JobID,
		Status:              string(j.status),
		BatchID:             j.batchID,
		ProvisionID:         j.provisionID,
		Model:               j.req.Model,
		Endpoint:            string(j.req.BatchParams.Endpoint),
		InputDataset:        j.req.BatchParams.InputFileID,
		CompletionWindow:    string(j.req.BatchParams.CompletionWindow),
		QueuedAt:            utils.TimeToPtr(j.queuedAt),
		ResourcePreparingAt: utils.TimeToPtr(j.resourcePreparingAt),
		SubmittingAt:        utils.TimeToPtr(j.submittingAt),
		ResourceFailedAt:    utils.TimeToPtr(j.resourceFailedAt),
		SubmitFailedAt:      utils.TimeToPtr(j.submitFailedAt),
		CancelRequestedAt:   utils.TimeToPtr(j.cancelRequestedAt),
		CancelledAt:         utils.TimeToPtr(j.canceledAt),
		ErrorMessage:        j.errMsg,
	}
	if j.req.ModelTemplate != nil {
		rec.ModelTemplateName = j.req.ModelTemplate.Name
		rec.ModelTemplateVersion = j.req.ModelTemplate.Version
	}
	if md := j.req.BatchParams.Metadata; len(md) > 0 {
		if b, err := json.Marshal(md); err == nil {
			rec.Metadata = datatypes.JSON(b)
		}
		// Handler-packed keys under aibrix.console.* (legacy: bare "display_name").
		if v := md["aibrix.console.display_name"]; v != "" {
			rec.Name = v
		} else if v := md["display_name"]; v != "" {
			rec.Name = v
		}
		rec.CreatedBy = md["aibrix.console.created_by"]
	}
	return rec
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
	// Backend may impose additional pre-flight checks. Default is a no-op.
	if err := q.backend.ValidateRequest(req); err != nil {
		return nil, err
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
		req:      req,
		status:   plannerapi.JobStatusQueued,
		queuedAt: now,
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
	q.persist(req.JobID)

	klog.Infof("[planner] enqueue job_id=%q", req.JobID)
	return &plannerapi.Job{
		JobID: req.JobID,
		Batch: placeholderBatch(req, statusFor(plannerapi.JobStatusQueued), now, time.Time{}),
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
	status := job.status
	batchID := job.batchID
	req := job.req
	queuedAt := job.queuedAt
	terminalAt := terminalTime(job)
	q.mu.RUnlock()

	// Forward to MDS only when an MDS batch exists AND the local status
	// hasn't already settled to a Planner-side terminal
	if batchID != "" && !status.IsTerminal() {
		klog.Infof("[planner] get_job job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.GetBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		q.syncFromBatch(jobID, batch)
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	return &plannerapi.Job{
		JobID: jobID,
		Batch: placeholderBatch(req, statusFor(status), queuedAt, terminalAt),
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
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	return &plannerapi.Job{
		JobID: jobID,
		Batch: placeholderBatch(j.req, statusFor(j.status), j.queuedAt, terminalTime(j)),
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
	status := job.status
	batchID := job.batchID
	req := job.req
	queuedAt := job.queuedAt
	now := time.Now()
	job.cancelRequestedAt = now
	preSubmit := batchID == "" && !status.IsTerminal()
	var terminalAt time.Time
	if preSubmit {
		job.status = plannerapi.JobStatusCancelled
		job.canceledAt = now
		terminalAt = now
	} else {
		terminalAt = terminalTime(job)
	}
	q.mu.Unlock()

	if preSubmit {
		q.persist(jobID)
		klog.Infof("[planner] cancel pre-submit job_id=%q prior_status=%s", jobID, status)
		return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, openai.BatchStatusCancelled, queuedAt, terminalAt)}, nil
	}
	if batchID != "" && !status.IsTerminal() {
		klog.Infof("[planner] cancel submitted job_id=%q batch_id=%q", jobID, batchID)
		batch, err := q.bc.CancelBatch(ctx, batchID)
		if err != nil {
			return nil, err
		}
		var toRelease string
		q.mu.Lock()
		if job, ok := q.jobs[jobID]; ok {
			toRelease = job.provisionID
			job.status = plannerapi.JobStatusCancelled
			job.canceledAt = now
			job.provisionID = ""
		}
		q.mu.Unlock()

		if toRelease != "" {
			q.releaseAfter(jobID, toRelease, "cancel submitted")
		}
		q.syncFromBatch(jobID, batch)
		return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
	}
	// Already terminal — return current view, no double-cancel side effects.
	return &plannerapi.Job{JobID: jobID, Batch: placeholderBatch(req, statusFor(status), queuedAt, terminalAt)}, nil
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
	tagged := make([]struct {
		jobID string
		batch *openai.Batch
	}, 0, len(resp.Data))
	var missingBatchIDs []string
	missingEntries := make(map[string]*plannerapi.Job)
	for _, b := range resp.Data {
		jobID := q.jobByBatch[b.ID]
		entry := &plannerapi.Job{JobID: jobID, Batch: b}
		out = append(out, entry)
		if jobID != "" {
			tagged = append(tagged, struct {
				jobID string
				batch *openai.Batch
			}{jobID, b})
		} else if b.ID != "" {
			missingBatchIDs = append(missingBatchIDs, b.ID)
			missingEntries[b.ID] = entry
		}
	}
	q.mu.RUnlock()

	// Recover JobIDs for batches not in the in-memory map (terminal/evicted
	// jobs after a Planner restart) from the durable store.
	if q.store != nil && len(missingBatchIDs) > 0 {
		recs, err := q.store.ListJobsByBatchIDs(ctx, missingBatchIDs)
		if err != nil {
			klog.Warningf("[planner] list jobs by batch ids: %v", err)
		} else {
			for batchID, entry := range missingEntries {
				if rec, ok := recs[batchID]; ok && rec.ID != "" {
					entry.JobID = rec.ID
				}
			}
		}
	}

	for _, t := range tagged {
		q.syncFromBatch(t.jobID, t.batch)
	}
	return &plannerapi.ListJobsResponse{Data: out, HasMore: resp.HasMore}, nil
}

// unsubmittedJobs returns the planner-tracked jobs that have no MDS batch yet
// TODO: scans the full q.jobs map; terminal jobs are never evicted so this
// degrades over process lifetime. Address when Phase 2 Reconciler lands by
// either evicting terminal entries or maintaining a separate active map.
func (q *Planner) unsubmittedJobs() []*plannerapi.Job {
	type snap struct {
		req        *plannerapi.EnqueueRequest
		status     plannerapi.JobStatus
		queuedAt   time.Time
		terminalAt time.Time
	}
	q.mu.RLock()
	unsubmitted := make([]snap, 0)
	for _, job := range q.jobs {
		if job.batchID == "" {
			unsubmitted = append(unsubmitted, snap{
				req:        job.req,
				status:     job.status,
				queuedAt:   job.queuedAt,
				terminalAt: terminalTime(job),
			})
		}
	}
	q.mu.RUnlock()
	sort.Slice(unsubmitted, func(i, k int) bool {
		return unsubmitted[i].queuedAt.After(unsubmitted[k].queuedAt)
	})
	out := make([]*plannerapi.Job, 0, len(unsubmitted))
	for _, job := range unsubmitted {
		out = append(out, &plannerapi.Job{
			JobID: job.req.JobID,
			Batch: placeholderBatch(job.req, statusFor(job.status), job.queuedAt, job.terminalAt),
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

// statusFor maps a JobStatus to the BatchStatus surfaced on placeholder batches.
func statusFor(s plannerapi.JobStatus) openai.BatchStatus {
	switch s {
	case plannerapi.JobStatusResourceFailed, plannerapi.JobStatusSubmitFailed:
		return openai.BatchStatusFailed
	case plannerapi.JobStatusCancelled:
		return openai.BatchStatusCancelled
	}
	return openai.BatchStatus(string(s))
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
