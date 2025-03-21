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
				Expect(func() { podsByDeployment = podArray.PodsByDeployments(name) }).ShouldNot(Panic())
				Expect(len(podsByDeployment)).To(Equal(5))
			}(deploymentName)
		}

		wg.Wait()
	})
})
