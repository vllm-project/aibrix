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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestScaleDownPrioritizesNotReadyThenLowerDeletionCost(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := rayclusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add RayCluster scheme: %v", err)
	}

	replicaSet := &orchestrationv1alpha1.RayClusterReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rs",
			Namespace: "default",
		},
	}
	keepReadyHighCost := readyRayCluster("keep-ready-high-cost", "default", "100", time.Unix(1, 0))
	deleteNotReadyHighCost := notReadyRayCluster("delete-not-ready-high-cost", "default", "1000", time.Unix(2, 0))
	deleteReadyLowCost := readyRayCluster("delete-ready-low-cost", "default", "-10", time.Unix(3, 0))

	reconciler := &RayClusterReplicaSetReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&keepReadyHighCost, &deleteNotReadyHighCost, &deleteReadyLowCost).
			Build(),
		Expectations: expectation.NewControllerExpectations(),
	}

	if err := reconciler.scaleDown(ctx, replicaSet, []rayclusterv1.RayCluster{
		keepReadyHighCost,
		deleteNotReadyHighCost,
		deleteReadyLowCost,
	}, 2); err != nil {
		t.Fatalf("scaleDown returned error: %v", err)
	}

	assertRayClusterExists(t, reconciler.Client, "default", "keep-ready-high-cost")
	assertRayClusterDeleted(t, reconciler.Client, "default", "delete-not-ready-high-cost")
	assertRayClusterDeleted(t, reconciler.Client, "default", "delete-ready-low-cost")
}

func TestScaleDownUsesNameAsFinalTieBreaker(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := rayclusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add RayCluster scheme: %v", err)
	}

	replicaSet := &orchestrationv1alpha1.RayClusterReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rs",
			Namespace: "default",
		},
	}
	createdAt := time.Unix(1, 0)
	keepByName := readyRayCluster("raycluster-b", "default", "0", createdAt)
	deleteByName := readyRayCluster("raycluster-a", "default", "0", createdAt)

	reconciler := &RayClusterReplicaSetReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(&keepByName, &deleteByName).
			Build(),
		Expectations: expectation.NewControllerExpectations(),
	}

	if err := reconciler.scaleDown(ctx, replicaSet, []rayclusterv1.RayCluster{
		keepByName,
		deleteByName,
	}, 1); err != nil {
		t.Fatalf("scaleDown returned error: %v", err)
	}

	assertRayClusterExists(t, reconciler.Client, "default", "raycluster-b")
	assertRayClusterDeleted(t, reconciler.Client, "default", "raycluster-a")
}

func readyRayCluster(name, namespace, deletionCost string, createdAt time.Time) rayclusterv1.RayCluster {
	cluster := rayCluster(name, namespace, deletionCost, createdAt)
	cluster.Status.Conditions = []metav1.Condition{
		{Type: string(rayclusterv1.RayClusterProvisioned), Status: metav1.ConditionTrue},
		{Type: string(rayclusterv1.HeadPodReady), Status: metav1.ConditionTrue},
	}
	cluster.Status.DesiredWorkerReplicas = 1
	cluster.Status.ReadyWorkerReplicas = 1
	return cluster
}

func notReadyRayCluster(name, namespace, deletionCost string, createdAt time.Time) rayclusterv1.RayCluster {
	return rayCluster(name, namespace, deletionCost, createdAt)
}

func rayCluster(name, namespace, deletionCost string, createdAt time.Time) rayclusterv1.RayCluster {
	return rayclusterv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(createdAt),
			Annotations: map[string]string{
				"controller.kubernetes.io/pod-deletion-cost": deletionCost,
			},
		},
	}
}

func assertRayClusterExists(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	cluster := &rayclusterv1.RayCluster{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cluster); err != nil {
		t.Fatalf("expected RayCluster %s/%s to exist, got error: %v", namespace, name, err)
	}
}

func assertRayClusterDeleted(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	cluster := &rayclusterv1.RayCluster{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cluster)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected RayCluster %s/%s to be deleted, got error: %v", namespace, name, err)
	}
}
