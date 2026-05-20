/*
Copyright 2025 The Aibrix Team.

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

package models

import (
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/utils"
	"gorm.io/datatypes"
)

// Job stores full batch job records including metadata-service owned fields.
type Job struct {
	RowID                uint64         `gorm:"column:row_id;primaryKey;autoIncrement"`
	ID                   string         `gorm:"column:id;size:64;not null;uniqueIndex:uniq_jobs_id"`
	Endpoint             string         `gorm:"column:endpoint;size:255;not null;default:''"`
	Model                string         `gorm:"column:model;size:255;not null;default:'';index:idx_jobs_model"`
	InputDataset         string         `gorm:"column:input_dataset;size:255;not null;default:''"`
	CompletionWindow     string         `gorm:"column:completion_window;size:64;not null;default:''"`
	Status               string         `gorm:"column:status;size:64;not null;default:'';index:idx_jobs_status"`
	OutputDataset        string         `gorm:"column:output_dataset;size:255;not null;default:''"`
	ErrorDataset         string         `gorm:"column:error_dataset;size:255;not null;default:''"`
	BatchCreatedAt       *time.Time     `gorm:"column:batch_created_at"`
	InProgressAt         *time.Time     `gorm:"column:in_progress_at"`
	ExpiresAt            *time.Time     `gorm:"column:expires_at"`
	FinalizingAt         *time.Time     `gorm:"column:finalizing_at"`
	CompletedAt          *time.Time     `gorm:"column:completed_at"`
	FailedAt             *time.Time     `gorm:"column:failed_at"`
	ExpiredAt            *time.Time     `gorm:"column:expired_at"`
	CancellingAt         *time.Time     `gorm:"column:cancelling_at"`
	CancelledAt          *time.Time     `gorm:"column:cancelled_at"`
	RequestCounts        datatypes.JSON `gorm:"column:request_counts"`
	Usage                datatypes.JSON `gorm:"column:usage_stats"`
	Metadata             datatypes.JSON `gorm:"column:metadata"`
	Name                 string         `gorm:"column:name;size:255;not null;default:''"`
	CreatedBy            string         `gorm:"column:created_by;size:255;not null;default:''"`
	ModelTemplateName    string         `gorm:"column:model_template_name;size:255;not null;default:''"`
	ModelTemplateVersion string         `gorm:"column:model_template_version;size:64;not null;default:''"`
	BatchID              string         `gorm:"column:batch_id;size:64;not null;default:'';index:idx_jobs_batch_id"`
	ProvisionID          string         `gorm:"column:provision_id;size:36;not null;default:''"`
	QueuedAt             *time.Time     `gorm:"column:queued_at"`
	ResourcePreparingAt  *time.Time     `gorm:"column:resource_preparing_at"`
	SubmittingAt         *time.Time     `gorm:"column:submitting_at"`
	ResourceFailedAt     *time.Time     `gorm:"column:resource_failed_at"`
	SubmitFailedAt       *time.Time     `gorm:"column:submit_failed_at"`
	CancelRequestedAt    *time.Time     `gorm:"column:cancel_requested_at"`
	ErrorMessage         string         `gorm:"column:error_msg;type:TEXT"`
	Deleted              bool           `gorm:"column:deleted;not null;default:false;index:idx_jobs_deleted"`
	CreatedAt            time.Time      `gorm:"column:created_at;autoCreateTime;index:idx_jobs_created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at;autoUpdateTime"`
}

func (Job) TableName() string { return "jobs" }

func init() {
	RegisterModel(&Job{})
}

// FromPB converts a pb.Job to Job.
func (j *Job) FromPB(src *pb.Job) error {
	j.ID = src.Id
	j.Endpoint = src.Endpoint
	j.Model = src.Model
	j.InputDataset = src.InputDataset
	j.CompletionWindow = src.CompletionWindow
	j.Status = src.Status
	j.OutputDataset = src.OutputDataset
	j.ErrorDataset = src.ErrorDataset

	// Convert protobuf timestamps (int64 unix seconds) to nullable *time.Time.
	j.BatchCreatedAt = utils.UnixToTimePtr(src.CreatedAt)
	j.InProgressAt = utils.UnixToTimePtr(src.InProgressAt)
	j.ExpiresAt = utils.UnixToTimePtr(src.ExpiresAt)
	j.FinalizingAt = utils.UnixToTimePtr(src.FinalizingAt)
	j.CompletedAt = utils.UnixToTimePtr(src.CompletedAt)
	j.FailedAt = utils.UnixToTimePtr(src.FailedAt)
	j.ExpiredAt = utils.UnixToTimePtr(src.ExpiredAt)
	j.CancellingAt = utils.UnixToTimePtr(src.CancellingAt)
	j.CancelledAt = utils.UnixToTimePtr(src.CancelledAt)

	requestCounts, err := jsonMarshalToDatatype(src.RequestCounts)
	if err != nil {
		return fmt.Errorf("failed to marshal request_counts: %w", err)
	}
	j.RequestCounts = requestCounts

	usage, err := jsonMarshalToDatatype(src.Usage)
	if err != nil {
		return fmt.Errorf("failed to marshal usage: %w", err)
	}
	j.Usage = usage

	metadata, err := jsonMarshalToDatatype(src.Metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	j.Metadata = metadata

	j.Name = src.Name
	j.CreatedBy = src.CreatedBy
	j.ModelTemplateName = src.ModelTemplateName
	j.ModelTemplateVersion = src.ModelTemplateVersion
	return nil
}

// ToPB converts Job to pb.Job.
func (j *Job) ToPB() (*pb.Job, error) {
	var requestCounts *pb.JobRequestCounts
	if len(j.RequestCounts) > 0 {
		requestCounts = &pb.JobRequestCounts{}
		if err := json.Unmarshal(j.RequestCounts, requestCounts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal request_counts: %w", err)
		}
	}

	var usage *pb.JobUsage
	if len(j.Usage) > 0 {
		usage = &pb.JobUsage{}
		if err := json.Unmarshal(j.Usage, usage); err != nil {
			return nil, fmt.Errorf("failed to unmarshal usage: %w", err)
		}
	}

	var metadata map[string]string
	if len(j.Metadata) > 0 {
		if err := json.Unmarshal(j.Metadata, &metadata); err != nil {
			return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}

	return &pb.Job{
		Id:                   j.ID,
		Object:               "batch",
		Endpoint:             j.Endpoint,
		Model:                j.Model,
		InputDataset:         j.InputDataset,
		CompletionWindow:     j.CompletionWindow,
		Status:               j.Status,
		OutputDataset:        j.OutputDataset,
		ErrorDataset:         j.ErrorDataset,
		CreatedAt:            utils.TimeToUnix(j.BatchCreatedAt),
		InProgressAt:         utils.TimeToUnix(j.InProgressAt),
		ExpiresAt:            utils.TimeToUnix(j.ExpiresAt),
		FinalizingAt:         utils.TimeToUnix(j.FinalizingAt),
		CompletedAt:          utils.TimeToUnix(j.CompletedAt),
		FailedAt:             utils.TimeToUnix(j.FailedAt),
		ExpiredAt:            utils.TimeToUnix(j.ExpiredAt),
		CancellingAt:         utils.TimeToUnix(j.CancellingAt),
		CancelledAt:          utils.TimeToUnix(j.CancelledAt),
		RequestCounts:        requestCounts,
		Usage:                usage,
		Metadata:             metadata,
		Name:                 j.Name,
		CreatedBy:            j.CreatedBy,
		ModelTemplateName:    j.ModelTemplateName,
		ModelTemplateVersion: j.ModelTemplateVersion,
	}, nil
}
