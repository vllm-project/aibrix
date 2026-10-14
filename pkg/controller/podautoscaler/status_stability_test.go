/*
Copyright 2024 The Aibrix Team.

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

package podautoscaler

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
)

// The controller watches PodAutoscaler without an event filter, so every status
// write enqueues the object again. Reconciling an autoscaler whose target and
// metrics are not changing must therefore settle and stop writing status;
// otherwise each write triggers the next reconcile and the controller never
// goes idle. The generated HPA is watched too, so the same holds for updates
// to it. The tests below reconcile one unchanged object repeatedly and count
// both kinds of write.

// statusStabilityHarness reconciles a single PodAutoscaler against the fake
// client and counts how often its status and its HPA are written.
type statusStabilityHarness struct {
	t            *testing.T
	client       client.Client
	reconciler   *PodAutoscalerReconciler
	scaleClient  *fakeWorkloadScaleClient
	key          ktypes.NamespacedName
	statusWrites int
	hpaUpdates   int
	// failStatusWrite, when positive, makes that PodAutoscaler status write
	// (counted from one) fail with errStatusWrite.
	failStatusWrite int
}

var errStatusWrite = errors.New("injected status write failure")

func newStatusStabilityHarness(t *testing.T, pa *autoscalingv1alpha1.PodAutoscaler, objs ...client.Object) *statusStabilityHarness {
	t.Helper()

	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	h := &statusStabilityHarness{
		t:           t,
		scaleClient: &fakeWorkloadScaleClient{},
		key:         ktypes.NamespacedName{Namespace: pa.Namespace, Name: pa.Name},
	}
	h.client = fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&autoscalingv1alpha1.PodAutoscaler{}, &autoscalingv2.HorizontalPodAutoscaler{}).
		WithObjects(append([]client.Object{pa}, objs...)...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*autoscalingv1alpha1.PodAutoscaler); ok && subResourceName == "status" {
					h.statusWrites++
					if h.statusWrites == h.failStatusWrite {
						return errStatusWrite
					}
				}
				return c.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
					h.hpaUpdates++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion})
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("Deployment"), apimeta.RESTScopeNamespace)

	h.reconciler = &PodAutoscalerReconciler{
		Client:              h.client,
		Scheme:              sch,
		EventRecorder:       record.NewFakeRecorder(1000),
		Mapper:              mapper,
		workloadScaleClient: h.scaleClient,
		autoScaler:          &fakeAutoScaler{},
		recommendations:     make(map[string][]timestampedRecommendation),
		monitor:             monitor.New(),
		scalingTargetToPA:   make(map[string]ktypes.NamespacedName),
		paToScalingKey:      make(map[ktypes.NamespacedName]string),
	}
	return h
}

// reconcile runs one reconcile and returns its error and the number of status writes it made.
func (h *statusStabilityHarness) reconcile() (int, error) {
	h.t.Helper()
	before := h.statusWrites
	_, err := h.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: h.key})
	return h.statusWrites - before, err
}

func (h *statusStabilityHarness) latest() *autoscalingv1alpha1.PodAutoscaler {
	h.t.Helper()
	pa := &autoscalingv1alpha1.PodAutoscaler{}
	if err := h.client.Get(context.Background(), h.key, pa); err != nil {
		h.t.Fatalf("get PodAutoscaler: %v", err)
	}
	return pa
}

// requireSettled reconciles rounds more times and fails if any of them writes
// status or updates the HPA.
func (h *statusStabilityHarness) requireSettled(rounds int, wantErr bool) {
	h.t.Helper()
	for round := 1; round <= rounds; round++ {
		hpaUpdatesBefore := h.hpaUpdates
		writes, err := h.reconcile()
		if updates := h.hpaUpdates - hpaUpdatesBefore; updates != 0 {
			h.t.Fatalf("steady round %d: the HPA was updated %d time(s) although nothing changed; "+
				"HPA objects are watched, so each update can enqueue the PodAutoscaler again", round, updates)
		}
		if (err != nil) != wantErr {
			h.t.Fatalf("steady round %d: Reconcile error = %v, want error %t", round, err, wantErr)
		}
		if writes != 0 {
			latest := h.latest()
			h.t.Fatalf("steady round %d: status was written %d time(s) for an unchanged PodAutoscaler; "+
				"each write enqueues the object again, so the controller never goes idle. conditions: %+v",
				round, writes, latest.Status.Conditions)
		}
	}
}

func stabilityPodAutoscaler(strategy autoscalingv1alpha1.ScalingStrategyType) *autoscalingv1alpha1.PodAutoscaler {
	pa := validPodAutoscalerForSpec()
	pa.Namespace = ns
	pa.Name = "test-pa"
	pa.Generation = 1
	pa.Spec.ScaleTargetRef.APIVersion = "apps/v1"
	pa.Spec.ScalingStrategy = strategy
	return pa
}

func stabilityDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "foo"}},
			},
		},
	}
}

func requireCondition(t *testing.T, pa *autoscalingv1alpha1.PodAutoscaler, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := apimeta.FindStatusCondition(pa.Status.Conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %s is missing; conditions: %+v", conditionType, pa.Status.Conditions)
	}
	if condition.Status != status || condition.Reason != reason {
		t.Fatalf("condition %s = %s/%s, want %s/%s", conditionType, condition.Status, condition.Reason, status, reason)
	}
}

func TestReconcileSettlesWithoutRewritingStatus(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := stabilityPodAutoscaler(strategy)
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))

			if _, err := h.reconcile(); err != nil {
				t.Fatalf("first Reconcile returned error: %v", err)
			}

			h.requireSettled(8, false)
			latest := h.latest()
			requireCondition(t, latest, ConditionValidSpec, metav1.ConditionTrue, ReasonAsExpected)
			requireCondition(t, latest, ConditionAbleToScale, metav1.ConditionTrue, "SucceededGetScale")
			requireCondition(t, latest, ConditionScalingActive, metav1.ConditionFalse, ReasonStable)
			requireCondition(t, latest, ConditionReady, metav1.ConditionTrue, ReasonAsExpected)
		})
	}
}

// A failure that repeats on every reconcile must settle as well: the failed
// reconcile is retried with backoff, and must not also re-trigger itself by
// rewriting the same failure into status.
func TestReconcileSettlesWhenScaleTargetIsMissing(t *testing.T) {
	// No Deployment exists, so fetching the scale target fails every time.
	h := newStatusStabilityHarness(t, stabilityPodAutoscaler(autoscalingv1alpha1.KPA))

	if _, err := h.reconcile(); err == nil {
		t.Fatal("first Reconcile succeeded, want an error for the missing scale target")
	}

	h.requireSettled(8, true)
	requireCondition(t, h.latest(), ConditionAbleToScale, metav1.ConditionFalse, "FailedGetScale")
}

func TestReconcileHPASettlesWithoutRewritingStatus(t *testing.T) {
	pa := stabilityPodAutoscaler(autoscalingv1alpha1.HPA)
	h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
	ctx := context.Background()
	hpaKey := ktypes.NamespacedName{Namespace: ns, Name: pa.Name + "-hpa"}

	// The first reconcile creates the HPA. Until the HPA controller reports
	// conditions, the PodAutoscaler carries the controller's own view.
	if _, err := h.reconcile(); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}
	if _, err := h.reconcile(); err != nil {
		t.Fatalf("second Reconcile returned error: %v", err)
	}
	h.requireSettled(4, false)
	latest := h.latest()
	requireCondition(t, latest, ConditionAbleToScale, metav1.ConditionTrue, ReasonConfigured)
	requireCondition(t, latest, ConditionScalingActive, metav1.ConditionFalse, ReasonStable)

	// Simulate the HPA controller reporting its status. Its ScalingActive is
	// True while desired equals current, which is the opposite of the value the
	// PodAutoscaler controller would derive on its own.
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := h.client.Get(ctx, hpaKey, hpa); err != nil {
		t.Fatalf("get HPA: %v", err)
	}
	hpa.Status.CurrentReplicas = 1
	hpa.Status.DesiredReplicas = 1
	hpa.Status.Conditions = []autoscalingv2.HorizontalPodAutoscalerCondition{
		{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue, Reason: "ReadyForNewScale", Message: "recommended size matches current size"},
		{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Reason: "ValidMetricFound", Message: "the HPA was able to successfully calculate a replica count"},
		{Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionFalse, Reason: "DesiredWithinRange", Message: "the desired count is within the acceptable range"},
	}
	if err := h.client.Status().Update(ctx, hpa); err != nil {
		t.Fatalf("update HPA status: %v", err)
	}

	// Picking up the HPA's status takes writes; allow two reconciles for it.
	for i := 0; i < 2; i++ {
		if _, err := h.reconcile(); err != nil {
			t.Fatalf("Reconcile after HPA status update returned error: %v", err)
		}
	}

	h.requireSettled(8, false)
	latest = h.latest()
	requireCondition(t, latest, ConditionAbleToScale, metav1.ConditionTrue, "ReadyForNewScale")
	requireCondition(t, latest, ConditionScalingActive, metav1.ConditionTrue, "ValidMetricFound")
	requireCondition(t, latest, "ScalingLimited", metav1.ConditionFalse, "DesiredWithinRange")
	if latest.Status.ActualScale != 1 || latest.Status.DesiredScale != 1 {
		t.Fatalf("scale mirrored from HPA = actual %d desired %d, want 1 and 1", latest.Status.ActualScale, latest.Status.DesiredScale)
	}

	// A spec change must still reach the HPA, once.
	latest.Spec.MaxReplicas += 3
	if err := h.client.Update(ctx, latest); err != nil {
		t.Fatalf("update PodAutoscaler spec: %v", err)
	}
	updatesBefore := h.hpaUpdates
	if _, err := h.reconcile(); err != nil {
		t.Fatalf("Reconcile after spec change returned error: %v", err)
	}
	if got := h.hpaUpdates - updatesBefore; got != 1 {
		t.Fatalf("HPA updates after a spec change = %d, want 1", got)
	}
	if err := h.client.Get(ctx, hpaKey, hpa); err != nil {
		t.Fatalf("get HPA: %v", err)
	}
	if hpa.Spec.MaxReplicas != latest.Spec.MaxReplicas {
		t.Fatalf("HPA maxReplicas = %d, want %d", hpa.Spec.MaxReplicas, latest.Spec.MaxReplicas)
	}
	h.requireSettled(4, false)
}

// A failed status write must fail the reconcile so that controller-runtime
// retries it with backoff instead of leaving the mirrored HPA status stale.
func TestReconcileHPAReturnsStatusUpdateError(t *testing.T) {
	pa := stabilityPodAutoscaler(autoscalingv1alpha1.HPA)
	h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
	ctx := context.Background()

	if _, err := h.reconcile(); err != nil {
		t.Fatalf("first Reconcile returned error: %v", err)
	}

	// The HPA controller reports new status, which reconcileHPA has to mirror.
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := h.client.Get(ctx, ktypes.NamespacedName{Namespace: ns, Name: pa.Name + "-hpa"}, hpa); err != nil {
		t.Fatalf("get HPA: %v", err)
	}
	hpa.Status.CurrentReplicas = 1
	hpa.Status.DesiredReplicas = 2
	if err := h.client.Status().Update(ctx, hpa); err != nil {
		t.Fatalf("update HPA status: %v", err)
	}

	// Nothing changed for the validation result, so the next status write is
	// the one reconcileHPA makes.
	h.failStatusWrite = h.statusWrites + 1
	writes, err := h.reconcile()
	if writes != 1 {
		t.Fatalf("status writes while mirroring the HPA = %d, want 1", writes)
	}
	if !errors.Is(err, errStatusWrite) {
		t.Fatalf("Reconcile error = %v, want the status write failure", err)
	}
	if got := h.latest().Status.DesiredScale; got != 0 {
		t.Fatalf("stored desiredScale = %d, want it unchanged after the failed write", got)
	}

	// The retry stores what the failed attempt could not.
	h.failStatusWrite = 0
	if _, err := h.reconcile(); err != nil {
		t.Fatalf("retried Reconcile returned error: %v", err)
	}
	if got := h.latest().Status.DesiredScale; got != 2 {
		t.Fatalf("stored desiredScale after the retry = %d, want 2", got)
	}
}

// reconcileCustomPA can fail after it has fetched the scale target. The
// conditions it owns must be stored on that path too.
func TestReconcileStoresStrategyConditionsWhenReadingReplicasFails(t *testing.T) {
	staleInvalidSpec := []metav1.Condition{
		{Type: ConditionValidSpec, Status: metav1.ConditionFalse, Reason: ReasonMetricsConfigError, ObservedGeneration: 1},
		{Type: ConditionAbleToScale, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec},
		{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec},
	}
	tests := []struct {
		name       string
		generation int64
		conditions []metav1.Condition
	}{
		{name: "new object", generation: 1},
		{name: "repaired after an invalid spec", generation: 2, conditions: staleInvalidSpec},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa := stabilityPodAutoscaler(autoscalingv1alpha1.KPA)
			pa.Generation = tt.generation
			for _, condition := range tt.conditions {
				apimeta.SetStatusCondition(&pa.Status.Conditions, condition)
			}
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))
			h.scaleClient.replicasErr = errors.New("replicas unavailable")

			if _, err := h.reconcile(); !errors.Is(err, h.scaleClient.replicasErr) {
				t.Fatalf("Reconcile error = %v, want the replicas failure", err)
			}

			latest := h.latest()
			requireCondition(t, latest, ConditionValidSpec, metav1.ConditionTrue, ReasonAsExpected)
			requireCondition(t, latest, ConditionAbleToScale, metav1.ConditionFalse, "FailedGetReplicas")
			requireCondition(t, latest, ConditionScalingActive, metav1.ConditionFalse, ReasonStable)
			h.requireSettled(4, true)
		})
	}
}

// The API server fills in defaults for fields makeHPA leaves unset. A stored
// HPA that differs from the desired one only by those defaults is up to date.
func TestHPAMatchesDesired(t *testing.T) {
	desiredFor := func(t *testing.T, mutate func(*autoscalingv1alpha1.PodAutoscaler)) *autoscalingv2.HorizontalPodAutoscaler {
		t.Helper()
		pa := stabilityPodAutoscaler(autoscalingv1alpha1.HPA)
		pa.Spec.MinReplicas = nil
		if mutate != nil {
			mutate(pa)
		}
		scalingContext := scalingctx.NewBaseScalingContext()
		if err := scalingContext.UpdateByPaTypes(pa); err != nil {
			t.Fatalf("UpdateByPaTypes: %v", err)
		}
		hpa, err := makeHPA(pa, scalingContext)
		if err != nil {
			t.Fatalf("makeHPA: %v", err)
		}
		return hpa
	}
	// stored returns hpa as the API server would hand it back.
	stored := func(hpa *autoscalingv2.HorizontalPodAutoscaler) *autoscalingv2.HorizontalPodAutoscaler {
		out := hpa.DeepCopy()
		out.ResourceVersion = "7"
		out.Status.CurrentReplicas = 2
		if out.Spec.MinReplicas == nil {
			out.Spec.MinReplicas = ptr.To(int32(1))
		}
		return out
	}

	t.Run("server defaults are not a difference", func(t *testing.T) {
		desired := desiredFor(t, nil)
		if !hpaMatchesDesired(stored(desired), desired) {
			t.Fatal("stored HPA with a defaulted minReplicas does not match the desired HPA")
		}
	})

	t.Run("default policies under a disabled select policy are not a difference", func(t *testing.T) {
		desired := desiredFor(t, func(pa *autoscalingv1alpha1.PodAutoscaler) {
			pa.Annotations = map[string]string{"autoscaling.aibrix.ai/max-scale-up-rate": "1"}
		})
		if desired.Spec.Behavior.ScaleUp.Policies != nil {
			t.Fatalf("test setup: scale-up policies = %+v, want none for a disabled policy", desired.Spec.Behavior.ScaleUp.Policies)
		}
		existing := stored(desired)
		existing.Spec.Behavior.ScaleUp.Policies = []autoscalingv2.HPAScalingPolicy{
			{Type: autoscalingv2.PodsScalingPolicy, Value: 4, PeriodSeconds: 15},
			{Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: 15},
		}
		if !hpaMatchesDesired(existing, desired) {
			t.Fatal("stored HPA with defaulted policies does not match the desired HPA")
		}
	})

	differences := map[string]func(*autoscalingv1alpha1.PodAutoscaler){
		"maxReplicas changed": func(pa *autoscalingv1alpha1.PodAutoscaler) { pa.Spec.MaxReplicas++ },
		"minReplicas set":     func(pa *autoscalingv1alpha1.PodAutoscaler) { pa.Spec.MinReplicas = ptr.To(int32(2)) },
		"target changed":      func(pa *autoscalingv1alpha1.PodAutoscaler) { pa.Spec.MetricsSources[0].TargetValue = "77" },
		"label added":         func(pa *autoscalingv1alpha1.PodAutoscaler) { pa.Labels = map[string]string{"team": "a"} },
		"annotation changed": func(pa *autoscalingv1alpha1.PodAutoscaler) {
			pa.Annotations = map[string]string{"autoscaling.aibrix.ai/max-scale-up-rate": "3"}
		},
		"owner changed": func(pa *autoscalingv1alpha1.PodAutoscaler) { pa.UID = "another-uid" },
	}
	for name, mutate := range differences {
		t.Run(name, func(t *testing.T) {
			existing := stored(desiredFor(t, nil))
			desired := desiredFor(t, mutate)
			if hpaMatchesDesired(existing, desired) {
				t.Fatal("a real difference was treated as up to date")
			}
			// The reverse direction covers a value being removed again.
			if hpaMatchesDesired(stored(desired), desiredFor(t, nil)) {
				t.Fatal("removing the difference again was treated as up to date")
			}
		})
	}
}

// computeStatus reports AbleToScale and ScalingActive itself when the strategy
// path will not run. When it will, it only supplies a starting value and
// leaves whatever that path last reported.
func TestComputeStatusLeavesStrategyConditionsToTheStrategyPath(t *testing.T) {
	strategyConditions := []metav1.Condition{
		{Type: ConditionAbleToScale, Status: metav1.ConditionTrue, Reason: "SucceededGetScale", Message: "successfully retrieved target scale"},
		{Type: ConditionScalingActive, Status: metav1.ConditionFalse, Reason: ReasonStable, Message: "desired=2, actual=2; reason=stable"},
	}
	newPA := func() *autoscalingv1alpha1.PodAutoscaler {
		pa := validPodAutoscalerForSpec()
		pa.Status.DesiredScale = 2
		pa.Status.ActualScale = 2
		for _, condition := range strategyConditions {
			apimeta.SetStatusCondition(&pa.Status.Conditions, condition)
		}
		return pa
	}

	t.Run("usable spec", func(t *testing.T) {
		pa := newPA()
		status := computeStatus(context.Background(), *pa, validOK(), validOK())

		for _, want := range strategyConditions {
			got := apimeta.FindStatusCondition(status.Conditions, want.Type)
			if got == nil || got.Status != want.Status || got.Reason != want.Reason || got.Message != want.Message {
				t.Fatalf("condition %s = %+v, want it left as %s/%s/%q", want.Type, got, want.Status, want.Reason, want.Message)
			}
		}
		ready := apimeta.FindStatusCondition(status.Conditions, ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			t.Fatalf("Ready = %+v, want True", ready)
		}
	})

	t.Run("usable spec with nothing reported yet", func(t *testing.T) {
		pa := validPodAutoscalerForSpec()
		status := computeStatus(context.Background(), *pa, validOK(), validOK())

		ableToScale := apimeta.FindStatusCondition(status.Conditions, ConditionAbleToScale)
		if ableToScale == nil || ableToScale.Status != metav1.ConditionTrue || ableToScale.Reason != ReasonConfigured {
			t.Fatalf("AbleToScale = %+v, want a starting value of True/%s", ableToScale, ReasonConfigured)
		}
		scalingActive := apimeta.FindStatusCondition(status.Conditions, ConditionScalingActive)
		if scalingActive == nil || scalingActive.Status != metav1.ConditionFalse || scalingActive.Reason != ReasonStable {
			t.Fatalf("ScalingActive = %+v, want a starting value of False/%s", scalingActive, ReasonStable)
		}
	})

	t.Run("usable spec after an invalid one", func(t *testing.T) {
		pa := validPodAutoscalerForSpec()
		apimeta.SetStatusCondition(&pa.Status.Conditions, metav1.Condition{
			Type: ConditionAbleToScale, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec,
		})
		status := computeStatus(context.Background(), *pa, validOK(), validOK())

		ableToScale := apimeta.FindStatusCondition(status.Conditions, ConditionAbleToScale)
		if ableToScale == nil || ableToScale.Status != metav1.ConditionTrue || ableToScale.Reason != ReasonConfigured {
			t.Fatalf("AbleToScale = %+v, want the invalid-spec verdict replaced by True/%s", ableToScale, ReasonConfigured)
		}
	})

	t.Run("invalid spec", func(t *testing.T) {
		pa := newPA()
		status := computeStatus(context.Background(), *pa, invalid(ReasonMetricsConfigError, "boom"), validOK())

		ableToScale := apimeta.FindStatusCondition(status.Conditions, ConditionAbleToScale)
		if ableToScale == nil || ableToScale.Status != metav1.ConditionFalse || ableToScale.Reason != ReasonInvalidSpec {
			t.Fatalf("AbleToScale = %+v, want False/%s", ableToScale, ReasonInvalidSpec)
		}
	})

	t.Run("conflicting autoscaler", func(t *testing.T) {
		pa := newPA()
		status := computeStatus(context.Background(), *pa, validOK(), invalid(ConditionConflict, "already controlled"))

		ableToScale := apimeta.FindStatusCondition(status.Conditions, ConditionAbleToScale)
		if ableToScale == nil || ableToScale.Status != metav1.ConditionFalse || ableToScale.Reason != ReasonInvalidSpec {
			t.Fatalf("AbleToScale = %+v, want False/%s", ableToScale, ReasonInvalidSpec)
		}
	})
}

// A PodAutoscaler that was rejected and then fixed must not keep the
// AbleToScale=False verdict from when its spec was invalid.
func TestReconcileClearsInvalidSpecVerdictOnceSpecIsValid(t *testing.T) {
	for _, strategy := range []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.KPA, autoscalingv1alpha1.HPA} {
		t.Run(string(strategy), func(t *testing.T) {
			pa := stabilityPodAutoscaler(strategy)
			// Left behind by generation 1, whose spec was invalid.
			pa.Generation = 2
			for _, condition := range []metav1.Condition{
				{Type: ConditionValidSpec, Status: metav1.ConditionFalse, Reason: ReasonMetricsConfigError, ObservedGeneration: 1},
				{Type: ConditionAbleToScale, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec},
				{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonInvalidSpec},
			} {
				apimeta.SetStatusCondition(&pa.Status.Conditions, condition)
			}
			h := newStatusStabilityHarness(t, pa, stabilityDeployment(pa.Spec.ScaleTargetRef.Name))

			for i := 0; i < 2; i++ {
				if _, err := h.reconcile(); err != nil {
					t.Fatalf("Reconcile returned error: %v", err)
				}
			}

			latest := h.latest()
			validSpec := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionValidSpec)
			if validSpec == nil || validSpec.Status != metav1.ConditionTrue {
				t.Fatalf("ValidSpec = %+v, want True", validSpec)
			}
			ableToScale := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionAbleToScale)
			if ableToScale == nil || ableToScale.Status != metav1.ConditionTrue {
				t.Fatalf("AbleToScale = %+v, want True once the spec is valid", ableToScale)
			}
			h.requireSettled(4, false)
		})
	}
}
