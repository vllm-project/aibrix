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

package rayclusterreplicaset

import (
	"context"
	"testing"
	"time"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/controller/util/expectation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcileCountsOnlyOwnedClustersMatchingSelector(t *testing.T) {
	tests := []struct {
		name          string
		selector      *metav1.LabelSelector
		otherRSLabels map[string]string
	}{
		{
			name: "selector with only matchExpressions",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"a"}},
				},
			},
			otherRSLabels: map[string]string{"app": "b"},
		},
		{
			name:          "another replicaset's cluster with the same labels",
			selector:      &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			otherRSLabels: map[string]string{"app": "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := rayclusterv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add RayCluster scheme: %v", err)
			}
			if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("add orchestration scheme: %v", err)
			}

			rsA := &orchestrationv1alpha1.RayClusterReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "rs-a", Namespace: "default", UID: "uid-a"},
				Spec: orchestrationv1alpha1.RayClusterReplicaSetSpec{
					Replicas: ptr.To(int32(1)),
					Selector: tt.selector,
					Template: orchestrationv1alpha1.RayClusterTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}},
					},
				},
			}
			rsB := &orchestrationv1alpha1.RayClusterReplicaSet{
				ObjectMeta: metav1.ObjectMeta{Name: "rs-b", Namespace: "default", UID: "uid-b"},
			}

			// cluster-a sorts first on scale down, so counting cluster-b as a
			// replica deletes rs-a's own cluster.
			ownCluster := ownedRayCluster("cluster-a", rsA, map[string]string{"app": "a"})
			otherCluster := ownedRayCluster("cluster-b", rsB, tt.otherRSLabels)

			reconciler := &RayClusterReplicaSetReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme).
					WithObjects(rsA, &ownCluster, &otherCluster).
					WithStatusSubresource(rsA).
					Build(),
				Expectations: expectation.NewControllerExpectations(),
			}

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "rs-a"}}); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			assertRayClusterExists(t, reconciler.Client, "default", "cluster-a")
			assertRayClusterExists(t, reconciler.Client, "default", "cluster-b")

			clusters := &rayclusterv1.RayClusterList{}
			if err := reconciler.List(ctx, clusters, client.InNamespace("default")); err != nil {
				t.Fatalf("list RayClusters: %v", err)
			}
			if len(clusters.Items) != 2 {
				t.Fatalf("expected 2 RayClusters, got %d", len(clusters.Items))
			}

			updated := &orchestrationv1alpha1.RayClusterReplicaSet{}
			if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "rs-a"}, updated); err != nil {
				t.Fatalf("get RayClusterReplicaSet: %v", err)
			}
			if updated.Status.Replicas != 1 {
				t.Fatalf("expected status.replicas 1, got %d", updated.Status.Replicas)
			}
		})
	}
}

func TestReconcileRejectsNilSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := rayclusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add RayCluster scheme: %v", err)
	}
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add orchestration scheme: %v", err)
	}

	rs := &orchestrationv1alpha1.RayClusterReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-a", Namespace: "default", UID: "uid-a"},
		Spec:       orchestrationv1alpha1.RayClusterReplicaSetSpec{Replicas: ptr.To(int32(1))},
	}
	reconciler := &RayClusterReplicaSetReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build(),
		Expectations: expectation.NewControllerExpectations(),
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "rs-a"}}); err == nil {
		t.Fatal("expected an error for a replicaset without a selector")
	}

	clusters := &rayclusterv1.RayClusterList{}
	if err := reconciler.List(context.Background(), clusters); err != nil {
		t.Fatalf("list RayClusters: %v", err)
	}
	if len(clusters.Items) != 0 {
		t.Fatalf("expected no RayClusters to be created, got %d", len(clusters.Items))
	}
}

func ownedRayCluster(name string, owner *orchestrationv1alpha1.RayClusterReplicaSet, labels map[string]string) rayclusterv1.RayCluster {
	cluster := readyRayCluster(name, owner.Namespace, "0", time.Unix(1, 0))
	cluster.Labels = labels
	cluster.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, controllerKind)}
	return cluster
}
