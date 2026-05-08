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

// Secret maps secrets table.
type Secret struct {
	ID             string    `gorm:"column:id;primaryKey;size:36"`
	Name           string    `gorm:"column:name;size:255;not null"`
	EncryptedValue string    `gorm:"column:encrypted_value;type:text;not null"`
	CreatedAt      time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (Secret) TableName() string { return "secrets" }

func init() {
	RegisterModel(&Secret{})
}

// FromPB converts a pb.Secret to Secret.
func (s *Secret) FromPB(src *pb.Secret) error {
	s.ID = src.Id
	s.Name = src.Name
	return nil
}

// ToPB converts Secret to pb.Secret.
func (s *Secret) ToPB() (*pb.Secret, error) {
	return &pb.Secret{
		Id:   s.ID,
		Name: s.Name,
	}, nil
}
