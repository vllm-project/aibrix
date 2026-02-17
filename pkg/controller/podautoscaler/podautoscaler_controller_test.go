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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
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
	if f.result == nil {
		return &ReplicaComputeResult{DesiredReplicas: req.CurrentReplicas}, nil
	}
	return f.result, f.err
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
