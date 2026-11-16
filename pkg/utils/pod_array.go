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
	"slices"
	"sync"

	v1 "k8s.io/api/core/v1"
)

// PodArray is a simple implementation of types.PodList indexed by deployment names.
//
// Pods may be a snapshot shared with other readers, such as the per-model
// Registry snapshot that every request reads through All(). Indexing therefore
// never writes to Pods: the per-deployment lists are built in their own slices.
type PodArray struct {
	Pods []*v1.Pod

	deployments      []string
	podsByDeployment map[string][]*v1.Pod
	indexOnce        sync.Once
	// indexPanic keeps a panic raised while indexing so that every later
	// ListByIndex or Indexes call re-panics with it, instead of the Once leaving
	// the index silently empty.
	indexPanic any
}

func (arr *PodArray) Len() int {
	if arr == nil {
		return 0
	}
	return len(arr.Pods)
}

// All returns Pods itself, not a copy. Callers must treat it as read-only: it may
// be a snapshot shared by concurrent requests.
func (arr *PodArray) All() []*v1.Pod {
	if arr == nil {
		return nil
	}
	return arr.Pods
}

// ListByIndex returns the pods of one deployment. Callers must treat the result
// as read-only: when all pods are in one deployment it is Pods itself, the same
// shared snapshot All returns, so an in-place writer such as
// FilterRoutablePodsInPlace must copy it first.
func (arr *PodArray) ListByIndex(deploymentName string) []*v1.Pod {
	if len(arr.Pods) == 0 {
		return nil
	}

	arr.buildIndex()

	return arr.podsByDeployment[deploymentName]
}

func (arr *PodArray) Indexes() []string {
	if len(arr.Pods) == 0 {
		return nil
	}

	arr.buildIndex()

	return arr.deployments
}

// buildIndex runs initDeployments once and re-raises its panic, if any, on every
// call, as the index did before it used sync.Once.
func (arr *PodArray) buildIndex() {
	arr.indexOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				arr.indexPanic = r
			}
		}()
		arr.initDeployments()
	})
	if arr.indexPanic != nil {
		panic(arr.indexPanic)
	}
}

// initDeployments groups the pods by deployment, keeping the first-seen order of
// the deployments and the original order of the pods within each one. It runs
// once, through buildIndex, so the index fields are published fully built.
func (arr *PodArray) initDeployments() {
	podsByDeployment := make(map[string][]*v1.Pod)
	var deployments []string
	for _, pod := range arr.Pods {
		deploymentName := DeploymentNameFromPod(pod) // Count "" in.
		if _, ok := podsByDeployment[deploymentName]; !ok {
			deployments = append(deployments, deploymentName)
		}
		podsByDeployment[deploymentName] = append(podsByDeployment[deploymentName], pod)
	}
	if len(deployments) == 1 {
		// Every pod is in the one deployment, so share Pods instead of a copy.
		podsByDeployment[deployments[0]] = arr.Pods
	}
	// Clip each list so that a caller appending to it cannot write into spare
	// capacity another caller of ListByIndex also sees. Pods itself may have
	// spare capacity when the caller built it with append.
	for name, pods := range podsByDeployment {
		podsByDeployment[name] = slices.Clip(pods)
	}

	arr.deployments = deployments
	arr.podsByDeployment = podsByDeployment
}

// ListPortsForPod returns each pod's ports keyed by pod key (namespace/name, see
// GeneratePodKey).
func (arr *PodArray) ListPortsForPod() map[string][]int {
	pods := arr.All()
	if len(pods) == 0 {
		return nil
	}

	podWithPort := make(map[string][]int, len(pods))
	for _, pod := range pods {
		ports := GetPortsForPod(pod)
		if ports == nil {
			ports = []int{}
		}
		podWithPort[GeneratePodKey(pod.Namespace, pod.Name)] = ports
	}

	return podWithPort
}
