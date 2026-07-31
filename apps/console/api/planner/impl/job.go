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
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/respjson"
	"github.com/vllm-project/aibrix/apps/console/api/common"
	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	pu "github.com/vllm-project/aibrix/apps/console/api/planner/utils"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
	"github.com/vllm-project/aibrix/apps/console/api/utils"

	"gorm.io/datatypes"
	"k8s.io/klog/v2"
)

// queuedJob is the Planner's in-memory snapshot of a Job.
type queuedJob struct {
	req                 *plannerapi.EnqueueRequest
	status              plannerapi.JobStatus
	provisionID         string
	batchID             string
	errMsg              string // populated when status is resource_failed / submit_failed
	queuedAt            time.Time
	expiresAt           time.Time
	plannedAt           time.Time
	resourcePreparingAt time.Time
	submittingAt        time.Time
	resourceFailedAt    time.Time
	submitFailedAt      time.Time
	cancelRequestedAt   time.Time
	canceledAt          time.Time
	expiredAt           time.Time
	completedAt         time.Time

	// mu protects fields that may be accessed concurrently
	mu sync.RWMutex

	queue pu.PriorityQueue[*queuedJob]
	batch *openai.Batch // MDS batch object
	// scheduledResource is the resource provision spec that the job is scheduled to run with.
	scheduledResource *rmtypes.ResourceProvisionSpec
	// allocatedResource is the resource provision result that the job is running with.
	allocatedResource *rmtypes.ProvisionResult
	pqPriority        int64 // priority for queue ordering (higher = more important)
	// readyToSubmit indicates provision is ready and job can be submitted to MDS
	// This is a runtime-only flag, not persisted to database
	readyToSubmit bool
	extraBody     json.RawMessage
}

func (j *queuedJob) Key() string {
	return j.req.JobID
}

// terminalTime returns the timestamp at which the job transitioned into a
// terminal state (resource_failed, submit_failed, cancelled, expired, completed)
func terminalTime(j *queuedJob) time.Time {
	switch j.status {
	case plannerapi.JobStatusResourceFailed:
		return j.resourceFailedAt
	case plannerapi.JobStatusSubmitFailed:
		return j.submitFailedAt
	case plannerapi.JobStatusCancelled:
		return j.canceledAt
	case plannerapi.JobStatusExpired:
		return j.expiredAt
	case plannerapi.JobStatusCompleted:
		return j.completedAt
	}
	return time.Time{}
}

func jobStateSnapshot(j *queuedJob) *plannerapi.JobState {
	if j == nil {
		return nil
	}
	return &plannerapi.JobState{
		BatchID:             j.batchID,
		ProvisionID:         j.provisionID,
		ErrorMessage:        j.errMsg,
		QueuedAt:            j.queuedAt,
		ResourcePreparingAt: j.resourcePreparingAt,
		SubmittingAt:        j.submittingAt,
		ResourceFailedAt:    j.resourceFailedAt,
		SubmitFailedAt:      j.submitFailedAt,
		CancelRequestedAt:   j.cancelRequestedAt,
		CancelledAt:         j.canceledAt,
	}
}

// batchFromModel constructs an openai.Batch from MDS-owned fields stored in models.Job.
func batchFromModel(rec *models.Job) *openai.Batch {
	batch := &openai.Batch{
		ID:               rec.BatchID,
		Model:            rec.Model,
		Endpoint:         rec.Endpoint,
		InputFileID:      rec.InputDataset,
		CompletionWindow: rec.CompletionWindow,
		Status:           plannerapi.JobStatus(rec.Status).ToBatchStatus(),
		CreatedAt:        utils.TimeToUnix(rec.BatchCreatedAt),
		OutputFileID:     rec.OutputDataset,
		ErrorFileID:      rec.ErrorDataset,
		InProgressAt:     utils.TimeToUnix(rec.InProgressAt),
		ExpiresAt:        utils.TimeToUnix(rec.ExpiresAt),
		FinalizingAt:     utils.TimeToUnix(rec.FinalizingAt),
		CompletedAt:      utils.TimeToUnix(rec.CompletedAt),
		FailedAt:         utils.TimeToUnix(rec.FailedAt),
		ExpiredAt:        utils.TimeToUnix(rec.ExpiredAt),
		CancellingAt:     utils.TimeToUnix(rec.CancellingAt),
		CancelledAt:      utils.TimeToUnix(rec.CancelledAt),
	}
	// Hydrate RequestCounts from stored JSON
	if len(rec.RequestCounts) > 0 {
		_ = json.Unmarshal(rec.RequestCounts, &batch.RequestCounts)
	}
	// Hydrate Usage from stored JSON
	if len(rec.Usage) > 0 {
		_ = json.Unmarshal(rec.Usage, &batch.Usage)
	}
	return batch
}

