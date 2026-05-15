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

// Package impl provides Planner implementations.
//
// Passthrough is a synchronous, non-persistent Planner used to wire the
// Console -> Planner -> RM -> MDS path end-to-end before the durable
// task store and async worker land. Enqueue inlines Provisioner.Provision
// followed by BatchClient.CreateBatch on the calling goroutine; reads
// (GetJob, ListJobs, Cancel) take a JobID and resolve to the MDS batch
// ID via an in-memory JobID -> batch.ID map populated on Enqueue.
//
// The map is restart-lossy and is replaced by the queued planner's
// durable index in a follow-up; crash recovery is out of scope here.
package impl

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Passthrough is a synchronous Planner that calls Provisioner.Provision
// and BatchClient.CreateBatch inline.
type Passthrough struct {
	bc   plannerclient.BatchClient
	prov provisioner.Provisioner

	mu         sync.RWMutex
	batchByJob map[string]string // JobID -> batch.ID (for GetJob / Cancel)
	jobByBatch map[string]string // batch.ID -> JobID (for ListJobs tagging)
}

// NewPassthrough constructs a Passthrough Planner. Both bc and prov are required.
func NewPassthrough(bc plannerclient.BatchClient, prov provisioner.Provisioner) *Passthrough {
	return &Passthrough{
		bc:         bc,
		prov:       prov,
		batchByJob: make(map[string]string),
		jobByBatch: make(map[string]string),
	}
}

var _ plannerapi.Planner = (*Passthrough)(nil)

func (p *Passthrough) Enqueue(ctx context.Context, req *plannerapi.EnqueueRequest) (*plannerapi.Job, error) {
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
	if p.prov == nil {
		return nil, fmt.Errorf("%w: missing provisioner", plannerapi.ErrInsufficientResources)
	}

	provReq := &rmtypes.ResourceProvision{
		Spec: rmtypes.ResourceProvisionSpec{
			Credential: rmtypes.ResourceCredential{Provider: p.prov.Type()},
		},
		IdempotencyKey: req.JobID,
	}
	provResult, err := p.prov.Provision(ctx, provReq)
	if err != nil {
		return nil, errors.Join(plannerapi.ErrInsufficientResources, err)
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
			// ResourceDetails left empty: the resource manager doesn't
			// return allocation details in kubernetes modes, and MDS doesn't read
			// them on this path yet. This part will be extended later with more
			// resource types.
		},
		ModelTemplate: req.ModelTemplate,
	}

	klog.Infof("[planner.passthrough] enqueue job_id=%q provision_id=%q model_template=%v",
		req.JobID, provResult.ProvisionID, req.ModelTemplate)

	batch, err := p.bc.CreateBatch(ctx, req.BatchParams, aibrix)
	if err != nil {
		return nil, err
	}

	p.remember(req.JobID, batch.ID)
	return &plannerapi.Job{JobID: req.JobID, Batch: batch}, nil
}

// GetJob resolves the JobID to its MDS batch.ID via the in-memory map
// and forwards to the MDS batch endpoint. Returns ErrJobNotFound when
// the JobID is unknown to this process (typical for jobs created
// before the BFF restarted, until ListJobs warms the cache).
func (p *Passthrough) GetJob(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	batchID, ok := p.lookup(jobID)
	if !ok {
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	klog.Infof("[planner.passthrough] get_job job_id=%q batch_id=%q", jobID, batchID)
	batch, err := p.bc.GetBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
}

// Cancel resolves JobID to batch.ID and forwards to MDS.
func (p *Passthrough) Cancel(ctx context.Context, jobID string) (*plannerapi.Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	batchID, ok := p.lookup(jobID)
	if !ok {
		return nil, fmt.Errorf("%w: job_id %q", plannerapi.ErrJobNotFound, jobID)
	}
	klog.Infof("[planner.passthrough] cancel job_id=%q batch_id=%q", jobID, batchID)
	batch, err := p.bc.CancelBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	return &plannerapi.Job{JobID: jobID, Batch: batch}, nil
}

// ListJobs walks MDS batches and tags each one with the JobID from the
// in-memory reverse map. Batches not enqueued in this process incarnation
// surface with empty JobID (crash recovery is out of scope).
func (p *Passthrough) ListJobs(ctx context.Context, req *plannerapi.ListJobsRequest) (*plannerapi.ListJobsResponse, error) {
	listReq := &plannerclient.ListBatchesRequest{}
	if req != nil {
		listReq.Limit = req.Limit
		listReq.After = req.After
	}
	klog.Infof("[planner.passthrough] list_jobs limit=%d after=%q", listReq.Limit, listReq.After)
	resp, err := p.bc.ListBatches(ctx, listReq)
	if err != nil {
		return nil, err
	}
	out := make([]*plannerapi.Job, 0, len(resp.Data))
	p.mu.RLock()
	for _, b := range resp.Data {
		out = append(out, &plannerapi.Job{JobID: p.jobByBatch[b.ID], Batch: b})
	}
	p.mu.RUnlock()
	return &plannerapi.ListJobsResponse{Data: out, HasMore: resp.HasMore}, nil
}

func (p *Passthrough) lookup(jobID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.batchByJob[jobID]
	return id, ok
}

func (p *Passthrough) remember(jobID, batchID string) {
	p.mu.Lock()
	p.batchByJob[jobID] = batchID
	p.jobByBatch[batchID] = jobID
	p.mu.Unlock()
}
