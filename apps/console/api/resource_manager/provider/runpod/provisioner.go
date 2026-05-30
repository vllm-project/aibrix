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

package runpod

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// runpodProvisioner implements provisioner.Provisioner for RunPod. Like the
// Lambda provisioner it owns the real pod lifecycle (create / reconcile /
// delete) and runs on a single service-level account. RunPod is not a built-in
// type, so its pod details ride in ExtensionProvisionResultDetails.
type runpodProvisioner struct {
	store  store.Store
	cfg    *Config
	client *Client
}

// newProvisioner builds a RunPod provisioner. A configured API key is required;
// selecting RunPod without one is an error rather than a silent degradation.
func newProvisioner(s store.Store, cfg *Config) (*runpodProvisioner, error) {
	if cfg == nil || cfg.APIKey == "" {
		return nil, &types.ProvisionerError{
			Message: "runpod provisioner selected but RUNPOD_API_KEY is not set",
			Code:    "MissingCredential",
		}
	}
	return &runpodProvisioner{store: s, cfg: cfg, client: NewClient(cfg)}, nil
}

// Type returns the provisioner type.
func (p *runpodProvisioner) Type() types.ResourceProvisionType {
	return types.ResourceProvisionTypeRunPod
}

// Provision creates one RunPod pod per replica and records a provision in the
// "provisioning" state.
func (p *runpodProvisioner) Provision(ctx context.Context, req *types.ResourceProvision) (*types.ProvisionResult, error) {
	if req == nil {
		return nil, types.ErrInvalidArgs
	}

	existing, err := p.store.GetProvisionByIdempotencyKey(ctx, req.IdempotencyKey)
	if err == nil && existing != nil {
		return existing, nil
	}
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, fmt.Errorf("get provision by idempotency key: %w", err)
	}

	groups := runpodGroups(req.Spec.Groups)
	for i := range groups {
		if groups[i].GpusPerReplica <= 0 {
			return nil, fmt.Errorf("%w: gpusPerReplica must be > 0", types.ErrInvalidArgs)
		}
	}

	var pods []types.RunPodPodDetail
	var createdIDs []string
	region := ""

	for i := range groups {
		g := groups[i]
		// RunPod's REST API has no GPU-type catalog endpoint, so the caller's
		// acceleratorPreference.preferredTypes are passed straight through as
		// RunPod gpuTypeIds (e.g. "NVIDIA H100 80GB HBM3"). RunPod picks among
		// them by availability.
		gpuTypeIds := allPreferredTypes(g.AcceleratorPreference)
		if len(gpuTypeIds) == 0 {
			p.deleteBestEffort(ctx, createdIDs)
			return nil, &types.ProvisionerError{
				Message: "runpod requires acceleratorPreference.preferredTypes (RunPod gpuTypeIds)",
				Code:    "NoGpuType",
			}
		}

		gpuCount := g.GpusPerReplica
		replicas := 1
		if g.Replicas != nil && *g.Replicas > 0 {
			replicas = *g.Replicas
		}

		for r := 0; r < replicas; r++ {
			pod, err := p.client.CreatePod(ctx, p.buildInput(req.IdempotencyKey, gpuTypeIds, gpuCount))
			if err != nil {
				p.deleteBestEffort(ctx, createdIDs)
				return nil, fmt.Errorf("create pod: %w", err)
			}
			createdIDs = append(createdIDs, pod.ID)
			pods = append(pods, podDetail(pod, gpuTypeIds[0]))
			if region == "" {
				region = pod.Machine.DataCenterId
			}
		}
	}

	now := time.Now()
	result := &types.ProvisionResult{
		ProvisionID:    uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Provider:       string(p.Type()),
		Status:         types.ProvisionStatusProvisioning,
		Region:         region,
		CreatedAt:      now,
		UpdatedAt:      now,
		RunPod:         &types.RunPodProvisionDetail{Pods: pods, Region: region},
	}

	if err := p.store.UpsertProvision(ctx, result); err != nil {
		p.deleteBestEffort(ctx, createdIDs)
		return nil, fmt.Errorf("upsert provision: %w", err)
	}
	return result, nil
}

// List reconciles the live status of non-terminal RunPod provisions on read.
func (p *runpodProvisioner) List(ctx context.Context, opts *types.ListOptions) ([]*types.ProvisionResult, error) {
	if opts == nil {
		opts = &types.ListOptions{}
	}

	results, err := p.store.ListProvisions(ctx, opts)
	if err != nil {
		return nil, err
	}

	for _, result := range results {
		if result.Provider != string(p.Type()) || result.RunPod == nil {
			continue
		}
		if result.Status != types.ProvisionStatusProvisioning && result.Status != types.ProvisionStatusRunning {
			continue
		}
		if p.reconcile(ctx, result) {
			if err := p.store.UpsertProvision(ctx, result); err != nil {
				return nil, fmt.Errorf("persist reconciled provision %s: %w", result.ProvisionID, err)
			}
		}
	}
	return results, nil
}

