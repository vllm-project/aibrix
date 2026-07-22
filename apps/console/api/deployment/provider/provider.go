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

package provider

import (
	"context"

	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type DeploymentProvider interface {
	Kind() string
	Validate(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) error
	Create(ctx context.Context, template *pb.ModelDeploymentTemplate, req *pb.CreateDeploymentRequest) (*pb.Deployment, error)
	Observe(ctx context.Context, deployment *pb.Deployment) (*ObservedStatus, error)
	Update(ctx context.Context, deployment *pb.Deployment) (*pb.Deployment, error)
	Delete(ctx context.Context, deployment *pb.Deployment) error
}

type ObservedStatus struct {
	Status     string
	Reason     string
	Message    string
	Replicas   string
	ObservedAt int64
}

type providerAliases interface {
	Aliases() []string
}

type Registry struct {
	providers   map[string]DeploymentProvider
	defaultKind string
}

func NewRegistry(providers ...DeploymentProvider) *Registry {
	r := &Registry{providers: map[string]DeploymentProvider{}}
	for _, p := range providers {
		if p == nil {
			continue
		}
		kind := p.Kind()
		if r.defaultKind == "" {
			r.defaultKind = kind
		}
		r.providers[kind] = p
		if aliases, ok := p.(providerAliases); ok {
			for _, alias := range aliases.Aliases() {
				r.providers[alias] = p
			}
		}
	}
	return r
}

func (r *Registry) Get(kind string) (DeploymentProvider, error) {
	if kind == "" {
		kind = r.defaultKind
	}
	p, ok := r.providers[kind]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported deployment provider kind %q", kind)
	}
	return p, nil
}

func ApplyObservedStatus(deployment *pb.Deployment, observed *ObservedStatus) *pb.Deployment {
	current := cloneDeployment(deployment)
	if current == nil || observed == nil {
		return current
	}
	if observed.Status != "" {
		current.Status = observed.Status
	}
	if observed.Replicas != "" {
		current.Replicas = observed.Replicas
	}
	return current
}

func cloneDeployment(src *pb.Deployment) *pb.Deployment {
	if src == nil {
		return nil
	}
	return proto.Clone(src).(*pb.Deployment)
}
