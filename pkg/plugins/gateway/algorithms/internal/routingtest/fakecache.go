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

package routingtest

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// FakeCache serves counts from maps. It embeds a nil cache.Cache, so a Cache method the
// router is not supposed to call panics instead of returning something plausible.
type FakeCache struct {
	cache.Cache

	// Running is what GetPodsRunningRequests reports, by pod name. A pod not in it is missing
	// from the result, as for a pod the cache does not know.
	Running    map[string]int64
	RunningErr error
	// NilCounts makes GetPodsRunningRequests answer (nil, nil), as a custom or mock cache might.
	NilCounts bool
	// PortRunning is the per-port realtime metric of data-parallel pods, by "<pod>/<port>".
	PortRunning map[string]float64

	// ReadySince backs GetPodsReadySince (UnixNano by pod name) and LatestPodReadySince (the max
	// of these values). A pod not in it is missing from GetPodsReadySince's result, same "no
	// ramp info" semantics as a pod the cache does not know about. nil by default, which makes
	// LatestPodReadySince report 0 -- applyRampAdjustment's early exit then skips
	// GetPodsReadySince entirely, exercising that no-op path for every test that doesn't
	// populate ReadySince, without needing to opt out of anything.
	ReadySince map[string]int64

	// ReadySinceCalls counts GetPodsReadySince invocations, so tests can assert
	// applyRampAdjustment's LatestPodReadySince early exit actually skipped the per-pod call
	// rather than merely producing the same end result via the slow path.
	ReadySinceCalls int

	Batches [][]string // pod names of every GetPodsRunningRequests call, in order
}

func (c *FakeCache) GetPodsRunningRequests(pods []*v1.Pod) (map[string]int64, error) {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	c.Batches = append(c.Batches, names)
	if c.RunningErr != nil {
		return nil, c.RunningErr
	}
	if c.NilCounts {
		return nil, nil
	}
	counts := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if n, ok := c.Running[pod.Name]; ok {
			counts[utils.GeneratePodKey(pod.Namespace, pod.Name)] = n
		}
	}
	return counts, nil
}

func (c *FakeCache) GetPodsReadySince(pods []*v1.Pod) (map[string]int64, error) {
	c.ReadySinceCalls++
	result := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if v, ok := c.ReadySince[pod.Name]; ok {
			result[utils.GeneratePodKey(pod.Namespace, pod.Name)] = v
		}
	}
	return result, nil
}

// LatestPodReadySince is the other half of cache.PodReadySinceProvider: the real *Store
// implementation reports a maintained running max in O(1); this fake computes the same value
// directly from ReadySince each call, which is fine at test scale and keeps the fake's behavior
// obviously correct by construction rather than needing its own separate upkeep.
func (c *FakeCache) LatestPodReadySince() int64 {
	var latest int64
	for _, v := range c.ReadySince {
		if v > latest {
			latest = v
		}
	}
	return latest
}

func (c *FakeCache) GetMetricValueByPod(podName, _, metricName string) (metrics.MetricValue, error) {
	if port, ok := strings.CutPrefix(metricName, metrics.RealtimeNumRequestsRunning+"/"); ok {
		if n, found := c.PortRunning[podName+"/"+port]; found {
			return &metrics.SimpleMetricValue{Value: n}, nil
		}
	}
	return nil, fmt.Errorf("no metric %s for pod %s", metricName, podName)
}
