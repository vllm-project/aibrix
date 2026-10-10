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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestReconcileReportsCreatedJobsWithoutRelisting(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Zero(t, result)
	require.Equal(t, 1, counting.nodeLists)
	require.Equal(t, 1, counting.jobLists)
	require.Zero(t, counting.jobGets)
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupRunning, updated.Status.Phase)
	require.Equal(t, int32(1), updated.Status.ActiveNodes)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning, updated.Status.Targets[0].Phase)
	require.NotEmpty(t, updated.Status.Targets[0].JobName)
	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 1)
	require.Equal(t, string(nodes[0].UID), jobs.Items[0].Annotations[TargetNodeUIDAnnotationKey])
	require.NotContains(t, jobs.Items[0].Annotations, AttemptAnnotationKey)
}

func TestReconcileContinuousCreatesFirstAttemptAndReportsRunning(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{
		Parallelism: ptr.To[int32](1), ContinuousRetryLimit: ptr.To[int32](2),
		ContinuousRetryIntervalSeconds: ptr.To[int64](300),
	}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Zero(t, result)
	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 1)
	require.Equal(t, "1", jobs.Items[0].Annotations[AttemptAnnotationKey])
	require.Equal(t, string(nodes[0].UID), jobs.Items[0].Annotations[TargetNodeUIDAnnotationKey])
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupRunning, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletionTime)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, int32(1), updated.Status.Targets[0].Attempt)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning, updated.Status.Targets[0].Phase)
}

func TestReconcileContinuousBecomesReadyAndSkipsSteadyStateWrites(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}
	require.NoError(t, reconcileOnce(r, request))

	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0].DeepCopy()
	completedAt := metav1.Now()
	job.Status.CompletionTime = &completedAt
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: completedAt,
	}}
	require.NoError(t, counting.Client.Status().Update(context.Background(), job))

	require.NoError(t, reconcileOnce(r, request))
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupReady, updated.Status.Phase)
	require.Equal(t, int32(1), updated.Status.SucceededNodes)
	require.Empty(t, updated.Status.Targets)
	require.Nil(t, updated.Status.CompletionTime)
	require.NotNil(t, updated.Status.LastConvergedTime)
	require.Equal(t, metav1.ConditionTrue, mustCondition(updated.Status.Conditions, "Ready").Status)
	require.Equal(t, metav1.ConditionFalse, mustCondition(updated.Status.Conditions, "Progressing").Status)
	require.Equal(t, metav1.ConditionFalse, mustCondition(updated.Status.Conditions, "Degraded").Status)
	firstConverged := updated.Status.LastConvergedTime.DeepCopy()
	statusUpdates := counting.statusUpdates

	require.NoError(t, reconcileOnce(r, request))
	require.Equal(t, statusUpdates, counting.statusUpdates)
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, firstConverged, updated.Status.LastConvergedTime)
	require.Nil(t, job.Spec.TTLSecondsAfterFinished)
}

func TestReconcileContinuousReportsOverlappingFailureConditions(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 2)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "all"}},
	}}
	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{
		Parallelism: ptr.To[int32](2), ContinuousRetryLimit: ptr.To[int32](1),
		ContinuousRetryIntervalSeconds: ptr.To[int64](300),
	}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}
	require.NoError(t, reconcileOnce(r, request))

	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 2)
	failed := jobs.Items[0].DeepCopy()
	failedAt := metav1.Now()
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: failedAt,
		Reason: "BackoffLimitExceeded", Message: "image pull failed",
	}}
	require.NoError(t, counting.Client.Status().Update(context.Background(), failed))

	result, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	require.Greater(t, result.RequeueAfter, time.Duration(0))
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupDegraded, updated.Status.Phase)
	require.Equal(t, int32(1), updated.Status.FailedNodes)
	require.Equal(t, int32(1), updated.Status.ActiveNodes)
	require.Equal(t, metav1.ConditionTrue, mustCondition(updated.Status.Conditions, "Progressing").Status)
	require.Equal(t, metav1.ConditionTrue, mustCondition(updated.Status.Conditions, "Degraded").Status)
	require.Equal(t, int32(1), mustTargetStatus(updated.Status.Targets, targetNodeForJob(failed)).Attempt)
}

func TestReconcileContinuousIgnoresMissingAndUnauthorizedTargets(t *testing.T) {
	warmup, namespace, _, scheme := reconcileTestObjects(t, 0)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"missing-node"}},
	}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nil)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	require.NoError(t, reconcileOnce(r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}))
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupPending, updated.Status.Phase)
	require.Zero(t, updated.Status.DesiredNodes)
	require.Zero(t, updated.Status.FailedNodes)
	require.Empty(t, updated.Status.Targets)
}

func TestReconcileContinuousCreatesRetryBeforeDeletingFailedAttempt(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{
		Parallelism: ptr.To[int32](1), ContinuousRetryLimit: ptr.To[int32](1),
		ContinuousRetryIntervalSeconds: ptr.To[int64](1),
	}
	revision := revisionFor(warmup)
	failed := (&ModelWarmupReconciler{}).jobForTarget(warmup, resolvedTarget{
		NodeName: nodes[0].Name, NodeUID: nodes[0].UID,
	}, revision, 1)
	require.NoError(t, ctrl.SetControllerReference(warmup, failed, scheme))
	failedAt := metav1.NewTime(time.Now().Add(-2 * time.Second))
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: failedAt,
	}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, failed)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 1)
	require.Equal(t, "2", jobs.Items[0].Annotations[AttemptAnnotationKey])
	require.True(t, strings.HasSuffix(jobs.Items[0].Name, "-a2"))
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupRunning, updated.Status.Phase)
	require.Equal(t, int32(2), updated.Status.Targets[0].Attempt)
}

