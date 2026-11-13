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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const probeFailure = "GET /health failed"

// blacklistedFor is how much longer the pod's breaker stays open.
func blacklistedFor(t *testing.T, b *PodBlacklist, pod *v1.Pod) time.Duration {
	t.Helper()
	state := failureStateFor(t, b, pod)
	state.mu.Lock()
	defer state.mu.Unlock()
	return time.Until(state.openUntil)
}

func failDead(b *PodBlacklist, pod *v1.Pod, sweeps int) {
	for i := 0; i < sweeps; i++ {
		b.RecordProbeResult(pod, ProbeDead, probeFailure)
	}
}

func TestRecordProbeResult_DeadPodIsBlacklistedForAnHour(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")

	for i := 1; i < podProbeFailureIterations; i++ {
		assert.False(t, b.RecordProbeResult(pod, ProbeDead, probeFailure))
		assert.False(t, b.IsBlacklisted(pod), "sweep %d of %d must only be re-checked, not blacklisted", i, podProbeFailureIterations)
	}

	assert.True(t, b.RecordProbeResult(pod, ProbeDead, probeFailure))
	assert.True(t, b.IsBlacklisted(pod))
	assert.Contains(t, b.BlacklistReason(pod), probeFailure)
	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, 1, state.tripCount)
	assert.Equal(t, 0, state.probeFailures)
	assert.Equal(t, 0, state.probeDeadSweeps)
}

func TestRecordProbeResult_DegradedPodGetsTheOrdinaryBackoff(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")

	for i := 1; i < podProbeFailureIterations; i++ {
		assert.False(t, b.RecordProbeResult(pod, ProbeDegraded, "GET /v1/models did not list the model"))
	}
	assert.True(t, b.RecordProbeResult(pod, ProbeDegraded, "GET /v1/models did not list the model"))

	assert.True(t, b.IsBlacklisted(pod))
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5,
		"only a dead pod earns the hour; a degraded one may be transient")
}

func TestRecordProbeResult_APassingSweepRestartsTheCounts(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")

	failDead(&b, pod, podProbeFailureIterations-1)
	b.RecordProbeResult(pod, ProbeHealthy, "")
	failDead(&b, pod, podProbeFailureIterations-1)
	assert.False(t, b.IsBlacklisted(pod), "dead sweeps on either side of a passing sweep are not consecutive")

	assert.True(t, b.RecordProbeResult(pod, ProbeDead, probeFailure))
	assert.True(t, b.IsBlacklisted(pod))
}

// Failing sweeps in a row still trip the ordinary breaker, but a degraded one
// in the run means the pod is not known to be dead, so it earns only the
// ordinary backoff and not the hour.
func TestRecordProbeResult_ADegradedSweepBreaksTheRunOfDeadOnes(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")

	failDead(&b, pod, podProbeFailureIterations-1)
	b.RecordProbeResult(pod, ProbeDegraded, "GET /v1/models did not list the model")

	assert.True(t, b.IsBlacklisted(pod))
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5)
}

func TestRecordProbeResult_HealthyProbeDoesNotTrackAPod(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")

	b.RecordProbeResult(pod, ProbeHealthy, "")

	_, tracked := b.pods.Load(PodKey(pod))
	assert.False(t, tracked, "a pod that has never failed should not get a breaker entry from a passing probe")
}

// The stale-entry case: failing requests already have the breaker cycling
// through short open/half-open backoffs, so each cycle would hand the dead
// address another real request. The sweep must move it to the long blacklist.
func TestRecordProbeResult_DeadSweepsExtendAnAlreadyOpenPod(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	require.True(t, b.IsBlacklisted(pod))
	require.InDelta(t, podBlacklistBaseDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5)

	failDead(&b, pod, podProbeFailureIterations)

	assert.True(t, b.IsBlacklisted(pod))
	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5)
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, 1, state.tripCount, "the pod was already open, so this is not another trip")
}

