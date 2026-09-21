/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package modelwarmup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestJobForBuildsSafeNodePinnedTemplate(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default"},
		Spec: modelv1alpha1.ModelWarmupSpec{
			ImagePreload: modelv1alpha1.ModelWarmupImagePreload{
				PullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
				Images: []modelv1alpha1.ModelWarmupImage{{
					Image: "busybox@sha256:abc", Command: []string{"sh", "-c", "exit 0"}, ImagePullPolicy: corev1.PullNever,
				}},
			},
			Policies: &modelv1alpha1.ModelWarmupPolicies{
				RetryLimit: ptr.To[int32](3), TTLSecondsAfterFinished: ptr.To[int32](60),
			},
		},
	}
	job := (&ModelWarmupReconciler{}).jobFor(warmup, "gpu-node-a", "revision")
	require.Equal(t, "gpu-node-a", job.Spec.Template.Spec.NodeName)
	require.EqualValues(t, 3, *job.Spec.BackoffLimit)
	require.EqualValues(t, 60, *job.Spec.TTLSecondsAfterFinished)
	require.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)
	require.Len(t, job.Spec.Template.Spec.Tolerations, 1)
	require.Equal(t, corev1.TolerationOpExists, job.Spec.Template.Spec.Tolerations[0].Operator)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	require.Equal(t, corev1.PullNever, container.ImagePullPolicy)
	require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.Empty(t, job.Spec.Template.Spec.Volumes)
}

func TestRevisionExcludesTargetMembershipAndIncludesTemplateInput(t *testing.T) {
	base := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		ImagePreload: modelv1alpha1.ModelWarmupImagePreload{Images: []modelv1alpha1.ModelWarmupImage{{
			Image: "busybox", Command: []string{"true"}, ImagePullPolicy: corev1.PullIfNotPresent,
		}}},
	}}
	revision := revisionFor(base)
	withTarget := base.DeepCopy()
	withTarget.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
	}}
	require.Equal(t, revision, revisionFor(withTarget))
	changed := base.DeepCopy()
	changed.Spec.ImagePreload.Images[0].ImagePullPolicy = corev1.PullAlways
	require.NotEqual(t, revision, revisionFor(changed))
}

func TestSetConditionKeepsConditionsMutuallyExclusive(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{}
	setCondition(warmup, "Ready", metav1.ConditionTrue, "Ready", "done")
	setCondition(warmup, "Degraded", metav1.ConditionTrue, "Failed", "failed")

	require.Len(t, warmup.Status.Conditions, 3)
	for _, condition := range warmup.Status.Conditions {
		if condition.Type == "Degraded" {
			require.Equal(t, metav1.ConditionTrue, condition.Status)
			require.WithinDuration(t, time.Now(), condition.LastTransitionTime.Time, time.Second)
		} else {
			require.Equal(t, metav1.ConditionFalse, condition.Status)
		}
	}
}

func TestUpdateStatusUsesGlobalStartTimeForPendingTimeout(t *testing.T) {
	start := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
		Spec: modelv1alpha1.ModelWarmupSpec{
			Targets: []modelv1alpha1.ModelWarmupTarget{{
				Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
			}},
			Policies: &modelv1alpha1.ModelWarmupPolicies{GlobalTimeoutSeconds: ptr.To[int64](60)},
		},
		Status: modelv1alpha1.ModelWarmupStatus{StartTime: &start},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build()}
	targets := map[string][]string{"node-a": {"target[0]"}}
	_, err := r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetFailed, warmup.Status.Targets[0].Phase)
	require.Equal(t, "Timeout", warmup.Status.Targets[0].Reason)
	require.Contains(t, warmup.Status.Targets[0].Message, "global timeout")
}

