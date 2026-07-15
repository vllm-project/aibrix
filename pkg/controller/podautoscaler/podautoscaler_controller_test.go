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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/circuitbreaker"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/types"
)

// ---- fakes ----
const ns = "ns1"

// fakeWorkloadScaleClient implements the subset of the WorkloadScaleClient used by the reconciler.
type fakeWorkloadScaleClient struct {
	selector           labels.Selector
	currentReplicas    int32
	setDesiredErr      error
	setDesiredReplicas []int32
}

func (f *fakeWorkloadScaleClient) Validate(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error {
	return nil
}

func (f *fakeWorkloadScaleClient) SetDesiredReplicas(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, replicas int32) error {
	f.setDesiredReplicas = append(f.setDesiredReplicas, replicas)
	return f.setDesiredErr
}

func (f *fakeWorkloadScaleClient) GetCurrentReplicasFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scaleObj *unstructured.Unstructured) (int32, error) {
	if f.currentReplicas != 0 {
		return f.currentReplicas, nil
	}
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
	result      *MetricRoundResult
	err         error
}

func (f *fakeAutoScaler) ComputeDesiredReplicas(ctx context.Context, req ReplicaComputeRequest) (*MetricRoundResult, error) {
	f.lastRequest = &req
	if f.err != nil {
		return f.result, f.err
	}
	if f.result == nil {
		return &MetricRoundResult{DesiredReplicas: req.CurrentReplicas, Valid: true, HasRecommendation: true}, nil
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

func TestValidateSpec_CircuitBreaker(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	validPA := func(strategy autoscalingv1alpha1.ScalingStrategyType, cfg *autoscalingv1alpha1.CircuitBreakerConfig) *autoscalingv1alpha1.PodAutoscaler {
		return &autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			ScaleTargetRef:  corev1.ObjectReference{Name: "test-deployment", Kind: "Deployment"},
			MinReplicas:     ptr.To(int32(1)),
			MaxReplicas:     5,
			ScalingStrategy: strategy,
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.RESOURCE,
				TargetMetric:     "cpu",
				TargetValue:      "50",
			}},
			CircuitBreaker: cfg,
		}}
	}

	for _, tt := range []struct {
		name    string
		pa      *autoscalingv1alpha1.PodAutoscaler
		invalid bool
	}{
		{name: "KPA enabled", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true})},
		{name: "APA enabled", pa: validPA(autoscalingv1alpha1.APA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true})},
		{name: "HPA enabled", pa: validPA(autoscalingv1alpha1.HPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true}), invalid: true},
		{name: "HPA disabled", pa: validPA(autoscalingv1alpha1.HPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: false})},
		{name: "invalid action", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, Action: "panic"}), invalid: true},
		{name: "invalid failure threshold", pa: validPA(autoscalingv1alpha1.APA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, FailureThreshold: -1}), invalid: true},
		{name: "invalid recovery threshold", pa: validPA(autoscalingv1alpha1.KPA, &autoscalingv1alpha1.CircuitBreakerConfig{Enabled: true, RecoveryThreshold: -1}), invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := r.validateSpec(tt.pa)
			if !tt.invalid {
				if !result.Valid {
					t.Fatalf("expected valid spec, got reason=%s message=%s", result.Reason, result.Message)
				}
				return
			}
			if result.Valid {
				t.Fatal("expected invalid circuit breaker configuration")
			}
			if result.Reason != ConditionValidSpec {
				t.Fatalf("expected reason=%s, got %s", ConditionValidSpec, result.Reason)
			}
			if result.Message == "" {
				t.Fatal("expected validation message")
			}
		})
	}
}

func TestValidateMetricsSourcesMultipleSources(t *testing.T) {
	r := &PodAutoscalerReconciler{}
	validSource := autoscalingv1alpha1.MetricSource{
		MetricSourceType: autoscalingv1alpha1.RESOURCE,
		TargetMetric:     "cpu",
		TargetValue:      "50",
	}

	t.Run("two valid sources", func(t *testing.T) {
		second := validSource
		second.TargetMetric = "memory"
		result := r.validateMetricsSources(&autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{validSource, second},
		}})
		if !result.Valid {
			t.Fatalf("expected two valid sources to pass, got reason=%s message=%s", result.Reason, result.Message)
		}
	})

	t.Run("empty sources", func(t *testing.T) {
		result := r.validateMetricsSources(&autoscalingv1alpha1.PodAutoscaler{})
		if result.Valid {
			t.Fatal("expected empty metricsSources to be invalid")
		}
	})

	t.Run("invalid second source", func(t *testing.T) {
		invalidSource := validSource
		invalidSource.TargetMetric = "disk"
		result := r.validateMetricsSources(&autoscalingv1alpha1.PodAutoscaler{Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MetricsSources: []autoscalingv1alpha1.MetricSource{validSource, invalidSource},
		}})
		if result.Valid {
			t.Fatal("expected invalid second metricsSource to be rejected")
		}
	})
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

