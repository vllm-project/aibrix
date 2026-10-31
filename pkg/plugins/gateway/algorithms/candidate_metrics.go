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

package routingalgorithms

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// Paths of the candidate stage, the path label of aibrix_routing_candidate_selections_total.
const (
	// candidatePathRanked: half the candidates were ranked on the strategies' estimates.
	candidatePathRanked = "ranked"
	// candidatePathNoEstimate: no strategy of the router has an estimate (random,
	// power-of-two), so all K candidates were drawn at random, as intended.
	candidatePathNoEstimate = "random_no_estimate"
	// candidatePathEstimateFailed: the router has estimates but none could be computed (for
	// example a cache without in-memory counts), so all K candidates were drawn at random.
	// A steady rate here means the stage is running without the ranking it was configured for.
	candidatePathEstimateFailed = "random_estimate_failed"
)

var (
	candidateSelectionsOnce sync.Once
	candidateSelections     *prometheus.CounterVec
)

// recordCandidateSelection counts one decision that the candidate stage narrowed, by model
// and path. Decisions the stage leaves alone (K = 0, K covering every ready pod, a strategy
// that is not candidate-safe) are not counted.
func recordCandidateSelection(model, path string) {
	candidateSelectionsOnce.Do(func() {
		vec := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "routing_candidate_selections_total",
				Help:      "Routing decisions narrowed to K candidate pods, by whether the candidates were ranked on in-memory estimates or drawn at random",
			},
			[]string{"model", "path"},
		)
		if err := prometheus.Register(vec); err != nil {
			registered, ok := err.(prometheus.AlreadyRegisteredError)
			if !ok {
				klog.ErrorS(err, "failed to register candidate selection metric")
				return
			}
			vec = registered.ExistingCollector.(*prometheus.CounterVec)
		}
		candidateSelections = vec
	})
	if candidateSelections != nil {
		candidateSelections.WithLabelValues(model, path).Inc()
	}
}
