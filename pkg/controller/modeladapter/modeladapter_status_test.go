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

package modeladapter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func readyCondition(t *testing.T, ma *modelv1alpha1.ModelAdapter) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(ma.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionReady))
	require.NotNil(t, cond, "Ready condition missing")
	return cond
}

func TestRecomputeReadiness(t *testing.T) {
	failedMsg := "Loading failed on all pods: pod a: boom"
	loadingFailed := NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionFalse,
		ModelAdapterLoadingErrorReason, failedMsg)
	staleReady := NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionTrue,
		ModelAdapterAvailable, "ModelAdapter default/adapter is ready")

	tests := []struct {
		name           string
		status         modelv1alpha1.ModelAdapterStatus
		wantPhase      modelv1alpha1.ModelAdapterPhase
		wantReady      int32
		wantCondStatus metav1.ConditionStatus
		wantReason     string
		wantMsgPart    string
	}{
		{
			name: "all instances loaded",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterScheduled, Instances: []string{"a", "b"},
				DesiredReplicas: 2, Candidates: 2,
			},
			wantPhase: modelv1alpha1.ModelAdapterRunning, wantReady: 2,
			wantCondStatus: metav1.ConditionTrue, wantReason: ModelAdapterAvailable, wantMsgPart: "is ready",
		},
		{
			name: "partially loaded still counts as running",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterRunning, Instances: []string{"a"},
				DesiredReplicas: 3, Candidates: 3,
			},
			wantPhase: modelv1alpha1.ModelAdapterRunning, wantReady: 1,
			wantCondStatus: metav1.ConditionTrue, wantReason: ModelAdapterAvailable, wantMsgPart: "is ready",
		},
		{
			name: "no candidates clears stale running",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterRunning, ReadyReplicas: 2,
				Conditions: []metav1.Condition{staleReady},
			},
			wantPhase: modelv1alpha1.ModelAdapterPending, wantReady: 0,
			wantCondStatus: metav1.ConditionFalse, wantReason: PodNotReadyReason, wantMsgPart: "no ready pods",
		},
		{
			name: "candidates but nothing loaded",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterScheduled, DesiredReplicas: 1, Candidates: 2,
			},
			wantPhase: modelv1alpha1.ModelAdapterPending, wantReady: 0,
			wantCondStatus: metav1.ConditionFalse, wantReason: ModelAdapterUnavailable, wantMsgPart: "2 candidate pods",
		},
		{
			name: "failed is preserved with its loading error",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterFailed, DesiredReplicas: 1, Candidates: 1,
				Conditions: []metav1.Condition{loadingFailed},
			},
			wantPhase: modelv1alpha1.ModelAdapterFailed, wantReady: 0,
			wantCondStatus: metav1.ConditionFalse, wantReason: ModelAdapterLoadingErrorReason, wantMsgPart: failedMsg,
		},
		{
			name: "failed recovers once an instance loads",
			status: modelv1alpha1.ModelAdapterStatus{
				Phase: modelv1alpha1.ModelAdapterFailed, Instances: []string{"a"},
				DesiredReplicas: 1, Candidates: 1,
				Conditions: []metav1.Condition{loadingFailed},
			},
			wantPhase: modelv1alpha1.ModelAdapterRunning, wantReady: 1,
			wantCondStatus: metav1.ConditionTrue, wantReason: ModelAdapterAvailable, wantMsgPart: "is ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ma := &modelv1alpha1.ModelAdapter{
				ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: "default"},
				Status:     tt.status,
			}
			recomputeReadiness(ma)

			assert.Equal(t, tt.wantPhase, ma.Status.Phase)
			assert.Equal(t, tt.wantReady, ma.Status.ReadyReplicas)
			cond := readyCondition(t, ma)
			assert.Equal(t, tt.wantCondStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
			assert.Contains(t, cond.Message, tt.wantMsgPart)
		})
	}
}

func TestRecomputeReadinessIsIdempotent(t *testing.T) {
	ma := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: "default"},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{"a"}, DesiredReplicas: 1, Candidates: 1,
		},
	}
	recomputeReadiness(ma)

	past := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	readyCondition(t, ma).LastTransitionTime = past
	before := ma.DeepCopy()

	recomputeReadiness(ma)

	assert.Equal(t, past, readyCondition(t, ma).LastTransitionTime, "unchanged status must keep its transition time")
	r := &ModelAdapterReconciler{}
	assert.False(t, r.inconsistentModelAdapterStatus(before.Status, ma.Status), "steady state must not trigger a status write")
}

func newStatusTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, discoveryv1.AddToScheme(s))
	require.NoError(t, modelv1alpha1.AddToScheme(s))
	return s
}

func newStatusTestAdapter(replicas *int32) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "test-adapter", Namespace: "default"},
		Spec: modelv1alpha1.ModelAdapterSpec{
			ArtifactURL: "huggingface://org/adapter",
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"model": "base"}},
			Replicas:    replicas,
		},
	}
}

func newStatusTestPod(name string, ready bool, readySince time.Time) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"model": "base"}},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(readySince),
			}},
		},
	}
}

func newStatusTestReconciler(t *testing.T, objs ...client.Object) *ModelAdapterReconciler {
	t.Helper()
	scheme := newStatusTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&modelv1alpha1.ModelAdapter{}).
		Build()
	return &ModelAdapterReconciler{Client: c, Scheme: scheme}
}

