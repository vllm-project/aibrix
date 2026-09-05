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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
)

// ---- fakes ----
const ns = "ns1"

// fakeWorkloadScaleClient implements the subset of the WorkloadScaleClient used by the reconciler.
type fakeWorkloadScaleClient struct {
	selector labels.Selector
}

func (f *fakeWorkloadScaleClient) Validate(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error {
	return nil
}

func (f *fakeWorkloadScaleClient) SetDesiredReplicas(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, replicas int32) error {
	return nil
}

func (f *fakeWorkloadScaleClient) GetCurrentReplicasFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scaleObj *unstructured.Unstructured) (int32, error) {
	return 1, nil
}

func (f *fakeWorkloadScaleClient) GetPodSelectorFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scaleObj *unstructured.Unstructured) (labels.Selector, error) {
	// Default to app=foo selector to simulate upstream scale selector.
	if f.selector == nil {
		req, _ := labels.NewRequirement("app", selection.Equals, []string{"foo"})
		f.selector = labels.NewSelector().Add(*req)
	}
	return f.selector, nil
}

// fakeAutoScaler captures the last request and returns a canned result.
type fakeAutoScaler struct {
	lastRequest *ReplicaComputeRequest
	result      *ReplicaComputeResult
	err         error
}

func (f *fakeAutoScaler) ComputeDesiredReplicas(ctx context.Context, req ReplicaComputeRequest) (*ReplicaComputeResult, error) {
	f.lastRequest = &req
	if f.err != nil {
		return f.result, f.err
	}
	if f.result == nil {
		return &ReplicaComputeResult{DesiredReplicas: req.CurrentReplicas, Valid: true}, nil
	}
	return f.result, f.err
}

func readyPod(ns, name string, lbls map[string]string) *corev1.Pod {
	pod := buildPod(ns, name, lbls)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	return pod
}

func notReadyPod(ns, name string, lbls map[string]string) *corev1.Pod {
	pod := buildPod(ns, name, lbls)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionFalse,
	}}
	return pod
}

func TestValidateMetricsSourcesAllowsK8sExternalMetrics(t *testing.T) {
	for _, metricSourceType := range []autoscalingv1alpha1.MetricSourceType{
		autoscalingv1alpha1.EXTERNAL,
		autoscalingv1alpha1.DOMAIN,
	} {
		t.Run(string(metricSourceType), func(t *testing.T) {
			r := &PodAutoscalerReconciler{}
			pa := &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: metricSourceType,
							TargetMetric:     "aibrix_test_queue_depth",
							TargetValue:      "40",
						},
					},
				},
			}

			result := r.validateMetricsSources(pa)

			if !result.Valid {
				t.Fatalf("expected Kubernetes external metrics source to be valid, got reason=%s message=%s", result.Reason, result.Message)
			}
		})
	}
}

func TestValidateMetricsSourcesRequiresTargetMetricForK8sExternalMetrics(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.EXTERNAL,
					TargetValue:      "40",
				},
			},
		},
	}

	result := r.validateMetricsSources(pa)

	if result.Valid {
		t.Fatal("expected Kubernetes external metrics source without targetMetric to be invalid")
	}
	if result.Reason != ReasonMetricsConfigError {
		t.Fatalf("expected reason=%s, got %s", ReasonMetricsConfigError, result.Reason)
	}
	if result.Message != "metricsSource[0]: targetMetric must be specified" {
		t.Fatalf("unexpected message: %s", result.Message)
	}
}

func TestValidateMetricsSourcesAllowsUnregisteredPodMetric(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
					Port:             "8000",
					Path:             "/metrics",
					TargetMetric:     "running_requests",
					TargetValue:      "1",
				},
			},
		},
	}

	result := r.validateMetricsSources(pa)

	if !result.Valid {
		t.Fatalf("expected unregistered POD targetMetric to be valid, got reason=%s message=%s", result.Reason, result.Message)
	}
}

func TestValidateMetricsSourcesAllowsKnownPodMetric(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
					Port:             "8000",
					Path:             "/metrics",
					TargetMetric:     "num_requests_running",
					TargetValue:      "1",
				},
			},
		},
	}

	result := r.validateMetricsSources(pa)

	if !result.Valid {
		t.Fatalf("expected known POD targetMetric to be valid, got reason=%s message=%s", result.Reason, result.Message)
	}
}

