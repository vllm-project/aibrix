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

package lambdacloud

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// lambdaProvisioner implements provisioner.Provisioner for Lambda Cloud.
//
// Unlike the Kubernetes provisioner (which only records an accounting entry),
// this provisioner owns the real VM lifecycle: Provision launches instances,
// List reconciles their status on read, and Release terminates them. It deals
// strictly with the resource layer (L0) — bringing vLLM up on the VM is the
// workload layer's responsibility, triggered after the provision reaches
// Running.
//
// Credentials are service-level: a single operator-configured account backs
// every request, the same model as the RunPod provisioner.
type lambdaProvisioner struct {
	store  store.Store
	cfg    *Config
	client *Client
}

// newProvisioner builds a Lambda provisioner. A configured API key is required;
// selecting Lambda without one is an error rather than a silent degradation.
func newProvisioner(s store.Store, cfg *Config) (*lambdaProvisioner, error) {
	if cfg == nil || cfg.APIKey == "" {
		return nil, &types.ProvisionerError{
			Message: "lambdaCloud provisioner selected but LAMBDA_CLOUD_API_KEY is not set",
			Code:    "MissingCredential",
		}
	}
	return &lambdaProvisioner{store: s, cfg: cfg, client: NewClient(cfg)}, nil
}

// Type returns the provisioner type.
func (p *lambdaProvisioner) Type() types.ResourceProvisionType {
	return types.ResourceProvisionTypeLambdaCloud
}

// Provision launches Lambda instances for the request and records a provision
// in the "provisioning" state. It returns once the launch is accepted; callers
// poll List until the provision reaches Running.
func (p *lambdaProvisioner) Provision(ctx context.Context, req *types.ResourceProvision) (*types.ProvisionResult, error) {
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

	sshKeys := p.cfg.SSHKeyNames
	defaultRegion := p.cfg.Region
	if len(sshKeys) == 0 {
		return nil, &types.ProvisionerError{
			Message: "lambda launch requires at least one SSH key (set LAMBDA_CLOUD_SSH_KEYS)",
			Code:    "MissingSSHKey",
		}
	}

	entries, err := p.client.ListInstanceTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list instance types: %w", err)
	}

	groups := lambdaGroups(req.Spec.Groups)
	for i := range groups {
		if groups[i].GpusPerReplica <= 0 {
			return nil, fmt.Errorf("%w: gpusPerReplica must be > 0", types.ErrInvalidArgs)
		}
	}

	var groupResults []types.LambdaCloudGroupResult
	chosenRegion := ""
	var launchedIDs []string // tracked for rollback on partial failure

	for i := range groups {
		g := groups[i]
		gpuType := firstPreferredType(g.AcceleratorPreference)
		region := lambdaPreferredRegion(&g, defaultRegion)

		instanceType, resolvedRegion, err := SelectInstanceType(entries, gpuType, g.GpusPerReplica, region)
		if err != nil {
			p.terminateBestEffort(ctx, launchedIDs)
			return nil, &types.ProvisionerError{Message: err.Error(), Code: "NoCapacity", Retry: true}
		}

		replicas := 1
		if g.Replicas != nil && *g.Replicas > 0 {
			replicas = *g.Replicas
		}

		// The Lambda API launches a single instance per call (no quantity
		// field), so issue one launch per replica and aggregate the IDs.
		instances := make([]types.LambdaCloudInstanceDetail, 0, replicas)
		for r := 0; r < replicas; r++ {
			ids, err := p.client.LaunchInstances(ctx, LaunchRequest{
				RegionName:       resolvedRegion,
				InstanceTypeName: instanceType,
				SSHKeyNames:      sshKeys,
				Name:             req.IdempotencyKey,
			})
			if err != nil {
				p.terminateBestEffort(ctx, launchedIDs)
				return nil, fmt.Errorf("launch instances: %w", err)
			}
			launchedIDs = append(launchedIDs, ids...)
			for _, id := range ids {
				instances = append(instances, types.LambdaCloudInstanceDetail{
					InstanceId:   id,
					InstanceType: instanceType,
					Status:       StatusBooting,
				})
			}
		}

		gpuTypeCopy, gpuCount := gpuType, g.GpusPerReplica
		groupResults = append(groupResults, types.LambdaCloudGroupResult{
			GroupRole:        g.GroupRole,
			Instances:        instances,
			Replicas:         len(instances),
			AcceleratorType:  &gpuTypeCopy,
			AcceleratorCount: &gpuCount,
		})
		if chosenRegion == "" {
			chosenRegion = resolvedRegion
		}
	}

	now := time.Now()
	result := &types.ProvisionResult{
		ProvisionID:    uuid.New().String(),
		IdempotencyKey: req.IdempotencyKey,
		Provider:       string(p.Type()),
		Status:         types.ProvisionStatusProvisioning,
		Region:         chosenRegion,
		CreatedAt:      now,
		UpdatedAt:      now,
		LambdaCloud: &types.LambdaCloudProvisionDetail{
			GroupResults: groupResults,
			Region:       types.LambdaCloudRegion{Region: chosenRegion},
		},
	}

	if err := p.store.UpsertProvision(ctx, result); err != nil {
		p.terminateBestEffort(ctx, launchedIDs)
		return nil, fmt.Errorf("upsert provision: %w", err)
	}
	return result, nil
}

