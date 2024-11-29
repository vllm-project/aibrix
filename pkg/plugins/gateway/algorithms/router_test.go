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

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/metrics"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNoPods(t *testing.T) {
	c := cache.Cache{}
	r1 := randomRouter{}
	model := ""
	targetPodIP, err := r1.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithNoIPPods(t *testing.T) {
	c := cache.Cache{
		Pods: map[string]*v1.Pod{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "p1",
				},
			},
			"p2": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "p2",
				},
			},
		},
	}
	model := ""

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods, model)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithIPPods(t *testing.T) {
	// two case:
	// case 1: pod ready
	// case 2: pod ready & terminating -> we can send request at this moment.
	c := cache.Cache{
		Pods: map[string]*v1.Pod{
			"p1": {
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
			"p2": {
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
		PodMetrics: map[string]map[string]metrics.MetricValue{
			"p1": {
				metrics.NumRequestsRunning:              &metrics.SimpleMetricValue{Value: 5},
				metrics.NumRequestsWaiting:              &metrics.SimpleMetricValue{Value: 5},
				metrics.NumRequestsSwapped:              &metrics.SimpleMetricValue{Value: 5},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 20},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 20},
			},
			"p2": {
				metrics.NumRequestsRunning:              &metrics.SimpleMetricValue{Value: 15},
				metrics.NumRequestsWaiting:              &metrics.SimpleMetricValue{Value: 15},
				metrics.NumRequestsSwapped:              &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgPromptThroughputToksPerS:     &metrics.SimpleMetricValue{Value: 15},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 2},
			},
		},
	}
	model := ""

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(context.TODO(), c.Pods, model)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods, model)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods, model)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)
}

// TestSelectRandomPod tests the selectRandomPod function.
func TestSelectRandomPod(t *testing.T) {
	tests := []struct {
		name      string
		pods      map[string]*v1.Pod
		expectErr bool
	}{
		{
			name: "Single ready pod",
			pods: map[string]*v1.Pod{
				"pod1": {
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
			pods: map[string]*v1.Pod{
				"pod1": {
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
				"pod2": {
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
				"pod3": {
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
			pods:      map[string]*v1.Pod{},
			expectErr: true,
		},
		{
			name: "Pods without IP",
			pods: map[string]*v1.Pod{
				"pod1": {
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
			podIP, err := selectRandomPod(tt.pods, r.Intn)
			if tt.expectErr {
				if err == nil {
					t.Errorf("expected an error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
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
