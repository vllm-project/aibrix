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
// as: ValidateRequest → Schedule → BuildDecision.
//   - Schedule produces the ResourceProvisionSpec; it is the hook for
//     future capacity-aware scheduling (replica sizing, gpu type selection, etc.).
//   - BuildDecision projects the ProvisionResult onto the batch submission
//     and folds in ready-state logging.
//
// Accepted-provision logging is opt-in via provisionResponseLogger.
//
// TODO: add Plan(ctx, req) before Schedule to fetch RM-catalog capacity
// (per-accelerator, quotas, etc.) so Schedule can be capacity-aware.
type plannerBackend interface {
	ValidateRequest(req *plannerapi.EnqueueRequest) error
	Schedule(ctx context.Context, req *plannerapi.EnqueueRequest) (spec rmtypes.ResourceProvisionSpec, gpuType string, gpusPerReplica int, err error)
	BuildDecision(spec rmtypes.ResourceProvisionSpec, prov *rmtypes.ProvisionResult, gpuType string, gpusPerReplica int) *plannerclient.PlannerDecision
}

// provisionResponseLogger is an optional capability to log provider-specific
// detail of an accepted provision.
type provisionResponseLogger interface {
	LogProvisionResponse(jobID string, prov *rmtypes.ProvisionResult, spec rmtypes.ResourceProvisionSpec)
}

// provisionOverride is an optional capability to bypass the real RM call
// and supply a ProvisionResult directly.
type provisionOverride interface {
	TryProvisionOverride(req *plannerapi.EnqueueRequest) (*rmtypes.ProvisionResult, bool)
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

// defaultPlannerBackend serves kubernetes / aws / lambdaCloud.
type defaultPlannerBackend struct {
	provider rmtypes.ResourceProvisionType
}

func (b *defaultPlannerBackend) ValidateRequest(*plannerapi.EnqueueRequest) error {
	// Lenient by default; ModelTemplate is optional.
	return nil
}

func (b *defaultPlannerBackend) Schedule(_ context.Context, req *plannerapi.EnqueueRequest) (rmtypes.ResourceProvisionSpec, string, int, error) {
	spec := rmtypes.ResourceProvisionSpec{
		Credential: rmtypes.ResourceCredential{Provider: b.provider},
	}
	if req == nil || req.ModelTemplate == nil {
		return spec, "", 0, nil
	}
	gpuType, gpusPerReplica, err := decodeAcceleratorFromTemplate(req.ModelTemplate)
	if err != nil {
		return rmtypes.ResourceProvisionSpec{}, "", 0, err
	}
	spec.Groups = &[]rmtypes.ResourceGroupSpec{buildProvisionGroupPlan(gpuType, gpusPerReplica)}
	return spec, gpuType, gpusPerReplica, nil
}

func (b *defaultPlannerBackend) BuildDecision(_ rmtypes.ResourceProvisionSpec, prov *rmtypes.ProvisionResult, _ string, _ int) *plannerclient.PlannerDecision {
	// Default decision shape; accelerator scalars are ignored here.
	return &plannerclient.PlannerDecision{ProvisionID: prov.ProvisionID}
}
