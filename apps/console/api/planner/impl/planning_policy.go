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
	"time"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	"github.com/vllm-project/aibrix/apps/console/api/planner/utils"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
	"k8s.io/klog/v2"
)

type PlanningPolicyType string

const (
	PlanningPolicyTypeSimple PlanningPolicyType = "simple"
)

// PolicyConfig holds configuration for planning policies.
type PolicyConfig struct {
	// MaxConcurrentProvisioning is the maximum number of jobs that can be
	// in the ResourcePreparing state simultaneously. Default is 1.
	MaxConcurrentProvisioning int
}

// DefaultPolicyConfig returns a PolicyConfig with default values.
func DefaultPolicyConfig() PolicyConfig {
	return PolicyConfig{
		MaxConcurrentProvisioning: 1,
	}
}

// PlanningInput includes the input parameters for the planning policy.
type PlanningInput[T utils.PriorityQueueItem] struct {
	PlannerBackend plannerBackend
	RunningQueue   utils.PriorityQueue[T]
	PendingQueue   utils.PriorityQueue[T]
}

func (i *PlanningInput[T]) validate() error {
	if i.PlannerBackend == nil {
		return fmt.Errorf("planner backend is nil")
	}
	if i.RunningQueue == nil {
		return fmt.Errorf("running queue is nil")
	}
	if i.PendingQueue == nil {
		return fmt.Errorf("pending queue is nil")
	}
	return nil
}

// PlanningPolicy is the plugin interface for making scheduling decisions.
type PlanningPolicy[T utils.PriorityQueueItem] interface {
	Type() PlanningPolicyType
	Plan(ctx context.Context, input PlanningInput[T]) error
}

// Factory constructs a PlanningPolicy. Policies self-register their
// factory from init(); last writer wins.
type Factory[T utils.PriorityQueueItem] func(cfg PolicyConfig) (PlanningPolicy[T], error)

var registry = map[rmtypes.ResourceProvisionType]map[PlanningPolicyType]any{}

// Register adds a policy factory. Intended to be called from a
// policy package's init(); last writer wins.
func RegisterPlanningPolicy[T utils.PriorityQueueItem](pt rmtypes.ResourceProvisionType, t PlanningPolicyType, f Factory[T]) {
	if _, ok := registry[pt]; !ok {
		registry[pt] = map[PlanningPolicyType]any{}
	}
	registry[pt][t] = f
}

// Lookup retrieves a policy factory by type.
func LookupPlanningPolicy[T utils.PriorityQueueItem](pt rmtypes.ResourceProvisionType, t PlanningPolicyType) (Factory[T], bool) {
	f, ok := registry[pt][t]
	if !ok {
		return nil, false
	}
	factory, ok := f.(Factory[T])
	if !ok {
		return nil, false
	}
	return factory, true
}

func newPlanningPolicy[T utils.PriorityQueueItem](pt rmtypes.ResourceProvisionType, t PlanningPolicyType, cfg PolicyConfig) (PlanningPolicy[T], error) {
	f, ok := LookupPlanningPolicy[T](pt, t)
	if !ok {
		return nil, fmt.Errorf("no policy factory found for resource provision type %s and policy type %s", pt, t)
	}
	p, err := f(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// SimplePolicy is a simple policy that advances pending jobs.
type SimplePolicy struct {
	cfg PolicyConfig
}

func (p *SimplePolicy) Type() PlanningPolicyType { return PlanningPolicyTypeSimple }

func (p *SimplePolicy) Plan(ctx context.Context, input PlanningInput[*queuedJob]) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := input.validate(); err != nil {
		return err
	}

	// Simple policy: only advance pending jobs if active provisioning jobs
	// are below the configured limit.
	activeProvisioningJobs := 0
	input.PendingQueue.ForEach(func(job *queuedJob) bool {
		job.mu.RLock()
		status := job.status
		hasSchedule := job.scheduledResource != nil
		job.mu.RUnlock()

		if (status == plannerapi.JobStatusQueued && hasSchedule) || status == plannerapi.JobStatusPlanned {
			// Job has been scheduled but hasn't started provisioning yet
			// Count it to prevent over-scheduling
			activeProvisioningJobs++
		}
		return true
	})
	input.RunningQueue.ForEach(func(job *queuedJob) bool {
		job.mu.RLock()
		expired := !job.expiresAt.IsZero() && job.expiresAt.Before(time.Now().UTC())
		status := job.status
		job.mu.RUnlock()

		if expired {
			// skip expired jobs
			return true
		}
		if status == plannerapi.JobStatusPlanned || status == plannerapi.JobStatusResourcePreparing {
			// Count jobs currently provisioning (Provision call in progress)
			// or waiting for provision to become ready
			activeProvisioningJobs++
		}
		return true
	})

	if activeProvisioningJobs >= p.cfg.MaxConcurrentProvisioning {
		return nil
	}

	// advance pending jobs up to the limit
	remainingSlots := p.cfg.MaxConcurrentProvisioning - activeProvisioningJobs
	klog.Infof("[planner] Plan maxConcurrentProvisioning=%d, activeProvisioningJobs=%d, remainingSlots=%d", p.cfg.MaxConcurrentProvisioning, activeProvisioningJobs, remainingSlots)

	scheduled := 0
	input.PendingQueue.ForEach(func(job *queuedJob) bool {
		if remainingSlots <= 0 {
			return false
		}
		job.mu.RLock()
		expired := !job.expiresAt.IsZero() && job.expiresAt.Before(time.Now().UTC())
		status := job.status
		hasSchedule := job.scheduledResource != nil
		job.mu.RUnlock()

		if expired {
			klog.Infof("[planner] Plan job_id=%q expired, skip", job.req.JobID)
			return true
		}
		if !status.IsTerminal() && status != plannerapi.JobStatusCancelling {
			// Already scheduled, skip
			if hasSchedule {
				return true
			}
			job.mu.Lock()
			if job.status.IsTerminal() || job.scheduledResource != nil {
				job.mu.Unlock()
				return true
			}
			spec, err := input.PlannerBackend.Schedule(ctx, job.req)
			if err != nil {
				job.mu.Unlock()
				klog.Warningf("[planner] Plan job_id=%q failed: %v", job.req.JobID, err)
				return true // skip this job, try next
			}
			job.scheduledResource = &spec
			job.mu.Unlock()
			remainingSlots--
			scheduled++

			if specJson, err := json.Marshal(spec); err == nil {
				klog.Infof("[planner] provision spec: %s", specJson)
			}
		}
		return true
	})

	return nil
}
