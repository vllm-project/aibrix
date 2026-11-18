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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// Tests for the request-failure source: RecordResponse blocks a pod after
// requestFailureThreshold upstream failures in a row.

// withRequestFailureThreshold sets the threshold for the test and restores it.
func withRequestFailureThreshold(t *testing.T, n int) {
	t.Helper()
	prev := requestFailureThreshold
	requestFailureThreshold = n
	t.Cleanup(func() { requestFailureThreshold = prev })
}

// failTimes records n responses with status and returns how many of them blocked the pod.
func failTimes(b *PodBlacklist, pod *v1.Pod, status, n int) (blocks int) {
	for i := 0; i < n; i++ {
		if b.RecordResponse(pod, status) {
			blocks++
		}
	}
	return blocks
}

// endRequestBlock fast-forwards past the end of pod's request-failure block without sleeping.
func endRequestBlock(t *testing.T, b *PodBlacklist, pod *v1.Pod) *podFailureState {
	t.Helper()
	state := failureStateFor(t, b, pod)
	state.mu.Lock()
	state.requestBlockedUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	return state
}

func TestIsPodFailureStatus(t *testing.T) {
	failing := []int{500, 502, 503, 504, 507, 599}
	for _, code := range failing {
		assert.True(t, IsPodFailureStatus(code), "%d is the pod's, or Envoy's on its behalf", code)
	}
	// A 4xx is the request's fault and a 429 is load every replica shares; a 501 is the engine
	// not implementing a route, for every request. None is a sign the pod is unhealthy.
	passing := []int{100, 200, 204, 206, 301, 304, 400, 401, 403, 404, 408, 409, 413, 422, 429, 499, 501}
	for _, code := range passing {
		assert.False(t, IsPodFailureStatus(code), "%d does not count against the pod", code)
	}
}

func TestRecordResponse_BlocksAfterThresholdConsecutiveFailures(t *testing.T) {
	withRequestFailureThreshold(t, 5)
	var b PodBlacklist
	pod, other := blacklistTestPod("1"), blacklistTestPod("2")
	pods := []*v1.Pod{pod, other}

	assert.Zero(t, failTimes(&b, pod, http.StatusServiceUnavailable, 4))
	assert.True(t, b.Allow(pod), "one failure short of the threshold")
	assert.Equal(t, pods, b.FilterBlacklisted(pods))

	assert.True(t, b.RecordResponse(pod, http.StatusBadGateway), "the fifth failure in a row blocks it")
	assert.False(t, b.Allow(pod))
	assert.True(t, b.IsBlacklisted(pod))
	assert.True(t, b.Allow(other), "only the pod that failed is excluded")
	assert.Equal(t, []*v1.Pod{other}, b.FilterBlacklisted(pods))
	assert.EqualValues(t, 1, b.excluded.Load())
	assert.Contains(t, b.BlacklistReason(pod), "5 upstream failures in a row, last status 502")
	assert.Contains(t, b.BlacklistReason(pod), "request trip #1")

	state := failureStateFor(t, &b, pod)
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), time.Until(state.requestBlockedUntil).Seconds(), 2,
		"the first block lasts the base backoff")
	assert.False(t, b.IsQuarantined(pod), "a pod that answers is not quarantined: the cache keeps scraping it")
	assert.Equal(t, circuitClosed, state.state, "the request source does not touch the probe breaker")
}

// Envoy-generated failures reach the gateway as ordinary statuses: a connect failure or reset
// is a 503, a timeout a 504. They block like the engine's own 500.
func TestRecordResponse_AnyAttributableStatusCountsTowardsTheSameStreak(t *testing.T) {
	withRequestFailureThreshold(t, 4)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	for _, code := range []int{500, 502, 503} {
		assert.False(t, b.RecordResponse(pod, code))
	}
	assert.True(t, b.RecordResponse(pod, 504))
	assert.False(t, b.Allow(pod))
}

func TestRecordResponse_AnyOtherResponseStartsTheStreakOver(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	for _, reset := range []int{200, 204, 400, 404, 429, 501} {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		for round := 0; round < 10; round++ {
			assert.Zero(t, failTimes(&b, pod, 503, 2), "status %d between failures", reset)
			assert.False(t, b.RecordResponse(pod, reset))
		}
		assert.True(t, b.Allow(pod), "failures that never reach the threshold in a row do not block (reset by %d)", reset)
		assert.Zero(t, b.excluded.Load())
	}
}

