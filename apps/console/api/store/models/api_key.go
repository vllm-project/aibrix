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

// APIKey maps api_keys table.
type APIKey struct {
	ID        string    `gorm:"column:id;primaryKey;size:36"`
	Name      string    `gorm:"column:name;size:255;not null"`
	KeyHash   string    `gorm:"column:key_hash;size:255;not null"`
	KeyPrefix string    `gorm:"column:key_prefix;size:255;not null;default:''"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (APIKey) TableName() string { return "api_keys" }

func init() {
	RegisterModel(&APIKey{})
}

// FromPB converts a pb.APIKey to APIKey.
func (a *APIKey) FromPB(src *pb.APIKey) error {
	a.ID = src.Id
	a.Name = src.Name
	return nil
}

// ToPB converts APIKey to pb.APIKey.
func (a *APIKey) ToPB() (*pb.APIKey, error) {
	return &pb.APIKey{
		Id:        a.ID,
		Name:      a.Name,
		SecretKey: a.KeyPrefix, // masked key prefix for display
		CreatedAt: a.CreatedAt.Format("Jan 02, 2006 3:04 PM"),
	}, nil
}
