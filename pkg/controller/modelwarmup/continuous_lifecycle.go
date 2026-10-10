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

package modelwarmup

import (
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type continuousCreateAction struct {
	Target            resolvedTarget
	Attempt           int32
	DeleteAfterCreate *batchv1.Job
}

type continuousPlan struct {
	Create              []continuousCreateAction
	DeleteBeforeCreate  []batchv1.Job
	RetryAfter          time.Duration
	TargetLimitExceeded bool
	JobsByTarget        map[types.UID]batchv1.Job
}

func buildContinuousPlan(
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string]resolvedTarget,
	jobs []batchv1.Job,
	policies warmupPolicies,
	now time.Time,
) continuousPlan {
	plan := continuousPlan{
		TargetLimitExceeded: len(targets) > modelv1alpha1.MaxModelWarmupTargets,
		JobsByTarget:        make(map[types.UID]batchv1.Job),
	}
	targetsByUID := make(map[types.UID]resolvedTarget, len(targets))
	names := make([]string, 0, len(targets))
	for name, target := range targets {
		names = append(names, name)
		targetsByUID[target.NodeUID] = target
	}
	sort.Strings(names)
	occupiedNames := make(map[string]struct{}, len(jobs))
	active := int32(0)
	for i := range jobs {
		job := jobs[i]
		if !metav1.IsControlledBy(&job, warmup) {
			continue
		}
		occupiedNames[job.Name] = struct{}{}
		if !isJobComplete(&job) && !isJobFailed(&job) {
			active++
		}
		uid := types.UID(job.Annotations[TargetNodeUIDAnnotationKey])
		attempt, err := strconv.ParseInt(job.Annotations[AttemptAnnotationKey], 10, 32)
		target, currentTarget := targetsByUID[uid]
		valid := job.Labels[RevisionLabelKey] == revision && uid != "" && err == nil && attempt > 0 &&
			currentTarget && target.NodeName == targetNodeForJob(&job)
		if !valid {
			if !plan.TargetLimitExceeded {
				plan.DeleteBeforeCreate = append(plan.DeleteBeforeCreate, job)
			}
			continue
		}
		if existing, ok := plan.JobsByTarget[uid]; ok {
			existingAttempt, _ := strconv.ParseInt(existing.Annotations[AttemptAnnotationKey], 10, 32)
			if attempt > existingAttempt {
				plan.DeleteBeforeCreate = append(plan.DeleteBeforeCreate, existing)
				plan.JobsByTarget[uid] = job
			} else {
				plan.DeleteBeforeCreate = append(plan.DeleteBeforeCreate, job)
			}
			continue
		}
		plan.JobsByTarget[uid] = job
	}
	if plan.TargetLimitExceeded {
		plan.DeleteBeforeCreate = nil
		return plan
	}
	for _, name := range names {
		target := targets[name]
		if job, exists := plan.JobsByTarget[target.NodeUID]; exists {
			if !isJobFailed(&job) {
				continue
			}
			attempt := continuousJobAttempt(&job)
			if attempt >= 1+policies.continuousRetryLimit {
				continue
			}
			deadline := continuousJobFinishedAt(&job).Add(
				time.Duration(policies.continuousRetryIntervalSeconds) * time.Second,
			)
			if now.Before(deadline) {
				delay := deadline.Sub(now)
				if plan.RetryAfter == 0 || delay < plan.RetryAfter {
					plan.RetryAfter = delay
				}
				continue
			}
			if active+int32(len(plan.Create)) >= policies.parallelism {
				continue
			}
			nextAttempt := attempt + 1
			desiredName := modelWarmupJobNameForAttempt(
				warmup.Name, string(warmup.UID), target.NodeName, revision, nextAttempt,
			)
			if _, occupied := occupiedNames[desiredName]; occupied {
				continue
			}
			plan.Create = append(plan.Create, continuousCreateAction{
				Target: target, Attempt: nextAttempt, DeleteAfterCreate: job.DeepCopy(),
			})
			continue
		}
		if active+int32(len(plan.Create)) >= policies.parallelism {
			break
		}
		desiredName := modelWarmupJobName(warmup.Name, string(warmup.UID), target.NodeName, revision)
		if _, exists := occupiedNames[desiredName]; exists {
			continue
		}
		plan.Create = append(plan.Create, continuousCreateAction{Target: target, Attempt: 1})
	}
	sort.Slice(plan.DeleteBeforeCreate, func(i, j int) bool {
		return plan.DeleteBeforeCreate[i].Name < plan.DeleteBeforeCreate[j].Name
	})
	return plan
}

func continuousJobAttempt(job *batchv1.Job) int32 {
	attempt, err := strconv.ParseInt(job.Annotations[AttemptAnnotationKey], 10, 32)
	if err != nil || attempt < 1 {
		return 0
	}
	return int32(attempt)
}

func continuousJobFinishedAt(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue &&
			!condition.LastTransitionTime.IsZero() {
			return condition.LastTransitionTime.Time
		}
	}
	return job.CreationTimestamp.Time
}