func constructBatchJson(batch *openai.Batch) {
	if batch == nil {
		return
	}

	// Set JSON field metadata for fields that have non-zero values.
	// This allows downstream code to check .Valid() to determine if the field is populated.
	if batch.RequestCounts.Completed > 0 || batch.RequestCounts.Failed > 0 || batch.RequestCounts.Total > 0 {
		if data, err := json.Marshal(batch.RequestCounts); err == nil {
			batch.JSON.RequestCounts = respjson.NewField(string(data))
		}
	}
	if batch.Usage.TotalTokens > 0 {
		if data, err := json.Marshal(batch.Usage); err == nil {
			batch.JSON.Usage = respjson.NewField(string(data))
		}
	}
	if len(batch.Errors.Data) > 0 {
		if data, err := json.Marshal(batch.Errors); err == nil {
			batch.JSON.Errors = respjson.NewField(string(data))
		}
	}
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

	var extraBody json.RawMessage
	if len(rec.Metadata) > 0 {
		var m map[string]string
		if err := json.Unmarshal(rec.Metadata, &m); err == nil {
			// extraBody is embedded in metadata
			if extraBodyJson, ok := m[common.BatchExtraBodyField]; ok {
				if len(extraBodyJson) > 0 {
					extraBody = json.RawMessage(extraBodyJson)
				}
				delete(m, common.BatchExtraBodyField)
			}
			req.BatchParams.Metadata = m
		} else {
			klog.Errorf("modelToJob: failed to marshal metadata: %v", err)
		}
	}
	// Hydrate batch object from MDS-owned fields stored in DB
	batch := batchFromModel(rec)
	if batch != nil {
		batch.Metadata = req.BatchParams.Metadata
		constructBatchJson(batch)
		// Inject extraBody into batch's raw JSON so ParseBatchExtraBody can extract it
		if extraBody != nil {
			batch = common.InjectExtraBodyToBatch(batch, extraBody)
		}
	}
	return &queuedJob{
		req:                 req,
		status:              plannerapi.JobStatus(rec.Status),
		provisionID:         rec.ProvisionID,
		batchID:             rec.BatchID,
		errMsg:              rec.ErrorMessage,
		queuedAt:            utils.TimeOrZero(rec.QueuedAt),
		expiresAt:           utils.TimeOrZero(rec.ExpiresAt),
		resourcePreparingAt: utils.TimeOrZero(rec.ResourcePreparingAt),
		submittingAt:        utils.TimeOrZero(rec.SubmittingAt),
		resourceFailedAt:    utils.TimeOrZero(rec.ResourceFailedAt),
		submitFailedAt:      utils.TimeOrZero(rec.SubmitFailedAt),
		cancelRequestedAt:   utils.TimeOrZero(rec.CancelRequestedAt),
		canceledAt:          utils.TimeOrZero(rec.CancelledAt),
		expiredAt:           utils.TimeOrZero(rec.ExpiredAt),
		completedAt:         utils.TimeOrZero(rec.CompletedAt),
		batch:               batch,
		extraBody:           extraBody,
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
		ExpiresAt:           utils.TimeToPtr(j.expiresAt),
		ResourcePreparingAt: utils.TimeToPtr(j.resourcePreparingAt),
		SubmittingAt:        utils.TimeToPtr(j.submittingAt),
		ResourceFailedAt:    utils.TimeToPtr(j.resourceFailedAt),
		SubmitFailedAt:      utils.TimeToPtr(j.submitFailedAt),
		CancelRequestedAt:   utils.TimeToPtr(j.cancelRequestedAt),
		CancelledAt:         utils.TimeToPtr(j.canceledAt),
		ExpiredAt:           utils.TimeToPtr(j.expiredAt),
		CompletedAt:         utils.TimeToPtr(j.completedAt),
		ErrorMessage:        j.errMsg,
	}
	// Persist frontend-computed RequestCountTotal if provided
	if j.req.RequestCountTotal > 0 {
		if data, err := json.Marshal(map[string]int32{"total": j.req.RequestCountTotal}); err == nil {
			rec.RequestCounts = datatypes.JSON(data)
		}
	}
	if j.req.ModelTemplate != nil {
		rec.ModelTemplateName = j.req.ModelTemplate.Name
		rec.ModelTemplateVersion = j.req.ModelTemplate.Version
	}
	if md := j.req.BatchParams.Metadata; len(md) > 0 {
		// extraBody is embedded in metadata
		if j.extraBody != nil {
			md[common.BatchExtraBodyField] = string(j.extraBody)
		}
		if b, err := json.Marshal(md); err == nil {
			rec.Metadata = datatypes.JSON(b)
		}
		// Handler-packed keys under aibrix.console.* (legacy: bare "display_name").
		if v := md[common.MetadataConsoleDisplayName]; v != "" {
			rec.Name = v
		} else if v := md[common.MetadataDisplayName]; v != "" {
			rec.Name = v
		}
		rec.CreatedBy = md[common.MetadataConsoleCreatedBy]
	}
	return rec
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