func TestRemainingGlobalTimeoutUsesWorkflowStartTime(t *testing.T) {
	now := time.Now()
	start := metav1.NewTime(now.Add(-40 * time.Second))
	warmup := &modelv1alpha1.ModelWarmup{
		Spec: modelv1alpha1.ModelWarmupSpec{Policies: &modelv1alpha1.ModelWarmupPolicies{
			GlobalTimeoutSeconds: ptr.To[int64](60),
		}},
		Status: modelv1alpha1.ModelWarmupStatus{StartTime: &start},
	}

	remaining, timedOut := remainingGlobalTimeout(warmup, now)
	require.False(t, timedOut)
	require.Equal(t, int64(20), remaining)

	remaining, timedOut = remainingGlobalTimeout(warmup, now.Add(21*time.Second))
	require.True(t, timedOut)
	require.Zero(t, remaining)
}

func TestUpdateStatusPreservesJobFailureDiagnostics(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup-job", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: "warmup", RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{{UID: warmup.UID}}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
		Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Reason: "BackoffLimitExceeded", Message: "image pull failed",
		}}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup, job).Build()}
	targets := map[string][]string{"node-a": {"target[0]"}}
	_, err := r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, "BackoffLimitExceeded", warmup.Status.Targets[0].Reason)
	require.Equal(t, "image pull failed", warmup.Status.Targets[0].Message)
}

func TestResolveTargetsDeduplicatesAndPreservesStableSources(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "warm"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"pool": "warm"}}},
	}
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodes...).Build()}
	warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{
		{Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-b", "node-a"}}},
		{NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "warm"}}},
	}}}
	targets, missing, err := r.resolveTargets(context.Background(), warmup)
	require.NoError(t, err)
	require.Empty(t, missing)
	require.Equal(t, []string{"target[0]", "target[1]"}, targets["node-a"])
	require.Equal(t, []string{"target[0]", "target[1]"}, targets["node-b"])
}

func TestResolveTargetsReportsMissingExplicitNode(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	warmup := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"missing-node"}},
	}}}}
	targets, missing, err := r.resolveTargets(context.Background(), warmup)
	require.NoError(t, err)
	require.Empty(t, targets)
	require.Equal(t, "NodeNotFound", missing["missing-node"])
}

func TestTargetLimitIncludesMissingNodes(t *testing.T) {
	targets := make(map[string][]string, modelv1alpha1.MaxModelWarmupTargets)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargets; i++ {
		targets[fmt.Sprintf("node-%d", i)] = []string{"target[0]"}
	}
	require.True(t, withinTargetLimit(targets, nil))
	require.False(t, withinTargetLimit(targets, map[string]string{"missing": "NodeNotFound"}))
}

func TestRevisionChangesForEveryTemplateInput(t *testing.T) {
	base := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		ImagePreload: modelv1alpha1.ModelWarmupImagePreload{
			Images: []modelv1alpha1.ModelWarmupImage{{Image: "busybox", Command: []string{"true"},
				ImagePullPolicy: corev1.PullIfNotPresent}},
		},
		Policies: &modelv1alpha1.ModelWarmupPolicies{Parallelism: ptr.To[int32](1),
			GlobalTimeoutSeconds: ptr.To[int64](60), RetryLimit: ptr.To[int32](1), TTLSecondsAfterFinished: ptr.To[int32](60)},
	}}
	baseRevision := revisionFor(base)
	variants := map[string]func(*modelv1alpha1.ModelWarmup){
		"image":   func(w *modelv1alpha1.ModelWarmup) { w.Spec.ImagePreload.Images[0].Image = "alpine" },
		"command": func(w *modelv1alpha1.ModelWarmup) { w.Spec.ImagePreload.Images[0].Command = []string{"echo"} },
		"args":    func(w *modelv1alpha1.ModelWarmup) { w.Spec.ImagePreload.Images[0].Args = []string{"ok"} },
		"pull policy": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.ImagePreload.Images[0].ImagePullPolicy = corev1.PullAlways
		},
		"secret": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.ImagePreload.PullSecrets = []corev1.LocalObjectReference{{Name: "registry"}}
		},
		"parallelism": func(w *modelv1alpha1.ModelWarmup) { *w.Spec.Policies.Parallelism = 2 },
		"timeout":     func(w *modelv1alpha1.ModelWarmup) { *w.Spec.Policies.GlobalTimeoutSeconds = 61 },
		"retry":       func(w *modelv1alpha1.ModelWarmup) { *w.Spec.Policies.RetryLimit = 2 },
		"ttl":         func(w *modelv1alpha1.ModelWarmup) { *w.Spec.Policies.TTLSecondsAfterFinished = 61 },
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			variant := base.DeepCopy()
			variant.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
				Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
			}}
			mutate(variant)
			require.NotEqual(t, baseRevision, revisionFor(variant))
		})
	}
	withMembership := base.DeepCopy()
	withMembership.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
	}}
	require.Equal(t, baseRevision, revisionFor(withMembership))
}