func TestReconcileContinuousStopsAfterRetryBudgetIsExhausted(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{
		Parallelism: ptr.To[int32](1), ContinuousRetryLimit: ptr.To[int32](0),
		ContinuousRetryIntervalSeconds: ptr.To[int64](1),
	}
	failed := (&ModelWarmupReconciler{}).jobForTarget(warmup, resolvedTarget{
		NodeName: nodes[0].Name, NodeUID: nodes[0].UID,
	}, revisionFor(warmup), 1)
	require.NoError(t, ctrl.SetControllerReference(warmup, failed, scheme))
	failedAt := metav1.NewTime(time.Now().Add(-time.Minute))
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: failedAt,
	}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, failed)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Zero(t, result)
	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs, client.InNamespace(warmup.Namespace)))
	require.Len(t, jobs.Items, 1)
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupDegraded, updated.Status.Phase)
	require.Equal(t, metav1.ConditionFalse, mustCondition(updated.Status.Conditions, "Progressing").Status)
	require.Equal(t, metav1.ConditionTrue, mustCondition(updated.Status.Conditions, "Degraded").Status)
}

func TestReconcileContinuousReportsTargetLimitWithoutJobMutations(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, modelv1alpha1.MaxModelWarmupTargets+1)
	warmup.Spec.Mode = modelv1alpha1.ModelWarmupModeContinuous
	warmup.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "all"}},
	}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	require.NoError(t, reconcileOnce(r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}))

	require.Zero(t, counting.createCalls)
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupDegraded, updated.Status.Phase)
	require.Equal(t, int32(modelv1alpha1.MaxModelWarmupTargets+1), updated.Status.DesiredNodes)
	require.Len(t, updated.Status.Targets, modelv1alpha1.MaxModelWarmupTargetDetails)
	require.Equal(t, int32(1+modelv1alpha1.MaxModelWarmupTargets-modelv1alpha1.MaxModelWarmupTargetDetails),
		updated.Status.OmittedTargetDetails)
	require.Equal(t, "TargetLimitExceeded", mustCondition(updated.Status.Conditions, "Degraded").Reason)
}

func TestReconcileSkipsSemanticNoopStatusWrite(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}

	_, err := r.Reconcile(context.Background(), request)
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, 1, counting.statusUpdates)
}

func TestReconcileWritesChangedStatus(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)}
	require.NoError(t, reconcileOnce(r, request))
	var jobs batchv1.JobList
	require.NoError(t, counting.Client.List(context.Background(), &jobs))
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0].DeepCopy()
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	require.NoError(t, counting.Client.Status().Update(context.Background(), job))

	_, err := r.Reconcile(context.Background(), request)

	require.NoError(t, err)
	require.Equal(t, 2, counting.statusUpdates)
	updated := &modelv1alpha1.ModelWarmup{}
	require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
	require.Equal(t, modelv1alpha1.ModelWarmupSucceeded, updated.Status.Phase)
}

func TestReconcileReturnsStatusConflict(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	counting.statusUpdateErr = apierrors.NewConflict(
		schema.GroupResource{Group: modelv1alpha1.GroupVersion.Group, Resource: "modelwarmups"},
		warmup.Name,
		fmt.Errorf("status changed"),
	)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.Error(t, err)
	require.True(t, apierrors.IsConflict(err))
	require.Equal(t, 1, counting.statusUpdates)
}

func TestReconcileDiagnosesForeignJobNameCollision(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	revision := revisionFor(warmup)
	foreign := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[0].Name, revision)
	foreign.OwnerReferences = []metav1.OwnerReference{{UID: "foreign-uid", Controller: ptr.To(true)}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, foreign)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.Error(t, err)
	require.Contains(t, err.Error(), "is not controlled by ModelWarmup")
	require.Contains(t, err.Error(), foreign.Name)
	require.Equal(t, 1, counting.createCalls)
	require.Equal(t, 1, counting.jobGets)
}

