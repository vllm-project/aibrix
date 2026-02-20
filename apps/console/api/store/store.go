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
)

// Store defines the storage interface for all console entities.
type Store interface {
	// Deployments
	ListDeployments(ctx context.Context, search string) ([]*pb.Deployment, error)
	GetDeployment(ctx context.Context, id string) (*pb.Deployment, error)
	CreateDeployment(ctx context.Context, req *pb.CreateDeploymentRequest) (*pb.Deployment, error)
	DeleteDeployment(ctx context.Context, id string) error

	// Jobs
	ListJobs(ctx context.Context, search, status string) ([]*pb.Job, error)
	GetJob(ctx context.Context, id string) (*pb.Job, error)
	CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.Job, error)

	// Models
	ListModels(ctx context.Context, search, category string) ([]*pb.Model, error)
	GetModel(ctx context.Context, id string) (*pb.Model, error)

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
}