func TestComputeScaleDecisionCircuitBreaker(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	frozen := int32(4)

	tests := []struct {
		name                string
		pa                  autoscalingv1alpha1.PodAutoscaler
		currentReplicas     int32
		round               *MetricRoundResult
		wantDesiredReplicas int32
		wantShouldScale     bool
		wantProtective      bool
		wantTransition      circuitbreaker.Transition
		wantCBState         autoscalingv1alpha1.CircuitBreakerState
		wantProtected       *int32
		wantHistoryCount    int
	}{
		{
			name:                "all healthy uses normal recommendation",
			pa:                  circuitBreakerDecisionPA(false, nil),
			currentReplicas:     4,
			round:               roundResult(6, types.MetricReasonMetricsHealthy, 1, 0, 0),
			wantDesiredReplicas: 6,
			wantShouldScale:     true,
		},
		{
			name:                "degraded scale up is allowed",
			pa:                  circuitBreakerDecisionPA(true, nil),
			currentReplicas:     4,
			round:               roundResult(6, types.MetricReasonPartialMetricCollectionFailure, 0, 1, 0),
			wantDesiredReplicas: 6,
			wantShouldScale:     true,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateClosed,
		},
		{
			name:                "degraded scale down keeps current replicas",
			pa:                  circuitBreakerDecisionPA(true, nil),
			currentReplicas:     4,
			round:               roundResult(2, types.MetricReasonPartialMetricCollectionFailure, 0, 1, 0),
			wantDesiredReplicas: 4,
			wantShouldScale:     false,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateClosed,
			wantHistoryCount:    1,
		},
		{
			name:                "all failed below threshold keeps current replicas",
			pa:                  circuitBreakerDecisionPA(true, nil),
			currentReplicas:     4,
			round:               failedRoundResult(1),
			wantDesiredReplicas: 4,
			wantShouldScale:     false,
			wantProtective:      true,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateClosed,
		},
		{
			name: "open freeze bypasses normal recommendation",
			pa: circuitBreakerDecisionPA(true, &autoscalingv1alpha1.CircuitBreakerStatus{
				State:             autoscalingv1alpha1.CircuitBreakerStateOpen,
				Action:            autoscalingv1alpha1.CircuitBreakerActionFreeze,
				ProtectedReplicas: &frozen,
			}),
			currentReplicas:     8,
			round:               failedRoundResult(1),
			wantDesiredReplicas: 4,
			wantShouldScale:     true,
			wantProtective:      true,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateOpen,
			wantProtected:       &frozen,
		},
		{
			name: "open max bypasses normal recommendation",
			pa: circuitBreakerDecisionPAWithAction(true, autoscalingv1alpha1.CircuitBreakerActionMax, &autoscalingv1alpha1.CircuitBreakerStatus{
				State:  autoscalingv1alpha1.CircuitBreakerStateOpen,
				Action: autoscalingv1alpha1.CircuitBreakerActionMax,
			}),
			currentReplicas:     4,
			round:               failedRoundResult(1),
			wantDesiredReplicas: 10,
			wantShouldScale:     true,
			wantProtective:      true,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateOpen,
		},
		{
			name: "open recovers on threshold and uses normal recommendation",
			pa: circuitBreakerDecisionPA(true, &autoscalingv1alpha1.CircuitBreakerStatus{
				State:         autoscalingv1alpha1.CircuitBreakerStateOpen,
				Action:        autoscalingv1alpha1.CircuitBreakerActionFreeze,
				RecoveryCount: 2,
			}),
			currentReplicas:     4,
			round:               roundResult(7, types.MetricReasonMetricsHealthy, 1, 0, 0),
			wantDesiredReplicas: 7,
			wantShouldScale:     true,
			wantTransition:      circuitbreaker.TransitionClosed,
			wantCBState:         autoscalingv1alpha1.CircuitBreakerStateClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newDecisionTestReconciler(tt.round, fixedNow)
			scaleObj := buildScaleObject("apps/v1", "Deployment", tt.pa.Namespace, tt.pa.Spec.ScaleTargetRef.Name)

			got, err := r.computeScaleDecision(context.Background(), tt.pa, scaleObj, tt.currentReplicas)

			if err != nil {
				t.Fatalf("computeScaleDecision returned error: %v", err)
			}
			if got.DesiredReplicas != tt.wantDesiredReplicas {
				t.Fatalf("desired replicas = %d, want %d", got.DesiredReplicas, tt.wantDesiredReplicas)
			}
			if got.ShouldScale != tt.wantShouldScale {
				t.Fatalf("shouldScale = %t, want %t", got.ShouldScale, tt.wantShouldScale)
			}
			if got.Protective != tt.wantProtective {
				t.Fatalf("protective = %t, want %t", got.Protective, tt.wantProtective)
			}
			if got.CircuitBreakerTransition != tt.wantTransition {
				t.Fatalf("transition = %q, want %q", got.CircuitBreakerTransition, tt.wantTransition)
			}
			if tt.wantCBState != "" {
				if got.CircuitBreakerStatus == nil {
					t.Fatalf("expected circuit breaker status")
				}
				if got.CircuitBreakerStatus.State != tt.wantCBState {
					t.Fatalf("circuit breaker state = %q, want %q", got.CircuitBreakerStatus.State, tt.wantCBState)
				}
				if !reflect.DeepEqual(got.CircuitBreakerStatus.ProtectedReplicas, tt.wantProtected) {
					t.Fatalf("protected replicas = %v, want %v", got.CircuitBreakerStatus.ProtectedReplicas, tt.wantProtected)
				}
			}
			if r.autoScaler.(*fakeAutoScaler).lastRequest.Timestamp != fixedNow {
				t.Fatalf("autoscaler timestamp = %v, want %v", r.autoScaler.(*fakeAutoScaler).lastRequest.Timestamp, fixedNow)
			}
			if tt.wantHistoryCount > 0 {
				key := tt.pa.Namespace + "/" + tt.pa.Name
				if gotHistoryCount := len(r.recommendations[key]); gotHistoryCount != tt.wantHistoryCount {
					t.Fatalf("recommendation history count = %d, want %d", gotHistoryCount, tt.wantHistoryCount)
				}
			}
		})
	}
}

