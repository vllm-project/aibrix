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

// ModelDeploymentTemplate maps model_deployment_templates table.
type ModelDeploymentTemplate struct {
	ID        string         `gorm:"column:id;primaryKey;size:36"`
	Name      string         `gorm:"column:name;size:255;not null;index:idx_model_tpl_lookup,priority:2;uniqueIndex:uk_model_tpl_name_ver,priority:2"`
	Version   string         `gorm:"column:version;size:64;not null;index:idx_model_tpl_lookup,priority:3;uniqueIndex:uk_model_tpl_name_ver,priority:3"`
	Status    string         `gorm:"column:status;size:64;not null;default:'active';index:idx_model_tpl_status"`
	ModelID   string         `gorm:"column:model_id;size:36;not null;index:idx_model_tpl_lookup,priority:1;index:idx_model_tpl_status;uniqueIndex:uk_model_tpl_name_ver,priority:1"`
	Spec      datatypes.JSON `gorm:"column:spec;not null"`
	CreatedAt time.Time      `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time      `gorm:"column:updated_at;autoUpdateTime"`
}

func (ModelDeploymentTemplate) TableName() string { return "model_deployment_templates" }

func init() {
	RegisterModel(&ModelDeploymentTemplate{})
}

// FromPB converts a pb.ModelDeploymentTemplate to ModelDeploymentTemplate.
func (t *ModelDeploymentTemplate) FromPB(src *pb.ModelDeploymentTemplate) error {
	spec, err := jsonMarshalToDatatype(src.Spec)
	if err != nil {
		return err
	}

	t.ID = src.Id
	t.Name = src.Name
	t.Version = src.Version
	t.Status = src.Status
	t.ModelID = src.ModelId
	t.Spec = spec

	if src.CreatedAt != "" {
		if ct, err := time.Parse(time.RFC3339, src.CreatedAt); err == nil {
			t.CreatedAt = ct
		}
	}
	if src.UpdatedAt != "" {
		if ut, err := time.Parse(time.RFC3339, src.UpdatedAt); err == nil {
			t.UpdatedAt = ut
		}
	}
	return nil
}

// ToPB converts ModelDeploymentTemplate to pb.ModelDeploymentTemplate.
func (t *ModelDeploymentTemplate) ToPB() (*pb.ModelDeploymentTemplate, error) {
	var spec *pb.ModelDeploymentTemplateSpec
	if len(t.Spec) > 0 {
		spec = &pb.ModelDeploymentTemplateSpec{}
		if err := json.Unmarshal(t.Spec, spec); err != nil {
			return nil, fmt.Errorf("failed to unmarshal spec: %w", err)
		}
	}

	return &pb.ModelDeploymentTemplate{
		Id:        t.ID,
		Name:      t.Name,
		Version:   t.Version,
		Status:    t.Status,
		ModelId:   t.ModelID,
		Spec:      spec,
		CreatedAt: t.CreatedAt.Format(time.RFC3339),
		UpdatedAt: t.UpdatedAt.Format(time.RFC3339),
	}, nil
}
