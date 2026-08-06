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
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	if f.result == nil {
		return &ReplicaComputeResult{DesiredReplicas: req.CurrentReplicas}, nil
	}
	return f.result, f.err
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
			pa.Spec.ScheduledBounds = nil
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

func TestValidateSpecRejectsInvalidScheduledBounds(t *testing.T) {
	tests := map[string]struct {
		scheduledBounds []autoscalingv1alpha1.ScheduledReplicaBounds
		wantMessage     string
	}{
		"invalid cron": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name: "invalid-cron", Cron: "*/5 * * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
			}},
			wantMessage: "spec.scheduledBounds[0].cron",
		},
		"invalid duration": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name: "invalid-duration", Cron: "0 9 * * *", MinReplicas: ptr.To(int32(1)),
			}},
			wantMessage: "spec.scheduledBounds[0].duration",
		},
		"invalid timezone": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name: "invalid-timezone", Timezone: "Mars/Olympus_Mons", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(1)),
			}},
			wantMessage: "spec.scheduledBounds[0].timezone",
		},
		"duplicate name": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
				{Name: "peak", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(2))},
				{Name: "peak", Cron: "0 12 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(3))},
			},
			wantMessage: "spec.scheduledBounds[1].name",
		},
		"missing override": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name: "missing-overrides", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour},
			}},
			wantMessage: "spec.scheduledBounds[0]",
		},
		"invalid effective bounds": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name: "invalid-effective-bounds", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(11)),
			}},
			wantMessage: "spec.scheduledBounds[0]",
		},
		"overlap": {
			scheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{
				{Name: "morning", Cron: "0 9 * * *", Duration: metav1.Duration{Duration: 2 * time.Hour}, MinReplicas: ptr.To(int32(2))},
				{Name: "late-morning", Cron: "0 10 * * *", Duration: metav1.Duration{Duration: time.Hour}, MinReplicas: ptr.To(int32(3))},
			},
			wantMessage: "spec.scheduledBounds[1]",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pa := validPodAutoscalerForSpec()
			pa.Spec.ScheduledBounds = tt.scheduledBounds

			result := (&PodAutoscalerReconciler{}).validateSpec(pa)

			if result.Valid {
				t.Fatal("expected invalid scheduled bounds to be rejected")
			}
			if result.Reason != ReasonInvalidSpec {
				t.Fatalf("expected reason=%s, got %s", ReasonInvalidSpec, result.Reason)
			}
			if !strings.Contains(result.Message, tt.wantMessage) {
				t.Fatalf("expected message %q to contain %q", result.Message, tt.wantMessage)
			}
		})
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

func activeScheduledBoundsCron() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%d 0-23 * * *", now.Minute())
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

func TestComputeScaleDecisionScheduledMaxClampsCurrentReplicasAboveScheduledMax(t *testing.T) {
	pa := autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MinReplicas:     ptr.To(int32(3)),
			MaxReplicas:     5,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "peak-limit",
				Cron:        activeScheduledBoundsCron(),
				Duration:    metav1.Duration{Duration: time.Hour},
				MaxReplicas: ptr.To(int32(3)),
			}},
		},
	}

	decision, err := (&PodAutoscalerReconciler{}).computeScaleDecision(context.Background(), pa, nil, 7)

	if err != nil {
		t.Fatalf("computeScaleDecision returned error: %v", err)
	}
	if decision.DesiredReplicas != 3 {
		t.Fatalf("expected scheduled max to clamp desired replicas to 3, got %d", decision.DesiredReplicas)
	}
	if !decision.ShouldScale {
		t.Fatal("expected scheduled max clamp to request scaling")
	}
}

func TestComputeScaleDecisionScheduledMinClampsCurrentReplicasBelowScheduledMin(t *testing.T) {
	pa := autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MinReplicas:     ptr.To(int32(3)),
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "peak-floor",
				Cron:        activeScheduledBoundsCron(),
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(5)),
			}},
		},
	}

	decision, err := (&PodAutoscalerReconciler{}).computeScaleDecision(context.Background(), pa, nil, 2)

	if err != nil {
		t.Fatalf("computeScaleDecision returned error: %v", err)
	}
	if decision.DesiredReplicas != 5 {
		t.Fatalf("expected scheduled min to clamp desired replicas to 5, got %d", decision.DesiredReplicas)
	}
	if !decision.ShouldScale {
		t.Fatal("expected scheduled min clamp to request scaling")
	}
}

func TestComputeScaleDecisionScheduledMinScalesFromZeroReplicas(t *testing.T) {
	pa := autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MinReplicas:     ptr.To(int32(3)),
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.KPA,
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "peak-floor",
				Cron:        activeScheduledBoundsCron(),
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(5)),
			}},
		},
	}

	decision, err := (&PodAutoscalerReconciler{}).computeScaleDecision(context.Background(), pa, nil, 0)

	if err != nil {
		t.Fatalf("computeScaleDecision returned error: %v", err)
	}
	if decision.DesiredReplicas != 5 {
		t.Fatalf("expected scheduled min to scale from zero replicas to 5, got %d", decision.DesiredReplicas)
	}
	if !decision.ShouldScale {
		t.Fatal("expected scheduled min to request scaling from zero replicas")
	}
}

func TestCreateScalingContextScheduledBounds(t *testing.T) {
	pa := autoscalingv1alpha1.PodAutoscaler{
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MinReplicas: ptr.To(int32(3)),
			MaxReplicas: 10,
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "peak",
				Cron:        activeScheduledBoundsCron(),
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(5)),
				MaxReplicas: ptr.To(int32(8)),
			}},
		},
	}

	scalingContext := (&PodAutoscalerReconciler{}).createScalingContext(pa)

	if got := scalingContext.GetMinReplicas(); got != 5 {
		t.Fatalf("expected scheduled min replicas 5 in scaling context, got %d", got)
	}
	if got := scalingContext.GetMaxReplicas(); got != 8 {
		t.Fatalf("expected scheduled max replicas 8 in scaling context, got %d", got)
	}
}

func TestMakeHPAScheduledBoundsReconcileHPAUsesEffectiveBounds(t *testing.T) {
	now := time.Date(2026, time.August, 5, 9, 30, 0, 0, time.UTC)
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = autoscalingv2.AddToScheme(sch)

	pa := &autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scheduled-pa",
			Namespace: ns,
		},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "scheduled-target",
			},
			MinReplicas:     ptr.To(int32(3)),
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.HPA,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "30",
			}},
			ScheduledBounds: []autoscalingv1alpha1.ScheduledReplicaBounds{{
				Name:        "peak",
				Cron:        "0 9 * * *",
				Duration:    metav1.Duration{Duration: time.Hour},
				MinReplicas: ptr.To(int32(5)),
				MaxReplicas: ptr.To(int32(8)),
			}},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(pa).Build()
	r := &PodAutoscalerReconciler{
		Client:  cl,
		nowFunc: func() time.Time { return now },
	}

	if _, err := r.reconcileHPA(context.Background(), *pa); err != nil {
		t.Fatalf("reconcileHPA returned error: %v", err)
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "scheduled-pa-hpa"}, hpa); err != nil {
		t.Fatalf("expected reconciled HPA to be created: %v", err)
	}
	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 5 {
		t.Fatalf("expected reconciled HPA minReplicas 5, got %v", hpa.Spec.MinReplicas)
	}
	if hpa.Spec.MaxReplicas != 8 {
		t.Fatalf("expected reconciled HPA maxReplicas 8, got %d", hpa.Spec.MaxReplicas)
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