func TestComputeScaleDecisionDegradedDisabledCompatibility(t *testing.T) {
	pa := circuitBreakerDecisionPA(false, nil)
	r := newDecisionTestReconciler(nil, time.Date(2026, 7, 13, 15, 0, 0, 0, time.UTC))
	r.autoScaler.(*fakeAutoScaler).err = assertAnError{}
	scaleObj := buildScaleObject("apps/v1", "Deployment", pa.Namespace, pa.Spec.ScaleTargetRef.Name)

	_, err := r.computeScaleDecision(context.Background(), pa, scaleObj, 4)

	if err == nil {
		t.Fatal("expected disabled circuit breaker to preserve autoscaler error")
	}
}

func TestReconcileCustomPACircuitBreakerActionFailurePersistsStatus(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC)
	pa := circuitBreakerDecisionPAWithAction(true, autoscalingv1alpha1.CircuitBreakerActionMax, &autoscalingv1alpha1.CircuitBreakerStatus{
		State:  autoscalingv1alpha1.CircuitBreakerStateOpen,
		Action: autoscalingv1alpha1.CircuitBreakerActionMax,
	})
	pa.Name = "pa-action-fail"
	scaleObj := buildScaleObject("apps/v1", "Deployment", pa.Namespace, pa.Spec.ScaleTargetRef.Name)
	pod := buildPod(pa.Namespace, "pod-0", map[string]string{"app": "foo"})

	sch := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&autoscalingv1alpha1.PodAutoscaler{}).
		WithObjects(&pa, scaleObj, pod).
		Build()

	scaleClient := &fakeWorkloadScaleClient{
		currentReplicas: 4,
		setDesiredErr:   assertAnError{},
	}
	r := &PodAutoscalerReconciler{
		Client:              cl,
		Mapper:              deploymentRESTMapper(),
		EventRecorder:       record.NewFakeRecorder(20),
		workloadScaleClient: scaleClient,
		autoScaler:          &fakeAutoScaler{result: failedRoundResult(1)},
		monitor:             monitor.New(),
		now:                 func() time.Time { return fixedNow },
	}

	_, err := r.reconcileCustomPA(context.Background(), pa)

	if err == nil {
		t.Fatal("expected protective scale failure")
	}
	if len(scaleClient.setDesiredReplicas) != 1 || scaleClient.setDesiredReplicas[0] != pa.Spec.MaxReplicas {
		t.Fatalf("scale calls = %v, want [%d]", scaleClient.setDesiredReplicas, pa.Spec.MaxReplicas)
	}
	var got autoscalingv1alpha1.PodAutoscaler
	if getErr := cl.Get(context.Background(), client.ObjectKey{Namespace: pa.Namespace, Name: pa.Name}, &got); getErr != nil {
		t.Fatalf("get updated PA: %v", getErr)
	}
	if got.Status.CircuitBreaker == nil {
		t.Fatalf("expected circuit breaker status to be persisted")
	}
	if got.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateOpen {
		t.Fatalf("circuit breaker state = %q, want Open", got.Status.CircuitBreaker.State)
	}
	able := findCondition(got.Status.Conditions, ConditionAbleToScale)
	if able == nil || able.Status != metav1.ConditionFalse || able.Reason != "ProtectiveActionFailed" {
		t.Fatalf("AbleToScale condition = %#v, want False/ProtectiveActionFailed", able)
	}
	triggered := findCondition(got.Status.Conditions, ConditionCircuitBreakerTriggered)
	if triggered == nil || triggered.Status != metav1.ConditionTrue {
		t.Fatalf("CircuitBreakerTriggered condition = %#v, want True", triggered)
	}

	scaleClient.setDesiredErr = nil
	if _, retryErr := r.reconcileCustomPA(context.Background(), got); retryErr != nil {
		t.Fatalf("retry reconcile after clearing scale error: %v", retryErr)
	}
	if !reflect.DeepEqual(scaleClient.setDesiredReplicas, []int32{pa.Spec.MaxReplicas, pa.Spec.MaxReplicas}) {
		t.Fatalf("scale calls = %v, want retry of max action", scaleClient.setDesiredReplicas)
	}
}

