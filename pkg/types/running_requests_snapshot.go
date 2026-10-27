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
	"sync"

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// RunningRequestsReader reads the live cross-gateway running-request count of pods, keyed by
// utils.GeneratePodKey. cache.Cache's GetPodsRunningRequests has this shape.
type RunningRequestsReader func(pods []*v1.Pod) (map[string]int64, error)

// runningRequestsSnapshot holds the running-request counts one routing decision has read so
// far. With Redis, every read of a pod list is a pipelined HGETALL per pod, and the inflight
// filter, the load-imbalance gate, the strategy and its auto-blend each read the same pods, so
// sharing the counts turns three or four fleet-wide reads into one.
type runningRequestsSnapshot struct {
	mu sync.Mutex
	// counts is the first read's result, extended by later reads of other pods. A pod read
	// without a count stays absent, as in the reader's result.
	counts map[string]int64
	// keys records every pod already read, with its utils.GeneratePodKey key, so a pod the
	// cache has no count for is not read again and no pod's key is built twice. The readers
	// of one decision pass the same *v1.Pod values; a different value for the same pod is
	// read again, and the count already held is kept.
	keys map[*v1.Pod]string
	// first is the pod list of the first read. keys is built from it only when a second read
	// comes, so a decision that reads once (e.g. random) pays nothing for the snapshot.
	first []*v1.Pod
	read  bool
}

// StartRunningRequestsSnapshot makes RunningRequests share counts until
// StopRunningRequestsSnapshot. The gateway brackets one routing decision with the pair. The
// admission that reserves the chosen pod (cache.AdmitPodRunningRequest) is not a read and
// always goes to the live counter.
func (r *RoutingContext) StartRunningRequestsSnapshot() {
	if r == nil {
		return
	}
	r.runningRequests = &runningRequestsSnapshot{}
}

// StopRunningRequestsSnapshot drops the shared counts, so later reads (a retried routing
// decision, or work that outlives it) see live values again.
func (r *RoutingContext) StopRunningRequestsSnapshot() {
	if r == nil {
		return
	}
	r.runningRequests = nil
}

// RunningRequests returns the running-request count of pods, keyed by utils.GeneratePodKey.
// Outside a snapshot it calls read. Inside one, it calls read only for the pods no earlier
// call has read, and answers the rest from the counts already read, so every reader in one
// routing decision sees the same value for a pod. A pod missing from the result has no count,
// as with read itself. A failed read is returned and not remembered, so the next call reads
// again. The result must not be modified: inside a snapshot it can be the shared counts.
func (r *RoutingContext) RunningRequests(pods []*v1.Pod, read RunningRequestsReader) (map[string]int64, error) {
	if r == nil || r.runningRequests == nil {
		return read(pods)
	}
	s := r.runningRequests
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.read {
		// The first read of the decision: its result is the snapshot.
		counts, err := read(pods)
		if err != nil {
			return nil, err
		}
		if counts == nil {
			counts = make(map[string]int64)
		}
		s.counts, s.first, s.read = counts, pods, true
		return counts, nil
	}
	if s.keys == nil {
		s.keys = make(map[*v1.Pod]string, len(s.first))
		for _, pod := range s.first {
			if pod != nil {
				s.keys[pod] = utils.GeneratePodKey(pod.Namespace, pod.Name)
			}
		}
		s.first = nil
	}

	var missing []*v1.Pod
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		if _, ok := s.keys[pod]; !ok {
			missing = append(missing, pod)
		}
	}
	if len(missing) > 0 {
		counts, err := read(missing)
		if err != nil {
			return nil, err
		}
		for _, pod := range missing {
			key := utils.GeneratePodKey(pod.Namespace, pod.Name)
			s.keys[pod] = key
			if _, held := s.counts[key]; held {
				continue
			}
			if count, ok := counts[key]; ok {
				s.counts[key] = count
			}
		}
	}

	result := make(map[string]int64, len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		key := s.keys[pod]
		if count, ok := s.counts[key]; ok {
			result[key] = count
		}
	}
	return result, nil
}
