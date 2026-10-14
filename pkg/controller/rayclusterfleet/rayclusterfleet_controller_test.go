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

package rayclusterfleet

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/rayclusterfleet/util"
	"github.com/vllm-project/aibrix/pkg/controller/util/expectation"
)

var _ = Describe("RayClusterFleet Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		rayclusterfleet := &orchestrationv1alpha1.RayClusterFleet{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind RayClusterFleet")
			err := k8sClient.Get(ctx, typeNamespacedName, rayclusterfleet)
			if err != nil && errors.IsNotFound(err) {
				labels := map[string]string{
					"app": "rayclusterfleet-test",
				}
				resource := &orchestrationv1alpha1.RayClusterFleet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: orchestrationv1alpha1.RayClusterFleetSpec{

						Replicas: ptr.To(int32(1)),
						Strategy: appsv1.DeploymentStrategy{
							Type: appsv1.RollingUpdateDeploymentStrategyType,
							RollingUpdate: &appsv1.RollingUpdateDeployment{
								MaxUnavailable: ptr.To(intstr.FromInt(0)),
								MaxSurge:       ptr.To(intstr.FromInt(1)),
							},
						},
						Selector: &metav1.LabelSelector{
							MatchLabels: labels,
						},
						Template: orchestrationv1alpha1.RayClusterTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: labels,
							},
							Spec: rayclusterv1.RayClusterSpec{
								RayVersion: "fake-ray-version",
								HeadGroupSpec: rayclusterv1.HeadGroupSpec{
									ServiceType: corev1.ServiceTypeClusterIP,
									Template: corev1.PodTemplateSpec{
										Spec: corev1.PodSpec{
											Containers: []corev1.Container{
												{
													Name:  "ray-head",
													Image: "rayproject/ray:2.10.0",
												},
											},
										},
									},
									RayStartParams: map[string]string{
										"dashboard-host": "0.0.0.0",
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
				Expect(k8sClient.Get(ctx, typeNamespacedName, rayclusterfleet)).To(Succeed())
			}
		})

		AfterEach(func() {
			replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					Expect(k8sClient.Delete(ctx, &replicaSets.Items[i])).To(Succeed())
				}
			}

			resource := &orchestrationv1alpha1.RayClusterFleet{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance RayClusterFleet")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &RayClusterFleetReconciler{
				Client:      k8sClient,
				Scheme:      k8sClient.Scheme(),
				Expectation: expectation.NewControllerExpectations(),
				Recorder:    record.NewFakeRecorder(10),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying that the Fleet owns one correctly configured ReplicaSet")
			replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			ownedReplicaSets := make([]*orchestrationv1alpha1.RayClusterReplicaSet, 0, len(replicaSets.Items))
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					ownedReplicaSets = append(ownedReplicaSets, &replicaSets.Items[i])
				}
			}
			Expect(ownedReplicaSets).To(HaveLen(1))

			replicaSet := ownedReplicaSets[0]
			firstReplicaSetName := replicaSet.Name
			firstReplicaSetUID := replicaSet.UID
			Expect(metav1.IsControlledBy(replicaSet, rayclusterfleet)).To(BeTrue())
			Expect(replicaSet.Spec.Replicas).NotTo(BeNil())
			Expect(*replicaSet.Spec.Replicas).To(Equal(int32(1)))
			Expect(replicaSet.Spec.Selector).NotTo(BeNil())
			Expect(replicaSet.Spec.Selector.MatchLabels).To(HaveKeyWithValue("app", "rayclusterfleet-test"))
			Expect(replicaSet.Spec.Template.Labels).To(HaveKeyWithValue("app", "rayclusterfleet-test"))

			By("Reconciling again without creating a duplicate ReplicaSet")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			ownedReplicaSets = ownedReplicaSets[:0]
			for i := range replicaSets.Items {
				if metav1.IsControlledBy(&replicaSets.Items[i], rayclusterfleet) {
					ownedReplicaSets = append(ownedReplicaSets, &replicaSets.Items[i])
				}
			}
			Expect(ownedReplicaSets).To(HaveLen(1))
			Expect(ownedReplicaSets[0].Name).To(Equal(firstReplicaSetName))
			Expect(ownedReplicaSets[0].UID).To(Equal(firstReplicaSetUID))
		})
	})
})