func TestReconcileCustomPACircuitBreakerPersistsStateAcrossRounds(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 16, 30, 0, 0, time.UTC)
	pa := circuitBreakerDecisionPA(true, nil)
	pa.Name = "pa-state-sequence"
	r, _, scaleClient, cl := newCircuitBreakerReconcileFixture(t, pa, roundResult(6, types.MetricReasonPartialMetricCollectionFailure, 0, 1, 0), fixedNow)

	runRound := func(round *MetricRoundResult) autoscalingv1alpha1.PodAutoscaler {
		t.Helper()
		var latest autoscalingv1alpha1.PodAutoscaler
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: pa.Namespace, Name: pa.Name}, &latest); err != nil {
			t.Fatalf("get latest PA: %v", err)
		}
		r.autoScaler.(*fakeAutoScaler).result = round
		if _, err := r.reconcileCustomPA(context.Background(), latest); err != nil {
			t.Fatalf("reconcile round: %v", err)
		}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: pa.Namespace, Name: pa.Name}, &latest); err != nil {
			t.Fatalf("get updated PA: %v", err)
		}
		return latest
	}

	latest := runRound(roundResult(6, types.MetricReasonPartialMetricCollectionFailure, 0, 1, 0))
	if latest.Status.CircuitBreaker == nil || latest.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateClosed {
		t.Fatalf("degraded scale-up circuit breaker status = %#v, want Closed", latest.Status.CircuitBreaker)
	}
	if latest.Status.DesiredScale != 6 {
		t.Fatalf("degraded scale-up desired scale = %d, want 6", latest.Status.DesiredScale)
	}

	latest = runRound(roundResult(2, types.MetricReasonPartialMetricCollectionFailure, 0, 1, 0))
	if latest.Status.DesiredScale != 4 {
		t.Fatalf("degraded scale-down desired scale = %d, want hold current 4", latest.Status.DesiredScale)
	}
	if latest.Status.CircuitBreaker.FailureCount != 0 {
		t.Fatalf("degraded round failure count = %d, want 0", latest.Status.CircuitBreaker.FailureCount)
	}

	for wantFailures := int32(1); wantFailures <= 2; wantFailures++ {
		latest = runRound(failedRoundResult(1))
		if latest.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateClosed {
			t.Fatalf("failure round %d state = %q, want Closed", wantFailures, latest.Status.CircuitBreaker.State)
		}
		if latest.Status.CircuitBreaker.FailureCount != wantFailures {
			t.Fatalf("failure round %d failure count = %d, want %d", wantFailures, latest.Status.CircuitBreaker.FailureCount, wantFailures)
		}
		if latest.Status.DesiredScale != 4 {
			t.Fatalf("failure round %d desired scale = %d, want hold current 4", wantFailures, latest.Status.DesiredScale)
		}
	}

	latest = runRound(failedRoundResult(1))
	if latest.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateOpen {
		t.Fatalf("third failure state = %q, want Open", latest.Status.CircuitBreaker.State)
	}
	if latest.Status.CircuitBreaker.ProtectedReplicas == nil || *latest.Status.CircuitBreaker.ProtectedReplicas != 4 {
		t.Fatalf("protected replicas = %v, want 4", latest.Status.CircuitBreaker.ProtectedReplicas)
	}
	triggered := findCondition(latest.Status.Conditions, ConditionCircuitBreakerTriggered)
	if triggered == nil || triggered.Status != metav1.ConditionTrue {
		t.Fatalf("CircuitBreakerTriggered condition = %#v, want True", triggered)
	}

	for wantRecoveries := int32(1); wantRecoveries <= 2; wantRecoveries++ {
		latest = runRound(roundResult(7, types.MetricReasonMetricsHealthy, 1, 0, 0))
		if latest.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateOpen {
			t.Fatalf("recovery round %d state = %q, want Open", wantRecoveries, latest.Status.CircuitBreaker.State)
		}
		if latest.Status.CircuitBreaker.RecoveryCount != wantRecoveries {
			t.Fatalf("recovery round %d recovery count = %d, want %d", wantRecoveries, latest.Status.CircuitBreaker.RecoveryCount, wantRecoveries)
		}
		if latest.Status.DesiredScale != 4 {
			t.Fatalf("recovery round %d desired scale = %d, want protected 4", wantRecoveries, latest.Status.DesiredScale)
		}
	}

	latest = runRound(roundResult(7, types.MetricReasonMetricsHealthy, 1, 0, 0))
	if latest.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateClosed {
		t.Fatalf("recovered state = %q, want Closed", latest.Status.CircuitBreaker.State)
	}
	if latest.Status.CircuitBreaker.RecoveryCount != 0 {
		t.Fatalf("recovered recovery count = %d, want 0", latest.Status.CircuitBreaker.RecoveryCount)
	}
	if latest.Status.DesiredScale != 7 {
		t.Fatalf("recovered desired scale = %d, want normal recommendation 7", latest.Status.DesiredScale)
	}
	triggered = findCondition(latest.Status.Conditions, ConditionCircuitBreakerTriggered)
	if triggered == nil || triggered.Status != metav1.ConditionFalse {
		t.Fatalf("CircuitBreakerTriggered condition = %#v, want False after recovery", triggered)
	}
	if !reflect.DeepEqual(scaleClient.setDesiredReplicas, []int32{6, 7}) {
		t.Fatalf("scale calls = %v, want [6 7]", scaleClient.setDesiredReplicas)
	}
}

