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
	"fmt"
	"regexp"
	"strconv"
	"time"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"gorm.io/datatypes"
)

// Deployment maps to deployments table.
type Deployment struct {
	RowID            uint64         `gorm:"column:row_id;primaryKey;autoIncrement"`
	ID               string         `gorm:"column:id;size:36;not null;uniqueIndex:uniq_deployments_id"`
	Name             string         `gorm:"column:name;size:255;not null;default:'';index:idx_deployments_name"`
	DeploymentID     string         `gorm:"column:deployment_id;size:255;not null;default:''"`
	BaseModel        string         `gorm:"column:base_model;size:255;not null;default:'';index:idx_deployments_base_model"`
	BaseModelID      string         `gorm:"column:base_model_id;size:255;not null;default:''"`
	MinReplicas      int32          `gorm:"column:min_replicas;not null;default:1"`
	MaxReplicas      int32          `gorm:"column:max_replicas;not null;default:1"`
	GpusPerReplica   int32          `gorm:"column:gpus_per_replica;not null;default:0"`
	GpuType          string         `gorm:"column:gpu_type;size:255;not null;default:''"`
	Region           string         `gorm:"column:region;size:255;not null;default:''"`
	CreatedBy        string         `gorm:"column:created_by;size:255;not null;default:'';index:idx_deployments_created_by"`
	Status           string         `gorm:"column:status;size:255;not null;default:'Deploying';index:idx_deployments_status"`
	ModelSource      string         `gorm:"column:model_source;size:255;not null;default:''"`
	ModelArtifactURL string         `gorm:"column:model_artifact_url;size:1000;not null;default:''"`
	Engine           string         `gorm:"column:engine;size:255;not null;default:''"`
	StartupCommand   string         `gorm:"column:startup_command;type:text"`
	EnvVars          datatypes.JSON `gorm:"column:env_vars"`
	ExtraArgs        datatypes.JSON `gorm:"column:extra_args"`
	Namespace        string         `gorm:"column:namespace;size:255;not null;default:'default'"`
	K8sResourceName  string         `gorm:"column:k8s_resource_name;size:255;not null;default:''"`
	Deleted          bool           `gorm:"column:deleted;not null;default:false;index"`
	CreatedAt        time.Time      `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt        time.Time      `gorm:"column:updated_at;autoUpdateTime"`
}

func (Deployment) TableName() string { return "deployments" }

func init() {
	RegisterModel(&Deployment{})
}

// FromPB converts a pb.Deployment to Deployment.
func (d *Deployment) FromPB(src *pb.Deployment) error {
	d.ID = src.Id
	d.Name = src.Name
	d.DeploymentID = src.DeploymentId
	d.BaseModel = src.BaseModel
	d.BaseModelID = src.BaseModelId

	// src.Replicas is in the format of %d[%d], parse it to MinReplicas and MaxReplicas
	minReplicas, maxReplicas, err := parseReplicas(src.Replicas)
	if err != nil {
		return fmt.Errorf("failed to parse replicas: %w", err)
	}

	if maxReplicas < minReplicas {
		return fmt.Errorf("max_replicas (%d) cannot be less than min_replicas (%d)", maxReplicas, minReplicas)
	}

	d.MinReplicas = minReplicas
	d.MaxReplicas = maxReplicas
	d.GpusPerReplica = src.GpusPerReplica
	d.GpuType = src.GpuType
	d.Region = src.Region
	d.CreatedBy = src.CreatedBy
	d.Status = src.Status
	return nil
}

// replicasRegex matches formats: "1" or "1[01]" or "10[05]"
var replicasRegex = regexp.MustCompile(`^(\d+)(?:\[(\d+)\])?$`)

func parseReplicas(replicasStr string) (int32, int32, error) {
	if len(replicasStr) == 0 {
		return 1, 1, nil
	}

	// Validate format using regex: must be %d or %d[%d]
	matches := replicasRegex.FindStringSubmatch(replicasStr)
	if matches == nil {
		return 1, 1, fmt.Errorf("invalid replicas format: %s", replicasStr)
	}

	// Parse min replicas
	minReplicas, err := strconv.ParseInt(matches[1], 10, 32)
	if err != nil || minReplicas < 0 {
		minReplicas = 1
	}

	// Parse max replicas
	maxReplicas := minReplicas
	if len(matches) > 2 && matches[2] != "" {
		maxVal, err := strconv.ParseInt(matches[2], 10, 32)
		if err == nil && maxVal >= 0 {
			maxReplicas = maxVal
		}
	}

	return int32(minReplicas), int32(maxReplicas), nil
}

// ToPB converts Deployment to pb.Deployment.
func (d *Deployment) ToPB() (*pb.Deployment, error) {
	return &pb.Deployment{
		Id:             d.ID,
		Name:           d.Name,
		DeploymentId:   d.DeploymentID,
		BaseModel:      d.BaseModel,
		BaseModelId:    d.BaseModelID,
		Replicas:       fmt.Sprintf("%d[%d]", d.MinReplicas, d.MaxReplicas),
		GpusPerReplica: d.GpusPerReplica,
		GpuType:        d.GpuType,
		Region:         d.Region,
		CreatedBy:      d.CreatedBy,
		Status:         d.Status,
	}, nil
}
