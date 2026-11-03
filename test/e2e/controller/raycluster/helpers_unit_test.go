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
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

func TestNewRayClusterFleet(t *testing.T) {
	fleet := newRayClusterFleet("test-namespace", "test-fleet", 2, true)

	if fleet.APIVersion != orchestrationv1alpha1.GroupVersion.String() || fleet.Kind != "RayClusterFleet" {
		t.Fatalf("Fleet GVK = %s %s, want %s RayClusterFleet", fleet.APIVersion, fleet.Kind,
			orchestrationv1alpha1.GroupVersion.String())
	}
	if fleet.Namespace != "test-namespace" || fleet.Name != "test-fleet" {
		t.Fatalf("Fleet key = %s/%s, want test-namespace/test-fleet", fleet.Namespace, fleet.Name)
	}
	if fleet.Spec.Replicas == nil || *fleet.Spec.Replicas != 2 || !fleet.Spec.Paused {
		t.Fatalf("Fleet spec replicas/paused = %v/%t, want 2/true", fleet.Spec.Replicas, fleet.Spec.Paused)
	}
	if fleet.Spec.Selector == nil || fleet.Spec.Selector.MatchLabels[fleetFixtureLabel] != "test-fleet" {
		t.Fatalf("Fleet selector = %#v, want fixture label", fleet.Spec.Selector)
	}
	if fleet.Spec.Template.Labels[fleetFixtureLabel] != "test-fleet" {
		t.Fatalf("Fleet template labels = %#v, want fixture label", fleet.Spec.Template.Labels)
	}
	if fleet.Spec.Template.Spec.HeadGroupSpec.Template.Labels[fleetFixtureLabel] != "test-fleet" {
		t.Fatalf("head Pod template labels = %#v, want fixture label", fleet.Spec.Template.Spec.HeadGroupSpec.Template.Labels)
	}
	if fleet.Spec.Template.Annotations[rayOverwriteContainerCmdAnnotation] != "true" {
		t.Fatalf("Fleet template annotations = %#v, want %s=true",
			fleet.Spec.Template.Annotations, rayOverwriteContainerCmdAnnotation)
	}
	if len(fleet.Spec.Template.Spec.WorkerGroupSpecs) != 0 {
		t.Fatalf("worker group count = %d, want 0", len(fleet.Spec.Template.Spec.WorkerGroupSpecs))
	}
	containers := fleet.Spec.Template.Spec.HeadGroupSpec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("head container count = %d, want 1", len(containers))
	}
	container := containers[0]
	if container.Image != rayClusterE2EImage || container.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("head image/pull policy = %q/%q, want %q/%q",
			container.Image, container.ImagePullPolicy, rayClusterE2EImage, corev1.PullIfNotPresent)
	}
	if len(container.Command) != 3 || container.Command[0] != "sh" ||
		container.Command[1] != "-c" || container.Command[2] != "sleep 3600" {
		t.Fatalf("head command = %#v, want [sh -c 'sleep 3600']", container.Command)
	}
}

func TestRayClusterReady(t *testing.T) {
	cluster := &rayv1.RayCluster{Status: rayv1.RayClusterStatus{Conditions: []metav1.Condition{
		{Type: string(rayv1.RayClusterProvisioned), Status: metav1.ConditionTrue},
		{Type: string(rayv1.HeadPodReady), Status: metav1.ConditionTrue},
	}}}
	if !rayClusterReady(cluster) {
		t.Fatal("rayClusterReady() = false, want true")
	}
	cluster.Status.Conditions[1].Status = metav1.ConditionFalse
	if rayClusterReady(cluster) {
		t.Fatal("rayClusterReady() = true with HeadPodReady false, want false")
	}
}

func TestControllerOwnerUID(t *testing.T) {
	uid := types.UID("owner-uid")
	object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
		{UID: uid, Kind: "RayClusterReplicaSet", Controller: ptrBool(true)},
	}}}
	got, ok := controllerOwnerUID(object, "RayClusterReplicaSet")
	if !ok || got != uid {
		t.Fatalf("controllerOwnerUID() = (%s, %t), want (%s, true)", got, ok, uid)
	}
	if _, ok := controllerOwnerUID(object, "RayClusterFleet"); ok {
		t.Fatal("controllerOwnerUID() found wrong owner kind")
	}
}

func ptrBool(value bool) *bool { return &value }