func TestReconcileOwnedCollisionWithWrongWarmupLabelRemainsExcluded(t *testing.T) {
	for name, mutateLabels := range map[string]func(*batchv1.Job){
		"missing": func(job *batchv1.Job) { delete(job.Labels, WarmupLabelKey) },
		"changed": func(job *batchv1.Job) { job.Labels[WarmupLabelKey] = "different-warmup" },
	} {
		t.Run(name+"/status-and-ttl", func(t *testing.T) {
			warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
			revision := revisionFor(warmup)
			collision := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[0].Name, revision)
			require.NoError(t, ctrl.SetControllerReference(warmup, collision, scheme))
			mutateLabels(collision)
			collision.Status.Conditions = []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}
			counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, collision)
			r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(warmup),
			})

			require.NoError(t, err)
			updated := &modelv1alpha1.ModelWarmup{}
			require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
			require.Equal(t, modelv1alpha1.ModelWarmupRunning, updated.Status.Phase)
			require.Zero(t, updated.Status.ActiveNodes)
			require.Zero(t, updated.Status.SucceededNodes)
			target := mustTargetStatus(updated.Status.Targets, nodes[0].Name)
			require.Equal(t, modelv1alpha1.ModelWarmupTargetPending, target.Phase)
			require.Empty(t, target.JobName)
			persisted := &batchv1.Job{}
			require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(collision), persisted))
			require.Nil(t, persisted.Spec.TTLSecondsAfterFinished)
		})

		t.Run(name+"/capacity", func(t *testing.T) {
			warmup, namespace, nodes, scheme := reconcileTestObjects(t, 2)
			warmup.Spec.Targets[0].Nodes.Names = []string{nodes[0].Name, nodes[1].Name}
			revision := revisionFor(warmup)
			collision := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[1].Name, revision)
			require.NoError(t, ctrl.SetControllerReference(warmup, collision, scheme))
			mutateLabels(collision)
			counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, collision)
			r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(warmup),
			})

			require.NoError(t, err)
			require.Equal(t, 2, counting.createCalls, "ineligible collision must not consume capacity")
			updated := &modelv1alpha1.ModelWarmup{}
			require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
			require.Equal(t, int32(1), updated.Status.ActiveNodes)
			require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning,
				mustTargetStatus(updated.Status.Targets, nodes[0].Name).Phase)
			require.Equal(t, modelv1alpha1.ModelWarmupTargetPending,
				mustTargetStatus(updated.Status.Targets, nodes[1].Name).Phase)
		})
	}
}

func TestReconcileEligibleOwnedCollisionFromCacheLagIsFoldedIn(t *testing.T) {
	t.Run("status and ttl", func(t *testing.T) {
		warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
		revision := revisionFor(warmup)
		collision := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[0].Name, revision)
		require.NoError(t, ctrl.SetControllerReference(warmup, collision, scheme))
		collision.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, collision)
		counting.omitJobsFromList = map[string]struct{}{collision.Name: {}}
		r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(warmup),
		})

		require.NoError(t, err)
		updated := &modelv1alpha1.ModelWarmup{}
		require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
		require.Equal(t, modelv1alpha1.ModelWarmupSucceeded, updated.Status.Phase)
		require.Equal(t, int32(1), updated.Status.SucceededNodes)
		persisted := &batchv1.Job{}
		require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(collision), persisted))
		require.Equal(t, ptr.To(modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished),
			persisted.Spec.TTLSecondsAfterFinished)
	})

	t.Run("active capacity", func(t *testing.T) {
		warmup, namespace, nodes, scheme := reconcileTestObjects(t, 2)
		warmup.Spec.Targets[0].Nodes.Names = []string{nodes[0].Name, nodes[1].Name}
		revision := revisionFor(warmup)
		collision := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[1].Name, revision)
		require.NoError(t, ctrl.SetControllerReference(warmup, collision, scheme))
		counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, collision)
		counting.omitJobsFromList = map[string]struct{}{collision.Name: {}}
		r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(warmup),
		})

		require.NoError(t, err)
		require.Equal(t, 1, counting.createCalls, "eligible active collision must consume capacity")
		updated := &modelv1alpha1.ModelWarmup{}
		require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(warmup), updated))
		require.Equal(t, int32(1), updated.Status.ActiveNodes)
		require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning,
			mustTargetStatus(updated.Status.Targets, nodes[1].Name).Phase)
		require.Equal(t, modelv1alpha1.ModelWarmupTargetPending,
			mustTargetStatus(updated.Status.Targets, nodes[0].Name).Phase)
	})
}

func TestReconcileUsesConstantSnapshotReads(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 400)
	warmup.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{
		{NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "all"}}},
		{NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"group": "all"}}},
	}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Equal(t, 1, counting.modelWarmupGets)
	require.Equal(t, 1, counting.namespaceGets)
	require.Equal(t, 1, counting.nodeLists)
	require.Equal(t, 1, counting.jobLists)
	require.Zero(t, counting.jobGets)
}

func TestReconcileAppliesTTLFromOwnedJobSnapshot(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	revision := revisionFor(warmup)
	targetJob := (&ModelWarmupReconciler{}).jobFor(warmup, nodes[0].Name, revision)
	removedTargetJob := (&ModelWarmupReconciler{}).jobFor(warmup, "removed-node", revision)
	for _, job := range []*batchv1.Job{targetJob, removedTargetJob} {
		require.NoError(t, ctrl.SetControllerReference(warmup, job, scheme))
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, targetJob, removedTargetJob)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	for _, original := range []*batchv1.Job{targetJob, removedTargetJob} {
		updated := &batchv1.Job{}
		require.NoError(t, counting.Client.Get(context.Background(), client.ObjectKeyFromObject(original), updated))
		require.Equal(t, ptr.To(modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished),
			updated.Spec.TTLSecondsAfterFinished)
	}
}

func TestJobForBuildsSafeNodePinnedTemplate(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
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
	require.Nil(t, job.Spec.TTLSecondsAfterFinished)
	require.EqualValues(t, modelv1alpha1.DefaultModelWarmupJobTimeoutSeconds, *job.Spec.ActiveDeadlineSeconds)
	require.False(t, *job.Spec.Template.Spec.AutomountServiceAccountToken)
	require.Equal(t, string(warmup.UID), job.Labels[WarmupLabelKey])
	require.Equal(t, warmup.Name, job.Annotations["model.aibrix.ai/warmup-name"])
	require.Empty(t, job.Spec.Template.Spec.Tolerations)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	container := job.Spec.Template.Spec.Containers[0]
	require.Equal(t, corev1.PullNever, container.ImagePullPolicy)
	require.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.Empty(t, job.Spec.Template.Spec.Volumes)
}

