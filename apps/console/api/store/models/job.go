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
	"gorm.io/datatypes"
)

// Job stores full batch job records including metadata-service owned fields.
type Job struct {
	ID                   string         `gorm:"column:id;primaryKey;size:64"`
	Object               string         `gorm:"column:object;size:64;not null;default:''"`
	Endpoint             string         `gorm:"column:endpoint;size:255;not null;default:''"`
	Model                string         `gorm:"column:model;size:255;not null;default:''"`
	InputDataset         string         `gorm:"column:input_dataset;size:255;not null;default:''"`
	CompletionWindow     string         `gorm:"column:completion_window;size:64;not null;default:''"`
	Status               string         `gorm:"column:status;size:64;not null;default:''"`
	OutputDataset        string         `gorm:"column:output_dataset;size:255;not null;default:''"`
	ErrorDataset         string         `gorm:"column:error_dataset;size:255;not null;default:''"`
	InProgressAt         time.Time      `gorm:"column:in_progress_at;not null;default:0"`
	ExpiresAt            time.Time      `gorm:"column:expires_at;not null;default:0"`
	FinalizingAt         time.Time      `gorm:"column:finalizing_at;not null;default:0"`
	CompletedAt          time.Time      `gorm:"column:completed_at;not null;default:0"`
	FailedAt             time.Time      `gorm:"column:failed_at;not null;default:0"`
	ExpiredAt            time.Time      `gorm:"column:expired_at;not null;default:0"`
	CancellingAt         time.Time      `gorm:"column:cancelling_at;not null;default:0"`
	CancelledAt          time.Time      `gorm:"column:cancelled_at;not null;default:0"`
	RequestCounts        datatypes.JSON `gorm:"column:request_counts"`
	Usage                datatypes.JSON `gorm:"column:usage"`
	Metadata             datatypes.JSON `gorm:"column:metadata"`
	Name                 string         `gorm:"column:name;size:255;not null;default:''"`
	CreatedBy            string         `gorm:"column:created_by;size:255;not null;default:''"`
	ModelTemplateName    string         `gorm:"column:model_template_name;size:255;not null;default:''"`
	ModelTemplateVersion string         `gorm:"column:model_template_version;size:64;not null;default:''"`
	CreatedAt            time.Time      `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt            time.Time      `gorm:"column:updated_at;autoUpdateTime"`
}

func (Job) TableName() string { return "jobs" }

func init() {
	RegisterModel(&Job{})
}

// FromPB converts a pb.Job to Job.
func (j *Job) FromPB(src *pb.Job) error {
	j.ID = src.Id
	j.Object = src.Object
	j.Endpoint = src.Endpoint
	j.Model = src.Model
	j.InputDataset = src.InputDataset
	j.CompletionWindow = src.CompletionWindow
	j.Status = src.Status
	j.OutputDataset = src.OutputDataset
	j.ErrorDataset = src.ErrorDataset
	j.CreatedAt = time.Unix(src.CreatedAt, 0)
	j.InProgressAt = time.Unix(src.InProgressAt, 0)
	j.ExpiresAt = time.Unix(src.ExpiresAt, 0)
	j.FinalizingAt = time.Unix(src.FinalizingAt, 0)
	j.CompletedAt = time.Unix(src.CompletedAt, 0)
	j.FailedAt = time.Unix(src.FailedAt, 0)
	j.ExpiredAt = time.Unix(src.ExpiredAt, 0)
	j.CancellingAt = time.Unix(src.CancellingAt, 0)
	j.CancelledAt = time.Unix(src.CancelledAt, 0)

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
		Object:               j.Object,
		Endpoint:             j.Endpoint,
		Model:                j.Model,
		InputDataset:         j.InputDataset,
		CompletionWindow:     j.CompletionWindow,
		Status:               j.Status,
		OutputDataset:        j.OutputDataset,
		ErrorDataset:         j.ErrorDataset,
		CreatedAt:            j.CreatedAt.Unix(),
		InProgressAt:         j.InProgressAt.Unix(),
		ExpiresAt:            j.ExpiresAt.Unix(),
		FinalizingAt:         j.FinalizingAt.Unix(),
		CompletedAt:          j.CompletedAt.Unix(),
		FailedAt:             j.FailedAt.Unix(),
		ExpiredAt:            j.ExpiredAt.Unix(),
		CancellingAt:         j.CancellingAt.Unix(),
		CancelledAt:          j.CancelledAt.Unix(),
		RequestCounts:        requestCounts,
		Usage:                usage,
		Metadata:             metadata,
		Name:                 j.Name,
		CreatedBy:            j.CreatedBy,
		ModelTemplateName:    j.ModelTemplateName,
		ModelTemplateVersion: j.ModelTemplateVersion,
	}, nil
}
