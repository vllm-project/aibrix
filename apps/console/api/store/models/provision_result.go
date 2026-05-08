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
	"time"

	"gorm.io/datatypes"
)

// ProvisionResult maps provision_results table.
type ProvisionResult struct {
	IdempotencyKey string         `gorm:"column:idempotency_key;primaryKey;size:255"`
	ProvisionID    string         `gorm:"column:provision_id;size:255;not null;index:idx_provision_results_provision_id;uniqueIndex:uk_provision_results_provision_id"`
	Region         string         `gorm:"column:region;size:255;not null;index:idx_provision_results_region"`
	Status         string         `gorm:"column:status;size:64;not null;index:idx_provision_results_status_deleted,priority:1"`
	Payload        datatypes.JSON `gorm:"column:payload"`
	CreatedAt      time.Time      `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt      time.Time      `gorm:"column:updated_at;autoUpdateTime"`
	Deleted        bool           `gorm:"column:deleted;not null;default:false;index:idx_provision_results_status_deleted,priority:2"`
}

func (ProvisionResult) TableName() string { return "provision_results" }

func init() {
	RegisterModel(&ProvisionResult{})
}