func TestRecordProbeResult_DeadSweepsOverrideAHalfOpenProbe(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	b.RecordProbeResult(pod, ProbeDegraded, "x") // creates the entry
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.state = circuitHalfOpen
	state.probeDeadline = time.Now().Add(podBlacklistProbeTimeout)
	state.mu.Unlock()

	failDead(&b, pod, podProbeFailureIterations)

	assert.True(t, b.IsBlacklisted(pod))
	assert.False(t, b.Allow(pod), "the hour ends the half-open probe that was in flight")
}

// An excluded pod would otherwise say nothing while the sweep counts up to the
// hour, so it could not be told from a pod the sweep never probes.
func TestRecordProbeResult_LogsDeadSweepsOfAnAlreadyExcludedPod(t *testing.T) {
	openPod := func() (*PodBlacklist, *v1.Pod) {
		b := &PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := blacklistTestPod("1")
		tripBreaker(b, pod)
		return b, pod
	}
	halfOpenPod := func(t *testing.T) (*PodBlacklist, *v1.Pod) {
		b, pod := openPod()
		state := failureStateFor(t, b, pod)
		state.mu.Lock()
		state.state = circuitHalfOpen
		state.probeDeadline = time.Now().Add(podBlacklistProbeTimeout)
		state.mu.Unlock()
		return b, pod
	}
	const excludedLine = "excluded pod failed a background probe as dead"

	for name, setup := range map[string]func(*testing.T) (*PodBlacklist, *v1.Pod){
		"open":      func(*testing.T) (*PodBlacklist, *v1.Pod) { return openPod() },
		"half-open": halfOpenPod,
	} {
		t.Run(name+" pod logs each dead sweep short of the hour", func(t *testing.T) {
			logs := captureKlog(t)
			b, pod := setup(t)

			for i := 1; i < podProbeFailureIterations; i++ {
				b.RecordProbeResult(pod, ProbeDead, probeFailure)
				assert.Contains(t, logs(), fmt.Sprintf("deadSweeps=%d", i))
			}
			assert.Contains(t, logs(), excludedLine)
			assert.Contains(t, logs(), `pod="ns/1"`)
			assert.Contains(t, logs(), `reason="`+probeFailure+`"`)

			require.True(t, b.RecordProbeResult(pod, ProbeDead, probeFailure))
			assert.Contains(t, logs(), "blacklisted for 1h0m0s")
			assert.NotContains(t, logs(), fmt.Sprintf("deadSweeps=%d", podProbeFailureIterations),
				"the sweep that reaches the threshold logs the blacklisting, not another count")
		})
	}

	t.Run("degraded and healthy sweeps stay silent", func(t *testing.T) {
		logs := captureKlog(t)
		b, pod := openPod()

		b.RecordProbeResult(pod, ProbeDegraded, "GET /v1/models did not list the model")
		b.RecordProbeResult(pod, ProbeHealthy, "")

		assert.NotContains(t, logs(), "background probe")
	})

	t.Run("a closed pod keeps its own line", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist

		b.RecordProbeResult(blacklistTestPod("1"), ProbeDead, probeFailure)

		assert.Contains(t, logs(), "pod failed a background probe; it will be re-checked")
		assert.NotContains(t, logs(), excludedLine)
	})
}

func TestRecordProbeResult_NeverShortensALongerBlacklisting(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	b.RecordProbeResult(pod, ProbeDegraded, "x")
	state := failureStateFor(t, &b, pod)
	longer := time.Now().Add(3 * time.Hour)
	state.mu.Lock()
	state.state = circuitOpen
	state.openUntil = longer
	state.mu.Unlock()

	failDead(&b, pod, podProbeFailureIterations)

	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, longer, state.openUntil)
}

func TestRecordProbeResult_DeadPodStaysOutWhileItStaysDead(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	failDead(&b, pod, podProbeFailureIterations)
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(20 * time.Minute) // most of the hour has passed
	state.mu.Unlock()

	failDead(&b, pod, podProbeFailureIterations)

	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, &b, pod).Seconds(), 5,
		"another run of dead sweeps renews the hour")
}