func TestValidateSpecRejectsKPARoleSubtargetForDeployment(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Name: "custom-pa-target",
				Kind: "Deployment",
			},
			SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
				RoleName: "prefill",
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     6,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.POD,
					ProtocolType:     autoscalingv1alpha1.HTTP,
					Port:             "8000",
					Path:             "/metrics",
					TargetMetric:     "num_requests_running",
					TargetValue:      "1",
				},
			},
		},
	}

	result := r.validateSpec(pa)

	if result.Valid {
		t.Fatal("expected KPA with subTargetSelector.roleName on a Deployment to be invalid")
	}
	if result.Reason != ReasonInvalidSpec {
		t.Fatalf("expected reason=%s, got %s", ReasonInvalidSpec, result.Reason)
	}
	if result.Message != "subTargetSelector.roleName is only supported for StormService." {
		t.Fatalf("unexpected message: %s", result.Message)
	}
}

func TestValidateSpecRejectsHPARoleSubtarget(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Name: "test-stormservice",
				Kind: "StormService",
			},
			SubTargetSelector: &autoscalingv1alpha1.SubTargetSelector{
				RoleName: "decode",
			},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     5,
			ScalingStrategy: autoscalingv1alpha1.HPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "50",
				},
			},
		},
	}

	result := r.validateSpec(pa)

	if result.Valid {
		t.Fatal("expected HPA with subTargetSelector.roleName to be invalid")
	}
	if result.Reason != ReasonInvalidScalingStrategy {
		t.Fatalf("expected reason=%s, got %s", ReasonInvalidScalingStrategy, result.Reason)
	}
	if result.Message != "subTargetSelector.roleName is not supported with scalingStrategy=HPA; use APA or KPA for StormService role-level autoscaling." {
		t.Fatalf("unexpected message: %s", result.Message)
	}
}

func TestValidateSpecRejectsNonPositiveMetricWindows(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Name: "test-deployment",
				Kind: "Deployment",
			},
			MaxReplicas:          5,
			ScalingStrategy:      autoscalingv1alpha1.KPA,
			ObserveWindowSeconds: ptr.To[int64](0),
			PanicWindowSeconds:   ptr.To[int64](-1),
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "50",
				},
			},
		},
	}

	result := r.validateSpec(pa)

	if result.Valid {
		t.Fatal("expected non-positive metric windows to be invalid")
	}
	if result.Reason != ReasonInvalidSpec {
		t.Fatalf("expected reason=%s, got %s", ReasonInvalidSpec, result.Reason)
	}
	if result.Message != "observeWindowSeconds must be greater than 0." {
		t.Fatalf("unexpected message: %s", result.Message)
	}
}

func TestValidateSpecRejectsMetricWindowOverflow(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	pa := &autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				Name: "test-deployment",
				Kind: "Deployment",
			},
			MaxReplicas:          5,
			ScalingStrategy:      autoscalingv1alpha1.KPA,
			ObserveWindowSeconds: ptr.To(maxMetricWindowSeconds + 1),
			MetricsSources: []autoscalingv1alpha1.MetricSource{
				{
					MetricSourceType: autoscalingv1alpha1.RESOURCE,
					TargetMetric:     "cpu",
					TargetValue:      "50",
				},
			},
		},
	}

	result := r.validateSpec(pa)

	if result.Valid {
		t.Fatal("expected oversized metric window to be invalid")
	}
	if result.Reason != ReasonInvalidSpec {
		t.Fatalf("expected reason=%s, got %s", ReasonInvalidSpec, result.Reason)
	}
	if result.Message != "observeWindowSeconds must be less than or equal to 3600." {
		t.Fatalf("unexpected message: %s", result.Message)
	}
}

func TestValidateSpecRejectsPanicWindowGreaterThanObserveWindow(t *testing.T) {
	tests := []struct {
		name                 string
		observeWindowSeconds *int64
		panicWindowSeconds   *int64
	}{
		{
			name:                 "custom panic exceeds custom observe",
			observeWindowSeconds: ptr.To[int64](60),
			panicWindowSeconds:   ptr.To[int64](120),
		},
		{
			name:                 "default panic exceeds custom observe",
			observeWindowSeconds: ptr.To[int64](30),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &PodAutoscalerReconciler{}
			pa := &autoscalingv1alpha1.PodAutoscaler{
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						Name: "test-deployment",
						Kind: "Deployment",
					},
					MaxReplicas:          5,
					ScalingStrategy:      autoscalingv1alpha1.KPA,
					ObserveWindowSeconds: tt.observeWindowSeconds,
					PanicWindowSeconds:   tt.panicWindowSeconds,
					MetricsSources: []autoscalingv1alpha1.MetricSource{
						{
							MetricSourceType: autoscalingv1alpha1.RESOURCE,
							TargetMetric:     "cpu",
							TargetValue:      "50",
						},
					},
				},
			}

			result := r.validateSpec(pa)

			if result.Valid {
				t.Fatal("expected panic window greater than observe window to be invalid")
			}
			if result.Reason != ReasonInvalidSpec {
				t.Fatalf("expected reason=%s, got %s", ReasonInvalidSpec, result.Reason)
			}
			if result.Message != "panicWindowSeconds must be less than or equal to observeWindowSeconds." {
				t.Fatalf("unexpected message: %s", result.Message)
			}
		})
	}
}