// A client error, a rate limit and a not-implemented route say nothing about the pod's health, so
// however many arrive they neither block it nor even start an entry.
func TestRecordResponse_NonAttributableStatusesNeverBlock(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	for i := 0; i < 100; i++ {
		for _, code := range []int{400, 404, 408, 413, 422, 429, 501} {
			assert.False(t, b.RecordResponse(pod, code))
		}
	}
	assert.True(t, b.Allow(pod))
	assert.Zero(t, b.tracked.Load(), "no failure, no entry")
}

// The healthy-fleet fast path: while nothing is tracked a success returns at once, so it
// neither allocates nor creates an entry, and FilterBlacklisted stays on its no-lookup path.
// Once some pod has an entry a success costs a lookup of its key, one small string, and still
// creates nothing.
func TestRecordResponse_SuccessCreatesNoEntryAndAllocatesNothing(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	var b PodBlacklist
	pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2")}
	for i := 0; i < 50; i++ {
		for _, pod := range pods {
			b.RecordResponse(pod, 200)
		}
	}
	assert.Zero(t, b.tracked.Load())
	assert.Equal(t, pods, b.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() {
		b.RecordResponse(pods[0], 200)
		_ = b.FilterBlacklisted(pods)
	}))

	// An entry that exists for another reason, with nothing to start over: the key lookup is
	// the whole cost, and the other pod's successes make no entry.
	b.RecordProbeResult(pods[0], ProbeDegraded, "x")
	for i := 0; i < 50; i++ {
		b.RecordResponse(pods[1], 200)
		b.RecordResponse(pods[0], 200)
	}
	assert.EqualValues(t, 1, b.tracked.Load())
	assert.LessOrEqual(t, testing.AllocsPerRun(100, func() { b.RecordResponse(pods[0], 200) }), 1.0)
}

func TestRecordResponse_FailuresWhileBlockedDoNotStack(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.Equal(t, 1, failTimes(&b, pod, 503, 3))
	state := failureStateFor(t, &b, pod)
	until := state.requestBlockedUntil

	// Requests that were in flight when the block began keep failing. They are one incident.
	assert.Zero(t, failTimes(&b, pod, 503, 50))

	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, 1, state.requestTrips, "in-flight failures do not escalate the backoff")
	assert.Equal(t, until, state.requestBlockedUntil, "or extend the block")
	assert.Zero(t, state.requestFailures)
}

// Successes that arrive while the pod is blocked are in-flight requests too: they must not
// count towards ending a probation that has not begun.
func TestRecordResponse_SuccessesWhileBlockedDoNotCountTowardsRecovery(t *testing.T) {
	withRequestFailureThreshold(t, 2)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.Equal(t, 1, failTimes(&b, pod, 503, 2))
	for i := 0; i < 5*podRequestRecoverySuccesses; i++ {
		b.RecordResponse(pod, 200)
	}
	state := failureStateFor(t, &b, pod)
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, 1, state.requestTrips)
	assert.Zero(t, state.requestSuccesses)
}

func TestRecordResponse_BlockEndsOnItsDeadlineAndPodIsOnProbation(t *testing.T) {
	withRequestFailureThreshold(t, 5)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.Equal(t, 1, failTimes(&b, pod, 503, 5))
	require.False(t, b.Allow(pod))

	endRequestBlock(t, &b, pod)
	assert.True(t, b.Allow(pod), "no probe request: the pod is routable again when the block ends")
	assert.False(t, b.IsBlacklisted(pod))
	assert.Empty(t, b.BlacklistReason(pod))

	// Probation: one failure, not five, blocks it again, and with the next backoff.
	assert.True(t, b.RecordResponse(pod, 503))
	assert.False(t, b.Allow(pod))
	state := failureStateFor(t, &b, pod)
	assert.Equal(t, 2, state.requestTrips)
	assert.InDelta(t, (2 * podBlacklistBaseDuration).Seconds(), time.Until(state.requestBlockedUntil).Seconds(), 2)
	assert.Contains(t, b.BlacklistReason(pod), "request trip #2")
}

