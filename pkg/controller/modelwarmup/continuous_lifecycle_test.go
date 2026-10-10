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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestContinuousPlanCreatesFirstAttemptForNewTarget(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	targets := map[string]resolvedTarget{
		"node-a": {NodeName: "node-a", NodeUID: types.UID("uid-a")},
	}
	policies := warmupPolicies{parallelism: 1, continuousRetryLimit: 2, continuousRetryIntervalSeconds: 300}

	plan := buildContinuousPlan(warmup, "revision", targets, nil, policies, time.Unix(1000, 0))

	require.False(t, plan.TargetLimitExceeded)
	require.Empty(t, plan.DeleteBeforeCreate)
	require.Len(t, plan.Create, 1)
	require.Equal(t, targets["node-a"], plan.Create[0].Target)
	require.Equal(t, int32(1), plan.Create[0].Attempt)
	require.Nil(t, plan.Create[0].DeleteAfterCreate)
	require.Zero(t, plan.RetryAfter)
}

func TestContinuousPlanUsesNodeUIDAndCleansDepartedJobs(t *testing.T) {
	warmup := continuousTestWarmup()
	now := time.Unix(1000, 0)
	tests := []struct {
		name        string
		targets     map[string]resolvedTarget
		jobs        []batchv1.Job
		wantCreate  int
		wantDelete  []string
		wantCurrent int
	}{
		{
			name: "matching successful UID remains covered",
			targets: map[string]resolvedTarget{
				"node-a": {NodeName: "node-a", NodeUID: "uid-a"},
			},
			jobs:        []batchv1.Job{continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, batchv1.JobComplete, now)},
			wantCurrent: 1,
		},
		{
			name: "same name replacement deletes old UID before creating",
			targets: map[string]resolvedTarget{
				"node-a": {NodeName: "node-a", NodeUID: "uid-new"},
			},
			jobs:       []batchv1.Job{continuousTestJob(warmup, "node-a", "uid-old", "revision", 1, batchv1.JobComplete, now)},
			wantDelete: []string{modelWarmupJobName(warmup.Name, string(warmup.UID), "node-a", "revision")},
		},
		{
			name:        "departed target deletes retained success",
			targets:     map[string]resolvedTarget{},
			jobs:        []batchv1.Job{continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, batchv1.JobComplete, now)},
			wantDelete:  []string{modelWarmupJobName(warmup.Name, string(warmup.UID), "node-a", "revision")},
			wantCurrent: 0,
		},
		{
			name: "malformed identity is deleted",
			targets: map[string]resolvedTarget{
				"node-a": {NodeName: "node-a", NodeUID: "uid-a"},
			},
			jobs: func() []batchv1.Job {
				job := continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, "", now)
				delete(job.Annotations, AttemptAnnotationKey)
				return []batchv1.Job{job}
			}(),
			wantDelete: []string{modelWarmupJobName(warmup.Name, string(warmup.UID), "node-a", "revision")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := buildContinuousPlan(warmup, "revision", tt.targets, tt.jobs,
				warmupPolicies{parallelism: 1, continuousRetryLimit: 2, continuousRetryIntervalSeconds: 300}, now)
			require.Len(t, plan.Create, tt.wantCreate)
			require.ElementsMatch(t, tt.wantDelete, continuousJobNames(plan.DeleteBeforeCreate))
			require.Len(t, plan.JobsByTarget, tt.wantCurrent)
		})
	}
}

func TestContinuousPlanRespectsActiveParallelismAndTargetLimit(t *testing.T) {
	warmup := continuousTestWarmup()
	now := time.Unix(1000, 0)
	targets := map[string]resolvedTarget{
		"node-a": {NodeName: "node-a", NodeUID: "uid-a"},
		"node-b": {NodeName: "node-b", NodeUID: "uid-b"},
	}
	active := continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, "", now)
	active.DeletionTimestamp = &metav1.Time{Time: now}

	plan := buildContinuousPlan(warmup, "revision", targets, []batchv1.Job{active},
		warmupPolicies{parallelism: 1, continuousRetryLimit: 2, continuousRetryIntervalSeconds: 300}, now)
	require.Empty(t, plan.Create, "a deleting non-terminal Job still consumes capacity")

	overLimit := make(map[string]resolvedTarget, modelv1alpha1.MaxModelWarmupTargets+1)
	for i := 0; i <= modelv1alpha1.MaxModelWarmupTargets; i++ {
		name := fmt.Sprintf("node-%04d", i)
		overLimit[name] = resolvedTarget{NodeName: name, NodeUID: types.UID("uid-" + name)}
	}
	plan = buildContinuousPlan(warmup, "revision", overLimit, []batchv1.Job{active},
		warmupPolicies{parallelism: 100, continuousRetryLimit: 2, continuousRetryIntervalSeconds: 300}, now)
	require.True(t, plan.TargetLimitExceeded)
	require.Empty(t, plan.Create)
	require.Empty(t, plan.DeleteBeforeCreate)
}