func TestJobForMergesImagePreloadAndCustomAction(t *testing.T) {
	privileged := true
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
		Spec: modelv1alpha1.ModelWarmupSpec{
			ImagePreload: modelv1alpha1.ModelWarmupImagePreload{
				Images:      []modelv1alpha1.ModelWarmupImage{{Image: "busybox"}},
				PullSecrets: []corev1.LocalObjectReference{{Name: "shared"}},
			},
			Custom: &modelv1alpha1.ModelWarmupCustomAction{
				InitContainers: []corev1.Container{{
					Name: "check", Image: "check:v1", Command: []string{"sh", "-c", "test -d /cache"},
					SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
				}},
				Containers: []corev1.Container{{
					Name: "download", Image: "download:v1", Command: []string{"sh", "-c"}, Args: []string{"download"},
					Env: []corev1.EnvVar{{Name: "MODEL", Value: "model-a"}},
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					}},
				}},
				Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				}}},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "shared"}, {Name: "custom"}},
			},
		},
	}

	job := (&ModelWarmupReconciler{}).jobFor(warmup, "gpu-node-a", "revision")
	pod := job.Spec.Template.Spec
	require.Equal(t, []string{"check"}, containerNames(pod.InitContainers))
	require.Equal(t, []string{"image-0", "download"}, containerNames(pod.Containers))
	require.Equal(t, []string{"cache"}, volumeNames(pod.Volumes))
	require.Equal(t, []corev1.LocalObjectReference{{Name: "shared"}, {Name: "custom"}}, pod.ImagePullSecrets)
	require.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.False(t, *pod.AutomountServiceAccountToken)
	require.Equal(t, []string{"gpu-node-a"}, pod.Affinity.NodeAffinity.
		RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values)
	require.False(t, *pod.Containers[0].SecurityContext.AllowPrivilegeEscalation)
	require.True(t, *pod.InitContainers[0].SecurityContext.Privileged)
	require.Equal(t, []string{"sh", "-c"}, pod.Containers[1].Command)
	require.Equal(t, "model-a", pod.Containers[1].Env[0].Value)
	require.Equal(t, resource.MustParse("100m"), pod.Containers[1].Resources.Requests[corev1.ResourceCPU])

	warmup.Spec.Custom.Containers[0].Env[0].Value = "mutated"
	require.Equal(t, "model-a", pod.Containers[1].Env[0].Value)

	*pod.InitContainers[0].SecurityContext.Privileged = false
	pod.Volumes[0].EmptyDir.Medium = corev1.StorageMediumMemory
	pod.ImagePullSecrets[1].Name = "mutated"
	require.True(t, *warmup.Spec.Custom.InitContainers[0].SecurityContext.Privileged)
	require.Equal(t, corev1.StorageMediumDefault, warmup.Spec.Custom.Volumes[0].EmptyDir.Medium)
	require.Equal(t, "custom", warmup.Spec.Custom.ImagePullSecrets[1].Name)
}

func TestJobForSupportsCustomActionWithoutImagePreload(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{
		ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
		Spec: modelv1alpha1.ModelWarmupSpec{Custom: &modelv1alpha1.ModelWarmupCustomAction{
			InitContainers: []corev1.Container{{Name: "check", Image: "check:v1"}},
			Containers:     []corev1.Container{{Name: "download", Image: "download:v1"}},
			Volumes: []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/model-cache"},
			}}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "custom"}},
		}},
	}

	pod := (&ModelWarmupReconciler{}).jobFor(warmup, "node-a", "revision").Spec.Template.Spec
	require.Equal(t, []string{"check"}, containerNames(pod.InitContainers))
	require.Equal(t, []string{"download"}, containerNames(pod.Containers))
	require.Equal(t, []string{"host"}, volumeNames(pod.Volumes))
	require.NotNil(t, pod.Volumes[0].HostPath)
	require.Equal(t, []corev1.LocalObjectReference{{Name: "custom"}}, pod.ImagePullSecrets)
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.Name)
	}
	return names
}

func volumeNames(volumes []corev1.Volume) []string {
	names := make([]string, 0, len(volumes))
	for _, volume := range volumes {
		names = append(names, volume.Name)
	}
	return names
}

func TestRevisionExcludesTargetMembershipAndIncludesTemplateInput(t *testing.T) {
	base := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		ImagePreload: modelv1alpha1.ModelWarmupImagePreload{Images: []modelv1alpha1.ModelWarmupImage{{
			Image: "busybox", Command: []string{"true"}, ImagePullPolicy: corev1.PullIfNotPresent,
		}}},
	}}
	revision := revisionFor(base)
	require.Equal(t, "ef7e2d9085ee", revision)
	withTarget := base.DeepCopy()
	withTarget.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
	}}
	require.Equal(t, revision, revisionFor(withTarget))
	changed := base.DeepCopy()
	changed.Spec.ImagePreload.Images[0].ImagePullPolicy = corev1.PullAlways
	require.NotEqual(t, revision, revisionFor(changed))
}

