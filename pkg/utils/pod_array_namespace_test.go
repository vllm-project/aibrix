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

package utils

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podInNamespace(namespace, name, deployment string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{DeploymentIdentifier: deployment},
		},
	}
}

var _ = Describe("PodArray with the same pod name in different namespaces", func() {
	It("Should keep each namespace's pods under their own deployment", func() {
		podA := podInNamespace("ns-a", "model-0", "deploy-a")
		podB := podInNamespace("ns-b", "model-0", "deploy-b")
		podArray := &PodArray{Pods: []*v1.Pod{podA, podB}}

		Expect(podArray.Indexes()).To(ConsistOf("deploy-a", "deploy-b"))
		Expect(podArray.ListByIndex("deploy-a")).To(ConsistOf(podA))
		Expect(podArray.ListByIndex("deploy-b")).To(ConsistOf(podB))
	})

	It("Should group several same-named pods by deployment regardless of input order", func() {
		a0 := podInNamespace("ns-a", "model-0", "deploy-a")
		b0 := podInNamespace("ns-b", "model-0", "deploy-b")
		a1 := podInNamespace("ns-a", "model-1", "deploy-a")
		b1 := podInNamespace("ns-b", "model-1", "deploy-b")
		podArray := &PodArray{Pods: []*v1.Pod{a0, b0, a1, b1}}

		Expect(podArray.Indexes()).To(ConsistOf("deploy-a", "deploy-b"))
		Expect(podArray.ListByIndex("deploy-a")).To(ConsistOf(a0, a1))
		Expect(podArray.ListByIndex("deploy-b")).To(ConsistOf(b0, b1))
	})
})