func TestRecordResponse_BackoffDoublesAndIsCapped(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	want := []time.Duration{1 * time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, d := range want {
		require.True(t, b.RecordResponse(pod, 500), "trip %d", i+1)
		state := failureStateFor(t, &b, pod)
		state.mu.RLock()
		assert.InDelta(t, d.Seconds(), time.Until(state.requestBlockedUntil).Seconds(), 2, "trip %d", i+1)
		state.mu.RUnlock()
		endRequestBlock(t, &b, pod)
	}
}

func TestRecordResponse_ResponsesInARowEndTheProbation(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.Equal(t, 1, failTimes(&b, pod, 503, 3))
	endRequestBlock(t, &b, pod)

	for i := 0; i < podRequestRecoverySuccesses-1; i++ {
		b.RecordResponse(pod, 200)
	}
	assert.True(t, b.RecordResponse(pod, 503), "still on probation one short of the recovery count")

	// Now recover fully: the failures reset the count, so it takes the whole run again.
	endRequestBlock(t, &b, pod)
	for i := 0; i < podRequestRecoverySuccesses; i++ {
		b.RecordResponse(pod, 200)
	}
	state := failureStateFor(t, &b, pod)
	state.mu.RLock()
	assert.Zero(t, state.requestTrips, "a recovered pod is not punished for its earlier blocks")
	state.mu.RUnlock()

	assert.Zero(t, failTimes(&b, pod, 503, 2), "off probation it takes the full threshold again")
	assert.True(t, b.RecordResponse(pod, 503))
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, 1, state.requestTrips)
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), time.Until(state.requestBlockedUntil).Seconds(), 2,
		"and the backoff starts over")
}

// A pod that passes /health and fails inference is what this source exists for. The probe
// sweep calls it healthy, which must neither lift the block nor shorten the backoff.
func TestRecordResponse_HealthyProbeSweepsDoNotLiftTheBlockOrResetTheBackoff(t *testing.T) {
	withRequestFailureThreshold(t, 2)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.Equal(t, 1, failTimes(&b, pod, 500, 2))

	for i := 0; i < 5*podProbeRecoveryIterations; i++ {
		b.RecordProbeResult(pod, ProbeHealthy, "")
	}
	assert.False(t, b.Allow(pod), "probes that pass do not lift a block requests put on the pod")

	endRequestBlock(t, &b, pod)
	b.RecordProbeResult(pod, ProbeHealthy, "") // a healthy sweep after the block resets the probe backoff
	assert.True(t, b.RecordResponse(pod, 500))
	assert.Equal(t, 2, failureStateFor(t, &b, pod).requestTrips, "but not the request backoff")
}

// The sources are independent: a request block does not disturb the probe breaker, and the
// probe breaker's trip count is not the request source's.
func TestRecordResponse_IsIndependentOfTheProbeBreaker(t *testing.T) {
	withRequestFailureThreshold(t, 2)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	state := failureStateFor(t, &b, pod)
	require.False(t, b.Allow(pod))
	tripsBefore := state.tripCount

	assert.Zero(t, failTimes(&b, pod, 503, 20), "responses to a pod already excluded by another source are ignored")
	state.mu.RLock()
	assert.Zero(t, state.requestTrips)
	assert.Zero(t, state.requestFailures)
	state.mu.RUnlock()

	lapseBlock(t, &b, pod)
	require.True(t, b.Allow(pod))
	assert.Equal(t, 1, failTimes(&b, pod, 503, 2))
	state.mu.RLock()
	defer state.mu.RUnlock()
	assert.Equal(t, tripsBefore, state.tripCount, "the probe backoff is not escalated by requests")
	assert.Equal(t, 1, state.requestTrips)
}

func TestRecordResponse_ThresholdZeroTurnsTheSourceOff(t *testing.T) {
	withRequestFailureThreshold(t, 0)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	assert.Zero(t, failTimes(&b, pod, 503, 100))
	assert.True(t, b.Allow(pod))
	assert.Zero(t, b.tracked.Load(), "off means off: no entry either")

	// The other sources are unaffected.
	tripBreaker(&b, pod)
	assert.False(t, b.Allow(pod))
}

func TestRecordResponse_DisabledAndNilAreNoOps(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	var b PodBlacklist
	pod := blacklistTestPod("1")

	assert.False(t, b.RecordResponse(nil, 503))

	prev := Enabled
	Enabled = false
	defer func() { Enabled = prev }()
	assert.False(t, b.RecordResponse(pod, 503))
	assert.Zero(t, b.tracked.Load())
	Enabled = true
	assert.True(t, b.RecordResponse(pod, 503), "and it works once the switch is on")
}

func TestRecordResponse_IgnoredPodsAreNeverRecorded(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	var b PodBlacklist
	for _, kind := range ignoredPodKinds {
		pod := kind.pod("1")
		assert.False(t, b.RecordResponse(pod, 503), kind.name)
		assert.True(t, b.Allow(pod), kind.name)
	}
	assert.Zero(t, b.tracked.Load(), "an ignored pod gets no entry")

	control := blacklistTestPod("control")
	assert.True(t, b.RecordResponse(control, 503), "a plain pod, same threshold, is blocked")
}

