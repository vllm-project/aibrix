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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"github.com/openai/openai-go/v3"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

func isBatchRunning(s plannerapi.JobStatus) bool {
	switch s {
	case plannerapi.JobStatusSubmitting, plannerapi.JobStatusValidating, plannerapi.JobStatusInProgress, plannerapi.JobStatusFinalizing:
		return true
	}
	return false
}

func updateStatusUnsafe(job *queuedJob, newStatus plannerapi.JobStatus) {
	if job.status == newStatus {
		return
	}

	job.status = newStatus
	now := time.Now().UTC()
	switch newStatus {
	case plannerapi.JobStatusQueued:
		job.queuedAt = now
	case plannerapi.JobStatusPlanned:
		job.plannedAt = now
	case plannerapi.JobStatusResourcePreparing:
		job.resourcePreparingAt = now
	case plannerapi.JobStatusSubmitting:
		job.submittingAt = now
	case plannerapi.JobStatusResourceFailed:
		job.resourceFailedAt = now
	case plannerapi.JobStatusSubmitFailed:
		job.submitFailedAt = now
	case plannerapi.JobStatusCancelled:
		job.canceledAt = now
	case plannerapi.JobStatusExpired:
		job.expiredAt = now
	case plannerapi.JobStatusCompleted:
		job.completedAt = now
	}
}

func handleCleanup(p *Planner, job *queuedJob, sourceStatus, targetStatus plannerapi.JobStatus) {
	job.mu.RLock()
	if job.status.IsTerminal() || job.status != sourceStatus {
		job.mu.RUnlock()
		return
	}

	jobID := job.req.JobID
	batchID := job.batchID
	provisionID := job.provisionID
	job.mu.RUnlock()

	if provisionID != "" {
		p.releaseAfter(jobID, provisionID, "provision cancel")
	}

	var batch *openai.Batch
	if batchID != "" && targetStatus != plannerapi.JobStatusExpired && targetStatus != plannerapi.JobStatusCompleted {
		klog.Infof("[planner] cancel submitted job_id=%q batch_id=%q", jobID, batchID)
		var err error
		if batch, err = p.bc.CancelBatch(p.baseCtx, batchID); err != nil {
			klog.Warningf("[planner] CancelBatch failed for job_id=%q: %v", jobID, err)
		}
	}

	job.mu.Lock()
	if !job.status.IsTerminal() {
		job.provisionID = ""
		job.batchID = ""
		job.batch = batch
		updateStatusUnsafe(job, targetStatus)
	}
	job.mu.Unlock()
	p.persist(job)
}

func handleProvisioning(p *Planner, job *queuedJob) {
	job.mu.Lock()
	jobID := job.req.JobID
	klog.Infof("[planner] executeProvisioning job_id=%q", jobID)
	status := job.status
	// Only process jobs that are Queued or Planned
	if status != plannerapi.JobStatusQueued && status != plannerapi.JobStatusPlanned {
		job.mu.Unlock()
		return
	}
	spec := job.scheduledResource
	if spec == nil {
		job.mu.Unlock()
		klog.Warningf("[planner] job_id=%q has no scheduled resource", jobID)
		return
	}

	// Set status to Provisioning if not already
	if job.status != plannerapi.JobStatusPlanned {
		job.status = plannerapi.JobStatusPlanned
		job.plannedAt = time.Now().UTC()
	}
	job.mu.Unlock()

	provReq := &rmtypes.ResourceProvision{
		Spec:           *spec,
		IdempotencyKey: jobID,
	}

	provResult, err := p.prov.Provision(p.baseCtx, provReq)
	if err != nil {
		klog.Warningf("[planner] Provision failed for job_id=%q: %v", jobID, err)
		p.markFailed(job, plannerapi.JobStatusResourceFailed, errors.Join(plannerapi.ErrInsufficientResources, err))
		return
	}

	if logger, ok := p.backend.(provisionResponseLogger); ok {
		logger.LogProvisionResponse(jobID, provResult, *spec)
	}

	job.mu.Lock()
	if job.provisionID != "" || job.status.IsTerminal() {
		job.mu.Unlock()
		if err := p.prov.Release(p.baseCtx, provResult.ProvisionID); err != nil {
			klog.Warningf("[planner] Cancel provision failed for provision_id=%q: %v", provResult.ProvisionID, err)
		} else {
			klog.Warningf("[planner] Cancel unused provision provision_id=%q", provResult.ProvisionID)
		}
		return
	}

	job.provisionID = provResult.ProvisionID
	job.resourcePreparingAt = time.Now().UTC()
	if job.status == plannerapi.JobStatusCancelling {
		job.mu.Unlock()
		handleCleanup(p, job, plannerapi.JobStatusCancelling, plannerapi.JobStatusCancelled)
		return
	}

	job.status = plannerapi.JobStatusResourcePreparing
	// Job is now in running queue
	job.queue = p.runningQueue
	job.mu.Unlock()
	p.persist(job)
	// It's ok if the job's status has changed in between, the processing logic
	// of running queue will handle it
	p.pendingQueue.Remove(jobID)
	// RunningQueue is a fifo queue, using 0 as priority
	p.runningQueue.Push(job, 0)
}

