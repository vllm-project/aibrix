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
	"encoding/json"
	"fmt"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/event"

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
	require.Empty(t, job.Spec.Template.Spec.NodeName)
	require.Equal(t, "gpu-node-a", job.Annotations[TargetNodeAnnotationKey])
	require.Equal(t, []string{"gpu-node-a"}, job.Spec.Template.Spec.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values)
	require.EqualValues(t, 3, *job.Spec.BackoffLimit)
	require.EqualValues(t, 60, *job.Spec.TTLSecondsAfterFinished)
	require.EqualValues(t, modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds, *job.Spec.ActiveDeadlineSeconds)
	require.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)
	require.Empty(t, job.Spec.Template.Spec.Tolerations)
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
	setCondition(warmup, "Complete", metav1.ConditionTrue, "Complete", "done")
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

func TestEffectiveWarmupPoliciesUsesControllerDefaultsAndOverrides(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{}
	effective := effectiveWarmupPolicies(warmup)
	require.Equal(t, modelv1alpha1.DefaultModelWarmupParallelism, effective.parallelism)
	require.Equal(t, modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds, effective.jobTimeoutSeconds)

	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{
		Parallelism: ptr.To[int32](2), JobTimeoutSeconds: ptr.To[int64](61),
	}
	effective = effectiveWarmupPolicies(warmup)
	require.Equal(t, int32(2), effective.parallelism)
	require.Equal(t, int64(61), effective.jobTimeoutSeconds)
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
			Type:   batchv1.JobFailed,
			Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "image pull failed",
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
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{ResourcePoolLabelKey: "warm"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{
			"pool": "warm", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
		}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{
			"pool": "warm", ResourcePoolLabelKey: "warm", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
		}}},
	}
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodes...).Build()}
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"},
		Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{
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
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{
		ResourcePoolLabelKey: "warm",
	}}}
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()}
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"},
		Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"missing-node"}},
		}}}}
	targets, missing, err := r.resolveTargets(context.Background(), warmup)
	require.NoError(t, err)
	require.Empty(t, targets)
	require.Equal(t, "NodeNotFound", missing["missing-node"])
}

func TestResolveTargetsRejectsUnauthorizedNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{
		ResourcePoolLabelKey: "pool-a",
	}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{
		ResourcePoolLabelKey: "pool-b", WarmupEnabledLabelKey: WarmupEnabledLabelValue,
	}}}
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(namespace, node).Build()}
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"},
		Spec: modelv1alpha1.ModelWarmupSpec{Targets: []modelv1alpha1.ModelWarmupTarget{{
			Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-b"}},
		}}}}

	targets, missing, err := r.resolveTargets(context.Background(), warmup)
	require.NoError(t, err)
	require.Empty(t, targets)
	require.Equal(t, "NodeNotAuthorized", missing["node-b"])
}

func TestTargetLimitIncludesMissingNodes(t *testing.T) {
	targets := make(map[string][]string, modelv1alpha1.MaxModelWarmupTargets)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargets; i++ {
		targets[fmt.Sprintf("node-%d", i)] = []string{"target[0]"}
	}
	require.True(t, withinTargetLimit(targets, nil))
	require.False(t, withinTargetLimit(targets, map[string]string{"missing": "NodeNotFound"}))
}

func TestRevisionChangesOnlyForImageWorkloadInputs(t *testing.T) {
	base := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		ImagePreload: modelv1alpha1.ModelWarmupImagePreload{
			Images: []modelv1alpha1.ModelWarmupImage{{Image: "busybox", Command: []string{"true"},
				ImagePullPolicy: corev1.PullIfNotPresent}},
		},
		Policies: &modelv1alpha1.ModelWarmupPolicies{Parallelism: ptr.To[int32](1),
			JobTimeoutSeconds: ptr.To[int64](60), RetryLimit: ptr.To[int32](1), TTLSecondsAfterFinished: ptr.To[int32](60)},
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
	withPolicyChange := base.DeepCopy()
	*withPolicyChange.Spec.Policies.JobTimeoutSeconds = 61
	require.Equal(t, baseRevision, revisionFor(withPolicyChange))
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
	require.Equal(t, modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds, *job.Spec.ActiveDeadlineSeconds)
	require.Nil(t, job.Spec.Template.Spec.SecurityContext)
	require.Empty(t, job.Spec.Template.Spec.Volumes)
	require.Empty(t, job.Spec.Template.Spec.Containers[0].Resources.Requests)
}

