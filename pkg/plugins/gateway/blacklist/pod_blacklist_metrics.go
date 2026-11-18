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

package blacklist

import (
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
)

// The reasons a pod becomes excluded, as the reason label of
// metrics.GatewayPodBlacklistExclusionsTotal.
const (
	excludeProbeDead     = "probe_dead"
	excludeProbeDegraded = "probe_degraded"
	excludeLoadOutlier   = "load_outlier"
	excludeFlakyReAdd    = "flaky_readd"
	// excludeRequestFailures: the pod answered requests with upstream failures in a row.
	excludeRequestFailures = "request_failures"
	// excludeRequestFailuresProbation: the same, for a pod that was already blocked once and
	// failed again on probation, where one failure is enough. Told apart from the first block
	// so a pod that keeps coming back and failing is visible, and the threshold can be tuned
	// on first blocks alone.
	excludeRequestFailuresProbation = "request_failures_probation"
)

// What became of a failing status, as the outcome label of
// metrics.GatewayPodBlacklistRequestFailuresTotal.
const (
	// failureCounted: the failure added to the pod's count, or blocked it.
	failureCounted = "counted"
	// failureIgnored: the pod was already excluded, so the failure was a request that was in
	// flight when the block began and was not counted.
	failureIgnored = "ignored"
)

// The reasons an exclusion is lifted before its deadline, as the reason label of
// metrics.GatewayPodBlacklistRecoveriesTotal.
const (
	recoverProbeHealthy = "probe_healthy"
	recoverLoadNormal   = "load_recovered"
)

// The labels carry the pod's model, never the pod: a series per pod would grow with every
// restart. The sweeps have no request, so the model comes from the pod.

func emitPodBlacklistCounter(metric string, pod *v1.Pod, label, value string) {
	_, model := podTarget(pod, "", "")
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, nil, metric,
		&metrics.SimpleMetricValue{Value: 1}, map[string]string{label: value})
}

func emitExclusion(pod *v1.Pod, reason string) {
	emitPodBlacklistCounter(metrics.GatewayPodBlacklistExclusionsTotal, pod, "reason", reason)
}

// emitRequestFailure counts a response whose status IsPodFailureStatus accepts. Only failures
// are emitted, never every response, so a healthy fleet pays nothing here; the status label is
// one of three values, and there is no pod label, so the series stay few.
func emitRequestFailure(pod *v1.Pod, status int, outcome string) {
	_, model := podTarget(pod, "", "")
	metrics.EmitMetricToPrometheus(&types.RoutingContext{Model: model}, nil, metrics.GatewayPodBlacklistRequestFailuresTotal,
		&metrics.SimpleMetricValue{Value: 1}, map[string]string{"status": strconv.Itoa(status), "outcome": outcome})
}

func emitRecovery(pod *v1.Pod, reason string) {
	emitPodBlacklistCounter(metrics.GatewayPodBlacklistRecoveriesTotal, pod, "reason", reason)
}

func emitProbeVerdict(pod *v1.Pod, result ProbeResult) {
	verdict := "healthy"
	switch result {
	case ProbeDegraded:
		verdict = "degraded"
	case ProbeDead:
		verdict = "dead"
	}
	emitPodBlacklistCounter(metrics.GatewayPodBlacklistProbeTotal, pod, "verdict", verdict)
}

// countExcludedNow is how many entries exclude their pod at now: load-excluded, open or
// blocked by request failures with a block that has yet to end. It looks at every entry,
// which are few, so it runs once per sweep and not per request.
func (b *PodBlacklist) countExcludedNow(now time.Time) int64 {
	var n int64
	b.pods.Range(func(_, v any) bool {
		state, ok := v.(*podFailureState)
		if !ok {
			return true
		}
		state.mu.RLock()
		if state.blocksLocked(now) {
			n++
		}
		state.mu.RUnlock()
		return true
	})
	return n
}

// publishExcludedPods sets the excluded-pods gauge. Each sweep calls it when it is done, so
// the gauge is as fresh as the last sweep.
func (b *PodBlacklist) publishExcludedPods() {
	metrics.EmitMetricToPrometheus(&types.RoutingContext{}, nil, metrics.GatewayPodBlacklistExcludedPods,
		&metrics.SimpleMetricValue{Value: float64(b.countExcludedNow(time.Now()))}, nil)
}
