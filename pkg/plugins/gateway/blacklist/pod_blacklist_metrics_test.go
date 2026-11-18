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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
)

// emitted is one counter increment or gauge set the blacklist sent to Prometheus.
type emitted struct {
	name   string
	labels map[string]string
	value  float64
}

// capturedMetrics swaps the Prometheus emitters for ones that record what they are given, and
// puts them back when the test ends.
type capturedMetrics struct {
	mu     sync.Mutex
	events []emitted
}

func captureMetrics(t *testing.T) *capturedMetrics {
	t.Helper()
	c := &capturedMetrics{}
	record := func(name string, value float64, names, values []string) {
		labels := make(map[string]string, len(names))
		for i, n := range names {
			labels[n] = values[i]
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.events = append(c.events, emitted{name, labels, value})
	}
	prevCounter, prevGauge := metrics.IncrementCounterMetricFnForTest, metrics.SetGaugeMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name, _ string, value float64, names []string, values ...string) {
		record(name, value, names, values)
	}
	metrics.SetGaugeMetricFnForTest = func(name, _ string, value float64, names []string, values ...string) {
		record(name, value, names, values)
	}
	t.Cleanup(func() {
		metrics.IncrementCounterMetricFnForTest, metrics.SetGaugeMetricFnForTest = prevCounter, prevGauge
	})
	return c
}

// count is how many times name was incremented with every one of labels.
func (c *capturedMetrics) count(name string, labels map[string]string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.name != name {
			continue
		}
		match := true
		for k, v := range labels {
			if e.labels[k] != v {
				match = false
			}
		}
		if match {
			n++
		}
	}
	return n
}

// last is the value the gauge name was last set to.
func (c *capturedMetrics) last(name string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.events) - 1; i >= 0; i-- {
		if c.events[i].name == name {
			return c.events[i].value, true
		}
	}
	return 0, false
}

func modelPod(name string) *v1.Pod {
	pod := blacklistTestPod(name)
	pod.Labels = map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}
	return pod
}

func TestExclusionOverCap(t *testing.T) {
	for _, tc := range []struct {
		percent     int
		kept, total int
		want        bool
	}{
		// The default: no more than half of the pods may be excluded.
		{50, 2, 2, false},
		{50, 1, 2, false},
		{50, 0, 2, true},
		{50, 2, 3, false},
		{50, 1, 3, true},
		{50, 2, 4, false},
		{50, 1, 4, true},
		{50, 8, 8, false},
		{50, 3, 8, true},
		{50, 4, 8, false},
		// Off: any share, all of them included, is within the cap.
		{100, 0, 5, false},
		{100, 1, 5, false},
		// Nothing may be excluded.
		{0, 5, 5, false},
		{0, 4, 5, true},
		// Nothing to judge.
		{50, 0, 0, false},
	} {
		restore := SetMaxExcludedPercentForTest(tc.percent)
		assert.Equal(t, tc.want, ExclusionOverCap(tc.kept, tc.total), "cap %d%%, %d of %d kept", tc.percent, tc.kept, tc.total)
		restore()
	}
}

func TestMaxExcludedPercent_DefaultsToHalf(t *testing.T) {
	assert.Equal(t, 50, defaultMaxExcludedPercent)
	restore := SetMaxExcludedPercentForTest(30)
	assert.Equal(t, 30, MaxExcludedPercent())
	restore()
	assert.Equal(t, maxExcludedPercent, MaxExcludedPercent())
}