func TestJobNamePreservesNodeAndRevisionSuffix(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: strings.Repeat("a", 63), Namespace: "default",
	}}
	revision := "123456789abc"
	first := (&ModelWarmupReconciler{}).jobFor(warmup, "node-a", revision)
	second := (&ModelWarmupReconciler{}).jobFor(warmup, "node-b", revision)

	require.LessOrEqual(t, len(first.Name), 63)
	require.True(t, strings.HasSuffix(first.Name, "-"+shortHash("node-a")+"-"+revision))
	require.True(t, strings.HasSuffix(second.Name, "-"+shortHash("node-b")+"-"+revision))
	require.NotEqual(t, first.Name, second.Name)
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
	completed.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	}}
	retrying := running.DeepCopy()
	retrying.Name = "retrying"
	retrying.Status.Failed = 1
	scheme := runtime.NewScheme()
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(running, completed, retrying).Build()}
	targets := map[string][]string{"node-b": {"target[0]"}}
	require.NoError(t, r.cleanupStaleJobs(context.Background(), warmup, "new", targets))
	require.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{}))
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(completed), &batchv1.Job{}))
	require.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(retrying), &batchv1.Job{}))
	err := r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{})
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(err))
}

func TestRetryingJobRemainsActiveUntilTerminalFailure(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "retrying", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: warmup.Name, RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{{UID: warmup.UID}},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}},
		},
		Status: batchv1.JobStatus{Failed: 1},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup, job).Build()}

	active, err := r.activeJobs(context.Background(), warmup, "rev")
	require.NoError(t, err)
	require.Equal(t, int32(1), active)
	_, err = r.updateStatus(
		context.Background(),
		warmup,
		"rev",
		map[string][]string{"node-a": {"target[0]"}},
		nil,
		"",
		"",
	)
	require.NoError(t, err)
	require.Equal(t, modelv1alpha1.ModelWarmupRunning, warmup.Status.Phase)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning, warmup.Status.Targets[0].Phase)
}

func TestUpdateStatusWaitsWhenSelectorResolvesNoTargets(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build()}

	_, err := r.updateStatus(context.Background(), warmup, "rev", nil, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, modelv1alpha1.ModelWarmupPending, warmup.Status.Phase)
	require.Nil(t, warmup.Status.CompletionTime)
	progressing := mustCondition(warmup.Status.Conditions, "Progressing")
	require.Equal(t, metav1.ConditionTrue, progressing.Status)
	require.Equal(t, "NoTargetsResolved", progressing.Reason)
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
	require.Equal(t, metav1.ConditionFalse, mustCondition(warmup.Status.Conditions, "Complete").Status)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Progressing").Status)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: warmup.Name, RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{{UID: warmup.UID}}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
		Status: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	require.NoError(t, r.Create(context.Background(), job))
	_, err = r.updateStatus(context.Background(), warmup, "rev", targets, nil, "", "")
	require.NoError(t, err)
	require.Empty(t, warmup.Status.Targets)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Complete").Status)
}