// reconcileTwice runs the initialising pass of DoReconcile and then one full pass,
// returning the second result and the adapter as persisted by the client.
func reconcileTwice(t *testing.T, r *ModelAdapterReconciler, key types.NamespacedName) (ctrl.Result, *modelv1alpha1.ModelAdapter) {
	t.Helper()
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: key}

	ma := &modelv1alpha1.ModelAdapter{}
	require.NoError(t, r.Get(ctx, key, ma))
	res, err := r.DoReconcile(ctx, req, ma)
	require.NoError(t, err)
	require.True(t, res.Requeue, "first pass only initialises the status")

	require.NoError(t, r.Get(ctx, key, ma))
	require.Equal(t, modelv1alpha1.ModelAdapterPending, ma.Status.Phase)
	res, err = r.DoReconcile(ctx, req, ma)
	require.NoError(t, err)

	require.NoError(t, r.Get(ctx, key, ma))
	return res, ma
}

func TestDoReconcileNoPodsLoadOnAll(t *testing.T) {
	adapter := newStatusTestAdapter(nil)
	r := newStatusTestReconciler(t, adapter)
	key := types.NamespacedName{Namespace: adapter.Namespace, Name: adapter.Name}

	res, ma := reconcileTwice(t, r, key)

	assert.Equal(t, ctrl.Result{}, res)
	assert.Equal(t, modelv1alpha1.ModelAdapterPending, ma.Status.Phase, "must not report Running without any pod")
	assert.Equal(t, int32(0), ma.Status.Candidates)
	assert.Equal(t, int32(0), ma.Status.DesiredReplicas)
	assert.Equal(t, int32(0), ma.Status.ReadyReplicas)
	cond := readyCondition(t, ma)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, PodNotReadyReason, cond.Reason)

	svc := &corev1.Service{}
	assert.NoError(t, r.Get(context.Background(), key, svc), "service is still created while waiting for pods")
}

func TestDoReconcileNoReadyPodsSinglePod(t *testing.T) {
	replicas := int32(1)
	adapter := newStatusTestAdapter(&replicas)
	pod := newStatusTestPod("base-0", false, time.Now().Add(-time.Minute))
	r := newStatusTestReconciler(t, adapter, pod)
	key := types.NamespacedName{Namespace: adapter.Namespace, Name: adapter.Name}

	res, ma := reconcileTwice(t, r, key)

	assert.Equal(t, time.Duration(RetryBackoffSeconds)*time.Second, res.RequeueAfter)
	assert.Equal(t, modelv1alpha1.ModelAdapterPending, ma.Status.Phase)
	assert.Equal(t, int32(1), ma.Status.DesiredReplicas, "desired count must be persisted while waiting")
	assert.Equal(t, int32(0), ma.Status.Candidates)
	assert.Equal(t, int32(0), ma.Status.ReadyReplicas)
	cond := readyCondition(t, ma)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, PodNotReadyReason, cond.Reason)
}

func TestDoReconcileUnstablePodSinglePod(t *testing.T) {
	replicas := int32(1)
	adapter := newStatusTestAdapter(&replicas)
	pod := newStatusTestPod("base-0", true, time.Now())
	r := newStatusTestReconciler(t, adapter, pod)
	key := types.NamespacedName{Namespace: adapter.Namespace, Name: adapter.Name}

	res, ma := reconcileTwice(t, r, key)

	assert.Equal(t, time.Duration(RetryBackoffSeconds)*time.Second, res.RequeueAfter)
	assert.Equal(t, modelv1alpha1.ModelAdapterPending, ma.Status.Phase)
	assert.Equal(t, int32(1), ma.Status.DesiredReplicas)
	assert.Equal(t, int32(1), ma.Status.Candidates, "candidate count must be persisted while waiting")
	assert.Equal(t, int32(0), ma.Status.ReadyReplicas)
	cond := readyCondition(t, ma)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, ModelAdapterUnavailable, cond.Reason)
	assert.Contains(t, cond.Message, "1 candidate")
}

func TestDoReconcileAllPodsGoneAfterRunning(t *testing.T) {
	adapter := newStatusTestAdapter(nil)
	adapter.Status = modelv1alpha1.ModelAdapterStatus{
		Phase:           modelv1alpha1.ModelAdapterRunning,
		Instances:       []string{"base-0"},
		Candidates:      1,
		DesiredReplicas: 1,
		ReadyReplicas:   1,
		Conditions: []metav1.Condition{
			NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeInitialized), metav1.ConditionUnknown,
				ModelAdapterInitializedReason, "Starting reconciliation"),
			NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionTrue,
				ModelAdapterAvailable, "ModelAdapter default/test-adapter is ready"),
		},
	}
	// Service and EndpointSlice from the earlier Running state still exist; the pod does not.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: adapter.Name, Namespace: adapter.Namespace}}
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: adapter.Name, Namespace: adapter.Namespace},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	r := newStatusTestReconciler(t, adapter, svc, eps)
	key := types.NamespacedName{Namespace: adapter.Namespace, Name: adapter.Name}
	ctx := context.Background()

	ma := &modelv1alpha1.ModelAdapter{}
	require.NoError(t, r.Get(ctx, key, ma))
	res, err := r.DoReconcile(ctx, ctrl.Request{NamespacedName: key}, ma)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)

	require.NoError(t, r.Get(ctx, key, ma))
	assert.Equal(t, modelv1alpha1.ModelAdapterPending, ma.Status.Phase, "must not stay Running once every pod is gone")
	assert.Equal(t, int32(0), ma.Status.ReadyReplicas, "ready count must not go stale")
	assert.Equal(t, int32(0), ma.Status.Candidates)
	assert.Equal(t, int32(0), ma.Status.DesiredReplicas)
	assert.Empty(t, ma.Status.Instances)
	cond := readyCondition(t, ma)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, PodNotReadyReason, cond.Reason)
}
