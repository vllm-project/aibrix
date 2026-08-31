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

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

var _ = ginkgo.Describe("RayClusterFleet controller test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-rayclusterfleet-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
		gomega.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(ns), ns)
		}, time.Second*3).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(k8sClient.Delete(ctx, ns)).To(gomega.Succeed())
	})

	ginkgo.It("creates an owned RayClusterReplicaSet and RayClusters, then aggregates replica status", func() {
		matchLabels := map[string]string{"app": "raycluster-fleet-basic"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "fleet-basic", matchLabels, 2, false)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		replicaSets := waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 1)
		gomega.Expect(replicaSets[0].Spec.Replicas).To(gomega.Equal(ptr.To(int32(2))))
		gomega.Expect(metav1.IsControlledBy(&replicaSets[0], fleet)).To(gomega.BeTrue())

		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 2)
		for i := range clusters {
			gomega.Expect(metav1.IsControlledBy(&clusters[i], &replicaSets[0])).To(gomega.BeTrue())
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}

		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 2, 2, 2, 2)
	})

	ginkgo.It("does not create a ReplicaSet when the selector is empty (SelectingAll)", func() {
		matchLabels := map[string]string{"app": "raycluster-fleet-selecting-all"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "fleet-selecting-all", matchLabels, 1, false)
		// Empty selector is the SelectingAll path in the fleet reconciler: it emits a
		// SelectingAll warning and returns without creating ReplicaSets.
		fleet.Spec.Selector = &metav1.LabelSelector{}
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			latest := &orchestrationapi.RayClusterFleet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
			g.Expect(latest.Status.ObservedGeneration).To(gomega.Equal(latest.Generation))
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		gomega.Consistently(func(g gomega.Gomega) {
			owned, err := listOwnedRayClusterReplicaSets(ctx, k8sClient, fleet)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(owned).To(gomega.BeEmpty())
		}, time.Second*3, time.Millisecond*250).Should(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			events := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, events, client.InNamespace(ns.Name))).To(gomega.Succeed())
			found := false
			for _, event := range events.Items {
				if event.InvolvedObject.Name == fleet.Name && event.Reason == "SelectingAll" {
					found = true
					break
				}
			}
			g.Expect(found).To(gomega.BeTrue(), "expected a SelectingAll warning event")
		}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
	})

	ginkgo.It("scales ReplicaSet replicas and aggregates Fleet status on scale up and down", func() {
		matchLabels := map[string]string{"app": "raycluster-fleet-scale"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "fleet-scale", matchLabels, 1, false)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		replicaSets := waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 1)
		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 1)
		markIntegrationRayClusterReady(ctx, k8sClient, &clusters[0])
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)

		updateIntegrationFleetReplicas(ctx, k8sClient, fleet, 3)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := &orchestrationapi.RayClusterReplicaSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&replicaSets[0]), latest)).To(gomega.Succeed())
			g.Expect(latest.Spec.Replicas).To(gomega.Equal(ptr.To(int32(3))))
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 3)
		for i := range clusters {
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 3, 3, 3, 3)

		updateIntegrationFleetReplicas(ctx, k8sClient, fleet, 1)
		gomega.Eventually(func(g gomega.Gomega) {
			latest := &orchestrationapi.RayClusterReplicaSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&replicaSets[0]), latest)).To(gomega.Succeed())
			g.Expect(latest.Spec.Replicas).To(gomega.Equal(ptr.To(int32(1))))
		}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())

		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 1)
		markIntegrationRayClusterReady(ctx, k8sClient, &clusters[0])
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)
	})

	ginkgo.It("does not roll out a template change while paused and resumes after unpausing", func() {
		matchLabels := map[string]string{"app": "raycluster-fleet-paused"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "fleet-paused", matchLabels, 1, false)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 1)
		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 1)
		markIntegrationRayClusterReady(ctx, k8sClient, &clusters[0])
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)

		updateIntegrationFleetPaused(ctx, k8sClient, fleet, true)
		updateIntegrationFleetRayVersion(ctx, k8sClient, fleet, "fake-ray-version-v2")

		gomega.Consistently(func(g gomega.Gomega) {
			owned, err := listOwnedRayClusterReplicaSets(ctx, k8sClient, fleet)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(owned).To(gomega.HaveLen(1))
		}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())

		updateIntegrationFleetPaused(ctx, k8sClient, fleet, false)
		waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 2)
	})

	ginkgo.It("creates a new ReplicaSet and rolls forward on a template change", func() {
		matchLabels := map[string]string{"app": "raycluster-fleet-rolling"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "fleet-rolling", matchLabels, 1, false)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		original := waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 1)
		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 1)
		markIntegrationRayClusterReady(ctx, k8sClient, &clusters[0])
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)

		updateIntegrationFleetRayVersion(ctx, k8sClient, fleet, "fake-ray-version-v2")

		replicaSets := waitForOwnedRayClusterReplicaSets(ctx, k8sClient, fleet, 2)
		var newReplicaSet *orchestrationapi.RayClusterReplicaSet
		for i := range replicaSets {
			if replicaSets[i].Name != original[0].Name {
				newReplicaSet = &replicaSets[i]
				break
			}
		}
		gomega.Expect(newReplicaSet).NotTo(gomega.BeNil())
		gomega.Expect(newReplicaSet.Spec.Template.Spec.RayVersion).To(gomega.Equal("fake-ray-version-v2"))

		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 2)
		for i := range clusters {
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}

		gomega.Eventually(func(g gomega.Gomega) {
			latestNew := &orchestrationapi.RayClusterReplicaSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(newReplicaSet), latestNew)).To(gomega.Succeed())
			g.Expect(latestNew.Spec.Replicas).To(gomega.Equal(ptr.To(int32(1))))

			latestOld := &orchestrationapi.RayClusterReplicaSet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(&original[0]), latestOld)).To(gomega.Succeed())
			g.Expect(latestOld.Spec.Replicas).To(gomega.Equal(ptr.To(int32(0))))
		}, time.Second*20, time.Millisecond*250).Should(gomega.Succeed())

		waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 1)
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)
	})
})

