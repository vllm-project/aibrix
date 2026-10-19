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
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	// A typed client always sends "strategy": {}, and a Deployment manifest often sets only the type.
	for i, tc := range []struct {
		name     string
		strategy map[string]interface{} // nil removes spec.strategy
	}{
		{"omits spec.strategy", nil},
		{"sets an empty spec.strategy", map[string]interface{}{}},
		{"sets only the strategy type", map[string]interface{}{"type": "RollingUpdate"}},
		{"sets an empty rollingUpdate", map[string]interface{}{"type": "RollingUpdate", "rollingUpdate": map[string]interface{}{}}},
	} {
		Context("When a resource "+tc.name, func() {
			ctx := context.Background()
			typeNamespacedName := types.NamespacedName{Name: fmt.Sprintf("test-resource-no-strategy-%d", i), Namespace: "default"}

			AfterEach(func() {
				fleet := &orchestrationv1alpha1.RayClusterFleet{}
				err := k8sClient.Get(ctx, typeNamespacedName, fleet)
				if errors.IsNotFound(err) {
					return
				}
				Expect(err).NotTo(HaveOccurred())
				replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
				Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
				for i := range replicaSets.Items {
					if metav1.IsControlledBy(&replicaSets.Items[i], fleet) {
						Expect(k8sClient.Delete(ctx, &replicaSets.Items[i])).To(Succeed())
					}
				}
				Expect(k8sClient.Delete(ctx, fleet)).To(Succeed())
			})

			It("should create a ReplicaSet", func() {
				By("creating the resource")
				labels := map[string]string{"app": typeNamespacedName.Name}
				content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&orchestrationv1alpha1.RayClusterFleet{
					ObjectMeta: metav1.ObjectMeta{Name: typeNamespacedName.Name, Namespace: typeNamespacedName.Namespace},
					Spec: orchestrationv1alpha1.RayClusterFleetSpec{
						Replicas: ptr.To(int32(1)),
						Selector: &metav1.LabelSelector{MatchLabels: labels},
						Template: orchestrationv1alpha1.RayClusterTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: labels},
							Spec: rayclusterv1.RayClusterSpec{
								RayVersion: "fake-ray-version",
								HeadGroupSpec: rayclusterv1.HeadGroupSpec{
									ServiceType: corev1.ServiceTypeClusterIP,
									Template: corev1.PodTemplateSpec{
										Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "ray-head", Image: "rayproject/ray:2.10.0"}}},
									},
									RayStartParams: map[string]string{"dashboard-host": "0.0.0.0"},
								},
							},
						},
					},
				})
				Expect(err).NotTo(HaveOccurred())
				obj := &unstructured.Unstructured{Object: content}
				obj.SetGroupVersionKind(orchestrationv1alpha1.GroupVersion.WithKind("RayClusterFleet"))
				if tc.strategy == nil {
					unstructured.RemoveNestedField(obj.Object, "spec", "strategy")
				} else {
					Expect(unstructured.SetNestedField(obj.Object, tc.strategy, "spec", "strategy")).To(Succeed())
				}
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())

				fleet := &orchestrationv1alpha1.RayClusterFleet{}
				Expect(k8sClient.Get(ctx, typeNamespacedName, fleet)).To(Succeed())

				By("reconciling the resource")
				controllerReconciler := &RayClusterFleetReconciler{
					Client:      k8sClient,
					Scheme:      k8sClient.Scheme(),
					Expectation: expectation.NewControllerExpectations(),
					Recorder:    record.NewFakeRecorder(10),
				}
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				Expect(err).NotTo(HaveOccurred())

				By("verifying that the Fleet owns one ReplicaSet")
				replicaSets := &orchestrationv1alpha1.RayClusterReplicaSetList{}
				Expect(k8sClient.List(ctx, replicaSets, client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
				owned := 0
				for i := range replicaSets.Items {
					if metav1.IsControlledBy(&replicaSets.Items[i], fleet) {
						owned++
					}
				}
				Expect(owned).To(Equal(1))

				if tc.strategy == nil {
					By("verifying that the API server stored the strategy default")
					Expect(k8sClient.Get(ctx, typeNamespacedName, fleet)).To(Succeed())
					Expect(fleet.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
					Expect(fleet.Spec.Strategy.RollingUpdate).NotTo(BeNil())
					Expect(fleet.Spec.Strategy.RollingUpdate.MaxSurge).To(Equal(ptr.To(intstr.FromString("25%"))))
					Expect(fleet.Spec.Strategy.RollingUpdate.MaxUnavailable).To(Equal(ptr.To(intstr.FromString("25%"))))
				}
			})
		})
	}
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
		paused    bool
		delay     time.Duration
		reason    string
		wantDelay time.Duration
	}{
		{true, time.Minute, util.PausedDeployReason, 0},
		{true, 20 * time.Minute, util.PausedDeployReason, 0},
		{false, time.Minute, util.ResumedDeployReason, 600 * time.Second},
		{false, time.Minute, util.ResumedDeployReason, 540 * time.Second},
	} {
		f.now = f.now.Add(step.delay)
		d := f.getFleet(t)
		d.Spec.Paused = step.paused
		if err := f.reconciler.Update(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		result, err := f.reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(d)})
		if err != nil || result != (reconcile.Result{RequeueAfter: step.wantDelay}) {
			t.Fatalf("reconcile = %+v, %v, want delay %v and nil error", result, err, step.wantDelay)
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
				wantDelay := time.Duration(0)
				if !paused {
					wantDelay = writeTime.Add(600 * time.Second).Sub(f.now)
				}
				if err != nil || result != (reconcile.Result{RequeueAfter: wantDelay}) {
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

func (f *fleetTestFixture) save(t *testing.T, rs *orchestrationv1alpha1.RayClusterReplicaSet) {
	t.Helper()
	g := NewWithT(t)
	objects := []client.Object{f.fleet}
	if rs != nil {
		objects = append(objects, rs)
	}
	for _, obj := range objects {
		// Ordinary updates preserve stored status; update a copy to retain our intended status.
		copy := obj.DeepCopyObject().(client.Object)
		g.Expect(f.reconciler.Update(context.Background(), copy)).To(Succeed())
		obj.SetResourceVersion(copy.GetResourceVersion())
		g.Expect(f.reconciler.Status().Update(context.Background(), obj)).To(Succeed())
	}
}

type deadlineSchedulingCase struct {
	name          string
	deadline      int32
	elapsed       time.Duration
	input, reason string
	want          time.Duration
	refresh       bool
}

func TestDeadlineScheduling(t *testing.T) {
	for _, tt := range []deadlineSchedulingCase{
		{"rolling stable|recreate stable|rolling up|rolling down|recreate down|recreate wait|unrelated status", 600, time.Minute, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, 540 * time.Second, false},
		{"replacement", 900, time.Minute, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, 840 * time.Second, false},
		{"updated|old reduction|ready|available", 600, time.Hour, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, 600 * time.Second, true},
		{"initialize|reenable", 600, time.Minute, "missing", util.FoundNewRSReason, 600 * time.Second, true},
		{"timeout|timeout paused|timeout resumed", 600, time.Hour, util.TimedOutReason, util.TimedOutReason, 0, false},
		{"availability loss|wait complete", 600, time.Hour, util.NewRSAvailableReason, util.NewRSAvailableReason, 0, false},
		{"scaling|rollback|selector|unsupported|deletion", 600, time.Minute, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, 0, false},
		{"nil|notfound", 600, time.Minute, util.ReplicaSetUpdatedReason, "", 0, false},
		{"extension", 900, 601 * time.Second, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, 299 * time.Second, false},
		{"shortening", 30, time.Minute, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
		{"new rollout", 600, time.Minute, util.ReplicaSetUpdatedReason, util.NewReplicaSetReason, 600 * time.Second, true},
		{"complete", 600, time.Hour, util.ReplicaSetUpdatedReason, util.NewRSAvailableReason, 0, true},
		{"timeout progress", 600, time.Hour, util.TimedOutReason, util.ReplicaSetUpdatedReason, 600 * time.Second, true},
		{"timeout complete", 600, time.Hour, util.TimedOutReason, util.NewRSAvailableReason, 0, true},
		{"wait missing", 600, time.Minute, "missing", "", 0, false},
		{"maximum", math.MaxInt32, time.Minute, util.ReplicaSetUpdatedReason, "", 0, false},
		{"zero", 0, time.Minute, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
		{"negative", -1, time.Minute, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
		{"minimum", math.MinInt32, time.Minute, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
		{"large", math.MaxInt32 - 1, time.Minute, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, time.Duration(math.MaxInt32-1)*time.Second - time.Minute, false},
		{"equality", 600, 600 * time.Second, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, time.Second, false},
		{"subsecond", 600, 600*time.Second - time.Millisecond, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, time.Second, false},
		{"one second", 600, 599 * time.Second, util.ReplicaSetUpdatedReason, util.ReplicaSetUpdatedReason, time.Second, false},
		{"expired", 600, 600*time.Second + time.Millisecond, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
		{"false", 600, time.Minute, util.FailedRSCreateReason, util.FailedRSCreateReason, 540 * time.Second, false},
		{"unknown", 600, time.Minute, util.ResumedDeployReason, util.ResumedDeployReason, 540 * time.Second, false},
		{"unset", 600, time.Minute, util.ReplicaSetUpdatedReason, util.TimedOutReason, 0, true},
	} {
		for _, name := range strings.Split(tt.name, "|") {
			tt.name = name
			for _, mode := range []string{"normal", "rejected", "committed", "partial"} {
				if mode != "normal" && tt.name != "updated" && tt.name != "complete" && tt.name != "expired" && tt.name != "initialize" && tt.name != "rolling up" || mode == "partial" && tt.name != "initialize" {
					continue
				}
				t.Run(tt.name+"/"+mode, func(t *testing.T) {
					tt := tt
					g, f := NewWithT(t), newFleetTestFixture(t, interceptor.Funcs{})
					rs, baseline := f.prepareDeadlineCase(t, tt)
					ctx, req := context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f.fleet)}
					if mode != "normal" {
						if tt.name != "expired" {
							f.now = baseline.Add(599 * time.Second)
						}
						originalClient, before, injected, writes := f.reconciler.Client, f.getFleet(t).Status, errors.NewServiceUnavailable("status failure"), 0
						f.reconciler.Client = interceptor.NewClient(f.reconciler.Client.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
							writes++
							if mode == "committed" || mode == "partial" && writes == 1 {
								g.Expect(c.SubResource(sub).Update(ctx, obj, opts...)).To(Succeed())
							}
							if mode == "partial" && writes == 1 {
								return nil
							}
							return injected
						}})
						result, err := f.reconciler.Reconcile(ctx, req)
						g.Expect(err == injected).To(BeTrue(), "original status error")
						g.Expect(result).To(Equal(reconcile.Result{}))
						stored := f.getFleet(t)
						if tt.name == "rolling up" {
							g.Expect(f.reconciler.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
							g.Expect(*rs.Spec.Replicas).To(Equal(int32(1)))
						}
						if mode == "rejected" {
							g.Expect(stored.Status).To(Equal(before))
						}
						// Retry reads whatever the failed request actually committed.
						f.reconciler.Client, f.now = originalClient, f.now.Add(2*time.Second)
						if tt.name == "updated" || tt.name == "initialize" {
							tt.want = 600 * time.Second
							if mode != "rejected" {
								tt.want -= 2 * time.Second
							}
						}
						if tt.name == "rolling up" {
							tt.want, tt.reason, tt.refresh = 0, util.TimedOutReason, true
						}
					}
					if tt.name == "expired" || tt.name == "zero" || tt.name == "negative" || tt.name == "minimum" || tt.name == "unset" {
						g.Expect(f.reconciler.requeueStuckDeployment(ctx, f.fleet, f.fleet.Status)).To(Equal(time.Second))
					}
					result, err := f.reconciler.Reconcile(ctx, req)
					f.assertDeadlineScheduling(t, tt, mode, baseline, result, err)
				})
			}
		}
	}
}

func (f *fleetTestFixture) prepareDeadlineCase(t *testing.T, tt deadlineSchedulingCase) (*orchestrationv1alpha1.RayClusterReplicaSet, time.Time) {
	t.Helper()
	g := NewWithT(t)
	ctx, rs, req := context.Background(), &orchestrationv1alpha1.RayClusterReplicaSet{}, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f.fleet)}
	g.Expect(f.reconciler.Get(ctx, client.ObjectKey{Namespace: "default", Name: "fleet-rs"}, rs)).To(Succeed())
	_, err := f.reconciler.Reconcile(ctx, req)
	g.Expect(err).To(Succeed())
	if tt.name == "reenable" {
		f.fleet.Spec.ProgressDeadlineSeconds = nil
		f.save(t, nil)
		_, err = f.reconciler.Reconcile(ctx, req)
		g.Expect(err).To(Succeed())
		f.fleet = f.getFleet(t)
	}
	baseline, cond := f.now, &f.fleet.Status.Conditions[0]
	f.now, f.fleet.Spec.ProgressDeadlineSeconds = f.now.Add(tt.elapsed), &tt.deadline
	cond.Reason = tt.input
	if status, ok := map[string]corev1.ConditionStatus{util.TimedOutReason: corev1.ConditionFalse, util.FailedRSCreateReason: corev1.ConditionFalse, util.ResumedDeployReason: corev1.ConditionUnknown}[tt.input]; ok {
		cond.Status = status
	}
	if tt.input == "missing" {
		f.fleet.Status.Conditions = nil
	}
	if strings.HasPrefix(tt.name, "recreate") || strings.HasPrefix(tt.name, "wait") {
		f.fleet.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	}
	switch tt.name {
	case "replacement":
		g.Expect(f.reconciler.Delete(ctx, f.fleet)).To(Succeed())
		f.fleet.UID, f.fleet.ResourceVersion = "replacement-uid", ""
		g.Expect(f.reconciler.Create(ctx, f.fleet)).To(Succeed())
		rs.OwnerReferences[0].UID = f.fleet.UID
	case "new rollout":
		f.fleet.Spec.Template.Spec.RayVersion = "new"
	case "timeout paused":
		f.fleet.Spec.Paused = true
	case "rolling up":
		rs.Spec.Replicas = ptr.To(int32(0))
	case "rolling down", "recreate down":
		old := rs.DeepCopy()
		old.Name, old.UID, old.ResourceVersion = "old", "old-uid", ""
		old.Spec.Template.Spec.RayVersion, old.Annotations[util.RevisionAnnotation] = "old", "0"
		g.Expect(f.reconciler.Create(ctx, old)).To(Succeed())
		f.fleet.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt(1))
	case "recreate wait", "wait missing", "wait complete":
		rs.Spec.Template.Spec.RayVersion, rs.Spec.Replicas = "old", ptr.To(int32(0))
		f.fleet.Status.UpdatedReplicas = 0
	case "unrelated status":
		f.fleet.Status.ObservedGeneration = 0
	case "complete", "timeout complete":
		rs.Status.ReadyReplicas, rs.Status.AvailableReplicas = 1, 1
	case "timeout progress", "updated":
		f.fleet.Status.UpdatedReplicas = 0
	case "old reduction":
		f.fleet.Status.Replicas = 2
	case "ready":
		rs.Status.ReadyReplicas = 1
	case "available":
		f.fleet.Spec.Replicas, rs.Annotations[util.DesiredReplicasAnnotation], rs.Status.AvailableReplicas = ptr.To(int32(2)), "2", 1
	case "nil":
		f.fleet.Spec.ProgressDeadlineSeconds = nil
	case "unset":
		cond.LastUpdateTime = metav1.Time{}
	case "scaling":
		f.fleet.Spec.Replicas = ptr.To(int32(2))
	case "rollback":
		f.fleet.Annotations[appsv1.DeprecatedRollbackTo] = "1"
	case "selector":
		f.fleet.Spec.Selector = &metav1.LabelSelector{}
	case "unsupported":
		f.fleet.Spec.Strategy.Type = "unsupported"
	case "deletion":
		f.fleet.Finalizers = []string{"test"}
	}
	// Kubernetes serializes metav1.Time at whole-second precision.
	data, err := json.Marshal(f.fleet.Status)
	g.Expect(err).To(Succeed())
	g.Expect(json.Unmarshal(data, &f.fleet.Status)).To(Succeed())
	f.save(t, rs)
	if tt.input == "missing" {
		g.Expect(f.reconciler.requeueStuckDeployment(ctx, f.fleet, f.fleet.Status)).To(Equal(time.Duration(-1)))
	}
	if tt.name == "deletion" || tt.name == "notfound" {
		g.Expect(f.reconciler.Delete(ctx, f.fleet)).To(Succeed())
	}
	return rs, baseline
}

func (f *fleetTestFixture) assertDeadlineScheduling(t *testing.T, tt deadlineSchedulingCase, mode string, baseline time.Time, result reconcile.Result, err error) {
	t.Helper()
	g, ctx, transition := NewWithT(t), context.Background(), baseline
	g.Expect(err).To(Succeed())
	g.Expect(result).To(Equal(reconcile.Result{RequeueAfter: tt.want}))
	if tt.name == "notfound" {
		return
	}
	stored := f.getFleet(t)
	if tt.name == "unrelated status" {
		g.Expect(stored.Status.ObservedGeneration).To(Equal(stored.Generation))
	}
	if tt.name == "scaling" {
		rs := &orchestrationv1alpha1.RayClusterReplicaSet{}
		g.Expect(f.reconciler.Get(ctx, client.ObjectKey{Namespace: stored.Namespace, Name: "fleet-rs"}, rs)).To(Succeed())
		g.Expect(*rs.Spec.Replicas).To(Equal(int32(2)))
		g.Expect(rs.Annotations).To(Equal(map[string]string{util.RevisionAnnotation: "1", util.DesiredReplicasAnnotation: "2", util.MaxReplicasAnnotation: "3"}))
	}
	if tt.name == "updated" {
		// Only the resulting condition owns the refreshed deadline.
		f.fleet.Status.Conditions = nil
		g.Expect(f.reconciler.requeueStuckDeployment(ctx, f.fleet, stored.Status)).To(Equal(tt.want))
	}
	got := util.GetDeploymentCondition(stored.Status, orchestrationv1alpha1.RayClusterFleetProgressing)
	if got == nil {
		g.Expect(tt.reason).To(BeEmpty())
		return
	}
	g.Expect(got.Reason).To(Equal(tt.reason))
	if tt.refresh {
		baseline = f.now.Truncate(time.Second)
		if (mode == "committed" || mode == "partial") && tt.name != "rolling up" {
			baseline = baseline.Add(-2 * time.Second)
		}
		if tt.input != util.ReplicaSetUpdatedReason || tt.reason == util.TimedOutReason {
			transition = baseline
		}
	}
	g.Expect(got.LastUpdateTime.Time.Equal(baseline)).To(BeTrue(), "update timestamp")
	g.Expect(got.LastTransitionTime.Time.Equal(transition)).To(BeTrue(), "transition timestamp")
	if tt.name == "rollback" {
		g.Expect(getRollbackTo(stored)).To(BeNil())
	}
	event := map[string]string{"selector": "Warning SelectingAll", "scaling": "Normal ScalingReplicaSet", "rollback": "Warning " + util.RollbackTemplateUnchanged}[tt.name]
	if event != "" {
		g.Expect(f.reconciler.Recorder.(*record.FakeRecorder).Events).To(Receive(ContainSubstring(event)))
	}
}

func TestDeadlineOperationErrors(t *testing.T) {
	for _, op := range []string{"get", "list", "cancel", "create", "update", "cleanup", "cleanup notfound", "swallow unhealthy", "swallow scale"} {
		t.Run(op, func(t *testing.T) {
			g, f := NewWithT(t), newFleetTestFixture(t, interceptor.Funcs{})
			ctx, rs := context.Background(), &orchestrationv1alpha1.RayClusterReplicaSet{}
			g.Expect(f.reconciler.Get(ctx, client.ObjectKey{Namespace: "default", Name: "fleet-rs"}, rs)).To(Succeed())
			if op == "create" {
				f.fleet.Spec.Template.Spec.RayVersion = "new"
			}
			if op == "update" {
				rs.Spec.Replicas = ptr.To(int32(0))
			}
			if strings.HasPrefix(op, "cleanup") || strings.HasPrefix(op, "swallow") {
				old := rs.DeepCopy()
				old.Name, old.UID, old.ResourceVersion = "old", "old-uid", ""
				old.Spec.Template.Spec.RayVersion, old.Annotations[util.RevisionAnnotation] = "old", "0"
				if strings.HasPrefix(op, "cleanup") {
					old.Spec.Replicas, old.Status.Replicas = ptr.To(int32(0)), 0
					rs.Status.ReadyReplicas, rs.Status.AvailableReplicas = 1, 1
					f.fleet.Status = calculateStatus([]*orchestrationv1alpha1.RayClusterReplicaSet{rs}, rs, f.fleet)
					f.fleet.Spec.RevisionHistoryLimit = ptr.To(int32(0))
				}
				if op == "swallow scale" {
					old.Status.AvailableReplicas = 1
				}
				g.Expect(f.reconciler.Create(ctx, old)).To(Succeed())
				f.fleet.Spec.Strategy.RollingUpdate.MaxUnavailable = ptr.To(intstr.FromInt(1))
			}
			f.save(t, rs)
			injected := error(errors.NewServiceUnavailable("operation failure"))
			wantErr, want := injected, time.Duration(0)
			if op == "cancel" {
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
				wantErr, injected = context.Canceled, context.Canceled
			}
			if strings.HasPrefix(op, "swallow") {
				wantErr, want = nil, 600*time.Second
			}
			if op == "cleanup notfound" {
				wantErr, injected = nil, errors.NewNotFound(orchestrationv1alpha1.GroupVersion.WithResource("rayclusterreplicasets").GroupResource(), "old")
			}
			calls, funcs := 0, interceptor.Funcs{}
			fail := func() error { calls++; return injected }
			switch op {
			case "get", "cancel":
				funcs.Get = func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					if op == "cancel" {
						calls++
						return ctx.Err()
					}
					return fail()
				}
			case "list":
				funcs.List = func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return fail() }
			case "create":
				funcs.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return fail() }
			case "update", "swallow unhealthy", "swallow scale":
				funcs.Update = func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return fail() }
			case "cleanup", "cleanup notfound":
				funcs.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return fail() }
			}
			f.reconciler.Client = interceptor.NewClient(f.reconciler.Client.(client.WithWatch), funcs)
			result, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(f.fleet)})
			g.Expect(err == wantErr).To(BeTrue(), "original operation error")
			g.Expect(result).To(Equal(reconcile.Result{RequeueAfter: want}))
			g.Expect(calls).To(BeNumerically(">", 0))
			if op == "create" {
				g.Expect(util.GetDeploymentCondition(f.getFleet(t).Status, orchestrationv1alpha1.RayClusterFleetProgressing).Reason).To(Equal(util.FailedRSCreateReason))
				g.Expect(f.reconciler.Recorder.(*record.FakeRecorder).Events).To(Receive(ContainSubstring("Warning " + util.FailedRSCreateReason)))
			}
		})
	}
}