// When the hour ends the ordinary half-open path takes over, so a pod that
// recovered is let back in by its first real request.
func TestRecordProbeResult_AfterTheHourTheHalfOpenPathTakesOver(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	failDead(&b, pod, podProbeFailureIterations)
	assert.False(t, b.Allow(pod))

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "the hour is over: the pod is a candidate again")
	require.True(t, b.ClaimProbe(pod), "the next request routed to it is the half-open probe")
	state.mu.Lock()
	assert.Equal(t, circuitHalfOpen, state.state)
	state.mu.Unlock()
	b.RecordResult(pod, "10.0.0.1:8000", 200)
	state.mu.Lock()
	assert.Equal(t, circuitClosed, state.state, "the probe's success closes the breaker")
	state.mu.Unlock()
	assert.False(t, b.IsBlacklisted(pod))
}

func TestRecordProbeResult_InFlightRequestResultsDoNotEndTheHour(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	failDead(&b, pod, podProbeFailureIterations)

	b.RecordResult(pod, "10.0.0.1:8000", 200)

	assert.True(t, b.IsBlacklisted(pod), "results of requests already in flight are ignored while the pod is open")
}

// fakePods is a PodSource over a fixed model -> pods map.
type fakePods map[string][]*v1.Pod

func (f fakePods) ListModels() []string {
	models := make([]string, 0, len(f))
	for model := range f {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (f fakePods) ListPodsByModel(model string) (types.PodList, error) {
	return &utils.PodArray{Pods: f[model]}, nil
}

func newProbeSweep(healthCheck func(string) bool, identityCheck func(string, string) bool, podsByModel map[string][]*v1.Pod) (*PodBlacklist, PodSource) {
	return &PodBlacklist{HealthCheck: healthCheck, IdentityCheck: identityCheck}, fakePods(podsByModel)
}

func podIPIs(ip string) func(string) bool {
	return func(addr string) bool { return strings.HasPrefix(addr, ip+":") }
}

func TestSweepPodProbes_BlacklistsOnlyThePodThatKeepsFailing(t *testing.T) {
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	var pod2Probes atomic.Int32
	bl, src := newProbeSweep(func(addr string) bool {
		if podIPIs("10.0.0.2")(addr) {
			pod2Probes.Add(1)
			return false
		}
		return true
	}, nil, map[string][]*v1.Pod{"test-model": {pod1, pod2, pod3}})

	for i := 1; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
		assert.False(t, bl.IsBlacklisted(pod2), "sweep %d of %d: still only being re-checked", i, podProbeFailureIterations)
	}

	bl.sweepPodProbes(src)
	assert.True(t, bl.IsBlacklisted(pod2), "pod2 failed %d sweeps in a row", podProbeFailureIterations)
	assert.False(t, bl.IsBlacklisted(pod1))
	assert.False(t, bl.IsBlacklisted(pod3))
	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, bl, pod2).Seconds(), 5, "/health not working is dead")
	assert.Equal(t, int32(podProbeFailureIterations), pod2Probes.Load())

	bl.sweepPodProbes(src)
	assert.Equal(t, int32(podProbeFailureIterations+1), pod2Probes.Load(),
		"a blacklisted pod keeps being probed, which is what renews the hour while it stays dead")
}

func TestSweepPodProbes_RecoveredPodIsNotBlacklisted(t *testing.T) {
	pod := blacklistTestPod("1")
	var healthy atomic.Bool
	bl, src := newProbeSweep(func(string) bool { return healthy.Load() }, nil,
		map[string][]*v1.Pod{"test-model": {pod}})

	for i := 1; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
	}
	healthy.Store(true)
	bl.sweepPodProbes(src)
	healthy.Store(false)
	for i := 1; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
	}

	assert.False(t, bl.IsBlacklisted(pod), "a pod that recovers partway through is not blacklisted by earlier failed sweeps")
}

