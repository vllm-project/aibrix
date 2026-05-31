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

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
)

// Quota maps quotas table.
type Quota struct {
	RowID        uint64    `gorm:"column:row_id;primaryKey;autoIncrement"`
	ID           string    `gorm:"column:id;size:36;not null;uniqueIndex:uniq_quotas_id"`
	Name         string    `gorm:"column:name;size:255;not null;default:'';index:idx_quotas_name"`
	QuotaID      string    `gorm:"column:quota_id;size:255;not null;default:'';uniqueIndex:uniq_quotas_quota_id"`
	CurrentUsage int32     `gorm:"column:current_usage;not null;default:0"`
	Quota        int32     `gorm:"column:quota;not null;default:0"`
	Deleted      bool      `gorm:"column:deleted;not null;default:false;index"`
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (Quota) TableName() string { return "quotas" }

func init() {
	RegisterModel(&Quota{})
}

// FromPB converts a pb.Quota to Quota.
func (q *Quota) FromPB(src *pb.Quota) error {
	q.ID = src.Id
	q.Name = src.Name
	q.QuotaID = src.QuotaId
	q.CurrentUsage = src.CurrentUsage
	q.Quota = src.Quota
	return nil
}

// computeUsagePercentage calculates usage percentage from current_usage and quota.
func (q *Quota) computeUsagePercentage() float64 {
	if q.Quota == 0 {
		return 0
	}
	return float64(q.CurrentUsage) / float64(q.Quota) * 100
}

// ToPB converts Quota to pb.Quota.
func (q *Quota) ToPB() (*pb.Quota, error) {
	return &pb.Quota{
		Id:              q.ID,
		Name:            q.Name,
		QuotaId:         q.QuotaID,
		CurrentUsage:    q.CurrentUsage,
		UsagePercentage: q.computeUsagePercentage(),
		Quota:           q.Quota,
	}, nil
}
