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
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podsFromCache(c *cache.Store) *utils.PodArray {
	return &utils.PodArray{Pods: c.ListPods()}
}

func requestContext(model string) *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), RouterNotSet, model, "", "id", "")
}

func TestSetFallback(t *testing.T) {
	deployment := "deployment"
	store := cache.NewForTest()
	store = cache.InitWithModelRouterProvider(store, NewSLORouter)
	storeCh := cache.InitWithAsyncPods(store, []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      deployment + "-replicaset-pod1",
				Namespace: "default",
				Labels: map[string]string{
					utils.DeploymentIdentifier: deployment,
				},
			},
			Status: v1.PodStatus{
				PodIP: "1.0.0.1",
				Conditions: []v1.PodCondition{
					{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					},
				},
			},
		},
	}, "test model")
	Init() // Required to initialize the router registry after store was initialized and before pods are added.
	store = <-storeCh

	_, err := store.GetRouter(RouterSLO.NewContext(context.Background(), "test model", "", "request id", ""))
	assert.Nil(t, err, "set fallback should wait router initialize")
}

func TestNoPods(t *testing.T) {
	c := cache.Store{}
	r1 := randomRouter{}
	model := ""
	targetPodIP, err := r1.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(requestContext(model), podsFromCache(&c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithNoIPPods(t *testing.T) {
	model := ""
	c := cache.NewWithPodsForTest([]*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p1",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p2",
			},
		},
	}, model)

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(requestContext(model), podsFromCache(c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: c,
	}
	targetPodIP, err = r3.Route(requestContext(model), podsFromCache(c))
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithIPPods(t *testing.T) {
	// two case:
	// case 1: pod ready
	// case 2: pod ready & terminating -> we can send request at this moment.
	model := "test-model"
	c := cache.NewWithPodsMetricsForTest(
		[]*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "p1",
				},
				Status: v1.PodStatus{
					PodIP: "0.0.0.0",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "p2",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.0",
					Conditions: []v1.PodCondition{
						{
							Type:   v1.PodReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
		},
		model,
		map[string]map[string]metrics.MetricValue{
			"p1": {
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 20},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 20},
			},
			"p2": {
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 45},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 2},
			},
		})

	pods := podsFromCache(c)
	assert.NotEqual(t, 0, pods.Len(), "No pods initiailized")

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")

	r2 := leastRequestRouter{
		cache: c,
	}
	targetPodIP, err = r2.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")

	r3 := throughputRouter{
		cache: c,
	}
	targetPodIP, err = r3.Route(requestContext(model), pods)
	assert.NoError(t, err)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
}

// TestSelectRandomPod tests the selectRandomPod function.
func TestSelectRandomPod(t *testing.T) {
	tests := []struct {
		name      string
		pods      []*v1.Pod
		expectErr bool
	}{
		{
			name: "Single ready pod",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "Multiple ready pods",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.1",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.2",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
				{
					Status: v1.PodStatus{
						PodIP: "10.0.0.3",
						Conditions: []v1.PodCondition{
							{
								Type:   v1.PodReady,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name:      "No pods",
			pods:      []*v1.Pod{},
			expectErr: true,
		},
		{
			name: "Pods without IP",
			pods: []*v1.Pod{
				{
					Status: v1.PodStatus{PodIP: ""},
				},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new random generator with a fixed seed for consistent test results
			// Seed randomness for consistent results in tests
			r := rand.New(rand.NewSource(42))
			chosenPod, err := utils.SelectRandomPod(tt.pods, r.Intn)
			if tt.expectErr {
				if err == nil {
					t.Errorf("expected an error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				podIP := chosenPod.Status.PodIP
				// Verify that the returned pod IP exists in the input map
				found := false
				for _, pod := range tt.pods {
					if pod.Status.PodIP == podIP {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("returned pod IP %v is not in the input pods", podIP)
				}
			}
		})
	}
}