func TestRecordResponse_EntryIsKeptAliveByTheBlockAndPrunedAfterwards(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.True(t, b.RecordResponse(pod, 503))
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.lastUpdated = time.Now().Add(-2 * podBlacklistEntryTTL) // ancient, but the block is running
	state.mu.Unlock()

	b.sweepStale(time.Now())
	assert.EqualValues(t, 1, b.tracked.Load(), "a running block is never pruned however quiet the pod is")
	assert.False(t, b.Allow(pod))

	// The block ends; the prune closes it so it stops counting as excluded, then drops the entry.
	endRequestBlock(t, &b, pod)
	b.sweepStale(time.Now())
	assert.Zero(t, b.excluded.Load(), "an ended block stops keeping requests off the fast path")
	assert.EqualValues(t, 1, b.tracked.Load(), "the entry carries the probation, so it stays for the TTL")

	b.sweepStale(time.Now().Add(podBlacklistEntryTTL + time.Minute))
	assert.Zero(t, b.tracked.Load())
}

func TestRecordResponse_EndedBlockStopsCountingOnTheNextResponse(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	require.True(t, b.RecordResponse(pod, 503))
	require.EqualValues(t, 1, b.excluded.Load())

	endRequestBlock(t, &b, pod)
	assert.EqualValues(t, 1, b.excluded.Load(), "counted until something closes it: it can only over-count")
	b.RecordResponse(pod, 200)
	assert.Zero(t, b.excluded.Load())
}

func TestRecordResponse_ExcludedCountAndGaugeTrackRequestBlocks(t *testing.T) {
	withRequestFailureThreshold(t, 1)
	captured := captureMetrics(t)
	var b PodBlacklist
	pod := modelPod("1")
	require.True(t, b.RecordResponse(pod, 503))

	assert.EqualValues(t, 1, b.countExcludedNow(time.Now()))
	b.publishExcludedPods()
	got, ok := captured.last(metrics.GatewayPodBlacklistExcludedPods)
	require.True(t, ok)
	assert.Equal(t, 1.0, got)
}

func TestRecordResponse_CountsTheExclusionOnceByReason(t *testing.T) {
	withRequestFailureThreshold(t, 3)
	captured := captureMetrics(t)
	var b PodBlacklist
	pod := modelPod("1")

	failTimes(&b, pod, 503, 40) // one block; the rest are in flight and ignored
	assert.Equal(t, 1, captured.count(metrics.GatewayPodBlacklistExclusionsTotal, map[string]string{"reason": excludeRequestFailures}))
	assert.Zero(t, captured.count(metrics.GatewayPodBlacklistExclusionsTotal, map[string]string{"reason": excludeProbeDead}))

	endRequestBlock(t, &b, pod)
	b.RecordResponse(pod, 503) // probation: a second block, counted
	assert.Equal(t, 2, captured.count(metrics.GatewayPodBlacklistExclusionsTotal, map[string]string{"reason": excludeRequestFailures}))
}

// Many requests finish at once on a busy pod. However they interleave, one incident is one block.
func TestRecordResponse_ConcurrentFailuresMakeOneBlock(t *testing.T) {
	withRequestFailureThreshold(t, 5)
	var b PodBlacklist
	pod := blacklistTestPod("1")

	var wg sync.WaitGroup
	var mu sync.Mutex
	blocks := 0
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if b.RecordResponse(pod, 503) {
					mu.Lock()
					blocks++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, blocks)
	assert.Equal(t, 1, failureStateFor(t, &b, pod).requestTrips)
	assert.False(t, b.Allow(pod))
	assert.EqualValues(t, 1, b.excluded.Load())
}

// Failures, successes, reads and the prune sweep from many goroutines at once, for the race
// detector, with the exact excluded count checked against a recount at the end.
func TestRecordResponse_RaceWithReadersAndTheSweep(t *testing.T) {
	withRequestFailureThreshold(t, 2)
	var b PodBlacklist
	pods := make([]*v1.Pod, 8)
	for i := range pods {
		pods[i] = blacklistTestPod(string(rune('a' + i)))
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				pod := pods[(g+i)%len(pods)]
				switch i % 4 {
				case 0, 1:
					b.RecordResponse(pod, 503)
				case 2:
					b.RecordResponse(pod, 200)
				default:
					_ = b.FilterBlacklisted(pods)
					_ = b.BlacklistReason(pod)
				}
				if i%50 == 0 {
					b.sweepStale(time.Now())
				}
			}
		}(g)
	}
	wg.Wait()

	var counted int64
	b.pods.Range(func(_, v any) bool {
		state := v.(*podFailureState)
		state.mu.RLock()
		if state.excludingLocked() {
			counted++
		}
		state.mu.RUnlock()
		return true
	})
	assert.Equal(t, counted, b.excluded.Load(), "excluded matches a recount of the entries")
}