func TestRevisionIncludesCustomActionInputs(t *testing.T) {
	newWarmup := func() *modelv1alpha1.ModelWarmup {
		return &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
			ImagePreload: modelv1alpha1.ModelWarmupImagePreload{Images: []modelv1alpha1.ModelWarmupImage{{Image: "busybox"}}},
			Custom: &modelv1alpha1.ModelWarmupCustomAction{
				InitContainers: []corev1.Container{{Name: "check", Image: "check:v1"}, {Name: "prepare", Image: "prepare:v1"}},
				Containers: []corev1.Container{{Name: "download", Image: "download:v1", Command: []string{"sh", "-c"},
					Env: []corev1.EnvVar{{Name: "MODEL", Value: "model-a"}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					}}}},
				Volumes:          []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "custom"}},
			},
		}}
	}
	base := newWarmup()
	baseRevision := revisionFor(base)
	variants := map[string]func(*modelv1alpha1.ModelWarmup){
		"init image": func(w *modelv1alpha1.ModelWarmup) { w.Spec.Custom.InitContainers[0].Image = "check:v2" },
		"init order": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.Custom.InitContainers[0], w.Spec.Custom.InitContainers[1] =
				w.Spec.Custom.InitContainers[1], w.Spec.Custom.InitContainers[0]
		},
		"regular command": func(w *modelv1alpha1.ModelWarmup) { w.Spec.Custom.Containers[0].Command = []string{"download"} },
		"regular env":     func(w *modelv1alpha1.ModelWarmup) { w.Spec.Custom.Containers[0].Env[0].Value = "model-b" },
		"regular resources": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.Custom.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("200m")
		},
		"volume source": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.Custom.Volumes[0].EmptyDir = nil
			w.Spec.Custom.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/var/lib/model-cache"}
		},
		"custom pull secret": func(w *modelv1alpha1.ModelWarmup) {
			w.Spec.Custom.ImagePullSecrets[0].Name = "other"
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			variant := newWarmup()
			mutate(variant)
			require.NotEqual(t, baseRevision, revisionFor(variant))
		})
	}
	withoutCustom := newWarmup()
	withoutCustom.Spec.Custom = nil
	withoutCustom.Spec.Targets = []modelv1alpha1.ModelWarmupTarget{{
		Nodes: &modelv1alpha1.ModelWarmupNodesTarget{Names: []string{"node-a"}},
	}}
	require.NotEqual(t, baseRevision, revisionFor(withoutCustom))
}

func TestRevisionCanonicalizesCustomActionJSON(t *testing.T) {
	newWarmup := func(requests corev1.ResourceList) *modelv1alpha1.ModelWarmup {
		return &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
			Custom: &modelv1alpha1.ModelWarmupCustomAction{Containers: []corev1.Container{{
				Name: "download", Image: "download:v1",
				Resources: corev1.ResourceRequirements{Requests: requests},
			}}},
		}}
	}
	firstRequests := corev1.ResourceList{}
	firstRequests[corev1.ResourceCPU] = resource.MustParse("100m")
	firstRequests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	secondRequests := corev1.ResourceList{}
	secondRequests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	secondRequests[corev1.ResourceCPU] = resource.MustParse("100m")
	require.Equal(t, revisionFor(newWarmup(firstRequests)), revisionFor(newWarmup(secondRequests)))
	// Numerically equivalent quantities must share a revision even when clients
	// use different string formats.
	secondRequests[corev1.ResourceMemory] = resource.MustParse("1073741824")
	secondRequests[corev1.ResourceCPU] = resource.MustParse("0.1")
	require.Equal(t, revisionFor(newWarmup(firstRequests)), revisionFor(newWarmup(secondRequests)))
	firstMemory := firstRequests[corev1.ResourceMemory]
	require.Equal(t, "1Gi", firstMemory.String())

	firstSize := resource.MustParse("1Gi")
	secondSize := resource.MustParse("1073741824")
	firstVolume := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		Custom: &modelv1alpha1.ModelWarmupCustomAction{Volumes: []corev1.Volume{{
			Name: "cache", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &firstSize},
			},
		}}},
	}}
	secondVolume := firstVolume.DeepCopy()
	secondVolume.Spec.Custom.Volumes[0].EmptyDir.SizeLimit = &secondSize
	require.Equal(t, revisionFor(firstVolume), revisionFor(secondVolume))
	require.Equal(t, "1Gi", firstVolume.Spec.Custom.Volumes[0].EmptyDir.SizeLimit.String())

	firstDivisor := resource.MustParse("1Gi")
	secondDivisor := resource.MustParse("1073741824")
	firstEnv := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		Custom: &modelv1alpha1.ModelWarmupCustomAction{Containers: []corev1.Container{{
			Name: "metrics", Image: "busybox", Env: []corev1.EnvVar{{
				Name: "MEMORY_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
					Resource: "limits.memory", Divisor: firstDivisor,
				}},
			}},
		}}},
	}}
	secondEnv := firstEnv.DeepCopy()
	secondEnv.Spec.Custom.Containers[0].Env[0].ValueFrom.ResourceFieldRef.Divisor = secondDivisor
	require.Equal(t, revisionFor(firstEnv), revisionFor(secondEnv))

	firstStorage := resource.MustParse("1Gi")
	secondStorage := resource.MustParse("1073741824")
	firstEphemeral := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		Custom: &modelv1alpha1.ModelWarmupCustomAction{Volumes: []corev1.Volume{{
			Name: "cache", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{Spec: corev1.PersistentVolumeClaimSpec{
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: firstStorage,
					}},
				}},
			}},
		}}},
	}}
	secondEphemeral := firstEphemeral.DeepCopy()
	secondEphemeral.Spec.Custom.Volumes[0].Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests[corev1.ResourceStorage] = secondStorage
	require.Equal(t, revisionFor(firstEphemeral), revisionFor(secondEphemeral))

	// Custom action slices use omitempty, so nil and empty serialize identically and intentionally share a revision.
	nilCollections := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		Custom: &modelv1alpha1.ModelWarmupCustomAction{},
	}}
	emptyCollections := &modelv1alpha1.ModelWarmup{Spec: modelv1alpha1.ModelWarmupSpec{
		Custom: &modelv1alpha1.ModelWarmupCustomAction{
			InitContainers:   []corev1.Container{},
			Containers:       []corev1.Container{},
			Volumes:          []corev1.Volume{},
			ImagePullSecrets: []corev1.LocalObjectReference{},
		},
	}}
	require.Equal(t, revisionFor(nilCollections), revisionFor(emptyCollections))
}