func TestReconcileCustomPACircuitBreakerRestoresFreezeFromStatus(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 16, 45, 0, 0, time.UTC)
	protected := int32(3)
	pa := circuitBreakerDecisionPA(true, &autoscalingv1alpha1.CircuitBreakerStatus{
		State:             autoscalingv1alpha1.CircuitBreakerStateOpen,
		Action:            autoscalingv1alpha1.CircuitBreakerActionFreeze,
		ProtectedReplicas: &protected,
	})
	pa.Name = "pa-restart-freeze"
	r, _, scaleClient, cl := newCircuitBreakerReconcileFixture(t, pa, failedRoundResult(1), fixedNow)
	scaleClient.currentReplicas = 8

	if _, err := r.reconcileCustomPA(context.Background(), pa); err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}
	if !reflect.DeepEqual(scaleClient.setDesiredReplicas, []int32{protected}) {
		t.Fatalf("scale calls = %v, want restored freeze target %d", scaleClient.setDesiredReplicas, protected)
	}

	var got autoscalingv1alpha1.PodAutoscaler
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: pa.Namespace, Name: pa.Name}, &got); err != nil {
		t.Fatalf("get updated PA: %v", err)
	}
	if got.Status.CircuitBreaker == nil || got.Status.CircuitBreaker.ProtectedReplicas == nil || *got.Status.CircuitBreaker.ProtectedReplicas != protected {
		t.Fatalf("persisted protected replicas = %#v, want %d", got.Status.CircuitBreaker, protected)
	}
}

