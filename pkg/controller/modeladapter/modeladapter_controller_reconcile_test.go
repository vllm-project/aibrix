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
	"net/http"
	"testing"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestReconcileLoading_NoActivePods_FallsThrough guards against the regression fixed
// alongside the cross-namespace pod lookup in pkg/cache: reconcileLoading used to
// return nil as soon as no active pods matched the ModelAdapter's selector, which
// skipped resetting ReadyReplicas/Instances and left the Ready condition stale (an
// adapter could keep reporting Ready:True long after its backing pod was gone). It
// must now fall through and report the adapter as unloaded instead of silently
// no-opping.
func TestReconcileLoading_NoActivePods_FallsThrough(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := modelv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}

	instance := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter1", Namespace: "default"},
		Spec: modelv1alpha1.ModelAdapterSpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "base-model"}},
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Phase:           modelv1alpha1.ModelAdapterRunning,
			DesiredReplicas: 1,
			ReadyReplicas:   1,
			// Instances already empty, as reconcileReplicas (Step 1, run before
			// reconcileLoading) would have cleared it once the backing pod disappeared.
			Instances: nil,
		},
	}

	// No pods registered in the client: the base model pod is gone.
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance).Build()
	r := &ModelAdapterReconciler{Client: cl, Scheme: scheme}

	err := r.reconcileLoading(context.Background(), instance, nil)
	if err == nil {
		t.Fatal("expected reconcileLoading to return an error when no active pods back the adapter, got nil")
	}
	if instance.Status.ReadyReplicas != 0 {
		t.Fatalf("expected ReadyReplicas to be reset to 0 once no pods back the adapter, got %d", instance.Status.ReadyReplicas)
	}
}

// TestDoReconcile_SingleReplica_PodDisappears_ResetsReadyReplicas guards a gap the
// reconcileLoading fix above didn't cover: in single-replica mode (Spec.Replicas: 1),
// when the backing pod disappears, reconcileReplicas -> reconcileLoadOnSinglePod takes
// the "no ready pods" branch and returns ctrl.Result{RequeueAfter: ...}. DoReconcile's
// Step 1 early-return on RequeueAfter>0 then skips reconcileLoading entirely for that
// cycle, so ReadyReplicas/Ready must be reset inside reconcileLoadOnSinglePod itself --
// otherwise a single-replica adapter keeps reporting Ready:True/ReadyReplicas:1
// indefinitely after its one pod is gone, even though reconcileLoading now handles the
// load-on-all case correctly.
func TestDoReconcile_SingleReplica_PodDisappears_ResetsReadyReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		modelv1alpha1.AddToScheme, corev1.AddToScheme, discoveryv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to register scheme: %v", err)
		}
	}

	replicas := int32(1)
	seed := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter1", Namespace: "default"},
		Spec: modelv1alpha1.ModelAdapterSpec{
			Replicas:    &replicas,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "base-model"}},
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Phase:           modelv1alpha1.ModelAdapterRunning,
			Instances:       []string{"test-pod"},
			Candidates:      1,
			DesiredReplicas: 1,
			ReadyReplicas:   1,
			Conditions: []metav1.Condition{
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeInitialized), metav1.ConditionTrue,
					ModelAdapterInitializedReason, "initialized"),
				NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionTrue,
					ModelAdapterAvailable, "ready"),
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeBound), metav1.ConditionTrue,
					ModelAdapterBoundReason, "bound"),
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionTrue,
					"Scheduled", "scheduled"),
			},
		},
	}

	// No pod registered in the client: "test-pod" is gone, and nothing else matches the selector.
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&modelv1alpha1.ModelAdapter{}).
		WithObjects(seed).
		Build()

	instance := &modelv1alpha1.ModelAdapter{}
	ctx := context.Background()
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "adapter1"}, instance); err != nil {
		t.Fatalf("failed to fetch seeded ModelAdapter: %v", err)
	}

	r := &ModelAdapterReconciler{Client: cl, Scheme: scheme, RuntimeConfig: config.RuntimeConfig{}}

	result, err := r.DoReconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "adapter1"}}, instance)
	if err != nil {
		t.Fatalf("expected DoReconcile to succeed (recoverable state), got error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("expected a RequeueAfter result while waiting for a replacement pod, got %+v", result)
	}

	if instance.Status.ReadyReplicas != 0 {
		t.Errorf("expected ReadyReplicas to be reset to 0 once the single backing pod is gone, got %d", instance.Status.ReadyReplicas)
	}
	if readyCond := apimeta.FindStatusCondition(instance.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionReady)); readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready condition to be False once the single backing pod is gone, got %v", readyCond)
	}
}