type fleetTestFixture struct {
	reconciler *RayClusterFleetReconciler
	fleet      *orchestrationv1alpha1.RayClusterFleet
	now        time.Time
}

func newFleetTestFixture(t *testing.T, funcs interceptor.Funcs) *fleetTestFixture {
	t.Helper()
	f := &fleetTestFixture{now: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	oldNowFn := util.NowFn
	util.NowFn = func() time.Time { return f.now }
	t.Cleanup(func() { util.NowFn = oldNowFn })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rayclusterv1.AddToScheme, orchestrationv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{"app": "fleet-test"}
	f.fleet = &orchestrationv1alpha1.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "default", UID: "fleet-uid", Generation: 1,
			Annotations: map[string]string{util.RevisionAnnotation: "1"}},
		Spec: orchestrationv1alpha1.RayClusterFleetSpec{
			Replicas: ptr.To(int32(1)), ProgressDeadlineSeconds: ptr.To(int32(600)), RevisionHistoryLimit: ptr.To(int32(10)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: orchestrationv1alpha1.RayClusterTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr.To(intstr.FromInt(1)), MaxUnavailable: ptr.To(intstr.FromInt(0))}},
		},
	}
	rs := &orchestrationv1alpha1.RayClusterReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet-rs", Namespace: "default", UID: "rs-uid", Labels: labels,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.fleet, controllerKind)},
			Annotations:     map[string]string{util.RevisionAnnotation: "1", util.DesiredReplicasAnnotation: "1", util.MaxReplicasAnnotation: "2"}},
		Spec: orchestrationv1alpha1.RayClusterReplicaSetSpec{Replicas: ptr.To(int32(1)),
			Selector: f.fleet.Spec.Selector.DeepCopy(), Template: *f.fleet.Spec.Template.DeepCopy()},
		Status: orchestrationv1alpha1.RayClusterReplicaSetStatus{Replicas: 1},
	}
	f.fleet.Status.Conditions = []orchestrationv1alpha1.RayClusterFleetCondition{{
		Type: orchestrationv1alpha1.RayClusterFleetProgressing, Status: corev1.ConditionTrue,
		Reason: util.ReplicaSetUpdatedReason, LastUpdateTime: metav1.NewTime(f.now), LastTransitionTime: metav1.NewTime(f.now),
	}}
	f.fleet.Status = calculateStatus([]*orchestrationv1alpha1.RayClusterReplicaSet{rs}, rs, f.fleet)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.fleet, rs).
		WithStatusSubresource(&orchestrationv1alpha1.RayClusterFleet{}, &orchestrationv1alpha1.RayClusterReplicaSet{}, &rayclusterv1.RayCluster{}).
		WithInterceptorFuncs(funcs).Build()
	f.reconciler = &RayClusterFleetReconciler{Client: c, Scheme: scheme,
		Expectation: expectation.NewControllerExpectations(), Recorder: record.NewFakeRecorder(10)}
	f.fleet = f.getFleet(t)
	return f
}