func TestReconcileCustomPACircuitBreakerStateIsIsolatedPerPodAutoscaler(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 16, 50, 0, 0, time.UTC)
	paA := circuitBreakerDecisionPA(true, nil)
	paA.Name = "pa-isolated-a"
	paB := circuitBreakerDecisionPA(true, nil)
	paB.Name = "pa-isolated-b"
	scaleObj := buildScaleObject("apps/v1", "Deployment", paA.Namespace, paA.Spec.ScaleTargetRef.Name)
	pod := buildPod(paA.Namespace, "pod-0", map[string]string{"app": "foo"})

	sch := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&autoscalingv1alpha1.PodAutoscaler{}).
		WithObjects(&paA, &paB, scaleObj, pod).
		Build()
	r := &PodAutoscalerReconciler{
		Client:              cl,
		Mapper:              deploymentRESTMapper(),
		EventRecorder:       record.NewFakeRecorder(20),
		workloadScaleClient: &fakeWorkloadScaleClient{currentReplicas: 4},
		autoScaler:          &fakeAutoScaler{result: failedRoundResult(1)},
		monitor:             monitor.New(),
		now:                 func() time.Time { return fixedNow },
	}

	if _, err := r.reconcileCustomPA(context.Background(), paA); err != nil {
		t.Fatalf("first paA reconcile: %v", err)
	}
	var latestA autoscalingv1alpha1.PodAutoscaler
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: paA.Namespace, Name: paA.Name}, &latestA); err != nil {
		t.Fatalf("get paA: %v", err)
	}
	if _, err := r.reconcileCustomPA(context.Background(), latestA); err != nil {
		t.Fatalf("second paA reconcile: %v", err)
	}
	if _, err := r.reconcileCustomPA(context.Background(), paB); err != nil {
		t.Fatalf("paB reconcile: %v", err)
	}

	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: paA.Namespace, Name: paA.Name}, &latestA); err != nil {
		t.Fatalf("get updated paA: %v", err)
	}
	var latestB autoscalingv1alpha1.PodAutoscaler
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: paB.Namespace, Name: paB.Name}, &latestB); err != nil {
		t.Fatalf("get updated paB: %v", err)
	}
	if latestA.Status.CircuitBreaker.FailureCount != 2 {
		t.Fatalf("paA failure count = %d, want 2", latestA.Status.CircuitBreaker.FailureCount)
	}
	if latestB.Status.CircuitBreaker.FailureCount != 1 {
		t.Fatalf("paB failure count = %d, want 1", latestB.Status.CircuitBreaker.FailureCount)
	}
}

func TestReconcileCustomPACircuitBreakerEventsOnlyOnTransitions(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 17, 0, 0, 0, time.UTC)
	pa := circuitBreakerDecisionPA(true, nil)
	pa.Name = "pa-events"
	pa.Spec.CircuitBreaker.FailureThreshold = 1
	r, recorder, _, cl := newCircuitBreakerReconcileFixture(t, pa, failedRoundResult(1), fixedNow)

	if _, err := r.reconcileCustomPA(context.Background(), pa); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	firstEvents := drainEvents(recorder)
	assertContainsEvent(t, firstEvents, "CircuitBreakerOpened")

	var updated autoscalingv1alpha1.PodAutoscaler
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: pa.Namespace, Name: pa.Name}, &updated); err != nil {
		t.Fatalf("get updated PA: %v", err)
	}
	r.autoScaler.(*fakeAutoScaler).result = failedRoundResult(1)
	if _, err := r.reconcileCustomPA(context.Background(), updated); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	secondEvents := drainEvents(recorder)
	assertNotContainsEvent(t, secondEvents, "CircuitBreakerOpened")
}

func TestReconcileCustomPACircuitBreakerActionFailureEvent(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	pa := circuitBreakerDecisionPAWithAction(true, autoscalingv1alpha1.CircuitBreakerActionMax, &autoscalingv1alpha1.CircuitBreakerStatus{
		State:  autoscalingv1alpha1.CircuitBreakerStateOpen,
		Action: autoscalingv1alpha1.CircuitBreakerActionMax,
	})
	pa.Name = "pa-action-fail-event"
	r, recorder, scaleClient, _ := newCircuitBreakerReconcileFixture(t, pa, failedRoundResult(1), fixedNow)
	scaleClient.setDesiredErr = assertAnError{}

	if _, err := r.reconcileCustomPA(context.Background(), pa); err == nil {
		t.Fatal("expected protective action failure")
	}

	events := drainEvents(recorder)
	assertContainsEvent(t, events, "CircuitBreakerActionFailed")
}