func TestRecreateOperationErrors(t *testing.T) {
	for _, tt := range []struct {
		name, operation                   string
		failAt                            int
		wantReplicas, wantMinReadySeconds int32
	}{
		{"initialization", "update", 1, 0, 0},
		{"scale-down", "update", 2, 0, 2},
		{"creation", "create", 1, 0, 0},
		{"scale-up", "update", 2, 0, 2},
		{"cleanup", "delete", 1, 1, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, f := NewWithT(t), newFleetTestFixture(t, interceptor.Funcs{})
			ctx, rs := context.Background(), &orchestrationv1alpha1.RayClusterReplicaSet{}
			g.Expect(f.reconciler.Get(ctx, client.ObjectKey{Namespace: "default", Name: "fleet-rs"}, rs)).To(Succeed())
			f.fleet.Spec.Strategy.Type, f.fleet.Spec.MinReadySeconds = appsv1.RecreateDeploymentStrategyType, 2
			f.fleet.Spec.RevisionHistoryLimit, rs.Spec.Replicas = ptr.To(int32(0)), ptr.To(int32(0))
			old := rs.DeepCopy()
			old.Name, old.UID, old.ResourceVersion = "old", "old-uid", ""
			old.Spec.Template.Spec.RayVersion, old.Annotations[util.RevisionAnnotation], old.Status.Replicas = "old", "0", 0
			if tt.name == "scale-down" {
				old.Spec.Replicas = ptr.To(int32(1))
			}
			if tt.name == "creation" {
				rs.Spec.Template.Spec.RayVersion, rs.Status.Replicas = "old", 0
			}
			if tt.name == "cleanup" {
				rs.Status.ReadyReplicas, rs.Status.AvailableReplicas = 1, 1
				f.fleet.Status = calculateStatus([]*orchestrationv1alpha1.RayClusterReplicaSet{rs}, rs, f.fleet)
			}
			g.Expect(f.reconciler.Create(ctx, old)).To(Succeed())
			f.save(t, rs)
			injected, calls := errors.NewServiceUnavailable("recreate operation failure"), 0
			fail, funcs := func() error { calls++; return injected }, interceptor.Funcs{}
			switch tt.operation {
			case "update":
				funcs.Update = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					calls++
					if calls == tt.failAt {
						return injected
					}
					return c.Update(ctx, obj, opts...)
				}
			case "create":
				funcs.Create = func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return fail() }
			case "delete":
				funcs.Delete = func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return fail() }
			}
			f.reconciler.Client = interceptor.NewClient(f.reconciler.Client.(client.WithWatch), funcs)
			result, err := f.reconciler.rolloutRecreate(ctx, f.getFleet(t), []*orchestrationv1alpha1.RayClusterReplicaSet{old, rs}, nil)
			g.Expect(err == injected).To(BeTrue(), "original operation error")
			g.Expect(result).To(Equal(reconcile.Result{}))
			g.Expect(calls).To(Equal(tt.failAt))
			g.Expect(f.reconciler.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())
			g.Expect(*rs.Spec.Replicas).To(Equal(tt.wantReplicas))
			g.Expect(rs.Spec.MinReadySeconds).To(Equal(tt.wantMinReadySeconds), "earlier initialization persists")
			if tt.name == "creation" {
				g.Expect(util.GetDeploymentCondition(f.getFleet(t).Status, orchestrationv1alpha1.RayClusterFleetProgressing).Reason).To(Equal(util.FailedRSCreateReason))
			}
		})
	}
}