func TestValidateSpecRejectsInvalidBaseReplicaBounds(t *testing.T) {
	tests := map[string]struct {
		mutate      func(*autoscalingv1alpha1.PodAutoscaler)
		wantReason  string
		wantMessage string
	}{
		"negative minReplicas": {
			mutate: func(pa *autoscalingv1alpha1.PodAutoscaler) {
				pa.Spec.MinReplicas = ptr.To(int32(-1))
			},
			wantReason:  ReasonInvalidBounds,
			wantMessage: "minReplicas must not be negative.",
		},
		"non-positive maxReplicas": {
			mutate: func(pa *autoscalingv1alpha1.PodAutoscaler) {
				pa.Spec.MaxReplicas = 0
			},
			wantReason:  ReasonInvalidBounds,
			wantMessage: "maxReplicas must be positive.",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := validPodAutoscalerForSpec()
			tt.mutate(pa)

			result := (&PodAutoscalerReconciler{}).validateSpec(pa)

			if result.Valid {
				t.Fatal("expected invalid base replica bounds to be rejected")
			}
			if result.Reason != tt.wantReason {
				t.Fatalf("expected reason=%s, got %s", tt.wantReason, result.Reason)
			}
			if result.Message != tt.wantMessage {
				t.Fatalf("expected message %q, got %q", tt.wantMessage, result.Message)
			}
		})
	}
}