func TestReconcileCustomPACircuitBreakerRecordsObservabilityOnceOnOpenActionFailure(t *testing.T) {
	fixedNow := time.Date(2026, 7, 13, 18, 30, 0, 0, time.UTC)
	pa := circuitBreakerDecisionPAWithAction(true, autoscalingv1alpha1.CircuitBreakerActionMax, nil)
	pa.Name = "pa-open-action-fail-event"
	pa.Spec.CircuitBreaker.FailureThreshold = 1
	r, recorder, scaleClient, _ := newCircuitBreakerReconcileFixture(t, pa, failedRoundResult(1), fixedNow)
	scaleClient.setDesiredErr = assertAnError{}

	if _, err := r.reconcileCustomPA(context.Background(), pa); err == nil {
		t.Fatal("expected protective action failure")
	}

	events := drainEvents(recorder)
	assertEventCount(t, events, "CircuitBreakerOpened", 1)
	assertEventCount(t, events, "CircuitBreakerActionFailed", 1)
}

func TestSetStatusPreservesCircuitBreaker(t *testing.T) {
	pa := circuitBreakerDecisionPA(true, &autoscalingv1alpha1.CircuitBreakerStatus{
		State:        autoscalingv1alpha1.CircuitBreakerStateOpen,
		Action:       autoscalingv1alpha1.CircuitBreakerActionFreeze,
		FailureCount: 3,
	})
	setCondition(&pa, ConditionCircuitBreakerTriggered, metav1.ConditionTrue, "CircuitBreakerOpened", "open")

	setStatus(&pa, 4, 4, false, "stable", true, nil)

	if pa.Status.CircuitBreaker == nil {
		t.Fatal("expected circuit breaker status to be preserved")
	}
	if pa.Status.CircuitBreaker.State != autoscalingv1alpha1.CircuitBreakerStateOpen {
		t.Fatalf("state = %q, want Open", pa.Status.CircuitBreaker.State)
	}
	if findCondition(pa.Status.Conditions, ConditionCircuitBreakerTriggered) == nil {
		t.Fatal("expected CircuitBreakerTriggered condition to be preserved")
	}
}

func TestApplyCircuitBreakerStatusDisabledClearsState(t *testing.T) {
	pa := circuitBreakerDecisionPA(false, &autoscalingv1alpha1.CircuitBreakerStatus{
		State: autoscalingv1alpha1.CircuitBreakerStateOpen,
	})
	setCondition(&pa, ConditionCircuitBreakerTriggered, metav1.ConditionTrue, "CircuitBreakerOpened", "open")

	applyCircuitBreakerStatus(&pa, &ScaleDecision{})

	if pa.Status.CircuitBreaker != nil {
		t.Fatalf("expected circuit breaker status to be cleared, got %#v", pa.Status.CircuitBreaker)
	}
	if findCondition(pa.Status.Conditions, ConditionCircuitBreakerTriggered) != nil {
		t.Fatal("expected CircuitBreakerTriggered condition to be removed")
	}
}

type assertAnError struct{}

func (assertAnError) Error() string { return "all metric sources failed" }

func newDecisionTestReconciler(round *MetricRoundResult, now time.Time) *PodAutoscalerReconciler {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = autoscalingv1alpha1.AddToScheme(sch)

	paPod := buildPod(ns, "p-0", map[string]string{"app": "foo"})
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(paPod).Build()
	return &PodAutoscalerReconciler{
		Client:              cl,
		workloadScaleClient: &fakeWorkloadScaleClient{},
		autoScaler:          &fakeAutoScaler{result: round},
		now:                 func() time.Time { return now },
	}
}

func circuitBreakerDecisionPA(enabled bool, status *autoscalingv1alpha1.CircuitBreakerStatus) autoscalingv1alpha1.PodAutoscaler {
	return circuitBreakerDecisionPAWithAction(enabled, autoscalingv1alpha1.CircuitBreakerActionFreeze, status)
}

