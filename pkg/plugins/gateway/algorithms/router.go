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

package routingalgorithms

import (
	"context"
	"fmt"
	"math/rand"

	v1 "k8s.io/api/core/v1"
)

const podMetricPort = "8000"

// Router defines the interface for routing logic to select target pods.
type Router interface {
	// Route returns the target pod
	Route(ctx context.Context, pods map[string]*v1.Pod) (string, error)
}

// selectRandomPod selects a random pod from the provided pod map.
// It returns an error if no ready pods are available.
func selectRandomPod(pods map[string]*v1.Pod) (string, error) {
	readyPods := filterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available among %d provided", len(pods))
	}
	randomPod := readyPods[rand.Intn(len(readyPods))]
	return randomPod.Status.PodIP, nil
}

// filterReadyPods filters and returns a list of pods that have a valid PodIP.
func filterReadyPods(pods map[string]*v1.Pod) []*v1.Pod {
	var readyPods []*v1.Pod
	for _, pod := range pods {
		if pod.Status.PodIP != "" && isPodReady(pod) {
			readyPods = append(readyPods, pod)
		}
	}
	return readyPods
}

func isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}