func TestUpdateStatusUsesCustomSuccessReason(t *testing.T) {
	for name, custom := range map[string]*modelv1alpha1.ModelWarmupCustomAction{
		"image preload": nil,
		"custom action": {},
	} {
		t.Run(name, func(t *testing.T) {
			warmup := &modelv1alpha1.ModelWarmup{
				ObjectMeta: metav1.ObjectMeta{Name: "warmup", Namespace: "default", UID: "warmup-uid"},
				Spec:       modelv1alpha1.ModelWarmupSpec{Custom: custom},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default", Labels: map[string]string{
					WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "rev",
				}, OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)}},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
			}
			job.Status = batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			}}}
			scheme := runtime.NewScheme()
			require.NoError(t, modelv1alpha1.AddToScheme(scheme))
			require.NoError(t, batchv1.AddToScheme(scheme))
			r := &ModelWarmupReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(warmup).WithObjects(warmup, job).Build()}

			_, err := r.updateStatus(context.Background(), warmup, "rev",
				testResolvedTargets(map[string][]string{"node-a": {"target[0]"}}), nil,
				[]batchv1.Job{*job}, []batchv1.Job{*job}, "", "")
			require.NoError(t, err)
			complete := mustCondition(warmup.Status.Conditions, "Complete")
			if custom == nil {
				require.Equal(t, "ImagePreloadSucceeded", complete.Reason)
			} else {
				require.Equal(t, "WarmupSucceeded", complete.Reason)
			}
		})
	}
}

func TestSetConditionKeepsConditionsMutuallyExclusive(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{}
	now := time.Now()
	setCondition(warmup, "Complete", metav1.ConditionTrue, "Complete", "done", now)
	setCondition(warmup, "Degraded", metav1.ConditionTrue, "Failed", "failed", now)

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
			Labels:          map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)}},
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
	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	_, err := r.updateStatus(context.Background(), warmup, "rev", targets, nil,
		[]batchv1.Job{*job}, []batchv1.Job{*job}, "", "")
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
	require.Equal(t, []string{"target[0]", "target[1]"}, targets["node-a"].Sources)
	require.Equal(t, []string{"target[0]", "target[1]"}, targets["node-b"].Sources)
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
	targets := make(map[string]resolvedTarget, modelv1alpha1.MaxModelWarmupTargets)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargets; i++ {
		name := fmt.Sprintf("node-%d", i)
		targets[name] = resolvedTarget{NodeName: name, Sources: []string{"target[0]"}}
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
		Name: strings.Repeat("a", 253), Namespace: "default", UID: "warmup-uid-a",
	}}
	revision := "123456789abc"
	first := (&ModelWarmupReconciler{}).jobFor(warmup, "node-a", revision)
	second := (&ModelWarmupReconciler{}).jobFor(warmup, "node-b", revision)

	require.LessOrEqual(t, len(first.Name), 63)
	require.True(t, strings.HasSuffix(first.Name,
		"-"+shortHash(string(warmup.UID))+"-"+shortHash("node-a")+"-"+revision))
	require.True(t, strings.HasSuffix(second.Name,
		"-"+shortHash(string(warmup.UID))+"-"+shortHash("node-b")+"-"+revision))
	require.NotEqual(t, first.Name, second.Name)
}

func TestJobForTargetPreservesAttemptOneNameAndRecordsIdentity(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: strings.Repeat("warmup-", 20), Namespace: "default", UID: "warmup-uid",
	}}
	target := resolvedTarget{NodeName: strings.Repeat("node-", 60), NodeUID: types.UID("node-uid")}
	revision := "123456789abc"

	once := (&ModelWarmupReconciler{}).jobForTarget(warmup, target, revision, 0)
	first := (&ModelWarmupReconciler{}).jobForTarget(warmup, target, revision, 1)
	second := (&ModelWarmupReconciler{}).jobForTarget(warmup, target, revision, 2)
	eleventh := (&ModelWarmupReconciler{}).jobForTarget(warmup, target, revision, 11)

	require.Equal(t, modelWarmupJobName(warmup.Name, string(warmup.UID), target.NodeName, revision), once.Name)
	require.Equal(t, once.Name, first.Name)
	require.Equal(t, "node-uid", once.Annotations[TargetNodeUIDAnnotationKey])
	require.NotContains(t, once.Annotations, AttemptAnnotationKey)
	require.Equal(t, "1", first.Annotations[AttemptAnnotationKey])
	require.Equal(t, "2", second.Annotations[AttemptAnnotationKey])
	require.Equal(t, "11", eleventh.Annotations[AttemptAnnotationKey])
	require.True(t, strings.HasSuffix(second.Name, "-a2"))
	require.True(t, strings.HasSuffix(eleventh.Name, "-a11"))
	require.LessOrEqual(t, len(second.Name), 63)
	require.LessOrEqual(t, len(eleventh.Name), 63)
}