// TestDoReconcile_HealsStaleBoundScheduledConditions guards against the other half of
// the same fix: DoReconcile only refreshed the Ready condition when the field-level
// status diff (inconsistentModelAdapterStatus) found a change. Once an adapter had
// been stably healthy for a cycle, oldInstance/instance stopped differing, so a Bound
// or Scheduled condition left stuck False by an earlier failure or pod migration (see
// reconcileLoading's podRemoved branch, or the Step 2 error path in DoReconcile) never
// got another chance to be corrected. DoReconcile must now re-assert Bound/Scheduled as
// True whenever the adapter is otherwise healthy, even if nothing else changed.
func TestDoReconcile_HealsStaleBoundScheduledConditions(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		modelv1alpha1.AddToScheme, corev1.AddToScheme, discoveryv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("failed to register scheme: %v", err)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":                      "base-model",
				constants.ModelLabelEngine: VLLMEngine,
			},
		},
		Status: corev1.PodStatus{
			PodIP: "127.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	// The pod already reports the adapter as loaded, so tryLoadModelAdapterOnPod's
	// re-verification of the existing instance succeeds without issuing a load call.
	srv := mockServer(t, "127.0.0.1", 8000, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(prepareModelApiResponseWithOneModel(VLLMEngine, "adapter1")))
	})
	defer srv.Close()

	seed := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter1", Namespace: "default"},
		Spec: modelv1alpha1.ModelAdapterSpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "base-model"}},
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Phase:           modelv1alpha1.ModelAdapterRunning,
			Instances:       []string{"test-pod"},
			Candidates:      1,
			DesiredReplicas: 1,
			ReadyReplicas:   1,
			Conditions: []metav1.Condition{
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeInitialized), metav1.ConditionTrue,
					ModelAdapterInitializedReason, "initialized"),
				NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionTrue,
					ModelAdapterAvailable, "ready"),
				// Stale False conditions left over from an earlier failure/migration whose
				// recovery path never flipped them back to True.
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeBound), metav1.ConditionFalse,
					ModelAdapterLoadingErrorReason, "stale from a prior failure"),
				NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionFalse,
					"Rescheduling", "stale from a prior migration"),
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&modelv1alpha1.ModelAdapter{}).
		WithObjects(pod, seed).
		Build()

	instance := &modelv1alpha1.ModelAdapter{}
	ctx := context.Background()
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "adapter1"}, instance); err != nil {
		t.Fatalf("failed to fetch seeded ModelAdapter: %v", err)
	}

	r := &ModelAdapterReconciler{
		Client:        cl,
		Scheme:        scheme,
		loraClient:    NewLoraClient(config.RuntimeConfig{}),
		RuntimeConfig: config.RuntimeConfig{},
	}

	result, err := r.DoReconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "adapter1"}}, instance)
	if err != nil {
		t.Fatalf("expected DoReconcile to succeed, got error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatalf("expected no requeue for an already-healthy adapter, got %+v", result)
	}

	for _, condType := range []string{
		string(modelv1alpha1.ModelAdapterConditionTypeBound),
		string(modelv1alpha1.ModelAdapterConditionTypeScheduled),
	} {
		cond := apimeta.FindStatusCondition(instance.Status.Conditions, condType)
		if cond == nil {
			t.Fatalf("expected condition %s to be present", condType)
		}
		if cond.Status != metav1.ConditionTrue {
			t.Errorf("expected condition %s to be healed to True, still %s (reason=%s)", condType, cond.Status, cond.Reason)
		}
	}
}

// TestPersistAnnotations_ScopesToManagedPrefix ensures persistAnnotations only
// syncs adapter.model.aibrix.ai/* keys. Concurrently-added unrelated annotations
// on the API object must survive the merge patch, and managed keys cleared on the
// in-memory instance must still be deleted.
func TestPersistAnnotations_ScopesToManagedPrefix(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := modelv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to register scheme: %v", err)
	}

	seed := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adapter1",
			Namespace: "default",
			Annotations: map[string]string{
				"user.custom/annotation":                "server-side-value",
				ScheduledPodsAnnotationKey:              "old-pod",
				RetryCountAnnotationKey + ".old-pod":    "3",
				LastRetryTimeAnnotationKey + ".old-pod": "2026-01-01T00:00:00Z",
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(seed).Build()
	r := &ModelAdapterReconciler{Client: cl, Scheme: scheme}

	// Stale in-memory view: missing the user annotation that landed on the API
	// object after the original fetch, with updated managed scheduling state and
	// cleared retry annotations for the departed pod.
	instance := &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "adapter1",
			Namespace: "default",
			Annotations: map[string]string{
				ScheduledPodsAnnotationKey: "new-pod",
			},
		},
	}

	if err := r.persistAnnotations(context.Background(), instance); err != nil {
		t.Fatalf("persistAnnotations returned error: %v", err)
	}

	latest := &modelv1alpha1.ModelAdapter{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "adapter1"}, latest); err != nil {
		t.Fatalf("failed to fetch patched ModelAdapter: %v", err)
	}

	if got := latest.Annotations["user.custom/annotation"]; got != "server-side-value" {
		t.Errorf("unrelated annotation was clobbered: got %q, want %q", got, "server-side-value")
	}
	if got := latest.Annotations[ScheduledPodsAnnotationKey]; got != "new-pod" {
		t.Errorf("managed annotation not updated: got %q, want %q", got, "new-pod")
	}
	if _, exists := latest.Annotations[RetryCountAnnotationKey+".old-pod"]; exists {
		t.Errorf("cleared managed retry-count annotation should have been deleted")
	}
	if _, exists := latest.Annotations[LastRetryTimeAnnotationKey+".old-pod"]; exists {
		t.Errorf("cleared managed last-retry-time annotation should have been deleted")
	}
}
