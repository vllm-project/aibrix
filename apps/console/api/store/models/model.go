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

// Model maps model catalog table.
type Model struct {
	ID            string         `gorm:"column:id;primaryKey;size:36"`
	Name          string         `gorm:"column:name;size:255;not null"`
	Provider      string         `gorm:"column:provider;size:255;not null;default:''"`
	IconBg        string         `gorm:"column:icon_bg;size:255;not null;default:''"`
	IconText      string         `gorm:"column:icon_text;size:255;not null;default:''"`
	IconTextColor string         `gorm:"column:icon_text_color;size:255;not null;default:''"`
	Categories    datatypes.JSON `gorm:"column:categories"`
	IsNew         bool           `gorm:"column:is_new;not null;default:false"`
	Pricing       datatypes.JSON `gorm:"column:pricing"`
	ContextLength string         `gorm:"column:context_length;size:255;not null;default:''"`
	Description   string         `gorm:"column:description;type:text"`
	Metadata      datatypes.JSON `gorm:"column:metadata"`
	Specification datatypes.JSON `gorm:"column:specification"`
	Tags          datatypes.JSON `gorm:"column:tags"`
	ServingName   string         `gorm:"column:serving_name;size:255;not null;default:''"`
	CreatedAt     time.Time      `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time      `gorm:"column:updated_at;autoUpdateTime"`
}

func (Model) TableName() string { return "models" }

func init() {
	RegisterModel(&Model{})
}

// FromPB converts a pb.Model to Model. Returns error if JSON marshaling fails.
func (m *Model) FromPB(src *pb.Model) error {
	categories, err := jsonMarshalToDatatype(src.Categories)
	if err != nil {
		return err
	}
	pricing, err := jsonMarshalToDatatype(src.Pricing)
	if err != nil {
		return err
	}
	metadata, err := jsonMarshalToDatatype(src.Metadata)
	if err != nil {
		return err
	}
	spec, err := jsonMarshalToDatatype(src.Specification)
	if err != nil {
		return err
	}
	tags, err := jsonMarshalToDatatype(src.Tags)
	if err != nil {
		return err
	}

	provider := ""
	if src.Metadata != nil {
		provider = src.Metadata.ProviderName
	}

	m.ID = src.Id
	m.Name = src.Name
	m.Provider = provider
	m.IconBg = src.IconBg
	m.IconText = src.IconText
	m.IconTextColor = src.IconTextColor
	m.Categories = categories
	m.IsNew = src.IsNew
	m.Pricing = pricing
	m.ContextLength = src.ContextLength
	m.Description = src.Description
	m.Metadata = metadata
	m.Specification = spec
	m.Tags = tags
	m.ServingName = src.ServingName
	return nil
}

// jsonMarshalToDatatype marshals v to datatypes.JSON.
func jsonMarshalToDatatype(v any) (datatypes.JSON, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

// ToPB converts Model to pb.Model.
func (m *Model) ToPB() (*pb.Model, error) {
	var categories []string
	if len(m.Categories) > 0 {
		if err := json.Unmarshal(m.Categories, &categories); err != nil {
			return nil, fmt.Errorf("failed to unmarshal categories: %w", err)
		}
	}

	var pricing *pb.ModelPricing
	if len(m.Pricing) > 0 {
		pricing = &pb.ModelPricing{}
		if err := json.Unmarshal(m.Pricing, pricing); err != nil {
			return nil, fmt.Errorf("failed to unmarshal pricing: %w", err)
		}
	}

	var metadata *pb.ModelMetadata
	if len(m.Metadata) > 0 {
		metadata = &pb.ModelMetadata{}
		if err := json.Unmarshal(m.Metadata, metadata); err != nil {
			return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
		}
	}

	var specification *pb.ModelSpecification
	if len(m.Specification) > 0 {
		specification = &pb.ModelSpecification{}
		if err := json.Unmarshal(m.Specification, specification); err != nil {
			return nil, fmt.Errorf("failed to unmarshal specification: %w", err)
		}
	}

	var tags []string
	if len(m.Tags) > 0 {
		if err := json.Unmarshal(m.Tags, &tags); err != nil {
			return nil, fmt.Errorf("failed to unmarshal tags: %w", err)
		}
	}

	return &pb.Model{
		Id:            m.ID,
		Name:          m.Name,
		IconBg:        m.IconBg,
		IconText:      m.IconText,
		IconTextColor: m.IconTextColor,
		Categories:    categories,
		IsNew:         m.IsNew,
		Pricing:       pricing,
		ContextLength: m.ContextLength,
		Description:   m.Description,
		Metadata:      metadata,
		Specification: specification,
		Tags:          tags,
		ServingName:   m.ServingName,
	}, nil
}