func TestValidateSpecRejectsInvalidTargetValue(t *testing.T) {
	tests := map[string]struct {
		targetValue string
		wantReason  string
		wantMessage string
	}{
		"kubernetes quantity format": {
			targetValue: "100m",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "100m" must be a valid number`,
		},
		"non-numeric": {
			targetValue: "abc",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "abc" must be a valid number`,
		},
		"zero": {
			targetValue: "0",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "0" must be a finite number greater than 0`,
		},
		"negative": {
			targetValue: "-5",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "-5" must be a finite number greater than 0`,
		},
		"NaN": {
			targetValue: "NaN",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "NaN" must be a finite number greater than 0`,
		},
		"positive infinity": {
			targetValue: "+Inf",
			wantReason:  ReasonMetricsConfigError,
			wantMessage: `metricsSource[0]: targetValue "+Inf" must be a finite number greater than 0`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := validPodAutoscalerForSpec()
			pa.Spec.MetricsSources[0].TargetValue = tt.targetValue

			result := (&PodAutoscalerReconciler{}).validateSpec(pa)

			if result.Valid {
				t.Fatalf("expected targetValue %q to be rejected", tt.targetValue)
			}
			if result.Reason != tt.wantReason {
				t.Fatalf("expected reason=%s, got %s", tt.wantReason, result.Reason)
			}
			if result.Message != tt.wantMessage {
				t.Fatalf("expected message %q, got %q", tt.wantMessage, result.Message)
			}
		})
	}
}

func TestValidateSpecRejectsHPATargetValueOverflow(t *testing.T) {
	tests := map[string]struct {
		targetMetric string
		targetValue  string
	}{
		"cpu":    {targetMetric: "cpu", targetValue: "2147483647.1"},
		"memory": {targetMetric: "memory", targetValue: "8796093022207.1"},
		"pods":   {targetMetric: "requests", targetValue: "9223372036854775808"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := validPodAutoscalerForSpec()
			pa.Spec.ScalingStrategy = autoscalingv1alpha1.HPA
			pa.Spec.MetricsSources[0].TargetMetric = tt.targetMetric
			pa.Spec.MetricsSources[0].TargetValue = tt.targetValue
			if tt.targetMetric != "cpu" && tt.targetMetric != "memory" {
				pa.Spec.MetricsSources[0].MetricSourceType = autoscalingv1alpha1.CUSTOM
			}

			result := (&PodAutoscalerReconciler{}).validateSpec(pa)

			if result.Valid {
				t.Fatalf("expected HPA targetValue %q for %s to be rejected", tt.targetValue, tt.targetMetric)
			}
			if result.Reason != ReasonMetricsConfigError {
				t.Fatalf("expected reason=%s, got %s", ReasonMetricsConfigError, result.Reason)
			}
			if !strings.Contains(result.Message, "must be representable as an HPA metric target") {
				t.Fatalf("unexpected message: %q", result.Message)
			}
		})
	}
}

func TestSetInvalidSpecStatusPersistsConditions(t *testing.T) {
	ctx := context.Background()

	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	pa := validPodAutoscalerForSpec()
	pa.APIVersion = autoscalingv1alpha1.GroupVersion.String()
	pa.Kind = "PodAutoscaler"
	pa.Namespace = ns
	pa.Name = "test-pa"
	pa.Generation = 7
	pa.Status.ScalingHistory = []autoscalingv1alpha1.ScalingDecision{{
		Timestamp:     metav1.NewTime(time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)),
		PreviousScale: 1,
		NewScale:      2,
		Reason:        "test scaling decision",
		Success:       true,
	}}
	wantScalingHistory := append([]autoscalingv1alpha1.ScalingDecision(nil), pa.Status.ScalingHistory...)
	// Simulate an existing PodAutoscaler after a successful reconcile. In this
	// case every condition updated by setInvalidSpecStatus already exists, so no
	// append can accidentally hide a shared Conditions backing array.
	for _, condition := range []metav1.Condition{
		{Type: ConditionValidSpec, Status: metav1.ConditionTrue, Reason: ReasonAsExpected},
		{Type: ConditionScalingActive, Status: metav1.ConditionFalse, Reason: ReasonStable},
		{Type: ConditionAbleToScale, Status: metav1.ConditionTrue, Reason: ReasonConfigured},
		{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonAsExpected},
	} {
		apimeta.SetStatusCondition(&pa.Status.Conditions, condition)
	}

	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&autoscalingv1alpha1.PodAutoscaler{}).
		WithObjects(pa).
		Build()
	r := &PodAutoscalerReconciler{Client: cl}

	if err := r.setInvalidSpecStatus(ctx, pa, ReasonMetricsConfigError, "boom"); err != nil {
		t.Fatalf("setInvalidSpecStatus returned error: %v", err)
	}

	latest := &autoscalingv1alpha1.PodAutoscaler{}
	if err := cl.Get(ctx, ktypes.NamespacedName{Namespace: ns, Name: "test-pa"}, latest); err != nil {
		t.Fatalf("failed to get updated PodAutoscaler: %v", err)
	}
	validSpec := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionValidSpec)
	if validSpec == nil || validSpec.Status != metav1.ConditionFalse {
		t.Fatalf("expected ValidSpec=False to be persisted, got %+v", validSpec)
	}
	if validSpec.Reason != ReasonMetricsConfigError {
		t.Fatalf("expected reason=%s, got %s", ReasonMetricsConfigError, validSpec.Reason)
	}
	if validSpec.ObservedGeneration != pa.Generation {
		t.Fatalf("expected observedGeneration=%d, got %d", pa.Generation, validSpec.ObservedGeneration)
	}
	ableToScale := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionAbleToScale)
	if ableToScale == nil || ableToScale.Status != metav1.ConditionFalse {
		t.Fatalf("expected AbleToScale=False to be persisted, got %+v", ableToScale)
	}
	ready := apimeta.FindStatusCondition(latest.Status.Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False to be persisted, got %+v", ready)
	}
	if len(latest.Status.ScalingHistory) != len(wantScalingHistory) {
		t.Fatalf("scaling history length changed: got %d, want %d", len(latest.Status.ScalingHistory), len(wantScalingHistory))
	}
	gotDecision, wantDecision := latest.Status.ScalingHistory[0], wantScalingHistory[0]
	if !gotDecision.Timestamp.Time.Equal(wantDecision.Timestamp.Time) ||
		gotDecision.PreviousScale != wantDecision.PreviousScale ||
		gotDecision.NewScale != wantDecision.NewScale ||
		gotDecision.Reason != wantDecision.Reason ||
		gotDecision.Success != wantDecision.Success ||
		gotDecision.Error != wantDecision.Error {
		t.Fatalf("scaling history changed: got %+v, want %+v", latest.Status.ScalingHistory, wantScalingHistory)
	}
	if !hasObservedInvalidSpec(latest) {
		t.Fatal("expected invalid spec to be gated for the observed generation")
	}
	latest.Generation++
	if hasObservedInvalidSpec(latest) {
		t.Fatal("expected a new spec generation to be reconciled")
	}
}

func TestComputeStatusDoesNotMutateInputConditions(t *testing.T) {
	pa := validPodAutoscalerForSpec()
	pa.Status.ScalingHistory = []autoscalingv1alpha1.ScalingDecision{{
		Timestamp:     metav1.NewTime(time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)),
		PreviousScale: 1,
		NewScale:      2,
		Reason:        "test scaling decision",
		Success:       true,
	}}
	for _, condition := range []metav1.Condition{
		{Type: ConditionValidSpec, Status: metav1.ConditionTrue, Reason: ReasonAsExpected},
		{Type: ConditionScalingActive, Status: metav1.ConditionFalse, Reason: ReasonStable},
		{Type: ConditionAbleToScale, Status: metav1.ConditionTrue, Reason: ReasonConfigured},
		{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonAsExpected},
	} {
		apimeta.SetStatusCondition(&pa.Status.Conditions, condition)
	}
	originalStatus := pa.Status.DeepCopy()

	newStatus := computeStatus(
		context.Background(),
		*pa,
		invalid(ReasonMetricsConfigError, "invalid targetValue"),
		validOK(),
	)

	if !reflect.DeepEqual(pa.Status, *originalStatus) {
		t.Fatalf("computeStatus mutated its input status: got %+v, want %+v", pa.Status, *originalStatus)
	}
	validSpec := apimeta.FindStatusCondition(newStatus.Conditions, ConditionValidSpec)
	if validSpec == nil || validSpec.Status != metav1.ConditionFalse {
		t.Fatalf("expected computed ValidSpec=False, got %+v", validSpec)
	}
	ready := apimeta.FindStatusCondition(newStatus.Conditions, ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected computed Ready=False, got %+v", ready)
	}
	if !reflect.DeepEqual(newStatus.ScalingHistory, pa.Status.ScalingHistory) {
		t.Fatalf("computeStatus did not preserve scaling history: got %+v, want %+v", newStatus.ScalingHistory, pa.Status.ScalingHistory)
	}
}

func validPodAutoscalerForSpec() *autoscalingv1alpha1.PodAutoscaler {
	return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
		ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
		MinReplicas:     ptr.To(int32(1)),
		MaxReplicas:     10,
		ScalingStrategy: autoscalingv1alpha1.KPA,
		MetricsSources: []autoscalingv1alpha1.MetricSource{{
			MetricSourceType: autoscalingv1alpha1.RESOURCE,
			TargetMetric:     "cpu",
			TargetValue:      "50",
		}},
	}}
}

func TestComputeScaleDecisionAllowsScheduledMinReplicasFromZero(t *testing.T) {
	activeTime := mustParseTime(t, "2026-08-03T09:30:00Z")
	tests := []struct {
		name          string
		schedule      autoscalingv1alpha1.PodAutoscalerSchedule
		wantDesired   int32
		wantShouldRun bool
	}{
		{
			name: "explicit scheduled min scales from zero",
			schedule: autoscalingv1alpha1.PodAutoscalerSchedule{
				Name:        "business-hours",
				StartTime:   "09:00",
				EndTime:     "10:00",
				MinReplicas: ptr.To[int32](4),
			},
			wantDesired:   4,
			wantShouldRun: true,
		},
		{
			name: "inherited min keeps scale from zero disabled",
			schedule: autoscalingv1alpha1.PodAutoscalerSchedule{
				Name:        "capacity-window",
				StartTime:   "09:00",
				EndTime:     "10:00",
				MaxReplicas: ptr.To[int32](8),
			},
			wantDesired:   0,
			wantShouldRun: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &PodAutoscalerReconciler{
				now: func() time.Time {
					return activeTime
				},
			}
			pa := *validPodAutoscalerForSpec()
			pa.Spec.Schedules = []autoscalingv1alpha1.PodAutoscalerSchedule{tt.schedule}

			decision, err := r.computeScaleDecision(context.Background(), pa, nil, 0)
			if err != nil {
				t.Fatalf("computeScaleDecision returned error: %v", err)
			}
			if decision.DesiredReplicas != tt.wantDesired {
				t.Fatalf("DesiredReplicas=%d, want %d", decision.DesiredReplicas, tt.wantDesired)
			}
			if decision.ShouldScale != tt.wantShouldRun {
				t.Fatalf("ShouldScale=%t, want %t", decision.ShouldScale, tt.wantShouldRun)
			}
		})
	}
}

func TestComputeScaleDecisionPendingGuardDoesNotOverrideHardBounds(t *testing.T) {
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	r := &PodAutoscalerReconciler{
		Client:              fake.NewClientBuilder().WithScheme(sch).Build(),
		workloadScaleClient: &fakeWorkloadScaleClient{},
		autoScaler: &fakeAutoScaler{
			result: &ReplicaComputeResult{
				DesiredReplicas:           12,
				Algorithm:                 "apa",
				Reason:                    "scale-up suppressed: pending replicas have no metrics yet",
				Valid:                     true,
				PendingReplicaGuardActive: true,
			},
		},
	}
	pa := *validPodAutoscalerForSpec()
	pa.Namespace = ns
	pa.Spec.ScalingStrategy = autoscalingv1alpha1.APA
	pa.Spec.MinReplicas = ptr.To[int32](6)
	pa.Spec.MaxReplicas = 10

	scaleObj := buildScaleObject("apps/v1", "Deployment", ns, "test-deployment")

	decision, err := r.computeScaleDecision(context.Background(), pa, scaleObj, 12)

	if err != nil {
		t.Fatalf("computeScaleDecision returned error: %v", err)
	}
	if decision.DesiredReplicas != 10 {
		t.Fatalf("DesiredReplicas=%d, want 10", decision.DesiredReplicas)
	}
	if !decision.ShouldScale {
		t.Fatal("expected maxReplicas boundary to override pending guard")
	}
	if decision.Reason != "All metrics below target" {
		t.Fatalf("Reason=%q", decision.Reason)
	}
}

func TestReplicaStateFromPodsUsesCurrentReplicasMinusReadyPodsAsPending(t *testing.T) {
	labels := map[string]string{"app": "foo"}
	state := replicaStateFromPods([]corev1.Pod{
		*readyPod(ns, "old-ready-1", labels),
		*readyPod(ns, "old-ready-2", labels),
		*readyPod(ns, "old-ready-3", labels),
		*readyPod(ns, "old-ready-4", labels),
		*readyPod(ns, "old-ready-5", labels),
		*notReadyPod(ns, "new-pending-1", labels),
		*notReadyPod(ns, "new-pending-2", labels),
	}, 4)

	if state.ReadyReplicas != 4 {
		t.Fatalf("ReadyReplicas=%d, want 4", state.ReadyReplicas)
	}
	if state.PendingReplicas != 0 {
		t.Fatalf("PendingReplicas=%d, want 0", state.PendingReplicas)
	}
}

func TestReplicaStateFromPodsTreatsMissingPodsAsPending(t *testing.T) {
	labels := map[string]string{"app": "foo"}
	state := replicaStateFromPods([]corev1.Pod{
		*readyPod(ns, "ready-1", labels),
		*readyPod(ns, "ready-2", labels),
	}, 4)

	if state.ReadyReplicas != 2 {
		t.Fatalf("ReadyReplicas=%d, want 2", state.ReadyReplicas)
	}
	if state.PendingReplicas != 2 {
		t.Fatalf("PendingReplicas=%d, want 2", state.PendingReplicas)
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("failed to parse time %q: %v", value, err)
	}
	return parsed
}

// ---- helpers ----

func buildPod(ns, name string, lbls map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    lbls,
		},
	}
}

func buildScaleObject(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func podNames(pods []corev1.Pod) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

func buildStormService(ns, name, roleName string, podGroupSize *int32) *orchestrationv1alpha1.StormService {
	ss := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				Spec: &orchestrationv1alpha1.RoleSetSpec{
					Roles: []orchestrationv1alpha1.RoleSpec{
						{
							Name:         roleName,
							PodGroupSize: podGroupSize,
						},
					},
				},
			},
		},
	}
	return ss
}

// ---- tests ----

// TestComputeMetricBasedReplicas_Deployment_NoIndexFilter verifies that when scaling a non-StormService
// workload (e.g., Deployment), the reconciler does NOT enforce PodGroupIndexLabelKey=0 and simply uses
// the base selector (app=foo), thus including all matching pods regardless of pod-group index.
func TestComputeMetricBasedReplicas_Deployment_NoIndexFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Prepare scheme.
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	// Pods: two with app=foo and different group index; one with a different app.
	p0 := buildPod(ns, "p-0", map[string]string{
		"app":                           "foo",
		constants.PodGroupIndexLabelKey: "0",
	})
	p1 := buildPod(ns, "p-1", map[string]string{
		"app":                           "foo",
		constants.PodGroupIndexLabelKey: "1",
	})
	pWrongApp := buildPod(ns, "p-other-app", map[string]string{
		"app":                           "bar",
		constants.PodGroupIndexLabelKey: "0",
	})

	cl := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(p0, p1, pWrongApp).
		Build()

	pa := autoscalingv1alpha1.PodAutoscaler{}
	pa.Namespace = ns

	// Scale target is a Deployment (not StormService).
	scaleObj := buildScaleObject("apps/v1", "Deployment", ns, "foo-deploy")

	// Fakes.
	wlc := &fakeWorkloadScaleClient{}
	as := &fakeAutoScaler{}

	r := &PodAutoscalerReconciler{
		Client:              cl,
		workloadScaleClient: wlc,
		autoScaler:          as,
	}
	scalingCtx := scalingctx.NewBaseScalingContext()

	currentReplicas := int32(2)
	res, err := r.computeMetricBasedReplicas(ctx, pa, scalingCtx, scaleObj, currentReplicas)
	if err != nil {
		t.Fatalf("computeMetricBasedReplicas returned error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil result")
	}
	if as.lastRequest == nil {
		t.Fatalf("autoscaler did not receive request")
	}
	if as.lastRequest.CurrentReplicas != currentReplicas {
		t.Fatalf("CurrentReplicas mismatch: got=%d want=%d", as.lastRequest.CurrentReplicas, currentReplicas)
	}

	got := podNames(as.lastRequest.Pods)
	want := []string{"p-0", "p-1"} // both foo pods should be included; wrong app excluded
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered pods mismatch, got=%v want=%v", got, want)
	}
}

// TestComputeMetricBasedReplicas_StormService_FiltersIndex0 verifies that when scaling a StormService,
// the reconciler enforces PodGroupIndexLabelKey=0 on top of the base selector.
func TestComputeMetricBasedReplicas_StormService_FiltersIndex0(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Prepare scheme.
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = orchestrationv1alpha1.AddToScheme(sch)

	ssName := "ss-1"

	p0 := buildPod(ns, "p-0", map[string]string{
		constants.StormServiceNameLabelKey: ssName,
		constants.RoleReplicaIndexLabelKey: "0",
		constants.RoleNameLabelKey:         "test-role",
		constants.PodGroupIndexLabelKey:    "0",
	})
	p1 := buildPod(ns, "p-1", map[string]string{
		constants.StormServiceNameLabelKey: ssName,
		constants.RoleReplicaIndexLabelKey: "0",
		constants.RoleNameLabelKey:         "test-role",
		constants.PodGroupIndexLabelKey:    "1",
	})
	pWrongApp := buildPod(ns, "p-other-app", map[string]string{
		constants.StormServiceNameLabelKey: "ss-2",
		constants.RoleReplicaIndexLabelKey: "0",
		constants.PodGroupIndexLabelKey:    "0",
	})

	p2 := buildPod(ns, "p-2", map[string]string{
		constants.StormServiceNameLabelKey: ssName,
		constants.RoleReplicaIndexLabelKey: "0",
		constants.PodGroupIndexLabelKey:    "0",
	})

	tests := []struct {
		name         string
		podGroupSize *int32 // nil, 1, 2
		wantPodNames []string
		roleName     string
	}{
		{
			name:         "Size=2 (Should filter, keep only index 0)",
			podGroupSize: ptr.To(int32(2)),
			wantPodNames: []string{"p-0"},
			roleName:     "test-role",
		},
		{
			name:         "Size=1 (Should NOT filter, keep all)",
			podGroupSize: ptr.To(int32(1)),
			wantPodNames: []string{"p-0", "p-1"},
			roleName:     "test-role",
		},
		{
			name:         "Size=nil (Should NOT filter, keep all with roleName)",
			podGroupSize: nil,
			wantPodNames: []string{"p-0", "p-1"},
			roleName:     "test-role",
		},
		{
			name:         "Size=nil (Should NOT filter, keep all)",
			podGroupSize: nil,
			wantPodNames: []string{"p-0", "p-1", "p-2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ss := buildStormService(ns, ssName, "test-role", tc.podGroupSize)

			cl := fake.NewClientBuilder().WithScheme(sch).
				WithObjects(
					p0.DeepCopy(),
					p1.DeepCopy(),
					p2.DeepCopy(),
					pWrongApp.DeepCopy(),
					ss,
				).
				Build()

			pa := autoscalingv1alpha1.PodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "test-pa"},
				Spec: autoscalingv1alpha1.PodAutoscalerSpec{
					ScaleTargetRef: corev1.ObjectReference{
						APIVersion: "orchestration.aibrix.ai/v1alpha1",
						Kind:       "stormservices",
						Namespace:  ns,
						Name:       ssName,
					},
				},
			}
			if tc.roleName != "" {
				pa.Spec.SubTargetSelector = &autoscalingv1alpha1.SubTargetSelector{
					RoleName: tc.roleName,
				}
			}

			scaleObj := buildScaleObject(orchestrationv1alpha1.GroupVersion.String(), StormService, ns, ssName)

			wlc := NewWorkloadScale(cl, nil)
			as := &fakeAutoScaler{} // reset fakeAutoScaler

			r := &PodAutoscalerReconciler{
				Client:              cl,
				workloadScaleClient: wlc,
				autoScaler:          as,
			}

			scalingCtx := scalingctx.NewBaseScalingContext()

			res, err := r.computeMetricBasedReplicas(ctx, pa, scalingCtx, scaleObj, 3)
			if err != nil {
				t.Fatalf("computeMetricBasedReplicas error: %v", err)
			}
			if res == nil {
				t.Fatal("expected non-nil result")
			}

			if as.lastRequest == nil {
				t.Fatal("autoscaler did not receive request")
			}

			// sort result
			got := podNames(as.lastRequest.Pods)
			sort.Strings(got)
			sort.Strings(tc.wantPodNames)

			if !reflect.DeepEqual(got, tc.wantPodNames) {
				t.Errorf("Mismatch for PodGroupSize %v.\nGot:  %v\nWant: %v",
					tc.podGroupSize, got, tc.wantPodNames)
			}
		})
	}
}

// TestComputeMetricBasedReplicas_RayClusterFleet_FiltersHeadOnly verifies that when scaling a RayClusterFleet,
// the reconciler adds requirement ray.io/node-type=head. It does NOT enforce pod-group index filtering.
func TestComputeMetricBasedReplicas_RayClusterFleet_FiltersHeadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Prepare scheme.
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = orchestrationv1alpha1.AddToScheme(sch)

	headIndex0 := buildPod(ns, "ray-head-index0", map[string]string{
		"app":                           "foo",
		"ray.io/node-type":              "head",
		constants.PodGroupIndexLabelKey: "0",
	})
	headIndex1 := buildPod(ns, "ray-head-index1", map[string]string{
		"app":                           "foo",
		"ray.io/node-type":              "head",
		constants.PodGroupIndexLabelKey: "1",
	})
	workerIndex0 := buildPod(ns, "ray-worker-index0", map[string]string{
		"app":                           "foo",
		"ray.io/node-type":              "worker",
		constants.PodGroupIndexLabelKey: "0",
	})

	cl := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(headIndex0, headIndex1, workerIndex0).
		Build()

	pa := autoscalingv1alpha1.PodAutoscaler{}
	pa.Namespace = ns

	// Scale target is RayClusterFleet; this should add node-type=head requirement only.
	scaleObj := buildScaleObject(orchestrationv1alpha1.GroupVersion.String(), RayClusterFleet, ns, "ray-fleet-1")

	wlc := &fakeWorkloadScaleClient{}
	as := &fakeAutoScaler{}

	r := &PodAutoscalerReconciler{
		Client:              cl,
		workloadScaleClient: wlc,
		autoScaler:          as,
	}
	scalingCtx := scalingctx.NewBaseScalingContext()

	res, err := r.computeMetricBasedReplicas(ctx, pa, scalingCtx, scaleObj, 1)
	if err != nil {
		t.Fatalf("computeMetricBasedReplicas returned error: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil result")
	}
	if as.lastRequest == nil {
		t.Fatalf("autoscaler did not receive request")
	}

	got := podNames(as.lastRequest.Pods)
	// Expect both head pods regardless of pod-group index; worker should be excluded.
	want := []string{"ray-head-index0", "ray-head-index1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered pods mismatch, got=%v want=%v", got, want)
	}
}

// ---- interface assertions (compile-time) ----

var (
	_ interface {
		GetPodSelectorFromScale(context.Context, *autoscalingv1alpha1.PodAutoscaler, *unstructured.Unstructured) (labels.Selector, error)
	} = (*fakeWorkloadScaleClient)(nil)

	_ interface {
		ComputeDesiredReplicas(context.Context, ReplicaComputeRequest) (*ReplicaComputeResult, error)
	} = (*fakeAutoScaler)(nil)
)