func (f *fleetTestFixture) getFleet(t *testing.T) *orchestrationv1alpha1.RayClusterFleet {
	t.Helper()
	d := &orchestrationv1alpha1.RayClusterFleet{}
	if err := f.reconciler.Get(context.Background(), client.ObjectKeyFromObject(f.fleet), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCheckPausedConditions(t *testing.T) {
	for _, paused := range []bool{true, false} {
		name, previousReason, wantReason, wantMessage := "pause", util.ReplicaSetUpdatedReason, util.PausedDeployReason, "Deployment is paused"
		if !paused {
			name, previousReason, wantReason, wantMessage = "resume", util.PausedDeployReason, util.ResumedDeployReason, "Deployment is resumed"
		}
		t.Run(name, func(t *testing.T) {
			f := newFleetTestFixture(t, interceptor.Funcs{})
			f.fleet.Spec.Paused = paused
			if err := f.reconciler.Update(context.Background(), f.fleet); err != nil {
				t.Fatal(err)
			}
			initial := &f.fleet.Status.Conditions[0]
			initial.Reason = previousReason
			if !paused {
				initial.Status = corev1.ConditionUnknown
			}
			if err := f.reconciler.Status().Update(context.Background(), f.fleet); err != nil {
				t.Fatal(err)
			}
			f.now = f.now.Add(20 * time.Minute)
			if err := f.reconciler.checkPausedConditions(context.Background(), f.getFleet(t)); err != nil {
				t.Fatal(err)
			}
			stored := f.getFleet(t)
			cond := util.GetDeploymentCondition(stored.Status, orchestrationv1alpha1.RayClusterFleetProgressing)
			if cond == nil || cond.Reason != wantReason || cond.Status != corev1.ConditionUnknown || cond.Message != wantMessage {
				t.Errorf("persisted Progressing condition = %+v, want Unknown/%s with message %q", cond, wantReason, wantMessage)
			}
			if !paused && util.DeploymentTimedOut(context.Background(), stored, &stored.Status) {
				t.Error("resumed fleet timed out against the old progress baseline")
			}
		})
	}
}

func TestCheckPausedConditionsNoUpdate(t *testing.T) {
	for _, tt := range []struct {
		name     string
		paused   bool
		reason   string
		deadline *int32
	}{
		{"already paused", true, util.PausedDeployReason, ptr.To(int32(600))},
		{"already resumed", false, util.ResumedDeployReason, ptr.To(int32(600))},
		{"timed out pause", true, util.TimedOutReason, ptr.To(int32(600))},
		{"timed out resume", false, util.TimedOutReason, ptr.To(int32(600))},
		{"nil deadline", true, util.ReplicaSetUpdatedReason, nil},
		{"maximum deadline", false, util.PausedDeployReason, ptr.To(int32(math.MaxInt32))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			f := newFleetTestFixture(t, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				writes++
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			}})
			f.fleet.Spec.Paused, f.fleet.Spec.ProgressDeadlineSeconds = tt.paused, tt.deadline
			if err := f.reconciler.Update(context.Background(), f.fleet); err != nil {
				t.Fatal(err)
			}
			f.fleet.Status.Conditions[0].Reason = tt.reason
			f.fleet.Status.Conditions[0].Status = corev1.ConditionUnknown
			if tt.reason == util.TimedOutReason {
				f.fleet.Status.Conditions[0].Status = corev1.ConditionFalse
			}
			if err := f.reconciler.Status().Update(context.Background(), f.fleet); err != nil {
				t.Fatal(err)
			}
			before := f.getFleet(t)
			writes = 0
			f.now = f.now.Add(time.Hour)
			if err := f.reconciler.checkPausedConditions(context.Background(), f.getFleet(t)); err != nil {
				t.Fatal(err)
			}
			if writes != 0 || !reflect.DeepEqual(before, f.getFleet(t)) {
				t.Errorf("no-op changed fleet or wrote status: writes=%d", writes)
			}
		})
	}
}