func TestUpdateStatusBoundsDetailsAndSerializedSize(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build()}
	missing := make(map[string]string, modelv1alpha1.MaxModelWarmupTargets)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargets; i++ {
		missing[fmt.Sprintf("node-%04d", i)] = "NodeNotFound"
	}

	_, err := r.updateStatus(context.Background(), warmup, "rev", nil, missing, "", "")
	require.NoError(t, err)
	require.Len(t, warmup.Status.Targets, modelv1alpha1.MaxModelWarmupTargetDetails)
	require.EqualValues(t, modelv1alpha1.MaxModelWarmupTargets-modelv1alpha1.MaxModelWarmupTargetDetails,
		warmup.Status.OmittedTargetDetails)
	encoded, err := json.Marshal(warmup)
	require.NoError(t, err)
	require.Less(t, len(encoded), 512*1024)

	worstCase := warmup.DeepCopy()
	for i := range worstCase.Status.Targets {
		worstCase.Status.Targets[i].NodeName = fmt.Sprintf("%s-%03d", strings.Repeat("n", 249), i)
		worstCase.Status.Targets[i].JobName = strings.Repeat("j", 63)
		worstCase.Status.Targets[i].Reason = strings.Repeat("r", modelv1alpha1.MaxModelWarmupDiagnosticLength)
		worstCase.Status.Targets[i].Message = strings.Repeat("m", modelv1alpha1.MaxModelWarmupDiagnosticLength)
	}
	encoded, err = json.Marshal(worstCase)
	require.NoError(t, err)
	require.Less(t, len(encoded), 1024*1024)
}

func TestUpdateStatusRetainsFailedDetailsBeforePendingWhenTruncated(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build()}
	targets := make(map[string][]string, modelv1alpha1.MaxModelWarmupTargetDetails)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargetDetails; i++ {
		targets[fmt.Sprintf("node-%04d", i)] = []string{"target[0]"}
	}

	_, err := r.updateStatus(context.Background(), warmup, "rev", targets,
		map[string]string{"zzz-failed": "NodeNotFound"}, "", "")
	require.NoError(t, err)
	require.EqualValues(t, modelv1alpha1.MaxModelWarmupTargetDetails+1, warmup.Status.DesiredNodes)
	require.Equal(t, int32(1), warmup.Status.FailedNodes)
	require.Equal(t, int32(1), warmup.Status.OmittedTargetDetails)
	require.Len(t, warmup.Status.Targets, modelv1alpha1.MaxModelWarmupTargetDetails)
	require.Equal(t, "zzz-failed", warmup.Status.Targets[0].NodeName)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetFailed, warmup.Status.Targets[0].Phase)
}

func TestTerminalOnceReturnsBeforeResolvingTargets(t *testing.T) {
	warmup := validWarmupForControllerTest("tenant", "warmup")
	warmup.Status.Phase = modelv1alpha1.ModelWarmupSucceeded
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup).WithObjects(warmup).Build(), Scheme: scheme}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})
	require.NoError(t, err)
	require.Zero(t, result)
	var jobs batchv1.JobList
	require.NoError(t, r.List(context.Background(), &jobs))
	require.Empty(t, jobs.Items)
}

func TestNodeEventsEnqueueOnlyActiveWarmupsAndIgnoreHeartbeats(t *testing.T) {
	active := validWarmupForControllerTest("tenant", "active")
	terminal := validWarmupForControllerTest("tenant", "terminal")
	terminal.Status.Phase = modelv1alpha1.ModelWarmupSucceeded
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(active, terminal).Build()

	requests := enqueueActiveModelWarmups(c)(context.Background(), &corev1.Node{})
	require.Equal(t, []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(active)}}, requests)

	predicate := nodeMembershipChanged()
	oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "a"}}}
	heartbeat := oldNode.DeepCopy()
	heartbeat.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	require.False(t, predicate.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: heartbeat}))
	labelUpdate := oldNode.DeepCopy()
	labelUpdate.Labels["pool"] = "b"
	require.True(t, predicate.Update(event.UpdateEvent{ObjectOld: oldNode, ObjectNew: labelUpdate}))
}

func validWarmupForControllerTest(namespace, name string) *modelv1alpha1.ModelWarmup {
	return &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: modelv1alpha1.ModelWarmupSpec{
			Targets: []modelv1alpha1.ModelWarmupTarget{{
				Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
			}},
			ImagePreload: modelv1alpha1.ModelWarmupImagePreload{Images: []modelv1alpha1.ModelWarmupImage{{
				Image: "busybox:1.36", Command: []string{"true"},
			}}},
		}}
}

func mustCondition(conditions []metav1.Condition, typ string) metav1.Condition {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition
		}
	}
	panic("condition not found")
}