// handleResourcePreparing queries provision status and marks job as readyToSubmit.
// This is executed by worker pool (query-only operation).
func handleResourcePreparing(p *Planner, job *queuedJob) {
	job.mu.RLock()
	jobID := job.req.JobID
	status := job.status
	provisionID := job.provisionID
	if status != plannerapi.JobStatusResourcePreparing {
		job.mu.RUnlock()
		return
	}
	if provisionID == "" {
		job.mu.RUnlock()
		p.markFailed(job, plannerapi.JobStatusResourceFailed, fmt.Errorf("job %q has no provision ID", jobID))
		return
	}
	job.mu.RUnlock()

	// Query provision status
	filter := &rmtypes.ListOptions{ProvisionIDs: &[]string{provisionID}}
	results, err := p.prov.List(p.baseCtx, filter)
	if err != nil {
		klog.Warningf("[planner] list provision failed job_id=%q provision_id=%q: %v", jobID, provisionID, err)
		return
	}

	if len(results) == 0 {
		p.markFailed(job, plannerapi.JobStatusResourceFailed, fmt.Errorf("provision %q not found", provisionID))
		return
	}

	provResult := results[0]
	switch provResult.Status {
	case rmtypes.ProvisionStatusRunning:
		// Provision is ready, mark job as ready to submit
		// The planningLoop will submit to MDS in next iteration
		job.mu.Lock()
		job.allocatedResource = provResult
		if !job.status.IsTerminal() && job.status == plannerapi.JobStatusResourcePreparing {
			job.readyToSubmit = true
			klog.Infof("[planner] job_id=%q provision ready, marked readyToSubmit", jobID)
		}
		job.mu.Unlock()

	case rmtypes.ProvisionStatusFailed, rmtypes.ProvisionStatusReleasing, rmtypes.ProvisionStatusReleased, rmtypes.ProvisionStatusReleaseFailed:
		p.markFailed(job, plannerapi.JobStatusResourceFailed, fmt.Errorf("provision failed: %s", provResult.ErrorMessage))

	default:
		// Still pending, wait for next iteration
	}
}

