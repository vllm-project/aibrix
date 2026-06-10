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

package impl

import (
	"context"
	"encoding/json"
	"fmt"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	plannerclient "github.com/vllm-project/aibrix/apps/console/api/planner/client"
	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/provisioner"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"k8s.io/utils/ptr"
)

// plannerBackend is the per-provisioner extension surface invoked per job
// as: ValidateRequest → Schedule → BuildRuntime → BuildResourceAllocation.
//   - Schedule produces the ResourceProvisionSpec; it is the hook for
//     future capacity-aware scheduling (replica sizing, gpu type selection, etc.).
//   - BuildResourceAllocation projects the ProvisionResult onto the batch
//     submission and folds in ready-state logging.
//   - BuildRuntime projects the ready ProvisionResult plus model-template
//     serving config onto the MDS RuntimeRef.
//
// Accepted-provision logging is opt-in via provisionResponseLogger.
type plannerBackend interface {
	ValidateRequest(req *plannerapi.EnqueueRequest) error
	Schedule(ctx context.Context, req *plannerapi.EnqueueRequest) (spec rmtypes.ResourceProvisionSpec, err error)
	BuildRuntime(req *plannerapi.EnqueueRequest, prov *rmtypes.ProvisionResult) (*plannerapi.RuntimeRef, error)
	BuildResourceAllocation(spec rmtypes.ResourceProvisionSpec, prov *rmtypes.ProvisionResult) plannerclient.ResourceAllocation
}

// provisionResponseLogger is an optional capability to log provider-specific
// detail of an accepted provision.
type provisionResponseLogger interface {
	LogProvisionResponse(jobID string, prov *rmtypes.ProvisionResult, spec rmtypes.ResourceProvisionSpec)
}

// backendFactory constructs a plannerBackend for a provisioner type.
type backendFactory func(prov provisioner.Provisioner) plannerBackend

// backendRegistry holds plannerBackend factories registered from init()
// by provider-specific files.
var backendRegistry = map[rmtypes.ResourceProvisionType]backendFactory{}

// RegisterBackend registers a factory for a provisioner type; last writer wins.
func RegisterBackend(t rmtypes.ResourceProvisionType, f backendFactory) {
	backendRegistry[t] = f
}

// newPlannerBackend returns the registered backend for prov, or
// defaultPlannerBackend when none is registered (or prov is nil).
func newPlannerBackend(prov provisioner.Provisioner) plannerBackend {
	if prov == nil {
		return &defaultPlannerBackend{}
	}
	t := prov.Type()
	if f, ok := backendRegistry[t]; ok {
		return f(prov)
	}
	return &defaultPlannerBackend{provider: t}
}

// decodeAcceleratorFromTemplate parses ModelTemplateRef.Spec (a protojson-
// encoded ModelDeploymentTemplateSpec) and returns the accelerator type
// and per-replica count. Returns ("", 0, nil) when ref or Spec is empty.
func decodeAcceleratorFromTemplate(ref *plannerapi.ModelTemplateRef) (gpuType string, gpusPerReplica int, err error) {
	if ref == nil || len(ref.Spec) == 0 {
		return "", 0, nil
	}
	var spec struct {
		Accelerator struct {
			Type  string `json:"type"`
			Count int    `json:"count"`
		} `json:"accelerator"`
	}
	if err := json.Unmarshal(ref.Spec, &spec); err != nil {
		return "", 0, fmt.Errorf("decode model_template.spec: %w", err)
	}
	return spec.Accelerator.Type, spec.Accelerator.Count, nil
}

// decodeEngineFromTemplate parses ModelTemplateRef.Spec (protojson-encoded
// ModelDeploymentTemplateSpec) and returns the engine serving image and serve
// args. Returns ("", nil, nil) when ref or Spec is empty. The handler marshals
// with UseProtoNames=true (handler/job.go), so JSON keys are snake_case.
func decodeEngineFromTemplate(ref *plannerapi.ModelTemplateRef) (image string, serveArgs []string, err error) {
	if ref == nil || len(ref.Spec) == 0 {
		return "", nil, nil
	}
	var spec struct {
		Engine struct {
			Image     string   `json:"image"`
			ServeArgs []string `json:"serve_args"`
		} `json:"engine"`
	}
	if err := json.Unmarshal(ref.Spec, &spec); err != nil {
		return "", nil, fmt.Errorf("decode model_template.spec engine: %w", err)
	}
	return spec.Engine.Image, spec.Engine.ServeArgs, nil
}

// buildProvisionGroupPlan composes the single-replica ResourceGroupSpec
// shared by all backends today.
func buildProvisionGroupPlan(gpuType string, gpusPerReplica int) rmtypes.ResourceGroupSpec {
	group := rmtypes.ResourceGroupSpec{
		Replicas:       ptr.To(1),
		GpusPerReplica: gpusPerReplica,
	}
	if gpuType != "" {
		group.AcceleratorPreference = &rmtypes.AcceleratorPreference{
			PreferredTypes: ptr.To([]string{gpuType}),
		}
	}
	return group
}

// defaultPlannerBackend serves providers that only need default scheduling and
// allocation behavior.
type defaultPlannerBackend struct {
	provider rmtypes.ResourceProvisionType
}

func (b *defaultPlannerBackend) ValidateRequest(*plannerapi.EnqueueRequest) error {
	// Lenient by default; ModelTemplate is optional.
	return nil
}

func (b *defaultPlannerBackend) Schedule(_ context.Context, req *plannerapi.EnqueueRequest) (spec rmtypes.ResourceProvisionSpec, err error) {
	spec.Credential = rmtypes.ResourceCredential{Provider: b.provider}
	if req == nil || req.ModelTemplate == nil {
		return
	}
	gpuType, gpusPerReplica, err := decodeAcceleratorFromTemplate(req.ModelTemplate)
	if err != nil {
		return
	}
	spec.Groups = &[]rmtypes.ResourceGroupSpec{buildProvisionGroupPlan(gpuType, gpusPerReplica)}
	return
}

func (b *defaultPlannerBackend) BuildRuntime(req *plannerapi.EnqueueRequest, prov *rmtypes.ProvisionResult) (*plannerapi.RuntimeRef, error) {
	if req == nil {
		return nil, fmt.Errorf("missing enqueue request")
	}
	image, serveArgs, err := decodeEngineFromTemplate(req.ModelTemplate)
	if err != nil {
		return nil, err
	}
	return plannerclient.RuntimeForProvisionResult(b.provider, prov, req.Model, image, serveArgs)
}

func (b *defaultPlannerBackend) BuildResourceAllocation(_ rmtypes.ResourceProvisionSpec, prov *rmtypes.ProvisionResult) plannerclient.ResourceAllocation {
	// Default allocation shape; accelerator scalars are ignored here.
	return &plannerclient.DefaultResourceAllocation{ProvisionID: prov.ProvisionID}
}
