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

package models

import (
	"time"

	"gorm.io/datatypes"
)

type CatalogViewType string

const (
	CatalogViewResource   CatalogViewType = "resource"
	CatalogViewPrediction CatalogViewType = "prediction"
)

// CatalogSnapshot stores the latest resource or prediction view collected
// for one provider. Provider and ViewType form the logical key.
type CatalogSnapshot struct {
	RowID       uint64          `gorm:"column:row_id;primaryKey;autoIncrement"`
	Provider    string          `gorm:"column:provider;size:32;not null;uniqueIndex:uniq_catalog_snapshots_provider_view,priority:1"`
	ViewType    CatalogViewType `gorm:"column:view_type;size:16;not null;uniqueIndex:uniq_catalog_snapshots_provider_view,priority:2"`
	WindowStart time.Time       `gorm:"column:window_start;not null"`
	WindowEnd   time.Time       `gorm:"column:window_end;not null"`
	Payload     datatypes.JSON  `gorm:"column:payload;not null"`
	CreatedAt   time.Time       `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time       `gorm:"column:updated_at;autoUpdateTime"`
}

func (CatalogSnapshot) TableName() string { return "catalog_snapshots" }

func init() {
	RegisterModel(&CatalogSnapshot{})
}