func TestReconcilePauseResume(t *testing.T) {
	f := newFleetTestFixture(t, interceptor.Funcs{})
	var previous *orchestrationv1alpha1.RayClusterFleetCondition
	for _, step := range []struct {
		paused bool
		delay  time.Duration
		reason string
	}{
		{true, time.Minute, util.PausedDeployReason},
		{true, 20 * time.Minute, util.PausedDeployReason},
		{false, time.Minute, util.ResumedDeployReason},
		{false, time.Minute, util.ResumedDeployReason},
	} {
		f.now = f.now.Add(step.delay)
		d := f.getFleet(t)
		d.Spec.Paused = step.paused
		if err := f.reconciler.Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		result, err := f.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(d)})
		if err != nil || result != (reconcile.Result{}) {
			t.Fatalf("reconcile = %+v, %v, want empty result and nil error", result, err)
		}
		stored := f.getFleet(t)
		cond := util.GetDeploymentCondition(stored.Status, orchestrationv1alpha1.RayClusterFleetProgressing)
		if cond == nil || cond.Reason != step.reason || cond.Status != corev1.ConditionUnknown {
			t.Fatalf("persisted Progressing condition = %+v, want Unknown/%s", cond, step.reason)
		}
		wantUpdate, wantTransition := f.now, f.now
		if previous != nil {
			wantTransition = previous.LastTransitionTime.Time
			if previous.Reason == step.reason {
				wantUpdate = previous.LastUpdateTime.Time
			}
		}
		if !cond.LastUpdateTime.Time.Equal(wantUpdate) || !cond.LastTransitionTime.Time.Equal(wantTransition) {
			t.Errorf("timestamps = %v/%v, want %v/%v", cond.LastUpdateTime, cond.LastTransitionTime, wantUpdate, wantTransition)
		}
		previous = cond
	}
}

func TestCheckPausedConditionsStatusError(t *testing.T) {
	for _, paused := range []bool{true, false} {
		for _, committed := range []bool{false, true} {
			name := "pause"
			wantReason := util.PausedDeployReason
			if !paused {
				name, wantReason = "resume", util.ResumedDeployReason
			}
			if committed {
				name += "/committed"
			} else {
				name += "/rejected"
			}
			t.Run(name, func(t *testing.T) {
				injectedErr := errors.NewServiceUnavailable("injected status write failure")
				fail := false
				f := newFleetTestFixture(t, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if fail && subresource == "status" {
						if committed {
							if err := c.Status().Update(ctx, obj, opts...); err != nil {
								return err
							}
						}
						return injectedErr
					}
					return c.SubResource(subresource).Update(ctx, obj, opts...)
				}})
				f.fleet.Spec.Paused = paused
				if err := f.reconciler.Update(context.Background(), f.fleet); err != nil {
					t.Fatal(err)
				}
				if !paused {
					f.fleet.Status.Conditions[0].Reason = util.PausedDeployReason
					f.fleet.Status.Conditions[0].Status = corev1.ConditionUnknown
				}
				if err := f.reconciler.Status().Update(context.Background(), f.fleet); err != nil {
					t.Fatal(err)
				}
				before := f.getFleet(t)
				f.now = f.now.Add(599 * time.Second)
				writeTime := f.now
				fail = true
				req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f.fleet)}
				result, err := f.reconciler.Reconcile(context.Background(), req)
				if err != injectedErr || result != (reconcile.Result{}) {
					t.Fatalf("reconcile = %+v, %v, want empty result and original error", result, err)
				}
				stored := f.getFleet(t)
				if !committed && !reflect.DeepEqual(stored.Status, before.Status) {
					t.Fatal("rejected write changed stored status")
				}
				if committed && util.GetDeploymentCondition(stored.Status, orchestrationv1alpha1.RayClusterFleetProgressing).Reason != wantReason {
					t.Fatal("committed write lost its condition")
				}
				fail = false
				f.now = f.now.Add(2 * time.Second) // Retry crosses the original progress deadline.
				if !committed {
					writeTime = f.now
				}
				result, err = f.reconciler.Reconcile(context.Background(), req)
				if err != nil || result != (reconcile.Result{}) {
					t.Fatalf("retry = %+v, %v", result, err)
				}
				cond := util.GetDeploymentCondition(f.getFleet(t).Status, orchestrationv1alpha1.RayClusterFleetProgressing)
				if cond == nil || cond.Reason != wantReason || !cond.LastUpdateTime.Time.Equal(writeTime) {
					t.Errorf("retry condition = %+v, want %s at %v", cond, wantReason, writeTime)
				}
			})
		}
	}
}
