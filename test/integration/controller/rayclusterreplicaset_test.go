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
	"sort"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

const rayClusterDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

var _ = ginkgo.Describe("RayClusterReplicaSet controller test", func() {
	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-rayclusterreplicaset-",
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

	ginkgo.It("creates, scales, reports status, and applies scale-down ordering", func() {
		matchLabels := map[string]string{"app": "raycluster-replicaset-lifecycle"}
		replicaSet := &orchestrationapi.RayClusterReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "raycluster-replicaset-lifecycle",
				Namespace: ns.Name,
			},
			Spec: orchestrationapi.RayClusterReplicaSetSpec{
				Replicas: ptr.To(int32(2)),
				Selector: &metav1.LabelSelector{
					MatchLabels: matchLabels,
				},
				Template: orchestrationapi.RayClusterTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: matchLabels,
					},
					Spec: makeIntegrationRayClusterSpec(),
				},
			},
		}
		gomega.Expect(k8sClient.Create(ctx, replicaSet)).To(gomega.Succeed())

		clusters := waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 2)
		for i := range clusters {
			markIntegrationRayClusterReady(ctx, k8sClient, &clusters[i])
		}
		waitForIntegrationReplicaSetStatus(ctx, k8sClient, replicaSet, 2, 2, 2, 2)

		latestReplicaSet := &orchestrationapi.RayClusterReplicaSet{}
		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replicaSet), latestReplicaSet)).To(gomega.Succeed())
		latestReplicaSet.Spec.Replicas = ptr.To(int32(3))
		gomega.Expect(k8sClient.Update(ctx, latestReplicaSet)).To(gomega.Succeed())

		clusters = waitForIntegrationRayClusters(ctx, k8sClient, ns.Name, matchLabels, 3)
		sort.Slice(clusters, func(i, j int) bool {
			return clusters[i].Name < clusters[j].Name
		})
		keepReadyHighCost := clusters[0]
		deleteNotReadyHighCost := clusters[1]
		deleteReadyLowCost := clusters[2]

		setIntegrationRayClusterReadinessAndDeletionCost(ctx, k8sClient, &keepReadyHighCost, true, "100")
		setIntegrationRayClusterReadinessAndDeletionCost(ctx, k8sClient, &deleteNotReadyHighCost, false, "1000")
		setIntegrationRayClusterReadinessAndDeletionCost(ctx, k8sClient, &deleteReadyLowCost, true, "-10")

		gomega.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replicaSet), latestReplicaSet)).To(gomega.Succeed())
		latestReplicaSet.Spec.Replicas = ptr.To(int32(1))
		gomega.Expect(k8sClient.Update(ctx, latestReplicaSet)).To(gomega.Succeed())

		gomega.Eventually(func() ([]string, error) {
			clusters := &rayv1.RayClusterList{}
			if err := k8sClient.List(ctx, clusters,
				client.InNamespace(ns.Name),
				client.MatchingLabels(matchLabels),
			); err != nil {
				return nil, err
			}
			names := make([]string, 0, len(clusters.Items))
			for _, cluster := range clusters.Items {
				names = append(names, cluster.Name)
			}
			sort.Strings(names)
			if len(names) != 1 {
				return names, fmt.Errorf("expected 1 RayCluster, got %d", len(names))
			}
			return names, nil
		}, time.Second*10, time.Millisecond*250).Should(gomega.Equal([]string{keepReadyHighCost.Name}))
	})
})

func makeIntegrationRayCluster(
	namespace string,
	name string,
	labels map[string]string,
	deletionCost string,
) *rayv1.RayCluster {
	return &rayv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				rayClusterDeletionCostAnnotation: deletionCost,
			},
		},
		Spec: makeIntegrationRayClusterSpec(),
	}
}

func makeIntegrationRayClusterSpec() rayv1.RayClusterSpec {
	return rayv1.RayClusterSpec{
		RayVersion: "fake-ray-version",
		HeadGroupSpec: rayv1.HeadGroupSpec{
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
	}
}

func markIntegrationRayClusterReady(ctx context.Context, k8sClient client.Client, cluster *rayv1.RayCluster) {
	setIntegrationRayClusterReadinessAndDeletionCost(ctx, k8sClient, cluster, true, "")
}

func setIntegrationRayClusterReadinessAndDeletionCost(
	ctx context.Context,
	k8sClient client.Client,
	cluster *rayv1.RayCluster,
	ready bool,
	deletionCost string,
) {
	latest := &rayv1.RayCluster{}
	gomega.Eventually(func(g gomega.Gomega) {
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), latest)).To(gomega.Succeed())
		if deletionCost != "" {
			if latest.Annotations == nil {
				latest.Annotations = map[string]string{}
			}
			latest.Annotations[rayClusterDeletionCostAnnotation] = deletionCost
			g.Expect(k8sClient.Update(ctx, latest)).To(gomega.Succeed())
		}
		if ready {
			latest.Status.Conditions = []metav1.Condition{
				{Type: string(rayv1.RayClusterProvisioned), Status: metav1.ConditionTrue},
				{Type: string(rayv1.HeadPodReady), Status: metav1.ConditionTrue},
			}
			latest.Status.DesiredWorkerReplicas = 1
			latest.Status.ReadyWorkerReplicas = 1
		} else {
			latest.Status.Conditions = nil
			latest.Status.DesiredWorkerReplicas = 1
			latest.Status.ReadyWorkerReplicas = 0
		}
		g.Expect(k8sClient.Status().Update(ctx, latest)).To(gomega.Succeed())
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

func waitForIntegrationRayClusters(
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	matchLabels map[string]string,
	expected int,
) []rayv1.RayCluster {
	var items []rayv1.RayCluster
	gomega.Eventually(func(g gomega.Gomega) {
		clusters := &rayv1.RayClusterList{}
		g.Expect(k8sClient.List(ctx, clusters,
			client.InNamespace(namespace),
			client.MatchingLabels(matchLabels),
		)).To(gomega.Succeed())
		g.Expect(clusters.Items).To(gomega.HaveLen(expected))
		items = clusters.Items
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
	return items
}

func waitForIntegrationReplicaSetStatus(
	ctx context.Context,
	k8sClient client.Client,
	replicaSet *orchestrationapi.RayClusterReplicaSet,
	replicas int32,
	fullyLabeledReplicas int32,
	readyReplicas int32,
	availableReplicas int32,
) {
	gomega.Eventually(func(g gomega.Gomega) {
		latest := &orchestrationapi.RayClusterReplicaSet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(replicaSet), latest)).To(gomega.Succeed())
		g.Expect(latest.Status.Replicas).To(gomega.Equal(replicas))
		g.Expect(latest.Status.FullyLabeledReplicas).To(gomega.Equal(fullyLabeledReplicas))
		g.Expect(latest.Status.ReadyReplicas).To(gomega.Equal(readyReplicas))
		g.Expect(latest.Status.AvailableReplicas).To(gomega.Equal(availableReplicas))
	}, time.Second*10, time.Millisecond*250).Should(gomega.Succeed())
}
