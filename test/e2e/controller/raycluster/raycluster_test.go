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

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestRayClusterFleetLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	harness := newRayClusterHarness(t, ctx)
	name := fixtureResourceName("lifecycle")
	fleet, err := harness.createFleet(ctx, newRayClusterFleet(harness.namespace, name, 1, false))
	if err != nil {
		t.Fatalf("create Fleet: %v", err)
	}
	replicaSet, _, err := establishFleetReady(ctx, harness, fleet)
	if err != nil {
		t.Fatal(err)
	}
	keepUID, err := scaleFleetAndCheckOrdering(ctx, harness, fleet, replicaSet)
	if err != nil {
		t.Fatal(err)
	}
	if err := restartAndCleanupFleet(ctx, t, harness, fleet, replicaSet, keepUID); err != nil {
		t.Fatal(err)
	}
}

func establishFleetReady(
	ctx context.Context,
	harness *rayClusterHarness,
	fleet *orchestrationv1alpha1.RayClusterFleet,
) (orchestrationv1alpha1.RayClusterReplicaSet, rayv1.RayCluster, error) {
	replicaSets, err := harness.waitForOwnedReplicaSets(ctx, fleet.UID, 1)
	if err != nil {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, err
	}
	replicaSet := replicaSets[0]
	if ownerUID, ok := controllerOwnerUID(&replicaSet, "RayClusterFleet"); !ok || ownerUID != fleet.UID {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, fmt.Errorf(
			"ReplicaSet owner UID = %s/%t, want Fleet UID %s", ownerUID, ok, fleet.UID,
		)
	}
	clusters, err := harness.waitForOwnedRayClusters(ctx, replicaSet.UID, 1)
	if err != nil {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, err
	}
	if ownerUID, ok := controllerOwnerUID(&clusters[0], "RayClusterReplicaSet"); !ok || ownerUID != replicaSet.UID {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, fmt.Errorf(
			"RayCluster owner UID = %s/%t, want ReplicaSet UID %s", ownerUID, ok, replicaSet.UID,
		)
	}
	if err := harness.waitForRayClustersReady(ctx, clusters); err != nil {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, err
	}
	if err := harness.waitForReplicaSetStatus(ctx, replicaSet.Name, 1, 1, 1); err != nil {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, err
	}
	if err := harness.waitForFleetStatus(ctx, fleet.Name, 1, 1, 1, 1); err != nil {
		return orchestrationv1alpha1.RayClusterReplicaSet{}, rayv1.RayCluster{}, err
	}
	return replicaSet, clusters[0], nil
}

func scaleFleetAndCheckOrdering(
	ctx context.Context,
	harness *rayClusterHarness,
	fleet *orchestrationv1alpha1.RayClusterFleet,
	replicaSet orchestrationv1alpha1.RayClusterReplicaSet,
) (types.UID, error) {
	if err := harness.updateFleet(ctx, fleet.Name, func(current *orchestrationv1alpha1.RayClusterFleet) {
		current.Spec.Replicas = ptr.To(int32(3))
	}); err != nil {
		return "", fmt.Errorf("scale Fleet to 3: %w", err)
	}
	clusters, err := harness.waitForOwnedRayClusters(ctx, replicaSet.UID, 3)
	if err != nil {
		return "", err
	}
	if err := harness.waitForRayClustersReady(ctx, clusters); err != nil {
		return "", err
	}
	if err := harness.waitForReplicaSetStatus(ctx, replicaSet.Name, 3, 3, 3); err != nil {
		return "", err
	}
	if err := harness.waitForFleetStatus(ctx, fleet.Name, 3, 3, 3, 3); err != nil {
		return "", err
	}
	for i := range clusters {
		cost := "0"
		if i == 0 {
			cost = "-100"
		} else if i == len(clusters)-1 {
			cost = "100"
		}
		if err := harness.setDeletionCost(ctx, &clusters[i], cost); err != nil {
			return "", fmt.Errorf("set deletion cost on RayCluster %s: %w", clusters[i].Name, err)
		}
	}
	keepUID := clusters[len(clusters)-1].UID
	if err := harness.updateFleet(ctx, fleet.Name, func(current *orchestrationv1alpha1.RayClusterFleet) {
		current.Spec.Replicas = ptr.To(int32(1))
	}); err != nil {
		return "", fmt.Errorf("scale Fleet to 1: %w", err)
	}
	clusters, err = harness.waitForOwnedRayClusters(ctx, replicaSet.UID, 1)
	if err != nil {
		return "", err
	}
	if clusters[0].UID != keepUID {
		return "", fmt.Errorf("scale-down retained RayCluster UID %s, want highest-cost UID %s", clusters[0].UID, keepUID)
	}
	return keepUID, nil
}