// submitToMDS submits batch to MDS
func submitToMDS(p *Planner, job *queuedJob) {
	job.mu.RLock()
	jobID := job.req.JobID
	status := job.status
	spec := job.scheduledResource
	alloc := job.allocatedResource
	req := job.req
	readyToSubmit := job.readyToSubmit
	if status != plannerapi.JobStatusResourcePreparing {
		job.mu.RUnlock()
		return
	}
	if !readyToSubmit {
		job.mu.RUnlock()
		return // Not ready yet, will be checked in next iteration
	}
	if spec == nil {
		job.mu.RUnlock()
		p.markFailed(job, plannerapi.JobStatusResourceFailed, fmt.Errorf("job %q has no scheduled resource", jobID))
		return
	}
	if alloc == nil {
		job.mu.RUnlock()
		p.markFailed(job, plannerapi.JobStatusResourceFailed, fmt.Errorf("job %q has no allocated resource", jobID))
		return
	}
	job.mu.RUnlock()

	runtime, err := p.backend.BuildRuntime(req, alloc)
	if err != nil {
		klog.Warningf("[planner] BuildRuntime failed job_id=%q: %v", jobID, err)
		p.markFailed(job, plannerapi.JobStatusResourceFailed, err)
		return
	}

	aibrix := plannerclient.AIBrixExtraBody{
		JobID:              jobID,
		Runtime:            runtime,
		ResourceAllocation: p.backend.BuildResourceAllocation(*spec, alloc),
		ModelTemplate:      req.ModelTemplate,
		Model:              req.Model,
	}

	if batchParamsJson, err := json.Marshal(req.BatchParams); err == nil {
		klog.Infof("[planner] BatchParams: %s", batchParamsJson)
	}

	if aibrixBodyJson, err := json.Marshal(aibrix); err == nil {
		klog.Infof("[planner] AIBrixExtraBody: %s", aibrixBodyJson)
	}

	batch, err := p.bc.CreateBatch(p.baseCtx, req.BatchParams, aibrix)
	if err != nil {
		klog.Warningf("[planner] CreateBatch failed job_id=%q: %v", jobID, err)
		p.markFailed(job, plannerapi.JobStatusSubmitFailed, err)
		return
	}

	klog.Infof("[planner] CreateBatch called job_id=%q batch_id=%q", jobID, batch.ID)

	job.mu.Lock()
	if job.status.IsTerminal() {
		job.mu.Unlock()
		return
	}

	job.batchID = batch.ID
	job.batch = batch
	job.submittingAt = time.Now().UTC()
	job.readyToSubmit = false // Clear flag after submission
	if job.status == plannerapi.JobStatusCancelling {
		job.mu.Unlock()
		handleCleanup(p, job, plannerapi.JobStatusCancelling, plannerapi.JobStatusCancelled)
		return
	}

	job.status = plannerapi.JobStatusSubmitting
	job.mu.Unlock()

	p.persist(job)
}

func handleRunning(p *Planner, job *queuedJob) {
	job.mu.RLock()
	jobID := job.req.JobID
	status := job.status
	batchID := job.batchID
	if !isBatchRunning(status) {
		job.mu.RUnlock()
		return
	}
	if batchID == "" {
		job.mu.RUnlock()
		p.markFailed(job, plannerapi.JobStatusSubmitFailed, fmt.Errorf("job %q has no batch ID", jobID))
		return
	}
	job.mu.RUnlock()

	batch, err := p.bc.GetBatch(p.baseCtx, batchID)
	if err != nil {
		klog.Warningf("[planner] GetBatch failed job_id=%q batch_id=%q: %v", jobID, batchID, err)
		// Don't mark failed, let it be polled again in next iteration
		return
	}

	newStatus := plannerapi.JobStatus(batch.Status)

	job.mu.Lock()
	if job.status.IsTerminal() || job.status == plannerapi.JobStatusCancelling {
		job.mu.Unlock()
		return
	}
	updateStatusUnsafe(job, newStatus)
	job.batch = batch // Update in-memory batch with latest from MDS
	rec := jobToModel(job)
	job.mu.Unlock()

	mergeBatchIntoModel(rec, batch)
	if p.store != nil {
		if err := p.store.UpsertJob(p.baseCtx, rec); err != nil {
			klog.Warningf("[planner] sync persist job_id=%q: %v", jobID, err)
		}
	}

	if newStatus.IsTerminal() {
		handleCleanup(p, job, status, newStatus)
	}
}