// reconcile refreshes each pod's status/IP and recomputes the aggregate
// provision status. RunPod has no dedicated runtime-status field, so readiness
// is inferred from desiredStatus==RUNNING plus a populated public IP.
func (p *runpodProvisioner) reconcile(ctx context.Context, result *types.ProvisionResult) bool {
	changed := false
	statuses := make([]string, 0, len(result.RunPod.Pods))

	for i := range result.RunPod.Pods {
		pod := &result.RunPod.Pods[i]
		live, err := p.client.GetPod(ctx, pod.PodId)
		if err != nil {
			statuses = append(statuses, pod.DesiredStatus)
			continue
		}
		statuses = append(statuses, podPhase(live))
		if pod.DesiredStatus != live.DesiredStatus {
			pod.DesiredStatus = live.DesiredStatus
			changed = true
		}
		if live.PublicIp != "" && (pod.PublicIp == nil || *pod.PublicIp != live.PublicIp) {
			ip := live.PublicIp
			pod.PublicIp = &ip
			changed = true
		}
	}

	if agg := aggregateRunPodStatus(statuses); agg != result.Status {
		result.Status = agg
		changed = true
	}
	if changed {
		result.UpdatedAt = time.Now()
	}
	return changed
}

// Release deletes all pods of a provision and marks it released.
func (p *runpodProvisioner) Release(ctx context.Context, provisionID string) error {
	result, err := p.store.GetProvision(ctx, provisionID)
	if err != nil {
		return fmt.Errorf("get provision: %w", err)
	}

	if result.RunPod != nil {
		// Delete every pod even if some fail, so a single failure doesn't leak
		// the rest; aggregate and report the errors.
		var errs []error
		for _, pod := range result.RunPod.Pods {
			if err := p.client.DeletePod(ctx, pod.PodId); err != nil {
				errs = append(errs, fmt.Errorf("delete pod %s: %w", pod.PodId, err))
			}
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
	}
	return p.store.UpdateProvisionStatus(ctx, provisionID, types.ProvisionStatusReleased)
}

// ============================================================================
// Helpers
// ============================================================================

func (p *runpodProvisioner) buildInput(name string, gpuTypeIds []string, gpuCount int) PodCreateInput {
	input := PodCreateInput{
		Name:            name,
		ComputeType:     "GPU",
		GpuTypeIds:      gpuTypeIds,
		GpuCount:        gpuCount,
		GpuTypePriority: "availability",
	}
	input.ImageName = p.cfg.ImageName
	input.CloudType = p.cfg.CloudType
	input.DataCenterIds = p.cfg.DataCenterIds
	input.ContainerDiskInGb = p.cfg.ContainerDiskInGb
	input.VolumeInGb = p.cfg.VolumeInGb
	input.Ports = p.cfg.Ports
	if input.CloudType == "" {
		input.CloudType = CloudTypeSecure
	}
	return input
}

func (p *runpodProvisioner) deleteBestEffort(ctx context.Context, ids []string) {
	for _, id := range ids {
		_ = p.client.DeletePod(ctx, id)
	}
}

func podDetail(pod *Pod, gpuTypeId string) types.RunPodPodDetail {
	detail := types.RunPodPodDetail{
		PodId:         pod.ID,
		GpuTypeId:     gpuTypeId,
		DesiredStatus: pod.DesiredStatus,
		DataCenterId:  pod.Machine.DataCenterId,
	}
	if pod.PublicIp != "" {
		ip := pod.PublicIp
		detail.PublicIp = &ip
	}
	return detail
}

func runpodGroups(groups *[]types.ResourceGroupSpec) []types.ResourceGroupSpec {
	if groups == nil || len(*groups) == 0 {
		return []types.ResourceGroupSpec{{GpusPerReplica: 1}}
	}
	return *groups
}

// allPreferredTypes returns the accelerator preference list verbatim, used as
// RunPod gpuTypeIds.
func allPreferredTypes(pref *types.AcceleratorPreference) []string {
	if pref == nil || pref.PreferredTypes == nil {
		return nil
	}
	return *pref.PreferredTypes
}

// podPhase maps a live pod to a coarse phase string for aggregation.
func podPhase(pod *Pod) string {
	switch pod.DesiredStatus {
	case StatusTerminated:
		return StatusTerminated
	case StatusExited:
		return StatusExited
	case StatusRunning:
		if pod.PublicIp != "" {
			return StatusRunning
		}
		return "" // running but not yet reachable -> still provisioning
	default:
		return ""
	}
}

// aggregateRunPodStatus folds per-pod phases into a single provision status.
func aggregateRunPodStatus(phases []string) types.ProvisionStatus {
	if len(phases) == 0 {
		return types.ProvisionStatusProvisioning
	}
	allRunning, allTerminated := true, true
	for _, ph := range phases {
		switch ph {
		case StatusRunning:
			allTerminated = false
		case StatusTerminated:
			allRunning = false
		case StatusExited:
			return types.ProvisionStatusFailed
		default: // not yet reachable
			allRunning, allTerminated = false, false
		}
	}
	switch {
	case allTerminated:
		return types.ProvisionStatusReleased
	case allRunning:
		return types.ProvisionStatusRunning
	default:
		return types.ProvisionStatusProvisioning
	}
}