// A model list that does not name the model is not proof of a dead process: it
// may be a reload in progress. The hook answers only yes or no, which is the
// degraded case.
func TestSweepPodProbes_IdentityCheckAppliesToVLLMAndSGLangOnly(t *testing.T) {
	vllmPod := blacklistTestEnginePod("1", "vllm")
	sglangPod := blacklistTestEnginePod("2", "sglang")
	otherPod := blacklistTestEnginePod("3", "xdit")
	var identityModel atomic.Value
	bl, src := newProbeSweep(alwaysHealthy, func(_, model string) bool {
		identityModel.Store(model)
		return false
	}, map[string][]*v1.Pod{"test-model": {vllmPod, sglangPod, otherPod}})

	for i := 0; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
	}

	assert.True(t, bl.IsBlacklisted(vllmPod))
	assert.True(t, bl.IsBlacklisted(sglangPod))
	assert.False(t, bl.IsBlacklisted(otherPod), "an engine that is not known to list models is probed on /health only")
	assert.Equal(t, "test-model", identityModel.Load())
	assert.Contains(t, bl.BlacklistReason(vllmPod), "GET "+podModelsCheckPath)
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), blacklistedFor(t, bl, vllmPod).Seconds(), 5)
}

func TestSweepPodProbes_ProbesAPodOnceEvenWhenItServesSeveralModels(t *testing.T) {
	pod := blacklistTestPod("1")
	var probes atomic.Int32
	bl, src := newProbeSweep(func(string) bool {
		probes.Add(1)
		return true
	}, nil, map[string][]*v1.Pod{"base-model": {pod}, "lora-adapter": {pod}})

	bl.sweepPodProbes(src)

	assert.Equal(t, int32(1), probes.Load())
}

func TestIsPDRolePod_NilIsNotAPDPod(t *testing.T) {
	assert.False(t, isPDRolePod(nil))
}

func TestSweepPodProbes_SkipsPrefillAndDecodePods(t *testing.T) {
	rolePod := func(name, role string) *v1.Pod {
		pod := blacklistTestPod(name)
		pod.Labels = map[string]string{pd.RoleLabel: role}
		return pod
	}
	prefill, decode := rolePod("1", pd.RolePrefill), rolePod("2", pd.RoleDecode)
	encode, plain := rolePod("3", "encode"), blacklistTestPod("4")

	var mu sync.Mutex
	probed := map[string]int{}
	bl, src := newProbeSweep(func(addr string) bool {
		mu.Lock()
		defer mu.Unlock()
		probed[addr]++
		return false // every pod that is probed fails
	}, nil, map[string][]*v1.Pod{"test-model": {prefill, decode, encode, plain}})

	for i := 0; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
	}

	assert.Zero(t, probed["10.0.0.1:8000"], "a prefill pod is not probed")
	assert.Zero(t, probed["10.0.0.2:8000"], "a decode pod is not probed")
	assert.Equal(t, podProbeFailureIterations, probed["10.0.0.3:8000"], "any other role is still probed")
	assert.Equal(t, podProbeFailureIterations, probed["10.0.0.4:8000"], "a pod with no role is still probed")
	assert.False(t, bl.IsBlacklisted(prefill))
	assert.False(t, bl.IsBlacklisted(decode))
	assert.True(t, bl.IsBlacklisted(encode))
	assert.True(t, bl.IsBlacklisted(plain))
}

// probeTestPod is a ready pod at the address of an httptest server, labelled
// with its engine and serving port.
func probeTestPod(t *testing.T, name, engine, hostport string) *v1.Pod {
	t.Helper()
	host, port, err := net.SplitHostPort(hostport)
	require.NoError(t, err)
	pod := blacklistTestEnginePod(name, engine)
	pod.Status.PodIP = host
	pod.Labels[constants.ModelLabelPort] = port
	return pod
}

