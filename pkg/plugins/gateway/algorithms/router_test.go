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
	"testing"

	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNoPods(t *testing.T) {
	c := cache.Cache{}
	r1 := randomRouter{}
	targetPodIP, err := r1.Route(context.TODO(), c.Pods)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods)
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

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(context.TODO(), c.Pods)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods)
	assert.Empty(t, targetPodIP, "targetPodIP must be empty")
	assert.Error(t, err, "no pod has IP")
}

func TestWithIPPods(t *testing.T) {
	c := cache.Cache{
		Pods: map[string]*v1.Pod{
			"p1": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "p1",
				},
				Status: v1.PodStatus{
					PodIP: "0.0.0.0",
				},
			},
			"p2": {
				ObjectMeta: metav1.ObjectMeta{
					Name: "p2",
				},
				Status: v1.PodStatus{
					PodIP: "1.0.0.0",
				},
			},
		},
		PodMetrics: map[string]map[string]*cache.MetricValue{
			"p1": {
				num_requests_running:                 &cache.MetricValue{Value: 5},
				num_requests_waiting:                 &cache.MetricValue{Value: 5},
				num_requests_swapped:                 &cache.MetricValue{Value: 5},
				avg_prompt_throughput_toks_per_s:     &cache.MetricValue{Value: 20},
				avg_generation_throughput_toks_per_s: &cache.MetricValue{Value: 20},
			},
			"p2": {
				num_requests_running:                 &cache.MetricValue{Value: 15},
				num_requests_waiting:                 &cache.MetricValue{Value: 15},
				num_requests_swapped:                 &cache.MetricValue{Value: 15},
				avg_prompt_throughput_toks_per_s:     &cache.MetricValue{Value: 15},
				avg_generation_throughput_toks_per_s: &cache.MetricValue{Value: 2},
			},
		},
	}

	r1 := randomRouter{}
	targetPodIP, err := r1.Route(context.TODO(), c.Pods)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)

	r2 := leastRequestRouter{
		cache: &c,
	}
	targetPodIP, err = r2.Route(context.TODO(), c.Pods)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)

	r3 := throughputRouter{
		cache: &c,
	}
	targetPodIP, err = r3.Route(context.TODO(), c.Pods)
	assert.NotEmpty(t, targetPodIP, "targetPodIP is not empty")
	assert.NoError(t, err)
}
