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

	"github.com/vllm-project/aibrix/pkg/utils"

	v1 "k8s.io/api/core/v1"
)

// Router defines the interface for routing logic to select target pods.
type Router interface {
	// TODO: add routeContext as a function parameter.
	// Route returns the target pod
	Route(ctx context.Context, pods map[string]*v1.Pod, model, message string) (string, error)
}

// selectRandomPodWithRand selects a random pod from the provided pod map.
// It returns an error if no ready pods are available.
func selectRandomPod(pods map[string]*v1.Pod, randomFn func(int) int) (string, error) {
	readyPods := utils.FilterReadyPods(pods)
	if len(readyPods) == 0 {
		return "", fmt.Errorf("no ready pods available for fallback")
	}
	randomPod := readyPods[randomFn(len(readyPods))]
	return randomPod.Status.PodIP, nil
}
