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

package types

import (
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// runningRequestsSnapshot holds the running-request counts of the first read in one routing
// decision. With Redis, every read of a pod list is a pipelined HGETALL per pod, and the
// inflight filter, the load-imbalance gate, the strategy and its auto-blend each read the same
// ready pods or a subset of them, so sharing the first read turns three or four fleet-wide
// reads into one. Like routingOverrides, it is written and read only by the request goroutine
// that routes, so it needs no lock.
type runningRequestsSnapshot struct {
	// counts is the first read's result, nil until a read succeeds.
	counts map[string]int64
}

// StartRunningRequestsSnapshot makes RunningRequests share the first read until
// StopRunningRequestsSnapshot. The gateway brackets one routing decision with the pair, so
// reads after the decision see live counts. The admission that reserves the chosen pod (cache.AdmitPodRunningRequest) is not a read and
// always goes to the live counter.
func (r *RoutingContext) StartRunningRequestsSnapshot() {
	if r == nil {
		return
	}
	r.runningRequests = &runningRequestsSnapshot{}
}

// StopRunningRequestsSnapshot drops the shared counts, so later reads see live values again.
func (r *RoutingContext) StopRunningRequestsSnapshot() {
	if r == nil {
		return
	}
	r.runningRequests = nil
}

// RunningRequests returns the running-request count of pods, keyed by utils.GeneratePodKey,
// as read returns it. Outside a snapshot it calls read. Inside one, the first successful read
// is kept, and a later call whose pods all have a count in it is answered from it, so every
// reader in one routing decision sees the same value for a pod. Any other call reads live and
// is not kept. The first read is lazy, so a decision that reads no counts pays nothing. The result must not be modified, and inside a snapshot it can hold
// pods besides the ones asked for, so look pods up by key rather than ranging over it.
func (r *RoutingContext) RunningRequests(pods []*v1.Pod, read func(pods []*v1.Pod) (map[string]int64, error)) (map[string]int64, error) {
	if r == nil || r.runningRequests == nil {
		return read(pods)
	}
	s := r.runningRequests
	if s.counts == nil {
		counts, err := read(pods)
		if err != nil || counts == nil {
			return counts, err
		}
		s.counts = counts
		return counts, nil
	}
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		if _, ok := s.counts[utils.GeneratePodKey(pod.Namespace, pod.Name)]; !ok {
			return read(pods)
		}
	}
	return s.counts, nil
}