func restartAndCleanupFleet(
	ctx context.Context,
	t *testing.T,
	harness *rayClusterHarness,
	fleet *orchestrationv1alpha1.RayClusterFleet,
	replicaSet orchestrationv1alpha1.RayClusterReplicaSet,
	keepUID types.UID,
) error {
	if err := harness.restartController(ctx, t); err != nil {
		return err
	}
	replicaSets, err := harness.waitForOwnedReplicaSets(ctx, fleet.UID, 1)
	if err != nil {
		return err
	}
	if replicaSets[0].UID != replicaSet.UID {
		return fmt.Errorf("controller restart replaced ReplicaSet UID %s, want %s", replicaSets[0].UID, replicaSet.UID)
	}
	clusters, err := harness.waitForOwnedRayClusters(ctx, replicaSet.UID, 1)
	if err != nil {
		return err
	}
	if clusters[0].UID != keepUID {
		return fmt.Errorf("controller restart replaced RayCluster UID %s, want %s", clusters[0].UID, keepUID)
	}
	if err := harness.waitForFleetStatus(ctx, fleet.Name, 1, 1, 1, 1); err != nil {
		return err
	}
	deletedFleetUID, err := harness.deleteFleetForeground(ctx, fleet.Name)
	if err != nil {
		return fmt.Errorf("delete Fleet: %w", err)
	}
	if deletedFleetUID != fleet.UID {
		return fmt.Errorf("deleted Fleet UID = %s, want %s", deletedFleetUID, fleet.UID)
	}
	return harness.waitForFleetResourcesGone(ctx, fleet.Name, fleet.UID, replicaSet.UID)
}

func TestRayClusterFleetPauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	harness := newRayClusterHarness(t, ctx)
	name := fixtureResourceName("pause")
	fleet, err := harness.createFleet(ctx, newRayClusterFleet(harness.namespace, name, 1, true))
	if err != nil {
		t.Fatalf("create paused Fleet: %v", err)
	}
	if err := harness.ensureNoChildren(ctx, fleet.UID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := harness.updateFleet(ctx, fleet.Name, func(current *orchestrationv1alpha1.RayClusterFleet) {
		current.Spec.Replicas = ptr.To(int32(2))
		if current.Spec.Template.Annotations == nil {
			current.Spec.Template.Annotations = map[string]string{}
		}
		current.Spec.Template.Annotations["e2e.aibrix.ai/paused-update"] = "latest"
	}); err != nil {
		t.Fatalf("update paused Fleet: %v", err)
	}
	if err := harness.ensureNoChildren(ctx, fleet.UID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := harness.updateFleet(ctx, fleet.Name, func(current *orchestrationv1alpha1.RayClusterFleet) {
		current.Spec.Paused = false
	}); err != nil {
		t.Fatalf("resume Fleet: %v", err)
	}
	replicaSets, err := harness.waitForOwnedReplicaSets(ctx, fleet.UID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replicaSets[0].Spec.Replicas == nil || *replicaSets[0].Spec.Replicas != 2 {
		t.Fatalf("resumed ReplicaSet replicas = %v, want 2", replicaSets[0].Spec.Replicas)
	}
	if replicaSets[0].Spec.Template.Annotations["e2e.aibrix.ai/paused-update"] != "latest" {
		t.Fatalf("resumed ReplicaSet template annotations = %#v, want latest marker",
			replicaSets[0].Spec.Template.Annotations)
	}
	clusters, err := harness.waitForOwnedRayClusters(ctx, replicaSets[0].UID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForRayClustersReady(ctx, clusters); err != nil {
		t.Fatal(err)
	}
	if err := harness.waitForFleetStatus(ctx, fleet.Name, 2, 2, 2, 2); err != nil {
		t.Fatal(err)
	}
	deletedFleetUID, err := harness.deleteFleetForeground(ctx, fleet.Name)
	if err != nil {
		t.Fatalf("delete resumed Fleet: %v", err)
	}
	if err := harness.waitForFleetResourcesGone(ctx, fleet.Name, deletedFleetUID, replicaSets[0].UID); err != nil {
		t.Fatal(err)
	}
}