// terminateBestEffort rolls back instances launched during a failed Provision.
// Errors are ignored: this is a cleanup path and any surviving instances remain
// visible in the Lambda console for manual handling.
func (p *lambdaProvisioner) terminateBestEffort(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	_ = p.client.TerminateInstances(ctx, ids)
}

// List reconciles the live status of non-terminal Lambda provisions on read,
// then returns the (possibly updated) records. This is what advances a
// provision from "provisioning" to "running" so the planner's readiness poll
// can observe it.
func (p *lambdaProvisioner) List(ctx context.Context, opts *types.ListOptions) ([]*types.ProvisionResult, error) {
	if opts == nil {
		opts = &types.ListOptions{}
	}

	results, err := p.store.ListProvisions(ctx, opts)
	if err != nil {
		return nil, err
	}

	for _, result := range results {
		if result.Provider != string(p.Type()) || result.LambdaCloud == nil {
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

// reconcile refreshes instance status/IPs in place and recomputes the aggregate
// provision status. It returns true if anything changed.
func (p *lambdaProvisioner) reconcile(ctx context.Context, result *types.ProvisionResult) bool {
	changed := false
	statuses := make([]string, 0)

	for gi := range result.LambdaCloud.GroupResults {
		group := &result.LambdaCloud.GroupResults[gi]
		for ii := range group.Instances {
			inst := &group.Instances[ii]
			live, err := p.client.GetInstance(ctx, inst.InstanceId)
			if err != nil {
				statuses = append(statuses, inst.Status)
				continue
			}
			statuses = append(statuses, live.Status)
			if inst.Status != live.Status {
				inst.Status = live.Status
				changed = true
			}
			if live.IP != "" && (inst.PublicIp == nil || *inst.PublicIp != live.IP) {
				ip := live.IP
				inst.PublicIp = &ip
				changed = true
			}
			if live.PrivateIP != "" && (inst.PrivateIp == nil || *inst.PrivateIp != live.PrivateIP) {
				ip := live.PrivateIP
				inst.PrivateIp = &ip
				changed = true
			}
		}
	}

	if agg := aggregateLambdaStatus(statuses); agg != result.Status {
		result.Status = agg
		changed = true
	}
	if changed {
		result.UpdatedAt = time.Now()
	}
	return changed
}

// Release terminates all instances of a provision and marks it released.
func (p *lambdaProvisioner) Release(ctx context.Context, provisionID string) error {
	result, err := p.store.GetProvision(ctx, provisionID)
	if err != nil {
		return fmt.Errorf("get provision: %w", err)
	}

	if result.LambdaCloud != nil {
		var ids []string
		for _, group := range result.LambdaCloud.GroupResults {
			for _, inst := range group.Instances {
				ids = append(ids, inst.InstanceId)
			}
		}
		if err := p.client.TerminateInstances(ctx, ids); err != nil {
			return fmt.Errorf("terminate instances: %w", err)
		}
	}

	return p.store.UpdateProvisionStatus(ctx, provisionID, types.ProvisionStatusReleased)
}

// ============================================================================
// Helpers
// ============================================================================

// lambdaGroups returns the request groups, defaulting to a single empty group
// when none are specified.
func lambdaGroups(groups *[]types.ResourceGroupSpec) []types.ResourceGroupSpec {
	if groups == nil || len(*groups) == 0 {
		return []types.ResourceGroupSpec{{GpusPerReplica: 1}}
	}
	return *groups
}

func firstPreferredType(pref *types.AcceleratorPreference) string {
	if pref == nil || pref.PreferredTypes == nil || len(*pref.PreferredTypes) == 0 {
		return ""
	}
	return (*pref.PreferredTypes)[0]
}

// lambdaPreferredRegion resolves a hard region preference from the group's
// Lambda region affinity, falling back to the service default.
func lambdaPreferredRegion(g *types.ResourceGroupSpec, defaultRegion string) string {
	if g.LambdaCloud != nil && g.LambdaCloud.RegionAffinity != nil {
		if ra := g.LambdaCloud.RegionAffinity.Region; ra != nil && ra.Required != nil && len(*ra.Required) > 0 {
			return (*ra.Required)[0]
		}
	}
	return defaultRegion
}

// aggregateLambdaStatus folds per-instance Lambda statuses into a single
// provision status: any failure wins, otherwise all-running => Running,
// all-terminated => Released, anything else => Provisioning.
func aggregateLambdaStatus(statuses []string) types.ProvisionStatus {
	if len(statuses) == 0 {
		return types.ProvisionStatusProvisioning
	}
	allRunning, allReleased := true, true
	for _, s := range statuses {
		switch s {
		case StatusActive:
			allReleased = false
		case StatusTerminated:
			allRunning = false
		case StatusUnhealthy:
			return types.ProvisionStatusFailed
		default: // booting, terminating, unknown
			allRunning, allReleased = false, false
		}
	}
	switch {
	case allReleased:
		return types.ProvisionStatusReleased
	case allRunning:
		return types.ProvisionStatusRunning
	default:
		return types.ProvisionStatusProvisioning
	}
}
