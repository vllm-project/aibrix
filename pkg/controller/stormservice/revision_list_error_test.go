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

package stormservice

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestReconcileReturnsControllerRevisionListError(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{orchestrationv1alpha1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	storm := &orchestrationv1alpha1.StormService{
		ObjectMeta: metav1.ObjectMeta{Name: "llm", Namespace: "default", UID: "llm-uid", Finalizers: []string{StormServiceFinalizer}},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "llm"}},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "llm"}},
				Spec: &orchestrationv1alpha1.RoleSetSpec{Roles: []orchestrationv1alpha1.RoleSpec{{
					Name: "engine", Replicas: ptr.To(int32(1)),
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "engine:v1"}}}},
				}}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(storm).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*appsv1.ControllerRevisionList); ok {
				return fmt.Errorf("temporary apiserver timeout")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	r := &StormServiceReconciler{Client: c, Scheme: scheme, EventRecorder: record.NewFakeRecorder(8)}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(storm)})
	if err == nil && !result.Requeue && result.RequeueAfter == 0 {
		t.Fatalf("ControllerRevision list error dropped: result=%+v err=%v", result, err)
	}
}