// The real probes end to end (no hooks): a recycled address whose new owner
// answers every GET with an empty 200, a listener that is gone, an engine that
// is up but no longer lists the model, and a healthy engine.
func TestSweepPodProbes_RealProbesTellDeadFromDegradedFromHealthy(t *testing.T) {
	const model = "test-model"

	imposter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}))
	defer imposter.Close()

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	goneAddr := strings.TrimPrefix(gone.URL, "http://")
	gone.Close()

	engineServing := func(models string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == podModelsCheckPath {
				_, _ = w.Write([]byte(models))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
	}
	healthy := engineServing(`{"object":"list","data":[{"id":"test-model"}]}`)
	defer healthy.Close()
	reloading := engineServing(`{"object":"list","data":[{"id":"other-model"}]}`)
	defer reloading.Close()

	imposterPod := probeTestPod(t, "imposter", "vllm", strings.TrimPrefix(imposter.URL, "http://"))
	gonePod := probeTestPod(t, "gone", "vllm", goneAddr)
	healthyPod := probeTestPod(t, "healthy", "vllm", strings.TrimPrefix(healthy.URL, "http://"))
	reloadingPod := probeTestPod(t, "reloading", "vllm", strings.TrimPrefix(reloading.URL, "http://"))

	var bl PodBlacklist
	src := fakePods{model: {imposterPod, gonePod, healthyPod, reloadingPod}}
	for i := 0; i < podProbeFailureIterations; i++ {
		bl.sweepPodProbes(src)
	}

	assert.True(t, bl.IsBlacklisted(imposterPod), "an empty 2xx /v1/models is another process")
	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, &bl, imposterPod).Seconds(), 5)
	assert.Contains(t, bl.BlacklistReason(imposterPod), "empty or non-JSON")

	assert.True(t, bl.IsBlacklisted(gonePod), "nothing listens on the address any more")
	assert.InDelta(t, podProbeDeadBlacklistDuration.Seconds(), blacklistedFor(t, &bl, gonePod).Seconds(), 5)

	assert.True(t, bl.IsBlacklisted(reloadingPod), "up, but not listing the model")
	assert.InDelta(t, podBlacklistBaseDuration.Seconds(), blacklistedFor(t, &bl, reloadingPod).Seconds(), 5,
		"degraded is not dead: only the ordinary backoff")

	assert.False(t, bl.IsBlacklisted(healthyPod))
}

// The sweep has no request to take the address from: the log line names the
// address and model the pod is probed on.
func TestRecordProbeResult_LogLinesNameTheAddressAndModel(t *testing.T) {
	logs := captureKlog(t)
	var b PodBlacklist
	pod := blacklistTestEnginePod("1", "vllm")
	pod.Labels[constants.ModelLabelName] = "test-model"
	pod.Labels[constants.ModelLabelPort] = "8000"

	for i := 0; i < podProbeFailureIterations-1; i++ {
		b.RecordProbeResult(pod, ProbeDead, probeFailure)
	}
	assert.Regexp(t, `addr="10\.0\.0\.1:8000" model="test-model"`, logs(), "the not-yet-blacklisted line")

	require.True(t, b.RecordProbeResult(pod, ProbeDead, probeFailure))
	assert.Contains(t, logs(), "pod ns/1 (addr=10.0.0.1:8000, model=test-model) blacklisted for 1h0m0s: background probe found it dead")
}