func TestContinuousPlanSchedulesBoundedRetries(t *testing.T) {
	warmup := continuousTestWarmup()
	target := resolvedTarget{NodeName: "node-a", NodeUID: "uid-a"}
	targets := map[string]resolvedTarget{"node-a": target}
	failedAt := time.Unix(1000, 0)
	policies := warmupPolicies{parallelism: 1, continuousRetryLimit: 2, continuousRetryIntervalSeconds: 300}

	t.Run("waits until retry deadline", func(t *testing.T) {
		failed := continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, batchv1.JobFailed, failedAt)
		plan := buildContinuousPlan(warmup, "revision", targets, []batchv1.Job{failed}, policies, failedAt.Add(100*time.Second))

		require.Empty(t, plan.Create)
		require.Equal(t, 200*time.Second, plan.RetryAfter)
		require.Empty(t, plan.DeleteBeforeCreate)
	})

	t.Run("creates next attempt before deleting predecessor", func(t *testing.T) {
		failed := continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, batchv1.JobFailed, failedAt)
		plan := buildContinuousPlan(warmup, "revision", targets, []batchv1.Job{failed}, policies, failedAt.Add(300*time.Second))

		require.Zero(t, plan.RetryAfter)
		require.Len(t, plan.Create, 1)
		require.Equal(t, int32(2), plan.Create[0].Attempt)
		require.Equal(t, failed.Name, plan.Create[0].DeleteAfterCreate.Name)
		require.Empty(t, plan.DeleteBeforeCreate)
	})

	t.Run("stops after configured additional attempts", func(t *testing.T) {
		failed := continuousTestJob(warmup, "node-a", "uid-a", "revision", 3, batchv1.JobFailed, failedAt)
		plan := buildContinuousPlan(warmup, "revision", targets, []batchv1.Job{failed}, policies, failedAt.Add(time.Hour))

		require.Empty(t, plan.Create)
		require.Zero(t, plan.RetryAfter)
		require.Empty(t, plan.DeleteBeforeCreate)
	})

	t.Run("restart keeps newest attempt and cleans older record", func(t *testing.T) {
		first := continuousTestJob(warmup, "node-a", "uid-a", "revision", 1, batchv1.JobFailed, failedAt)
		second := continuousTestJob(warmup, "node-a", "uid-a", "revision", 2, "", failedAt.Add(time.Second))
		plan := buildContinuousPlan(warmup, "revision", targets, []batchv1.Job{first, second}, policies, failedAt.Add(time.Hour))

		require.Empty(t, plan.Create)
		require.Equal(t, second.Name, plan.JobsByTarget[target.NodeUID].Name)
		require.ElementsMatch(t, []string{first.Name}, continuousJobNames(plan.DeleteBeforeCreate))
	})
}

func continuousTestWarmup() *modelv1alpha1.ModelWarmup {
	return &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
}

func continuousTestJob(
	warmup *modelv1alpha1.ModelWarmup,
	nodeName string,
	nodeUID types.UID,
	revision string,
	attempt int32,
	condition batchv1.JobConditionType,
	transition time.Time,
) batchv1.Job {
	job := (&ModelWarmupReconciler{}).jobForTarget(warmup, resolvedTarget{
		NodeName: nodeName,
		NodeUID:  nodeUID,
	}, revision, attempt)
	job.OwnerReferences = []metav1.OwnerReference{controllerOwnerReference(warmup)}
	if condition != "" {
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(transition),
		}}
	}
	return *job
}

func continuousJobNames(jobs []batchv1.Job) []string {
	names := make([]string, 0, len(jobs))
	for i := range jobs {
		names = append(names, jobs[i].Name)
	}
	return names
}
