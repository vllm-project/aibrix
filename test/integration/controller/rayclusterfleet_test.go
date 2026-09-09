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

const (
	rayClusterFleetTimeout  = time.Second * 15
	rayClusterFleetInterval = time.Millisecond * 250
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
		}, time.Second*3, rayClusterFleetInterval).Should(gomega.Succeed())
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(gomega.Succeed())
	})

	ginkgo.It("creates an owned RayClusterReplicaSet for a Fleet", func() {
		labels := map[string]string{"app": "rayclusterfleet-basic"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "rayclusterfleet-basic", labels, 1)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		replicaSets := waitForIntegrationFleetReplicaSets(ctx, k8sClient, fleet, 1)
		replicaSet := replicaSets[0]
		gomega.Expect(metav1.IsControlledBy(&replicaSet, fleet)).To(gomega.BeTrue())
		gomega.Expect(replicaSet.Spec.Replicas).NotTo(gomega.BeNil())
		gomega.Expect(*replicaSet.Spec.Replicas).To(gomega.Equal(int32(1)))
		gomega.Expect(replicaSet.Spec.Selector).NotTo(gomega.BeNil())
		gomega.Expect(replicaSet.Spec.Selector.MatchLabels).To(gomega.HaveKeyWithValue("app", "rayclusterfleet-basic"))
		gomega.Expect(replicaSet.Spec.Template.Labels).To(gomega.HaveKeyWithValue("app", "rayclusterfleet-basic"))
	})

	ginkgo.It("rejects an empty selector with SelectingAll and creates no ReplicaSet", func() {
		fleet := makeIntegrationRayClusterFleet(ns.Name, "rayclusterfleet-empty-selector", nil, 1)
		fleet.Spec.Selector = &metav1.LabelSelector{}
		fleet.Spec.Template.Labels = nil
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		gomega.Eventually(func(g gomega.Gomega) {
			latest := &orchestrationapi.RayClusterFleet{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(fleet), latest)).To(gomega.Succeed())
			g.Expect(latest.Status.ObservedGeneration).To(gomega.Equal(latest.Generation))
		}, rayClusterFleetTimeout, rayClusterFleetInterval).Should(gomega.Succeed())

		gomega.Consistently(func() int {
			return countIntegrationFleetReplicaSets(ctx, k8sClient, fleet)
		}, time.Second, rayClusterFleetInterval).Should(gomega.Equal(0))

		gomega.Eventually(func(g gomega.Gomega) {
			events := &corev1.EventList{}
			g.Expect(k8sClient.List(ctx, events, client.InNamespace(ns.Name))).To(gomega.Succeed())
			found := false
			for i := range events.Items {
				event := &events.Items[i]
				if event.InvolvedObject.Name == fleet.Name && event.Reason == "SelectingAll" {
					found = true
					break
				}
			}
			g.Expect(found).To(gomega.BeTrue(), "expected SelectingAll warning event for Fleet")
		}, rayClusterFleetTimeout, rayClusterFleetInterval).Should(gomega.Succeed())
	})

	ginkgo.It("aggregates status while scaling a Fleet up and down", func() {
		labels := map[string]string{"app": "rayclusterfleet-scaling"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "rayclusterfleet-scaling", labels, 1)
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, labels, 1)
		for i := range clusters {
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)

		updateIntegrationFleetReplicas(ctx, k8sClient, fleet, 3)
		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, labels, 3)
		for i := range clusters {
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 3, 3, 3, 3)

		updateIntegrationFleetReplicas(ctx, k8sClient, fleet, 1)
		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, labels, 1)
		markIntegrationRayClusterReady(ctx, k8sClient, &clusters[0])
		waitForIntegrationFleetStatus(ctx, k8sClient, fleet, 1, 1, 1, 1)
	})

	ginkgo.It("does not create a ReplicaSet until a newly-created paused Fleet is resumed", func() {
		labels := map[string]string{"app": "rayclusterfleet-paused"}
		fleet := makeIntegrationRayClusterFleet(ns.Name, "rayclusterfleet-paused", labels, 1)
		fleet.Spec.Paused = true
		fleet.Spec.ProgressDeadlineSeconds = ptr.To(int32(600))
		gomega.Expect(k8sClient.Create(ctx, fleet)).To(gomega.Succeed())

		gomega.Consistently(func() int {
			return countIntegrationFleetReplicaSets(ctx, k8sClient, fleet)
		}, time.Second, rayClusterFleetInterval).Should(gomega.Equal(0))

		updateIntegrationFleetPaused(ctx, k8sClient, fleet, false)
		waitForIntegrationFleetReplicaSets(ctx, k8sClient, fleet, 1)
		waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, labels, 1)
	})
})

func makeIntegrationRayClusterFleet(
	namespace string,
	name string,
	labels map[string]string,
	replicas int32,
) *orchestrationapi.RayClusterFleet {
	return &orchestrationapi.RayClusterFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: orchestrationapi.RayClusterFleetSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: orchestrationapi.RayClusterTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: makeIntegrationRayClusterSpec(),
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromInt(0)),
					MaxSurge:       ptr.To(intstr.FromInt(1)),
				},
			},
			RevisionHistoryLimit:    ptr.To(int32(10)),
			ProgressDeadlineSeconds: ptr.To(int32(600)),
		},
	}
}

func waitForIntegrationFleetReplicaSets(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
	expected int,
) []orchestrationapi.RayClusterReplicaSet {
	var owned []orchestrationapi.RayClusterReplicaSet
	gomega.Eventually(func(g gomega.Gomega) {
		list := &orchestrationapi.RayClusterReplicaSetList{}
		g.Expect(k8sClient.List(ctx, list, client.InNamespace(fleet.Namespace))).To(gomega.Succeed())
		owned = owned[:0]
		for i := range list.Items {
			if metav1.IsControlledBy(&list.Items[i], fleet) {
				owned = append(owned, list.Items[i])
			}
		}
		g.Expect(owned).To(gomega.HaveLen(expected))
	}, rayClusterFleetTimeout, rayClusterFleetInterval).Should(gomega.Succeed())
	return owned
}

func countIntegrationFleetReplicaSets(
	ctx context.Context,
	k8sClient client.Client,
	fleet *orchestrationapi.RayClusterFleet,
) int {
	list := &orchestrationapi.RayClusterReplicaSetList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(fleet.Namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], fleet) {
			count++
		}
	}
	return count
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
	}, time.Second*5, rayClusterFleetInterval).Should(gomega.Succeed())
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
	}, time.Second*5, rayClusterFleetInterval).Should(gomega.Succeed())
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
		g.Expect(latest.Status.ObservedGeneration).To(gomega.Equal(latest.Generation))
		g.Expect(latest.Status.Replicas).To(gomega.Equal(replicas))
		g.Expect(latest.Status.UpdatedReplicas).To(gomega.Equal(updatedReplicas))
		g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(readyReplicas))
		g.Expect(latest.Status.AvailableReplicas).To(gomega.Equal(availableReplicas))
		g.Expect(latest.Status.UnavailableReplicas).To(gomega.Equal(int32(0)))
		g.Expect(latest.Status.ScalingTargetSelector).To(gomega.ContainSubstring("ray.io/node-type=head"))
	}, rayClusterFleetTimeout, rayClusterFleetInterval).Should(gomega.Succeed())
}
