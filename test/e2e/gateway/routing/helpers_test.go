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

package e2e

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	framework "github.com/vllm-project/aibrix/test/e2e/framework"
)

var e2eConfig = framework.LoadConfig()

var (
	gatewayURL = e2eConfig.GatewayURL
	apiKey     = e2eConfig.APIKey

	createOpenAIClientWithRoutingStrategy = framework.NewOpenAIClientWithRoutingStrategy
	createOpenAIClientWithConfigProfile   = framework.NewOpenAIClientWithConfigProfile
)

const (
	modelName      = framework.ModelName
	modelNameQwen3 = framework.ModelNameQwen3
)

// maxPodDiscoveryRequests bounds discoverRoutablePods. Random sampling alone needs about this
// many requests to see every pod reliably.
const maxPodDiscoveryRequests = 30

// discoverRoutablePods returns the sorted names of the pods the gateway routes the test model to.
// It lists the model's ready Pods, then sends random-routed requests until each of them has served
// one, so the result holds only pods the gateway can reach and each of them has seen traffic.
func discoverRoutablePods(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	pods, err := framework.InitializeKubernetesClient(t).CoreV1().Pods(e2eConfig.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "model.aibrix.ai/name=" + modelName,
	})
	require.NoError(t, err)
	readyPods := make(map[string]struct{})
	for i := range pods.Items {
		if isPodReady(&pods.Items[i]) {
			readyPods[pods.Items[i].Name] = struct{}{}
		}
	}
	require.NotEmpty(t, readyPods, "no ready pods found for model %s", modelName)

	discovered := make(map[string]struct{})
	for i := 0; i < maxPodDiscoveryRequests && !containsAllPods(discovered, readyPods); i++ {
		if pod := getTargetPodFromChatCompletion(t, fmt.Sprintf("Pod discovery request %d", i), "random"); pod != "" {
			discovered[pod] = struct{}{}
		}
	}
	names := make([]string, 0, len(discovered))
	for pod := range discovered {
		names = append(names, pod)
	}
	sort.Strings(names)
	return names
}

func containsAllPods(set, subset map[string]struct{}) bool {
	for pod := range subset {
		if _, ok := set[pod]; !ok {
			return false
		}
	}
	return true
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