// Only the sweep's one-hour block quarantines a pod (the cache stops scraping
// it); every other kind of exclusion leaves its metrics alone.
func TestIsQuarantined_OnlyTheDeadPodsHourCounts(t *testing.T) {
	t.Run("dead pod is quarantined for the hour, not before", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")

		failDead(&b, pod, podProbeFailureIterations-1)
		assert.False(t, b.IsQuarantined(pod), "still only being re-checked")

		failDead(&b, pod, 1)
		assert.True(t, b.IsQuarantined(pod))
	})

	t.Run("the hour ends the quarantine", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		failDead(&b, pod, podProbeFailureIterations)
		require.True(t, b.IsQuarantined(pod))

		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.quarantinedUntil = time.Now().Add(-time.Second)
		state.openUntil = time.Now().Add(-time.Second)
		state.mu.Unlock()

		assert.False(t, b.IsQuarantined(pod), "after the hour the pod is back on the half-open path")
	})

	t.Run("a healthy sweep does not end it", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		failDead(&b, pod, podProbeFailureIterations)

		b.RecordProbeResult(pod, ProbeHealthy, "")

		assert.True(t, b.IsQuarantined(pod))
	})

	t.Run("ordinary exclusions are not quarantine", func(t *testing.T) {
		pod := blacklistTestPod("1")

		degraded := PodBlacklist{}
		for i := 0; i < podProbeFailureIterations; i++ {
			degraded.RecordProbeResult(pod, ProbeDegraded, "x")
		}
		require.True(t, degraded.IsBlacklisted(pod))
		assert.False(t, degraded.IsQuarantined(pod), "a degraded pod only gets the ordinary backoff")

		requests := PodBlacklist{HealthCheck: alwaysUnhealthy}
		tripBreaker(&requests, pod)
		require.True(t, requests.IsBlacklisted(pod))
		assert.False(t, requests.IsQuarantined(pod), "a request-driven trip is not quarantine")

		load := PodBlacklist{}
		load.SetLoadExcluded(pod, true)
		assert.False(t, load.IsQuarantined(pod), "a load exclusion is not quarantine")

		flaky := PodBlacklist{}
		flaky.OnPodFlakyReAdd(pod)
		assert.False(t, flaky.IsQuarantined(pod), "a flaky re-add exclusion is not quarantine")
	})

	t.Run("unknown and nil pods", func(t *testing.T) {
		var b PodBlacklist
		assert.False(t, b.IsQuarantined(blacklistTestPod("1")))
		assert.False(t, b.IsQuarantined(nil))
	})
}

// The stale-entry sweep must not cut the hour short: a pod that recovers while
// blacklisted is only passed healthy probes, which do not refresh lastUpdated.
func TestSweepStale_KeepsABlockedPodForItsWholeHour(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	failDead(&b, pod, podProbeFailureIterations)
	b.RecordProbeResult(pod, ProbeHealthy, "") // it has recovered; nothing touches the entry from here

	now := time.Now()
	b.sweepStale(now.Add(podBlacklistEntryTTL + time.Minute))
	assert.True(t, b.IsBlacklisted(pod), "31 minutes into the hour")
	assert.True(t, b.IsQuarantined(pod))

	b.sweepStale(now.Add(podProbeDeadBlacklistDuration - time.Minute))
	_, kept := b.pods.Load(PodKey(pod))
	assert.True(t, kept, "still inside the hour")

	// Once the hour is over the entry ages out like any other untouched one.
	b.sweepStale(now.Add(podProbeDeadBlacklistDuration + podBlacklistEntryTTL + time.Minute))
	_, kept = b.pods.Load(PodKey(pod))
	assert.False(t, kept, "an entry that is well past its block is pruned")
}

// A pod that answers /health correctly but slowly is healthy, not dead. The
// fleet's sglang pods take 1.4-1.7 s to answer /health, which the sweep once
// judged dead (its wait was the 1 s a tripping request could afford), so it
// blacklisted them for an hour, over and over. This one is slower than a
// tripping request will wait but well inside what the sweep does.
func TestSweepPodProbes_ASlowButHealthyEngineIsNotDead(t *testing.T) {
	const model = "test-model"
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == podModelsCheckPath {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model"}]}`))
			return
		}
		select {
		case <-time.After(podHealthCheckTimeout + 300*time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	addr := strings.TrimPrefix(slow.URL, "http://")
	pod := probeTestPod(t, "slow", "sglang", addr)

	var bl PodBlacklist
	bl.sweepPodProbes(fakePods{model: {pod}})

	_, tracked := bl.pods.Load(PodKey(pod))
	assert.False(t, tracked, "a healthy probe leaves no failure behind")
	assert.False(t, bl.IsBlacklisted(pod))

	// The same pod does fail within the wait a tripping request can afford; the
	// sweep has its own, longer one because nothing waits on it. (A short wait
	// here keeps the test quick: it only needs to be shorter than the pod.)
	result, failure := bl.probe(addr, model, "sglang", 100*time.Millisecond)
	assert.Equal(t, ProbeDead, result)
	assert.Contains(t, failure, "/health")
	assert.Greater(t, podProbeTimeout, podHealthCheckTimeout)
}
