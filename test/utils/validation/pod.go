/*
Copyright 2025 The Aibrix Team.

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

package validation

import (
	"context"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func WaitForPodsCreated(ctx context.Context, k8sClient client.Client,
	ns, podLabelKey, podLabelValue string, expected int) {
	gomega.Eventually(func(g gomega.Gomega) int {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(ns),
			client.MatchingLabels{podLabelKey: podLabelValue},
		)).To(gomega.Succeed())
		return len(podList.Items)
	}, time.Second*10, time.Millisecond*250).Should(gomega.Equal(expected))
}

func MarkPodsReady(ctx context.Context, k8sClient client.Client, ns, podLabelKey, podLabelValue string) {
	gomega.Eventually(func(g gomega.Gomega) {
		podList := &corev1.PodList{}
		g.Expect(k8sClient.List(ctx, podList,
			client.InNamespace(ns),
			client.MatchingLabels{podLabelKey: podLabelValue},
		)).To(gomega.Succeed())

		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.DeletionTimestamp != nil {
				continue
			}
			MakePodReady(pod)
			g.Expect(k8sClient.Status().Update(ctx, pod)).To(gomega.Succeed())
		}
	}, time.Second*5, time.Millisecond*250).Should(gomega.Succeed())
}

func MakePodReady(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
			Reason: "TestReady",
		},
	}
}

func MakePodTemplate(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "test",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test-container",
					Image: image,
				},
			},
		},
	}
}
