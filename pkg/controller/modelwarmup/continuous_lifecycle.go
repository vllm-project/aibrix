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
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

func (r *ModelWarmupReconciler) reconcileContinuous(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string]resolvedTarget,
	jobs []batchv1.Job,
) (ctrl.Result, error) {
	now := time.Now()
	policies := effectiveWarmupPolicies(warmup)
	plan := buildContinuousPlan(warmup, revision, targets, jobs, policies, now)
	for i := range plan.DeleteBeforeCreate {
		if err := r.deleteContinuousJob(ctx, &plan.DeleteBeforeCreate[i]); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	created := make([]*batchv1.Job, 0, len(plan.Create))
	for _, action := range plan.Create {
		job := r.jobForTarget(warmup, action.Target, revision, action.Attempt)
		if err := ctrl.SetControllerReference(warmup, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("create Continuous ModelWarmup Job %s/%s: %w", job.Namespace, job.Name, err)
			}
			existing := &batchv1.Job{}
			if getErr := r.Get(ctx, client.ObjectKeyFromObject(job), existing); getErr != nil {
				return ctrl.Result{}, fmt.Errorf("diagnose existing Continuous ModelWarmup Job %s/%s: %w",
					job.Namespace, job.Name, getErr)
			}
			if !sameContinuousJobIdentity(existing, job, warmup) {
				return ctrl.Result{}, fmt.Errorf("job %s/%s already exists with a different Continuous identity",
					existing.Namespace, existing.Name)
			}
			job = existing
		}
		created = append(created, job)
		if action.DeleteAfterCreate != nil {
			if err := r.deleteContinuousJob(ctx, action.DeleteAfterCreate); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	result := ctrl.Result{RequeueAfter: plan.RetryAfter}
	return r.updateContinuousStatus(ctx, warmup, revision, targets, plan, created, policies, now, result)
}

func (r *ModelWarmupReconciler) deleteContinuousJob(ctx context.Context, job *batchv1.Job) error {
	return r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
}

func sameContinuousJobIdentity(existing, desired *batchv1.Job, warmup *modelv1alpha1.ModelWarmup) bool {
	return metav1.IsControlledBy(existing, warmup) &&
		existing.Labels[WarmupLabelKey] == desired.Labels[WarmupLabelKey] &&
		existing.Labels[RevisionLabelKey] == desired.Labels[RevisionLabelKey] &&
		existing.Annotations[TargetNodeAnnotationKey] == desired.Annotations[TargetNodeAnnotationKey] &&
		existing.Annotations[TargetNodeUIDAnnotationKey] == desired.Annotations[TargetNodeUIDAnnotationKey] &&
		existing.Annotations[AttemptAnnotationKey] == desired.Annotations[AttemptAnnotationKey]
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

func (r *ModelWarmupReconciler) updateContinuousStatus(
	ctx context.Context,
	warmup *modelv1alpha1.ModelWarmup,
	revision string,
	targets map[string]resolvedTarget,
	plan continuousPlan,
	created []*batchv1.Job,
	policies warmupPolicies,
	now time.Time,
	result ctrl.Result,
) (ctrl.Result, error) {
	desired := warmup.DeepCopy()
	if desired.Status.StartTime == nil {
		start := metav1.NewTime(now)
		desired.Status.StartTime = &start
	}
	desired.Status.CompletionTime = nil

	previousTargets := make(map[string]modelv1alpha1.ModelWarmupTargetStatus, len(desired.Status.Targets))
	for _, target := range desired.Status.Targets {
		previousTargets[target.NodeName] = target
	}
	jobsByTarget := make(map[types.UID]batchv1.Job, len(plan.JobsByTarget)+len(created))
	for uid, job := range plan.JobsByTarget {
		jobsByTarget[uid] = job
	}
	for _, job := range created {
		if job != nil {
			jobsByTarget[types.UID(job.Annotations[TargetNodeUIDAnnotationKey])] = *job
		}
	}

	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	details := make([]modelv1alpha1.ModelWarmupTargetStatus, 0, len(targets))
	active, pending, succeeded, failed, retryableFailures := int32(0), int32(0), int32(0), int32(0), int32(0)
	for _, name := range names {
		target := targets[name]
		item := modelv1alpha1.ModelWarmupTargetStatus{
			NodeName: name, Source: primarySource(target.Sources), SourceCount: int32(len(target.Sources)), Revision: revision,
			Phase: modelv1alpha1.ModelWarmupTargetPending, Message: boundedDiagnostic("waiting for a warmup job"),
		}
		job, exists := jobsByTarget[target.NodeUID]
		if !exists {
			pending++
		} else {
			item.JobName = job.Name
			item.Attempt = continuousJobAttempt(&job)
			switch {
			case isJobComplete(&job):
				succeeded++
				continue
			case isJobFailed(&job):
				item.Phase = modelv1alpha1.ModelWarmupTargetFailed
				item.Reason, item.Message = jobFailureDetails(&job)
				item.Reason = boundedDiagnostic(item.Reason)
				item.Message = boundedDiagnostic(item.Message)
				failed++
				if item.Attempt < 1+policies.continuousRetryLimit {
					retryableFailures++
				}
			default:
				item.Phase = modelv1alpha1.ModelWarmupTargetRunning
				item.Message = boundedDiagnostic("warmup job is running")
				active++
			}
		}
		previous := previousTargets[name]
		preserveTargetTransition(&item, previous.LastTransitionTime, previous.Phase,
			previous.Reason, previous.Message, now)
		details = append(details, item)
	}
	sort.Slice(details, func(i, j int) bool {
		leftPriority := targetDetailPriority(details[i].Phase)
		rightPriority := targetDetailPriority(details[j].Phase)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return details[i].NodeName < details[j].NodeName
	})
	omitted := 0
	if len(details) > modelv1alpha1.MaxModelWarmupTargetDetails {
		omitted = len(details) - modelv1alpha1.MaxModelWarmupTargetDetails
		details = details[:modelv1alpha1.MaxModelWarmupTargetDetails]
	}

	desired.Status.ObservedRevision = revision
	desired.Status.DesiredNodes = int32(len(targets))
	desired.Status.ActiveNodes = active
	desired.Status.SucceededNodes = succeeded
	desired.Status.FailedNodes = failed
	desired.Status.OmittedTargetDetails = int32(omitted)
	desired.Status.Targets = details
	progressing := active+pending > 0 || retryableFailures > 0
	switch {
	case plan.TargetLimitExceeded:
		desired.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		setContinuousConditions(desired, false, false, true,
			"TargetLimitExceeded", fmt.Sprintf("resolved %d targets; maximum is %d", len(targets), modelv1alpha1.MaxModelWarmupTargets), now)
	case len(targets) == 0:
		desired.Status.Phase = modelv1alpha1.ModelWarmupPending
		setContinuousConditions(desired, false, false, false,
			"NoTargetsResolved", "waiting for target nodes to match the configured selectors", now)
	case failed > 0:
		desired.Status.Phase = modelv1alpha1.ModelWarmupDegraded
		setContinuousConditions(desired, false, progressing, true,
			"TargetFailed", "one or more current target jobs failed", now)
	case active+pending > 0:
		desired.Status.Phase = modelv1alpha1.ModelWarmupRunning
		setContinuousConditions(desired, false, true, false,
			"JobsRunning", "waiting for current target jobs", now)
	default:
		desired.Status.Phase = modelv1alpha1.ModelWarmupReady
		setContinuousConditions(desired, true, false, false,
			"CurrentTargetsSucceeded", "all current target jobs succeeded", now)
	}
	if desired.Status.Phase == modelv1alpha1.ModelWarmupReady && warmup.Status.Phase != modelv1alpha1.ModelWarmupReady {
		converged := metav1.NewTime(now)
		desired.Status.LastConvergedTime = &converged
	}
	if modelWarmupStatusEqual(warmup.Status, desired.Status) {
		return result, nil
	}
	if err := r.Status().Update(ctx, desired); err != nil {
		return result, err
	}
	desired.DeepCopyInto(warmup)
	return result, nil
}

func setContinuousConditions(
	warmup *modelv1alpha1.ModelWarmup,
	ready, progressing, degraded bool,
	reason, message string,
	now time.Time,
) {
	transition := metav1.NewTime(now)
	conditions := make([]metav1.Condition, 0, 3)
	for _, conditionType := range []string{"Ready", "Progressing", "Degraded"} {
		active := ready && conditionType == "Ready" || progressing && conditionType == "Progressing" ||
			degraded && conditionType == "Degraded"
		status := metav1.ConditionFalse
		conditionReason, conditionMessage := "NotActive", "condition is not active"
		if active {
			status = metav1.ConditionTrue
			conditionReason, conditionMessage = reason, message
		}
		condition := metav1.Condition{Type: conditionType, Status: status, Reason: conditionReason,
			Message: conditionMessage, ObservedGeneration: warmup.Generation, LastTransitionTime: transition}
		if old := meta.FindStatusCondition(warmup.Status.Conditions, conditionType); old != nil &&
			old.Status == condition.Status && old.Reason == condition.Reason && old.Message == condition.Message {
			condition.LastTransitionTime = old.LastTransitionTime
		}
		conditions = append(conditions, condition)
	}
	warmup.Status.Conditions = conditions
}