func TestJobTemplateOwnerReferenceAndDeterministicName(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
		Spec: modelv1alpha1.ModelWarmupSpec{ImagePreload: modelv1alpha1.ModelWarmupImagePreload{
			Images: []modelv1alpha1.ModelWarmupImage{{Image: "busybox", Command: []string{"true"}}},
		}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	job := (&ModelWarmupReconciler{}).jobFor(warmup, "node-a", "rev")
	require.NoError(t, ctrl.SetControllerReference(warmup, job, scheme))
	jobAgain := (&ModelWarmupReconciler{}).jobFor(warmup, "node-a", "rev")
	require.Equal(t, job.Name, jobAgain.Name)
	require.Len(t, job.OwnerReferences, 1)
	require.True(t, *job.OwnerReferences[0].Controller)
	require.Equal(t, warmup.UID, job.OwnerReferences[0].UID)
	require.Equal(t, modelv1alpha1.DefaultModelWarmupGlobalTimeoutSeconds, *job.Spec.ActiveDeadlineSeconds)
	require.Nil(t, job.Spec.Template.Spec.SecurityContext)
	require.Empty(t, job.Spec.Template.Spec.Volumes)
	require.Empty(t, job.Spec.Template.Spec.Containers[0].Resources.Requests)
}

func TestCleanupStaleJobsDeletesRunningAndKeepsCompleted(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default"}}
	running := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default",
			Labels: map[string]string{WarmupLabelKey: warmup.Name, RevisionLabelKey: "old"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
	}
	completed := running.DeepCopy()
	completed.Name = "completed"
	completed.Status.Succeeded = 1
	scheme := runtime.NewScheme()
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(running, completed).Build()}
	targets := map[string][]string{"node-b": {"target[0]"}}
	require.NoError(t, r.cleanupStaleJobs(context.Background(), warmup, "new", targets))
	require.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{}))
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(completed), &batchv1.Job{}))
	err := r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{})
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(err))
}

func TestUpdateStatusPreservesAndUpdatesTargetTransitionTime(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
		Status:     modelv1alpha1.ModelWarmupStatus{StartTime: ptr.To(metav1.Now())},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build()}
	targets := map[string][]string{"node-a": {"target[0]"}}
	_, err := r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	first := *warmup.Status.Targets[0].LastTransitionTime
	_, err = r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, first, *warmup.Status.Targets[0].LastTransitionTime)
	require.Equal(t, metav1.ConditionFalse, mustCondition(warmup.Status.Conditions, "Ready").Status)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Progressing").Status)
	warmup.Status.Targets[0].LastTransitionTime = ptr.To(metav1.NewTime(time.Now().Add(-time.Minute)))
	oldTransition := *warmup.Status.Targets[0].LastTransitionTime
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: warmup.Name, RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{{UID: warmup.UID}}},
		Spec:   batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	require.NoError(t, r.Create(context.Background(), job))
	_, err = r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	require.NotEqual(t, oldTransition, *warmup.Status.Targets[0].LastTransitionTime)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Ready").Status)
}

func mustCondition(conditions []metav1.Condition, typ string) metav1.Condition {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition
		}
	}
	panic("condition not found")
}
