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
	"math/rand"
	"sync"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// getPodWithDeployment generates a v1.Pod object with deployment information.
// It takes the deployment name as input and returns a pointer to a v1.Pod object.
func getPodWithDeployment(deploymentName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName + "-pod",
			Namespace: "default",
			Labels: map[string]string{
				DeploymentIdentifier: deploymentName,
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "main-container",
					Image: "nginx:latest",
				},
			},
		},
	}
}

var _ = Describe("PodArray", func() {
	It("Should work with nil object", func() {
		var podArray *PodArray
		Expect(podArray.Len()).To(Equal(0))
		Expect(podArray.All()).To(BeNil())
	})

	It("Should All() return original pods", func() {
		pods := []*v1.Pod{getPodWithDeployment("deployment")}
		podArray := &PodArray{Pods: pods}
		Expect(len(podArray.All())).To(Equal(1))
		Expect(&podArray.All()[0]).To(Equal(&pods[0]))
	})

	It("Should Index() skip nil pods", func() {
		pods := []*v1.Pod{}
		podArray := &PodArray{Pods: pods}
		Expect(podArray.Indexes()).To(BeNil())
		podArray = &PodArray{Pods: nil}
		Expect(podArray.Indexes()).To(BeNil())
	})

	It("Should Indexes() return single deployment names for monogenous deployment", func() {
		deployment := "deployment-1"
		pods := []*v1.Pod{
			getPodWithDeployment(deployment),
			getPodWithDeployment(deployment),
		}
		podArray := &PodArray{Pods: pods}
		Expect(podArray.Indexes()).To(Equal([]string{deployment}))
	})

	It("Should Indexes() return multiple different deployment names for heterogneous deployments", func() {
		deployments := []string{"deployment-1", "deployment-2"}
		pods := []*v1.Pod{
			getPodWithDeployment(deployments[0]),
			getPodWithDeployment(deployments[1]),
			getPodWithDeployment(deployments[1]),
			getPodWithDeployment(deployments[0]),
			getPodWithDeployment(deployments[0]),
		}
		podArray := &PodArray{Pods: pods}
		Expect(podArray.Indexes()).To(Equal(deployments))
	})

	It("Should ListByIndex() skip nil pods", func() {
		pods := []*v1.Pod{}
		podArray := &PodArray{Pods: pods}
		Expect(podArray.ListByIndex("deployment-1")).To(BeNil())
		podArray = &PodArray{Pods: nil}
		Expect(podArray.ListByIndex("deployment-1")).To(BeNil())
	})

	It("Should ListByIndex() reuse pods array for monogenous deployment", func() {
		deployment := "deployment-1"
		pods := []*v1.Pod{
			getPodWithDeployment(deployment),
			getPodWithDeployment(deployment),
		}
		podArray := &PodArray{Pods: pods}
		Expect(&podArray.ListByIndex("deployment-1")[0]).To(Equal(&pods[0]))
	})

	It("Should InListByIndex() reuse pods array for heterogneous deployments", func() {
		deployments := []string{"deployment-1", "deployment-2"}
		pods := []*v1.Pod{
			getPodWithDeployment(deployments[0]),
			getPodWithDeployment(deployments[1]),
			getPodWithDeployment(deployments[1]),
			getPodWithDeployment(deployments[0]),
			getPodWithDeployment(deployments[0]),
		}
		podArray := &PodArray{Pods: pods}
		Expect(&podArray.ListByIndex("deployment-1")[0]).To(Equal(&pods[0]))
		Expect(&podArray.ListByIndex("deployment-2")[0]).To(Equal(&pods[3]))
	})

	It("Should perform current PodsByDeployments call correctly", func() {
		deploymentNames := []string{"deployment-1", "deployment-2"}
		var pods []*v1.Pod
		for _, deploymentName := range deploymentNames {
			for i := 0; i < 5; i++ {
				pods = append(pods, getPodWithDeployment(deploymentName))
			}
		}
		podArray := &PodArray{Pods: pods}

		var wg sync.WaitGroup
		concurrency := 10

		for i := 0; i < concurrency; i++ {
			deploymentName := deploymentNames[rand.Int31n(int32(len(deploymentNames)))]
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				var podsByDeployment []*v1.Pod
				Expect(func() { podsByDeployment = podArray.ListByIndex(name) }).ShouldNot(Panic())
				Expect(len(podsByDeployment)).To(Equal(5))
			}(deploymentName)
		}

		wg.Wait()
	})
})
