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

package roleset

import (
	"context"
	"math"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"

	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type RollingManager interface {
	Next(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) error
}

type RollingManagerSequential struct {
	cli client.Client
}

func (m *RollingManagerSequential) Next(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (err error) {
	// 1. ensure pod replica meet expectations
	var scaling bool
	for _, role := range roleSet.Spec.Roles {
		klog.Infof("[RollingManagerSequential.Next] start to scale roleset %s/%s role %s", roleSet.Namespace, roleSet.Name, role.Name)
		s, err := GetRoleSyncer(m.cli, &role).Scale(ctx, roleSet, &role)
		if err != nil {
			return err
		}
		scaling = scaling || s
	}
	if scaling {
		klog.Infof("[RollingManagerSequential.Next] waiting for roleset %s/%s to be scaled", roleSet.Namespace, roleSet.Name)
		return nil
	}
	// 2. do the rollout process for each role
	// TODO: in future, consider the rollout sequence based on the role's priority
	for _, role := range roleSet.Spec.Roles {
		klog.Infof("[RollingManagerSequential.Next] start to rollout roleset %s/%s role %s", roleSet.Namespace, roleSet.Name, role.Name)
		err := GetRoleSyncer(m.cli, &role).Rollout(ctx, roleSet, &role)
		if err != nil {
			return err
		}
		if ready, err := GetRoleSyncer(m.cli, &role).AllReady(ctx, roleSet, &role); err != nil {
			return err
		} else if !ready {
			// each time we only update one role in sequential mode
			klog.Infof("[RollingManagerSequential.Next] waiting for roleset %s/%s role %s's rollout process to be finished", roleSet.Namespace, roleSet.Name, role.Name)
			break
		}
	}
	return nil
}

type RollingManagerParallel struct {
	cli client.Client
}

func (m *RollingManagerParallel) Next(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (err error) {
	// 1. ensure pod replica meet expectations
	var scaling bool
	for _, role := range roleSet.Spec.Roles {
		klog.Infof("[RollingManagerParallel.Next] start to scale roleset %s/%s role %s", roleSet.Namespace, roleSet.Name, role.Name)
		s, err := GetRoleSyncer(m.cli, &role).Scale(ctx, roleSet, &role)
		if err != nil {
			return err
		}
		scaling = scaling || s
	}
	if scaling {
		klog.Infof("[RollingManagerParallel.Next] waiting for roleset %s/%s to be scaled", roleSet.Namespace, roleSet.Name)
		return
	}
	// 2. do the rollout process for each role
	for _, role := range roleSet.Spec.Roles {
		klog.Infof("[RollingManagerParallel.Next] start to rollout roleset %s/%s role %s", roleSet.Namespace, roleSet.Name, role.Name)
		err := GetRoleSyncer(m.cli, &role).Rollout(ctx, roleSet, &role)
		if err != nil {
			return err
		}
	}
	return
}

type RollingManagerInterleave struct {
	cli client.Client
}

// Interleaved rollout: update roles in alternating steps,
// using (maxSurge + maxUnavailable) as the step size for all roles
func (m *RollingManagerInterleave) Next(ctx context.Context, roleSet *orchestrationv1alpha1.RoleSet) (err error) {
	// 1. ensure pod replica meet expectations
	var scaling bool
	for _, role := range roleSet.Spec.Roles {
		klog.Infof("[RollingManagerInterleave.Next] start to scale roleset %s/%s role %s", roleSet.Namespace, roleSet.Name, role.Name)
		s, err := GetRoleSyncer(m.cli, &role).Scale(ctx, roleSet, &role)
		if err != nil {
			return err
		}
		scaling = scaling || s
	}
	if scaling {
		klog.Infof("[RollingManagerInterleave.Next] waiting for roleset %s/%s to be scaled", roleSet.Namespace, roleSet.Name)
		return nil
	}
	// 2. do the rollout process for each role
	roleSteps := make(map[string]int32)
	roleReadyStatus := make(map[string]bool)
	currentStep := int32(math.MaxInt32)
	allRolesReady := true

	// Check the current step for each role and determine the minimum step across all roles as the global current step
	for _, role := range roleSet.Spec.Roles {
		allReady, currentRoleStep, err := GetRoleSyncer(m.cli, &role).CheckCurrentStep(ctx, roleSet, &role)
		if err != nil {
			klog.Errorf("[RollingManagerInterleave.Next] Failed to get current step for role %s in roleset %s/%s: %v", role.Name, roleSet.Namespace, roleSet.Name, err)
			continue
		}
		// Store each role's step and readiness status in a map to avoid redundant calculations later
		roleSteps[role.Name] = currentRoleStep
		roleReadyStatus[role.Name] = allReady
		if allReady {
			klog.Infof("[RollingManagerInterleave.Next] Role %s in roleset %s/%s is already ready", role.Name, roleSet.Namespace, roleSet.Name)
			continue
		} else {
			allRolesReady = false
			if currentRoleStep < currentStep {
				currentStep = currentRoleStep
			}
		}
		klog.Infof("[RollingManagerInterleave.Next] Role %s in roleset %s/%s has possible current step: %d", role.Name, roleSet.Namespace, roleSet.Name, currentRoleStep)
	}

	// If all roles are ready, the rollout is complete
	if allRolesReady {
		klog.Infof("[RollingManagerInterleave.Next] roleset %s/%s all roles' rollout step %d finished",
			roleSet.Namespace, roleSet.Name, currentStep)
		return nil
	}
	if currentStep == int32(math.MaxInt32) {
		currentStep = int32(1)
	}
	klog.Infof("[RollingManagerInterleave.Next] Final current step for roleset %s/%s: %d",
		roleSet.Namespace, roleSet.Name, currentStep)

	for _, role := range roleSet.Spec.Roles {
		// Skip roles that are already fully ready
		if roleReadyStatus[role.Name] {
			continue
		}
		// Skip roles that have already passed the current global step
		if roleSteps[role.Name] > currentStep {
			continue
		}

		klog.Infof("[RollingManagerInterleave.Next] start to rollout roleset %s/%s role %s at step %d",
			roleSet.Namespace, roleSet.Name, role.Name, currentStep)

		// Only update roles that match the current global step.
		// This ensures that faster roles wait until all roles complete the current step before proceeding.
		err = GetRoleSyncer(m.cli, &role).RolloutByStep(ctx, roleSet, &role, currentStep)
		if err != nil {
			return err
		}
	}

	return nil
}
