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

package gateway

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// Inflight rejection scopes. The response and the log reason are identical whether the cap was
// checked on the candidate set or on every ready pod, and traces are sampled, so this label is
// what tells an operator whether a 429 came from the sampled set (candidates) or from a fleet
// that is entirely at the cap (fleet).
const (
	inflightScopeCandidates = "candidates"
	inflightScopeFleet      = "fleet"
)

var (
	inflightRejectionsOnce sync.Once
	inflightRejections     *prometheus.CounterVec
)

// recordInflightRejection counts a 429 from the per-replica inflight cap by the set the cap was
// evaluated on. See inflightScopeCandidates and inflightScopeFleet.
func recordInflightRejection(model, scope string) {
	inflightRejectionsOnce.Do(func() {
		vec := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "routing_inflight_rejections_total",
				Help:      "Requests rejected because every pod considered was at the per-replica inflight cap, by whether the cap was checked on the candidate set or on every ready pod",
			},
			[]string{"model", "scope"},
		)
		if err := prometheus.Register(vec); err != nil {
			registered, ok := err.(prometheus.AlreadyRegisteredError)
			if !ok {
				klog.ErrorS(err, "failed to register inflight rejection metric")
				return
			}
			vec = registered.ExistingCollector.(*prometheus.CounterVec)
		}
		inflightRejections = vec
	})
	if inflightRejections != nil {
		inflightRejections.WithLabelValues(model, scope).Inc()
	}
}
