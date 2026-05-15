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

package store

import (
	"context"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// Store defines the storage interface for all console entities.
type Store interface {
	// Deployments
	ListDeployments(ctx context.Context, search string) ([]*pb.Deployment, error)
	GetDeployment(ctx context.Context, id string) (*pb.Deployment, error)
	CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error)
	DeleteDeployment(ctx context.Context, id string) error

	// Jobs — store persists only the Console-owned fields of *pb.Job
	// (id, created_by, future: organization, tags, ...). OpenAI Batch fields
	// on the passed-in *pb.Job (status, usage, request_counts, timestamps,
	// etc.) are ignored on write and left at their zero values on read; the
	// JobHandler aggregates store output with state fetched from the metadata
	// service /v1/batches API to produce the wire-level *pb.Job.
	UpsertJob(ctx context.Context, job *pb.Job) error
	GetJob(ctx context.Context, id string) (*pb.Job, error) // (nil, nil) when not found
	ListJobs(ctx context.Context, ids []string) (map[string]*pb.Job, error)
	DeleteJob(ctx context.Context, id string) error

	// Models
	ListModels(ctx context.Context, search, category string) ([]*pb.Model, error)
	GetModel(ctx context.Context, id string) (*pb.Model, error)

	// Model Deployment Templates. modelID is the parent — Get/Update/Delete
	// validate that the addressed template actually belongs to it, returning
	// NotFound otherwise so URLs are canonical.
	//
	// ResolveModelDeploymentTemplate looks up by (modelID, name, version);
	// version="" means "latest active". This is the path used by clients
	// that pin templates from outside the UI (e.g. batch SDK callers passing
	// model_template / model_template_version).
	ListModelDeploymentTemplates(ctx context.Context, modelID, statusFilter, name string) ([]*pb.ModelDeploymentTemplate, error)
	GetModelDeploymentTemplate(ctx context.Context, modelID, id string) (*pb.ModelDeploymentTemplate, error)
	CreateModelDeploymentTemplate(ctx context.Context, req *pb.CreateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error)
	UpdateModelDeploymentTemplate(ctx context.Context, req *pb.UpdateModelDeploymentTemplateRequest) (*pb.ModelDeploymentTemplate, error)
	DeleteModelDeploymentTemplate(ctx context.Context, modelID, id string) error
	ResolveModelDeploymentTemplate(ctx context.Context, modelID, name, version string) (*pb.ModelDeploymentTemplate, error)

	// API Keys
	ListAPIKeys(ctx context.Context) ([]*pb.APIKey, error)
	CreateAPIKey(ctx context.Context, name string) (*pb.APIKey, string, error) // returns key + full secret
	DeleteAPIKey(ctx context.Context, id string) error

	// Secrets
	ListSecrets(ctx context.Context, search string) ([]*pb.Secret, error)
	CreateSecret(ctx context.Context, name, value string) (*pb.Secret, error)
	DeleteSecret(ctx context.Context, id string) error

	// Quotas
	ListQuotas(ctx context.Context, search string) ([]*pb.Quota, error)

	// Provision
	// GetProvision retrieves a stored provision result by provision ID.
	// Returns nil and a NotFound error if the key doesn't exist.
	GetProvision(ctx context.Context, provisionId string) (*types.ProvisionResult, error)

	// GetProvisionByIdempotencyKey retrieves a stored provision result by idempotency key.
	// Returns nil and a NotFound error if the key doesn't exist.
	GetProvisionByIdempotencyKey(ctx context.Context, idempotencyKey string) (*types.ProvisionResult, error)

	// UpsertProvision inserts/updates a provision result with the given idempotency key.
	UpsertProvision(ctx context.Context, result *types.ProvisionResult) error

	// UpdateProvisionStatus updates the status of a provision result.
	UpdateProvisionStatus(ctx context.Context, provisionId string, status types.ProvisionStatus) error

	// DeleteProvision marks a stored provision result by provision ID as deleted.
	DeleteProvision(ctx context.Context, provisionId string) error

	// ExistsProvision checks if a provision exists for the given provision ID.
	ExistsProvision(ctx context.Context, provisionId string) (bool, error)

	// ListProvisions lists stored provision results.
	ListProvisions(ctx context.Context, options *types.ListOptions) ([]*types.ProvisionResult, error)

	// Close closes the store.
	Close() error
}