func TestJobNameSeparatesWarmupsWithTheSameTruncatedPrefix(t *testing.T) {
	firstWarmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: strings.Repeat("a", 80) + "-first", UID: "warmup-uid-first",
	}}
	secondWarmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: strings.Repeat("a", 80) + "-second", UID: "warmup-uid-second",
	}}

	first := (&ModelWarmupReconciler{}).jobFor(firstWarmup, "node-a", "123456789abc")
	second := (&ModelWarmupReconciler{}).jobFor(secondWarmup, "node-a", "123456789abc")

	require.NotEqual(t, first.Name, second.Name)
}

func TestCleanupStaleJobsDeletesRunningAndKeepsCompleted(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	running := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: warmup.Namespace,
			Labels:          map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "old"},
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)}},
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
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, running, completed, retrying)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})

	require.NoError(t, err)
	require.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{}))
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(completed), &batchv1.Job{}))
	require.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(retrying), &batchv1.Job{}))
	err = r.Get(context.Background(), client.ObjectKeyFromObject(running), &batchv1.Job{})
	require.Error(t, err)
	require.True(t, apierrors.IsNotFound(err))
	require.Zero(t, counting.createCalls, "deleted active stale jobs consume this round's capacity")
}

func TestCleanupStaleJobsNeverDeletesForeignLabeledJobs(t *testing.T) {
	warmup, namespace, nodes, scheme := reconcileTestObjects(t, 1)
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: warmup.Namespace,
		Labels: map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "old"},
		OwnerReferences: []metav1.OwnerReference{{
			UID: "foreign-uid", Controller: ptr.To(true),
		}},
	}}
	counting := newCountingModelWarmupClient(scheme, warmup, namespace, nodes, foreign)
	r := &ModelWarmupReconciler{Client: counting, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(warmup)})
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &batchv1.Job{}))
}

func TestRetryingJobRemainsActiveUntilTerminalFailure(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "retrying", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)},
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

	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	snapshot := buildReconcileSnapshot(warmup, "rev", targets, nil, []batchv1.Job{*job})
	require.Equal(t, int32(1), snapshot.ActiveJobs)
	_, err := r.updateStatus(
		context.Background(),
		warmup,
		"rev",
		targets,
		nil,
		snapshot.jobsForStatus(nil),
		[]batchv1.Job{*job},
		"",
		"",
	)
	require.NoError(t, err)
	require.Equal(t, modelv1alpha1.ModelWarmupRunning, warmup.Status.Phase)
	require.Equal(t, modelv1alpha1.ModelWarmupTargetRunning, warmup.Status.Targets[0].Phase)
}

func TestActiveJobsIgnoresForeignLabeledJobs(t *testing.T) {
	warmup := &modelv1alpha1.ModelWarmup{ObjectMeta: metav1.ObjectMeta{
		Name: "warmup", Namespace: "default", UID: "warmup-uid",
	}}
	owned := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "owned", Namespace: "default",
		Labels:          map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "rev"},
		OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)},
	}}
	foreign := owned.DeepCopy()
	foreign.Name = "foreign"
	foreign.OwnerReferences = []metav1.OwnerReference{{UID: "foreign-uid", Controller: ptr.To(true)}}

	snapshot := buildReconcileSnapshot(
		warmup,
		"rev",
		testResolvedTargets(map[string][]string{"node-a": {"target[0]"}}),
		nil,
		[]batchv1.Job{*owned, *foreign},
	)
	require.Equal(t, int32(1), snapshot.ActiveJobs)
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

	_, err := r.updateStatus(context.Background(), warmup, "rev", nil, nil, nil, nil, "", "")
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
	targets := testResolvedTargets(map[string][]string{"node-a": {"target[0]"}})
	_, err := r.updateStatus(context.Background(), warmup, "rev", targets, nil, nil, nil, "", "")
	require.NoError(t, err)
	first := *warmup.Status.Targets[0].LastTransitionTime
	_, err = r.updateStatus(context.Background(), warmup, "rev", targets, nil, nil, nil, "", "")
	require.NoError(t, err)
	require.Equal(t, first, *warmup.Status.Targets[0].LastTransitionTime)
	require.Equal(t, metav1.ConditionFalse, mustCondition(warmup.Status.Conditions, "Complete").Status)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Progressing").Status)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default",
			Labels:          map[string]string{WarmupLabelKey: string(warmup.UID), RevisionLabelKey: "rev"},
			OwnerReferences: []metav1.OwnerReference{controllerOwnerReference(warmup)}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeName: "node-a"}}},
		Status: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	require.NoError(t, r.Create(context.Background(), job))
	_, err = r.updateStatus(context.Background(), warmup, "rev", targets, nil,
		[]batchv1.Job{*job}, []batchv1.Job{*job}, "", "")
	require.NoError(t, err)
	require.Empty(t, warmup.Status.Targets)
	require.Equal(t, metav1.ConditionTrue, mustCondition(warmup.Status.Conditions, "Complete").Status)
	latestJob := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(job), latestJob))
	require.Equal(t, ptr.To(modelv1alpha1.DefaultModelWarmupTTLSecondsAfterFinished),
		latestJob.Spec.TTLSecondsAfterFinished)
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

	_, err := r.updateStatus(context.Background(), warmup, "rev", nil, missing, nil, nil, "", "")
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
	targets := make(map[string]resolvedTarget, modelv1alpha1.MaxModelWarmupTargetDetails)
	for i := 0; i < modelv1alpha1.MaxModelWarmupTargetDetails; i++ {
		name := fmt.Sprintf("node-%04d", i)
		targets[name] = resolvedTarget{NodeName: name, Sources: []string{"target[0]"}}
	}

	_, err := r.updateStatus(context.Background(), warmup, "rev", targets,
		map[string]string{"zzz-failed": "NodeNotFound"}, nil, nil, "", "")
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