func makeIntegrationRayClusterFleet(
	namespace string,
	name string,
	matchLabels map[string]string,
	replicas int32,
	paused bool,
) *orchestrationapi.RayClusterFleet {
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return &orchestrationapi.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationapi.RayClusterFleetSpec{
			Replicas: ptr.To(replicas),
			Paused:   paused,
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Template: orchestrationapi.RayClusterTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: matchLabels,
				},
				Spec: makeIntegrationRayClusterSpec(),
			},
		},
	}
}

func listOwnedRayClusterReplicaSets(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
) ([]orchestrationapi.RayClusterReplicaSet, error) {
	list := &orchestrationapi.RayClusterReplicaSetList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(fleet.Namespace)); err != nil {
		return nil, err
	}
	owned := make([]orchestrationapi.RayClusterReplicaSet, 0, len(list.Items))
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], fleet) {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

func waitForOwnedRayClusterReplicaSets(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	expected int,
) []orchestrationapi.RayClusterReplicaSet {
	var items []orchestrationapi.RayClusterReplicaSet
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterFleet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
		var err error
		items, err = listOwnedRayClusterReplicaSets(ctx, k8sClient, latest)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(items).To(gomega.HaveLen(expected))
	}, time.Second*15, time.Millisecond*250).Should(gomega.Succeed())
	return items
}

func waitForIntegrationFleetStatus(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	replicas int32,
	updatedReplicas int32,
	readyReplicas int32,
	availableReplicas int32,
) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterFleet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
		g.Expect(latest.Status.Replicas).To(gomega.Equal(replicas),
			fmt.Sprintf("replicas=%d updated=%d ready=%d available=%d",
				latest.Status.Replicas, latest.Status.UpdatedReplicas, latest.Status.ReadyReplicas, latest.Status.AvailableReplicas))
		g.Expect(latest.Status.UpdatedReplicas).To(gomega.Equal(updatedReplicas))
		g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(readyReplicas))
		g.Expect(latest.Status.AvailableReplicas).To(gomega.Equal(availableReplicas))
	}, time.Second*20, time.Millisecond*250).Should(gomega.Succeed())
}

func updateIntegrationFleetReplicas(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	replicas int32,
) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterFleet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
		latest.Spec.Replicas = ptr.To(replicas)
		g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

func updateIntegrationFleetPaused(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	paused bool,
) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterFleet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
		latest.Spec.Paused = paused
		g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

func updateIntegrationFleetRayVersion(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	rayVersion string,
) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterFleet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
		latest.Spec.Template.Spec.RayVersion = rayVersion
		g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}