func circuitBreakerDecisionPAWithAction(enabled bool, action autoscalingv1alpha1.CircuitBreakerAction, status *autoscalingv1alpha1.CircuitBreakerStatus) autoscalingv1alpha1.PodAutoscaler {
	return autoscalingv1alpha1.PodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pa-decision"},
		Spec: autoscalingv1alpha1.PodAutoscalerSpec{
			MaxReplicas:     10,
			ScalingStrategy: autoscalingv1alpha1.APA,
			ScaleTargetRef:  corev1.ObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "worker"},
			MetricsSources: []autoscalingv1alpha1.MetricSource{{
				MetricSourceType: autoscalingv1alpha1.POD,
				TargetMetric:     "queue",
				TargetValue:      "50",
			}},
			CircuitBreaker: &autoscalingv1alpha1.CircuitBreakerConfig{
				Enabled:           enabled,
				Action:            action,
				FailureThreshold:  3,
				RecoveryThreshold: 3,
			},
		},
		Status: autoscalingv1alpha1.PodAutoscalerStatus{
			CircuitBreaker: status,
		},
	}
}

func roundResult(desired int32, reason string, healthy, degraded, failed int) *MetricRoundResult {
	return &MetricRoundResult{
		DesiredReplicas:     desired,
		Algorithm:           "apa",
		Reason:              "apa scaling based on current metrics",
		Valid:               true,
		HasRecommendation:   true,
		TotalSourceCount:    healthy + degraded + failed,
		HealthySourceCount:  healthy,
		DegradedSourceCount: degraded,
		FailedSourceCount:   failed,
		HealthReason:        reason,
	}
}

func failedRoundResult(failed int) *MetricRoundResult {
	return &MetricRoundResult{
		Valid:             false,
		HasRecommendation: false,
		TotalSourceCount:  failed,
		FailedSourceCount: failed,
		HealthReason:      types.MetricReasonAllMetricSourcesFailed,
	}
}

func newCircuitBreakerReconcileFixture(
	t *testing.T,
	pa autoscalingv1alpha1.PodAutoscaler,
	round *MetricRoundResult,
	now time.Time,
) (*PodAutoscalerReconciler, *record.FakeRecorder, *fakeWorkloadScaleClient, client.Client) {
	t.Helper()

	scaleObj := buildScaleObject("apps/v1", "Deployment", pa.Namespace, pa.Spec.ScaleTargetRef.Name)
	pod := buildPod(pa.Namespace, "pod-0", map[string]string{"app": "foo"})
	sch := runtime.NewScheme()
	_ = autoscalingv1alpha1.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	cl := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&autoscalingv1alpha1.PodAutoscaler{}).
		WithObjects(&pa, scaleObj, pod).
		Build()
	scaleClient := &fakeWorkloadScaleClient{currentReplicas: 4}
	recorder := record.NewFakeRecorder(20)
	return &PodAutoscalerReconciler{
		Client:              cl,
		Mapper:              deploymentRESTMapper(),
		EventRecorder:       recorder,
		workloadScaleClient: scaleClient,
		autoScaler:          &fakeAutoScaler{result: round},
		monitor:             monitor.New(),
		now:                 func() time.Time { return now },
	}, recorder, scaleClient, cl
}

func drainEvents(recorder *record.FakeRecorder) []string {
	events := []string{}
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func assertContainsEvent(t *testing.T, events []string, reason string) {
	t.Helper()
	for _, event := range events {
		if strings.Contains(event, reason) {
			return
		}
	}
	t.Fatalf("events %v did not contain %q", events, reason)
}

func assertNotContainsEvent(t *testing.T, events []string, reason string) {
	t.Helper()
	for _, event := range events {
		if strings.Contains(event, reason) {
			t.Fatalf("events %v unexpectedly contained %q", events, reason)
		}
	}
}

func assertEventCount(t *testing.T, events []string, reason string, want int) {
	t.Helper()
	got := 0
	for _, event := range events {
		if strings.Contains(event, reason) {
			got++
		}
	}
	if got != want {
		t.Fatalf("event count for %q = %d, want %d; events=%v", reason, got, want, events)
	}
}

func deploymentRESTMapper() *apimeta.DefaultRESTMapper {
	gv := schema.GroupVersion{Group: "apps", Version: "v1"}
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.AddSpecific(
		schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployment"},
		apimeta.RESTScopeNamespace,
	)
	return mapper
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// ---- interface assertions (compile-time) ----

var (
	_ interface {
		GetPodSelectorFromScale(context.Context, *autoscalingv1alpha1.PodAutoscaler, *unstructured.Unstructured) (labels.Selector, error)
	} = (*fakeWorkloadScaleClient)(nil)

	_ interface {
		ComputeDesiredReplicas(context.Context, ReplicaComputeRequest) (*MetricRoundResult, error)
	} = (*fakeAutoScaler)(nil)
)