func testResolvedTargets(sourcesByNode map[string][]string) map[string]resolvedTarget {
	targets := make(map[string]resolvedTarget, len(sourcesByNode))
	for node, sources := range sourcesByNode {
		targets[node] = resolvedTarget{NodeName: node, Sources: sources}
	}
	return targets
}

func mustCondition(conditions []metav1.Condition, typ string) metav1.Condition {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition
		}
	}
	panic("condition not found")
}

func mustTargetStatus(
	targets []modelv1alpha1.ModelWarmupTargetStatus,
	node string,
) modelv1alpha1.ModelWarmupTargetStatus {
	for _, target := range targets {
		if target.NodeName == node {
			return target
		}
	}
	panic("target status not found")
}

func controllerOwnerReference(warmup *modelv1alpha1.ModelWarmup) metav1.OwnerReference {
	return metav1.OwnerReference{UID: warmup.UID, Controller: ptr.To(true)}
}

type countingModelWarmupClient struct {
	client.Client
	modelWarmupGets  int
	namespaceGets    int
	jobGets          int
	nodeLists        int
	jobLists         int
	createCalls      int
	statusUpdates    int
	statusUpdateErr  error
	omitJobsFromList map[string]struct{}
}

func newCountingModelWarmupClient(
	scheme *runtime.Scheme,
	warmup *modelv1alpha1.ModelWarmup,
	namespace *corev1.Namespace,
	nodes []corev1.Node,
	extra ...client.Object,
) *countingModelWarmupClient {
	objects := make([]client.Object, 0, 2+len(nodes)+len(extra))
	objects = append(objects, warmup, namespace)
	for i := range nodes {
		objects = append(objects, &nodes[i])
	}
	objects = append(objects, extra...)
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(warmup, &batchv1.Job{}).
		WithObjects(objects...).Build()
	return &countingModelWarmupClient{Client: base}
}

func (c *countingModelWarmupClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	switch obj.(type) {
	case *modelv1alpha1.ModelWarmup:
		c.modelWarmupGets++
	case *corev1.Namespace:
		c.namespaceGets++
	case *batchv1.Job:
		c.jobGets++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *countingModelWarmupClient) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	switch list.(type) {
	case *corev1.NodeList:
		c.nodeLists++
	case *batchv1.JobList:
		c.jobLists++
	}
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	jobs, ok := list.(*batchv1.JobList)
	if !ok || len(c.omitJobsFromList) == 0 {
		return nil
	}
	visible := jobs.Items[:0]
	for i := range jobs.Items {
		if _, omitted := c.omitJobsFromList[jobs.Items[i].Name]; !omitted {
			visible = append(visible, jobs.Items[i])
		}
	}
	jobs.Items = visible
	return nil
}

func (c *countingModelWarmupClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{SubResourceWriter: c.Client.Status(), client: c}
}

func (c *countingModelWarmupClient) Create(
	ctx context.Context,
	obj client.Object,
	opts ...client.CreateOption,
) error {
	c.createCalls++
	return c.Client.Create(ctx, obj, opts...)
}

type countingStatusWriter struct {
	client.SubResourceWriter
	client *countingModelWarmupClient
}

func (w *countingStatusWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	w.client.statusUpdates++
	if w.client.statusUpdateErr != nil {
		return w.client.statusUpdateErr
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func reconcileTestObjects(
	t *testing.T,
	nodeCount int,
) (*modelv1alpha1.ModelWarmup, *corev1.Namespace, []corev1.Node, *runtime.Scheme) {
	t.Helper()
	warmup := validWarmupForControllerTest("tenant", "warmup")
	warmup.UID = "warmup-uid"
	warmup.Spec.Policies = &modelv1alpha1.ModelWarmupPolicies{Parallelism: ptr.To[int32](1)}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: warmup.Namespace,
		Labels: map[string]string{
			ResourcePoolLabelKey: "warm",
		},
	}}
	nodes := make([]corev1.Node, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%03d", i)
		if i == 0 {
			name = "node-a"
		}
		nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("uid-" + name),
			Labels: map[string]string{
				"group":               "all",
				ResourcePoolLabelKey:  "warm",
				WarmupEnabledLabelKey: WarmupEnabledLabelValue,
			},
		}})
	}
	scheme := runtime.NewScheme()
	require.NoError(t, modelv1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return warmup, namespace, nodes, scheme
}

func reconcileOnce(r *ModelWarmupReconciler, request ctrl.Request) error {
	_, err := r.Reconcile(context.Background(), request)
	return err
}
