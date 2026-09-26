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
	"sync"

	v1 "k8s.io/api/core/v1"
)

// PodArray is a simple implementation of types.PodList indexed by deployment names.
// Pods and the returned slices are read-only after publication. Lazy indexing
// never changes the shared backing array.
type PodArray struct {
	Pods []*v1.Pod

	deployments      []string
	podsByDeployment map[string][]*v1.Pod
	indexOnce        sync.Once
}

func (arr *PodArray) Len() int {
	if arr == nil {
		return 0
	}
	return len(arr.Pods)
}

func (arr *PodArray) All() []*v1.Pod {
	if arr == nil {
		return nil
	}
	return arr.Pods
}

func (arr *PodArray) ListByIndex(deploymentName string) []*v1.Pod {
	if len(arr.Pods) == 0 {
		return nil
	}

	arr.indexOnce.Do(arr.initDeployments)

	return arr.podsByDeployment[deploymentName]
}

func (arr *PodArray) Indexes() []string {
	if len(arr.Pods) == 0 {
		return nil
	}

	arr.indexOnce.Do(arr.initDeployments)

	return arr.deployments
}

func (arr *PodArray) initDeployments() {
	podsByDeployment := make(map[string][]*v1.Pod)
	var deployments []string
	for _, pod := range arr.Pods {
		deployment := DeploymentNameFromPod(pod)
		if _, exists := podsByDeployment[deployment]; !exists {
			deployments = append(deployments, deployment)
		}
		podsByDeployment[deployment] = append(podsByDeployment[deployment], pod)
	}
	// A single deployment can share the already immutable snapshot.
	if len(deployments) == 1 {
		podsByDeployment[deployments[0]] = arr.Pods
	}
	arr.deployments = deployments
	arr.podsByDeployment = podsByDeployment
}

func (arr *PodArray) ListPortsForPod() map[string][]int {
	pods := arr.All()
	if len(pods) == 0 {
		return nil
	}

	podWithPort := make(map[string][]int, len(pods))
	for _, pod := range pods {
		ports := GetPortsForPod(pod)
		if len(ports) > 0 {
			podWithPort[pod.Name] = append(podWithPort[pod.Name], ports...)
		} else {
			podWithPort[pod.Name] = []int{}
		}
	}

	return podWithPort
}