func TestMetrics_ExclusionsAreCountedOnceByReason(t *testing.T) {
	c := captureMetrics(t)
	var b PodBlacklist
	reason := func(r string) map[string]string { return map[string]string{"reason": r, "model": "test-model"} }

	degraded := modelPod("1")
	tripBreaker(&b, degraded)
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeProbeDegraded)),
		"only the sweep that trips the breaker counts, not the failed sweeps before it")

	dead := modelPod("2")
	for i := 0; i < podProbeFailureIterations; i++ {
		b.RecordProbeResult(dead, ProbeDead, "x")
	}
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeProbeDead)))
	for i := 0; i < podProbeFailureIterations; i++ {
		b.RecordProbeResult(dead, ProbeDead, "x") // renews the hour for a pod that is already out
	}
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeProbeDead)),
		"renewing a block is not a new exclusion")

	loaded := modelPod("3")
	b.SetLoadExcluded(loaded, true)
	b.SetLoadExcluded(loaded, true)
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeLoadOutlier)))

	flapped := modelPod("4")
	b.OnPodFlakyReAdd(flapped)
	b.OnPodFlakyReAdd(flapped)
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeFlakyReAdd)))

	// A flap on a pod a sweep has already blocked is not a new exclusion either.
	b.OnPodFlakyReAdd(dead)
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistExclusionsTotal, reason(excludeFlakyReAdd)))
}

func TestMetrics_RecoveriesAreCountedByReason(t *testing.T) {
	c := captureMetrics(t)
	var b PodBlacklist

	loaded := modelPod("1")
	b.SetLoadExcluded(loaded, true)
	b.SetLoadExcluded(loaded, false)
	b.SetLoadExcluded(loaded, false)
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistRecoveriesTotal, map[string]string{"reason": recoverLoadNormal}))

	dead := modelPod("2")
	for i := 0; i < podProbeFailureIterations; i++ {
		b.RecordProbeResult(dead, ProbeDead, "x")
	}
	for i := 0; i < podProbeRecoveryIterations; i++ {
		b.RecordProbeResult(dead, ProbeHealthy, "")
	}
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistRecoveriesTotal, map[string]string{"reason": recoverProbeHealthy}))
}

func TestMetrics_EverySweepProbeIsCountedByVerdict(t *testing.T) {
	c := captureMetrics(t)
	healthy, dead := modelPod("1"), modelPod("2")
	b := &PodBlacklist{HealthCheck: func(addr string) bool { return addr == "10.0.0.1:8000" }}

	b.sweepPodProbes(fakePods{"test-model": {healthy, dead}})

	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistProbeTotal, map[string]string{"verdict": "healthy"}))
	assert.Equal(t, 1, c.count(metrics.GatewayPodBlacklistProbeTotal, map[string]string{"verdict": "dead"}))
	assert.Zero(t, c.count(metrics.GatewayPodBlacklistProbeTotal, map[string]string{"verdict": "degraded"}))
}

func TestMetrics_ExcludedPodsGaugeIsPublishedAtTheEndOfEachSweep(t *testing.T) {
	c := captureMetrics(t)
	b := &PodBlacklist{HealthCheck: alwaysUnhealthy}
	pods := fakePods{"test-model": {modelPod("1"), modelPod("2"), modelPod("3")}}
	b.SetLoadExcluded(pods["test-model"][2], true)

	b.sweepPodProbes(pods)
	got, ok := c.last(metrics.GatewayPodBlacklistExcludedPods)
	require.True(t, ok, "the probe sweep publishes the gauge")
	assert.Equal(t, 1.0, got, "one pod is excluded, the others have failed one probe each")

	for i := 0; i < podProbeFailureIterations-1; i++ {
		b.sweepPodProbes(pods)
	}
	got, _ = c.last(metrics.GatewayPodBlacklistExcludedPods)
	assert.Equal(t, 3.0, got, "all three: the sweep found each dead, and one of them is load-excluded as well")

	lapseBlock(t, b, pods["test-model"][0])
	assert.Equal(t, int64(2), b.countExcludedNow(time.Now()), "a block that has ended is no longer an exclusion")

	b.sweepLoadOutliers(pods, loadTable{}.depth, nil)
	_, ok = c.last(metrics.GatewayPodBlacklistExcludedPods)
	assert.True(t, ok, "the load sweep publishes it too")
}
