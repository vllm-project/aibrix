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
// forward straight to BatchClient.
//
// EnqueueRequest.JobID / EnqueueResult.JobID are part of the Planner
// contract for the queued planner that follows; passthrough echoes the
// field through but does not maintain its own JobID <-> BatchID map.
// pb.Job.Id today is MDS batch.ID — Console-facing identity reverts to
// MDS namespace until the queued planner provides durable JobID storage.
package impl

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
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
}

// NewPassthrough constructs a Passthrough Planner. Both bc and prov are required.
func NewPassthrough(bc plannerclient.BatchClient, prov provisioner.Provisioner) *Passthrough {
	return &Passthrough{bc: bc, prov: prov}
}

var _ plannerapi.Planner = (*Passthrough)(nil)

func (p *Passthrough) Enqueue(ctx context.Context, req *plannerapi.EnqueueRequest) (*plannerapi.EnqueueResult, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", plannerapi.ErrInvalidJob)
	}
	if req.BatchPayload.InputFileID == "" {
		return nil, fmt.Errorf("%w: missing input_file_id", plannerapi.ErrInvalidJob)
	}
	if req.BatchPayload.Endpoint == "" {
		return nil, fmt.Errorf("%w: missing endpoint", plannerapi.ErrInvalidJob)
	}

	taskID := "tsk-" + uuid.NewString()
	provReq := &rmtypes.ResourceProvision{
		Spec: rmtypes.ResourceProvisionSpec{
			Credential: rmtypes.ResourceCredential{Provider: p.prov.Type()},
		},
		IdempotencyKey: taskID,
	}
	provResult, err := p.prov.Provision(ctx, provReq)
	if err != nil {
		return nil, errors.Join(plannerapi.ErrInsufficientResources, err)
	}

	submission := &plannerclient.MDSBatchSubmission{
		InputFileID:      req.BatchPayload.InputFileID,
		Endpoint:         req.BatchPayload.Endpoint,
		CompletionWindow: req.BatchPayload.CompletionWindow,
		Metadata:         req.BatchPayload.Metadata,
		ExtraBody: plannerclient.MDSExtraBody{
			AIBrix: plannerclient.AIBrixExtraBody{
				JobID: req.JobID,
				PlannerDecision: &struct {
					ProvisionID               string `json:"provision_id,omitempty"`
					ProvisionResourceDeadline int64  `json:"provision_resource_deadline,omitempty"`
				}{
					ProvisionID: provResult.ProvisionID,
				},
				ModelTemplate: req.ModelTemplate,
			},
		},
	}

	klog.Infof("[planner.passthrough] enqueue job_id=%q task_id=%q provision_id=%q model_template=%v",
		req.JobID, taskID, provResult.ProvisionID, req.ModelTemplate)

	batch, err := p.bc.CreateBatch(ctx, submission)
	if err != nil {
		return nil, err
	}

	return &plannerapi.EnqueueResult{JobID: req.JobID, Batch: batch}, nil
}

// GetJob forwards directly to the MDS batch endpoint. Today the
// caller's input is the MDS batch.ID; once durable JobID storage
// lands the planner will translate JobID -> BatchID here first.
func (p *Passthrough) GetJob(ctx context.Context, jobID string) (*openai.Batch, error) {
	if jobID == "" {
		return nil, fmt.Errorf("%w: empty job_id", plannerapi.ErrInvalidJob)
	}
	klog.Infof("[planner.passthrough] get_job id=%q", jobID)
	return p.bc.GetBatch(ctx, jobID)
}

// ListJobs forwards MDS batches verbatim.
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
	return &plannerapi.ListJobsResponse{Data: resp.Data, HasMore: resp.HasMore}, nil
}
