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
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func blacklistTestPod(name string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: v1.PodStatus{
			PodIP:      "10.0.0." + name,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// blacklistTestEnginePod is blacklistTestPod carrying the engine label the
// blacklist reads to decide whether the /v1/models identity check applies.
func blacklistTestEnginePod(name, engine string) *v1.Pod {
	pod := blacklistTestPod(name)
	pod.Labels = map[string]string{constants.ModelLabelEngine: engine}
	return pod
}

// podAddr returns a fake "ip:port" for pod, matching what
// types.RoutingContext.TargetAddress() would hand RecordResult in
// production. Its value is never actually dialed in these tests: every test
// that trips the breaker stubs PodBlacklist.HealthCheck so the confirmation
// step never makes a real network call.
func podAddr(pod *v1.Pod) string {
	return pod.Status.PodIP + ":8000"
}

// alwaysUnhealthy/alwaysHealthy stub PodBlacklist.HealthCheck so tests never
// make a real network call: the confirmation step that gates an actual trip
// (see RecordResult) always agrees with — respectively contradicts — the
// tracked-failure-status signal.
func alwaysUnhealthy(string) bool { return false }
func alwaysHealthy(string) bool   { return true }

func alwaysModelGone(string, string) bool   { return false }
func alwaysModelServed(string, string) bool { return true }

func TestPodBlacklist_TripsAfterConsecutive400s(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 400)
		assert.False(t, b.IsBlacklisted(pod), "should not blacklist before threshold is reached (i=%d)", i)
	}

	b.RecordResult(pod, podAddr(pod), 400)
	assert.True(t, b.IsBlacklisted(pod), "should blacklist once the threshold is reached and /health also fails")
}

// TestPodBlacklist_SuccessInBetweenDoesNotClearTheWindow pins down a
// deliberate policy change from the old consecutive-failure breaker: the
// window counts failures within podBlacklistFailureWindow regardless of any
// successes interleaved, so a pod that's merely flaky (some requests fine,
// most failing) still trips instead of perpetually resetting to zero.
func TestPodBlacklist_SuccessInBetweenDoesNotClearTheWindow(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 400)
	}
	b.RecordResult(pod, podAddr(pod), 200)
	assert.False(t, b.IsBlacklisted(pod), "a success alone must not trip the breaker")

	b.RecordResult(pod, podAddr(pod), 400)
	assert.True(t, b.IsBlacklisted(pod), "a success interleaved with failures must not reset the failure window")
}

// TestPodBlacklist_AnyErrorStatusAccumulatesTowardTheWindow pins down the
// other policy change: unlike the old breaker (which only tracked 400/404 and
// the two stream sentinels, deliberately excluding 5xx/429), isFailureStatus
// treats any non-2xx outcome as a failure, so a 500 counts toward the same
// window as a 400.
func TestPodBlacklist_AnyErrorStatusAccumulatesTowardTheWindow(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 400)
	}
	assert.False(t, b.IsBlacklisted(pod))

	b.RecordResult(pod, podAddr(pod), 500)
	assert.True(t, b.IsBlacklisted(pod), "a 500 must accumulate toward the same failure window as a 400")
}

// TestPodBlacklist_FailuresOutsideWindowDoNotAccumulate verifies the window
// boundary itself: failures older than podBlacklistFailureWindow are pruned
// on the next RecordResult call and do not count toward the threshold.
func TestPodBlacklist_FailuresOutsideWindowDoNotAccumulate(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	// Seed the window with failures already outside podBlacklistFailureWindow.
	v, _ := b.pods.LoadOrStore(PodKey(pod), &podFailureState{})
	state := v.(*podFailureState)
	old := time.Now().Add(-podBlacklistFailureWindow - time.Second)
	state.mu.Lock()
	state.failureTimestamps = make([]time.Time, FailureThreshold-1)
	for i := range state.failureTimestamps {
		state.failureTimestamps[i] = old
	}
	state.mu.Unlock()

	b.RecordResult(pod, podAddr(pod), 400) // one fresh failure
	assert.False(t, b.IsBlacklisted(pod), "expired failures must not count toward the threshold")

	state.mu.Lock()
	count := len(state.failureTimestamps)
	state.mu.Unlock()
	assert.Equal(t, 1, count, "expired failures should have been pruned from the window")
}

func TestPodBlacklist_TripsAfterConsecutive404s(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 404)
		assert.False(t, b.IsBlacklisted(pod), "should not blacklist before threshold is reached (i=%d)", i)
	}

	b.RecordResult(pod, podAddr(pod), 404)
	assert.True(t, b.IsBlacklisted(pod), "should blacklist once the threshold is reached via 404s")
}

func TestPodBlacklist_400And404CountTowardTheSameStreak(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	// Alternate between 400 and 404 - both are tracked failure statuses, so
	// they should accumulate together toward the same streak.
	for i := 0; i < FailureThreshold-1; i++ {
		if i%2 == 0 {
			b.RecordResult(pod, podAddr(pod), 400)
		} else {
			b.RecordResult(pod, podAddr(pod), 404)
		}
	}
	assert.False(t, b.IsBlacklisted(pod))

	b.RecordResult(pod, podAddr(pod), 404)
	assert.True(t, b.IsBlacklisted(pod), "400s and 404s should accumulate toward the same consecutive-failure streak")
}

func TestPodBlacklist_OnlyTrippedPodIsBlacklisted(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	tripped := blacklistTestPod("1")
	other := blacklistTestPod("2")

	tripBreaker(&b, tripped)

	assert.True(t, b.IsBlacklisted(tripped))
	assert.False(t, b.IsBlacklisted(other))
}

func TestPodBlacklist_BlacklistReason(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	tripped := blacklistTestPod("1")
	loadExcluded := blacklistTestPod("2")
	both := blacklistTestPod("3")
	untouched := blacklistTestPod("4")

	assert.Equal(t, "", b.BlacklistReason(untouched), "a pod with no state at all has no reason")

	tripBreaker(&b, tripped)
	reason := b.BlacklistReason(tripped)
	assert.Contains(t, reason, "failures within")
	assert.Contains(t, reason, "confirmed unhealthy")
	assert.NotContains(t, reason, "load outlier")

	b.SetLoadExcluded(loadExcluded, true)
	assert.Equal(t, "load outlier: running+waiting request count is a statistical outlier among its replicas", b.BlacklistReason(loadExcluded))

	tripBreaker(&b, both)
	b.SetLoadExcluded(both, true)
	combined := b.BlacklistReason(both)
	assert.Contains(t, combined, "load outlier")
	assert.Contains(t, combined, "failures within")

	names := b.BlacklistReasons([]*v1.Pod{tripped, loadExcluded, untouched})
	if assert.Len(t, names, 2) {
		assert.Contains(t, names[0], "1: ")
		assert.Contains(t, names[1], "2: load outlier")
	}
}

func TestPodBlacklist_FilterBlacklisted(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	tripped := blacklistTestPod("1")
	other := blacklistTestPod("2")

	tripBreaker(&b, tripped)

	filtered := b.FilterBlacklisted([]*v1.Pod{tripped, other})
	assert.Equal(t, []*v1.Pod{other}, filtered)
}

// sweepStale checks an entry's age and deletes it under the entry's own lock, and deletes
// only the state it checked. A fresh entry stored under the same key while the sweep was
// waiting for the old one's lock is not its to remove.
func TestPodBlacklist_SweepStaleNeverDeletesAReplacementEntry(t *testing.T) {
	var b PodBlacklist
	key := PodKey(blacklistTestPod("1"))
	stale := &podFailureState{} // never updated, so far past the TTL
	b.pods.Store(key, stale)
	b.tracked.Add(1)

	stale.mu.Lock() // the sweep finds the entry and then waits here
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		b.sweepStale(time.Now())
	}()
	time.Sleep(100 * time.Millisecond) // let the sweep reach the lock
	fresh := &podFailureState{lastUpdated: time.Now()}
	b.pods.Store(key, fresh)
	stale.mu.Unlock()
	<-swept

	got, ok := b.pods.Load(key)
	require.True(t, ok, "the replacement entry must survive the sweep")
	assert.Same(t, fresh, got)
	assert.Equal(t, int64(1), b.tracked.Load())
}

// Entries are created by requests and pruned by the sweep on different goroutines. However
// they interleave, tracked must equal the number of stored entries: FilterBlacklisted skips
// every lookup when it reads zero, so an undercount would let a tripped pod through.
func TestPodBlacklist_TrackedStaysExactWhileSweepRacesWithWriters(t *testing.T) {
	var b PodBlacklist
	pods := make([]*v1.Pod, 8)
	for i := range pods {
		pods[i] = blacklistTestPod(strconv.Itoa(i + 1))
	}
	everythingIsStale := time.Now().Add(2 * podBlacklistEntryTTL)

	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 500; i++ {
				b.SetLoadExcluded(pods[(w+i)%len(pods)], i%2 == 0)
			}
		}(w)
	}
	stop := make(chan struct{})
	sweeper := make(chan struct{})
	go func() {
		defer close(sweeper)
		for {
			select {
			case <-stop:
				return
			default:
				b.sweepStale(everythingIsStale)
			}
		}
	}()
	writers.Wait()
	close(stop)
	<-sweeper

	var stored int64
	b.pods.Range(func(_, _ any) bool { stored++; return true })
	assert.Equal(t, stored, b.tracked.Load())
}

// While nothing is tracked, which is a healthy fleet, FilterBlacklisted is on every
// request's path and must cost neither a lookup nor an allocation per candidate.
func TestPodBlacklist_FilterBlacklisted_NothingTrackedAllocatesNothing(t *testing.T) {
	var b PodBlacklist
	pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")}

	assert.Equal(t, pods, b.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = b.FilterBlacklisted(pods) }))
}

// The fast path keys off tracked, so it has to count entries exactly: created once on
// first use however many times the pod is touched, and gone again once pruned.
func TestPodBlacklist_TrackedCountsEntriesThroughCreationAndPruning(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod1, pod2 := blacklistTestPod("1"), blacklistTestPod("2")

	b.RecordResult(pod1, "10.0.0.1:8000", 500)
	b.RecordResult(pod1, "10.0.0.1:8000", 500)
	b.SetLoadExcluded(pod2, true)
	b.SetLoadExcluded(pod2, false)
	b.SetLoadExcluded(blacklistTestPod("3"), false) // clearing a never-excluded pod makes no entry
	assert.Equal(t, int64(2), b.tracked.Load())
	assert.Equal(t, []*v1.Pod{pod2}, b.FilterBlacklisted([]*v1.Pod{pod2}), "a tracked pod goes through Allow, not the fast path")

	b.sweepStale(time.Now())
	assert.Equal(t, int64(2), b.tracked.Load(), "fresh entries are not pruned")

	b.sweepStale(time.Now().Add(2 * podBlacklistEntryTTL))
	assert.Zero(t, b.tracked.Load())
	pods := []*v1.Pod{pod1, pod2}
	assert.Equal(t, pods, b.FilterBlacklisted(pods))
}

func TestPodBlacklist_SetLoadExcluded(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	other := blacklistTestPod("2")

	assert.False(t, b.IsBlacklisted(pod))
	assert.True(t, b.Allow(pod))

	b.SetLoadExcluded(pod, true)
	assert.True(t, b.IsBlacklisted(pod), "pod flagged as a load outlier should be blacklisted")
	assert.False(t, b.Allow(pod), "a load-excluded pod should not be admitted into routing")
	assert.False(t, b.IsBlacklisted(other), "an unrelated pod must be unaffected")

	filtered := b.FilterBlacklisted([]*v1.Pod{pod, other})
	assert.Equal(t, []*v1.Pod{other}, filtered)

	b.SetLoadExcluded(pod, false)
	assert.False(t, b.IsBlacklisted(pod), "pod should be un-blacklisted once its load converges back to normal")
	assert.True(t, b.Allow(pod))
}

func TestPodBlacklist_LoadExcludedIndependentOfFailureBreaker(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	// Trip the tracked-failure breaker, then also flag it as a load outlier.
	tripBreaker(&b, pod)
	assert.True(t, b.IsBlacklisted(pod))

	b.SetLoadExcluded(pod, true)
	assert.True(t, b.IsBlacklisted(pod))

	// Clearing the load-outlier flag alone must not resurrect a pod whose
	// failure breaker is still open.
	b.SetLoadExcluded(pod, false)
	assert.True(t, b.IsBlacklisted(pod), "pod should remain blacklisted while its failure breaker is still open")
}

func TestPodBlacklist_HealthyPodIsNotBlacklisted_FalseAlarm(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysHealthy}
	pod := blacklistTestPod("1")

	// Enough consecutive tracked failures to cross the threshold, but the
	// pod's own /health endpoint says it's fine — e.g. a client is sending
	// consistently malformed requests to an otherwise-healthy pod.
	tripBreaker(&b, pod)

	assert.False(t, b.IsBlacklisted(pod), "a healthy /health check should veto blacklisting even after threshold consecutive tracked failures")

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	failures := len(state.failureTimestamps)
	trip := state.tripCount
	state.mu.Unlock()
	assert.Equal(t, 0, failures, "the window should clear after a false alarm, same as a genuine reset")
	assert.Equal(t, 0, trip, "a false alarm is not a real trip")
}

func TestPodBlacklist_405_BlacklistsEvenWhenHealthCheckIsHealthy(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysHealthy}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold; i++ {
		b.RecordResult(pod, podAddr(pod), http.StatusMethodNotAllowed, "test-model")
	}

	assert.True(t, b.IsBlacklisted(pod), "HTTP 405 must blacklist the pod even when GET /health is healthy")
}

func identityMustNotRun(t *testing.T) func(string, string) bool {
	return func(string, string) bool {
		t.Error("identity check must not run for this pod/model")
		return true
	}
}

func recordThreshold(b *PodBlacklist, pod *v1.Pod, status int, model string) {
	for i := 0; i < FailureThreshold; i++ {
		b.RecordResult(pod, podAddr(pod), status, model)
	}
}

// TestPodBlacklist_Identity_TripsWhenListenerIsNotTheEngine is the stale
// registration case: another process now owns the recycled ip:port, answers
// GET /health 200 and rejects (or mangles) every inference request. For a
// vLLM/SGLang pod the model list must catch it whatever status the imposter
// returns, not only 405.
func TestPodBlacklist_Identity_TripsWhenListenerIsNotTheEngine(t *testing.T) {
	for _, status := range []int{400, 404, 405, 429, 500, 501, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := PodBlacklist{HealthCheck: alwaysHealthy, IdentityCheck: alwaysModelGone}
			pod := blacklistTestEnginePod("1", "vllm")

			recordThreshold(&b, pod, status, "test-model")

			assert.True(t, b.IsBlacklisted(pod), "a listener that answers /health but is not the engine for the model must trip on HTTP %d", status)
		})
	}
}

// TestPodBlacklist_Identity_RouteRejectIsDecidedByIdentityAlone covers the
// other side of the 405 rule: a client sending the wrong method to a healthy
// engine makes it answer 405, and that must not blacklist the pod. /health is
// stubbed unhealthy to prove it is not what decides route-reject statuses.
func TestPodBlacklist_Identity_RouteRejectIsDecidedByIdentityAlone(t *testing.T) {
	for _, status := range []int{404, 405, 501} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			b := PodBlacklist{HealthCheck: alwaysUnhealthy, IdentityCheck: alwaysModelServed}
			pod := blacklistTestEnginePod("1", "vllm")

			recordThreshold(&b, pod, status, "test-model")

			assert.False(t, b.IsBlacklisted(pod), "HTTP %d from an engine that still lists the model is a false alarm", status)
		})
	}
}

// TestPodBlacklist_Identity_OtherStatusesNeedHealthAndIdentity keeps /health
// in the loop for statuses that say "sick" rather than "wrong API": a dead
// engine core behind a live API server still lists its models, so only
// /health can see it.
func TestPodBlacklist_Identity_OtherStatusesNeedHealthAndIdentity(t *testing.T) {
	t.Run("healthy and right engine is a false alarm", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysHealthy, IdentityCheck: alwaysModelServed}
		pod := blacklistTestEnginePod("1", "sglang")

		recordThreshold(&b, pod, 500, "test-model")

		assert.False(t, b.IsBlacklisted(pod))
	})

	t.Run("engine core dead behind a live API server trips", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysUnhealthy, IdentityCheck: alwaysModelServed}
		pod := blacklistTestEnginePod("1", "vllm")

		recordThreshold(&b, pod, 500, "test-model")

		assert.True(t, b.IsBlacklisted(pod), "a failing /health must still trip even though /v1/models lists the model")
	})
}

// TestPodBlacklist_Identity_SkippedWhereItCannotApply pins down the cases
// that must keep the pre-identity behavior: an engine that is not known to
// list models (its /v1/models may have another shape), and a result recorded
// without a model name to look for.
func TestPodBlacklist_Identity_SkippedWhereItCannotApply(t *testing.T) {
	t.Run("engine not known to list models", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysHealthy, IdentityCheck: identityMustNotRun(t)}
		pod := blacklistTestEnginePod("1", "xdit")

		recordThreshold(&b, pod, 500, "test-model")

		assert.False(t, b.IsBlacklisted(pod), "confirmation for such an engine stays /health only")
	})

	t.Run("unlabeled pod", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysHealthy, IdentityCheck: identityMustNotRun(t)}
		pod := blacklistTestPod("1")

		recordThreshold(&b, pod, 500, "test-model")

		assert.False(t, b.IsBlacklisted(pod))
	})

	t.Run("no model name", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysHealthy, IdentityCheck: identityMustNotRun(t)}
		pod := blacklistTestEnginePod("1", "vllm")

		for i := 0; i < FailureThreshold; i++ {
			b.RecordResult(pod, podAddr(pod), 500)
		}

		assert.False(t, b.IsBlacklisted(pod))
	})
}

// TestPodBlacklist_Identity_StaleEntryEndToEnd replays the incident with the
// real probes (no stubbed checks): a recycled address whose new owner answers
// every GET with an empty 200 and rejects POST with 405, versus a real engine
// that also returns 405 to a client's wrong-method request.
func TestPodBlacklist_Identity_StaleEntryEndToEnd(t *testing.T) {
	const model = "test-model"

	imposter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"error":"Method Not Allowed"}`))
	}))
	defer imposter.Close()

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case podHealthCheckPath:
			w.WriteHeader(http.StatusOK)
		case podModelsCheckPath:
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model"}]}`))
		}
	}))
	defer engine.Close()

	for name, tc := range map[string]struct {
		srv         *httptest.Server
		blacklisted bool
	}{
		"imposter on the recycled address is blacklisted": {imposter, true},
		"real engine answering 405 is not":                {engine, false},
	} {
		t.Run(name, func(t *testing.T) {
			var b PodBlacklist
			pod := blacklistTestEnginePod("1", "vllm")
			addr := strings.TrimPrefix(tc.srv.URL, "http://")

			for i := 0; i < FailureThreshold; i++ {
				b.RecordResult(pod, addr, http.StatusMethodNotAllowed, model)
			}

			assert.Equal(t, tc.blacklisted, b.IsBlacklisted(pod))
		})
	}
}

func TestEngineListsModels(t *testing.T) {
	for engine, want := range map[string]bool{
		"vllm":      true,
		"VLLM":      true,
		"sglang":    true,
		"":          false,
		"xdit":      false,
		"vllm-omni": false,
	} {
		assert.Equal(t, want, engineListsModels(engine), "engine %q", engine)
	}
}

// TestPodBlacklist_404_ModelAwareConfirmation_TripsWhenModelGone verifies
// that a 404 streak with a model name is confirmed against ModelCheck (GET
// /v1/models in production) instead of the generic /health endpoint. Generic
// /health is stubbed healthy here (as vLLM/SGLang's typically is whenever
// the engine process is merely up) to prove it is the engine model list --
// not /health -- that decides the outcome: the engine no longer serves the
// model, so the streak must still trip.
func TestPodBlacklist_404_ModelAwareConfirmation_TripsWhenModelGone(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysHealthy, ModelCheck: alwaysModelGone}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 404, "test-model")
	}
	b.RecordResult(pod, podAddr(pod), 404, "test-model")

	assert.True(t, b.IsBlacklisted(pod), "a 404 streak must trip once the engine no longer lists the requested model, even though generic /health reports healthy")
}

// TestPodBlacklist_404_ModelAwareConfirmation_FalseAlarmWhenModelStillServed
// is the converse: generic /health is stubbed unhealthy, but the engine
// still lists the model that 404'd (e.g. a transient per-request issue
// unrelated to routing), so the streak must reset instead of tripping.
func TestPodBlacklist_404_ModelAwareConfirmation_FalseAlarmWhenModelStillServed(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy, ModelCheck: alwaysModelServed}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 404, "test-model")
	}
	b.RecordResult(pod, podAddr(pod), 404, "test-model")

	assert.False(t, b.IsBlacklisted(pod), "a 404 streak must not trip when the engine still lists the requested model, even though generic /health reports unhealthy")
}

// TestPodBlacklist_404_FallsBackToHealthCheckWithoutModelAwareness pins down
// that callers who don't supply a model (the variadic argument omitted --
// e.g. the StatusStreamClosed call site, or any pre-existing caller)
// keep the old generic-/health confirmation for 404, same as every other
// tracked status.
func TestPodBlacklist_404_FallsBackToHealthCheckWithoutModelAwareness(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysHealthy}
	pod := blacklistTestPod("1")

	tripBreaker404(&b, pod)

	assert.False(t, b.IsBlacklisted(pod), "without a model name, a 404 streak should fall back to the generic /health confirmation like before")
}

// TestPodBlacklist_RecordResult_DoesNotHoldLockDuringConfirmation is a
// regression test for RecordResult previously holding the per-pod mutex for
// the full duration of its /health (or model-aware) confirmation call, which
// can take up to podHealthCheckTimeout. Every other concurrent caller
// touching this pod -- Allow, in particular, which the routing hot path
// calls on every candidate -- must not be stuck waiting on that same lock
// while the confirmation is in flight.
func TestPodBlacklist_RecordResult_DoesNotHoldLockDuringConfirmation(t *testing.T) {
	confirmationStarted := make(chan struct{})
	releaseConfirmation := make(chan struct{})
	var once sync.Once
	b := PodBlacklist{HealthCheck: func(string) bool {
		once.Do(func() { close(confirmationStarted) })
		<-releaseConfirmation
		return false
	}}
	pod := blacklistTestPod("1")

	for i := 0; i < FailureThreshold-1; i++ {
		b.RecordResult(pod, podAddr(pod), 400)
	}

	recordDone := make(chan struct{})
	go func() {
		b.RecordResult(pod, podAddr(pod), 400) // crosses the threshold -- triggers the confirmation
		close(recordDone)
	}()

	select {
	case <-confirmationStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation check never started")
	}

	// The confirmation is now blocked in-flight on releaseConfirmation.
	// Allow must not be stuck waiting on state.mu while that happens.
	allowResult := make(chan bool, 1)
	go func() { allowResult <- b.Allow(pod) }()

	select {
	case allowed := <-allowResult:
		assert.True(t, allowed, "the breaker hasn't tripped yet -- the pod should still be allowed while confirmation is in flight")
	case <-time.After(2 * time.Second):
		t.Fatal("Allow blocked on state.mu while RecordResult's confirmation check was in flight -- the lock must be released during that call")
	}

	close(releaseConfirmation)
	<-recordDone

	assert.True(t, b.IsBlacklisted(pod), "pod should be blacklisted once the (failing) confirmation completes")
}

// failureStateFor returns the internal circuit-breaker state tracked for
// pod, failing the test if RecordResult/Allow has never touched it. Tests
// use this to fast-forward time-based transitions (backoff elapsed, probe
// timed out, entry gone stale) without actually sleeping.
func failureStateFor(t *testing.T, b *PodBlacklist, pod *v1.Pod) *podFailureState {
	t.Helper()
	v, ok := b.pods.Load(PodKey(pod))
	if !ok {
		t.Fatalf("no failure state tracked for pod %s", pod.Name)
	}
	return v.(*podFailureState)
}

func tripBreaker(b *PodBlacklist, pod *v1.Pod) {
	for i := 0; i < FailureThreshold; i++ {
		b.RecordResult(pod, podAddr(pod), 400)
	}
}

func tripBreaker404(b *PodBlacklist, pod *v1.Pod) {
	for i := 0; i < FailureThreshold; i++ {
		b.RecordResult(pod, podAddr(pod), 404)
	}
}

func TestPodBlacklist_BackoffDurationDoublesAndCaps(t *testing.T) {
	assert.Equal(t, podBlacklistBaseDuration, backoffDuration(1))
	assert.Equal(t, 2*podBlacklistBaseDuration, backoffDuration(2))
	assert.Equal(t, 4*podBlacklistBaseDuration, backoffDuration(3))
	assert.Equal(t, podBlacklistMaxDuration, backoffDuration(100), "backoff must cap at podBlacklistMaxDuration")
}

func TestPodBlacklist_HalfOpen_AdmitsExactlyOneProbeAfterBackoffElapses(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	require := assert.New(t)
	require.True(b.IsBlacklisted(pod))

	// Fast-forward past the backoff window without sleeping.
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "once the backoff elapses the pod is a routing candidate")
	assert.True(t, b.ClaimProbe(pod), "the request routed to the pod claims the half-open probe")
	assert.False(t, b.Allow(pod), "a concurrent caller must not get a second probe while one is already in flight")
	assert.False(t, b.ClaimProbe(pod), "a second request routed to the pod cannot claim the probe again")
}

// Allow is called on every ready pod before the router picks one. If it claimed
// the half-open probe, every candidate the router passed over would be stuck
// half-open with a probe nobody reports back for, and excluded until
// probeDeadline.
func TestPodBlacklist_Allow_DoesNotClaimTheHalfOpenProbe(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	for i := 0; i < 3; i++ {
		assert.True(t, b.Allow(pod), "a candidate the router passes over leaves the pod a candidate for the next request")
	}
	state.mu.Lock()
	assert.Equal(t, circuitOpen, state.state, "Allow must not move the breaker to half-open")
	state.mu.Unlock()

	assert.True(t, b.ClaimProbe(pod), "the first request that is actually routed to the pod claims the probe")
	state.mu.Lock()
	assert.Equal(t, circuitHalfOpen, state.state)
	state.mu.Unlock()
}

func TestPodBlacklist_FilterBlacklisted_KeepsARecoveringPodForTheNextRequest(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	recovering := blacklistTestPod("recovering")
	other := blacklistTestPod("other")
	tripBreaker(&b, recovering)
	state := failureStateFor(t, &b, recovering)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	pods := []*v1.Pod{recovering, other}

	// The router picks `other`; nothing is claimed.
	assert.Equal(t, pods, b.FilterBlacklisted(pods))
	// So the next request still considers the recovering pod, rather than
	// finding it excluded for the rest of the probe deadline.
	assert.Equal(t, pods, b.FilterBlacklisted(pods))
}

func TestPodBlacklist_ClaimProbe_OnlyWhereAProbeIsDue(t *testing.T) {
	t.Run("nil pod", func(t *testing.T) {
		var b PodBlacklist
		assert.False(t, b.ClaimProbe(nil))
	})

	t.Run("pod with no entry", func(t *testing.T) {
		var b PodBlacklist
		assert.False(t, b.ClaimProbe(blacklistTestPod("1")))
		assert.Zero(t, b.tracked.Load(), "claiming must not start tracking a pod")
	})

	t.Run("closed breaker", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysHealthy}
		pod := blacklistTestPod("1")
		b.RecordResult(pod, podAddr(pod), 400) // creates the entry, still closed
		assert.False(t, b.ClaimProbe(pod))
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		defer state.mu.Unlock()
		assert.Equal(t, circuitClosed, state.state)
	})

	// Routing falls back to excluded pods when every pod is excluded. That
	// request is not a probe: the pod's backoff is not over.
	t.Run("open pod still inside its backoff", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := blacklistTestPod("1")
		tripBreaker(&b, pod)
		assert.False(t, b.ClaimProbe(pod))
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		defer state.mu.Unlock()
		assert.Equal(t, circuitOpen, state.state)
	})

	t.Run("half-open pod whose probe is still in flight", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := blacklistTestPod("1")
		tripBreaker(&b, pod)
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.openUntil = time.Now().Add(-time.Second)
		state.mu.Unlock()
		require.True(t, b.ClaimProbe(pod))
		state.mu.Lock()
		deadline := state.probeDeadline
		state.mu.Unlock()

		assert.False(t, b.ClaimProbe(pod))
		state.mu.Lock()
		defer state.mu.Unlock()
		assert.Equal(t, deadline, state.probeDeadline, "a refused claim must not extend the deadline")
	})

	t.Run("claim sets the probe deadline", func(t *testing.T) {
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := blacklistTestPod("1")
		tripBreaker(&b, pod)
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.openUntil = time.Now().Add(-time.Second)
		state.mu.Unlock()

		require.True(t, b.ClaimProbe(pod))
		state.mu.Lock()
		defer state.mu.Unlock()
		assert.WithinDuration(t, time.Now().Add(podBlacklistProbeTimeout), state.probeDeadline, 2*time.Second)
	})
}

// A load-excluded pod is out of routing whatever its breaker says. If routing
// falls back to it anyway, that request is still a valid probe for the breaker.
func TestPodBlacklist_ClaimProbe_LoadExcludedPodIsStillAdmittedByTheBreaker(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	b.SetLoadExcluded(pod, true)

	assert.False(t, b.Allow(pod), "a load-excluded pod is not a candidate")
	assert.True(t, b.ClaimProbe(pod))
}

func TestPodBlacklist_HalfOpenProbeSuccess_ClosesBreaker(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "probe should be admitted")
	require.True(t, b.ClaimProbe(pod))
	b.RecordResult(pod, podAddr(pod), 200) // probe succeeds outright, no health check involved

	assert.False(t, b.IsBlacklisted(pod), "a successful probe should close the breaker")
	assert.True(t, b.Allow(pod), "closed breaker should admit normally")

	state.mu.Lock()
	trip := state.tripCount
	state.mu.Unlock()
	assert.Equal(t, 0, trip, "a successful probe should clear tripCount so a recovered pod isn't punished for its earlier failures")
}

func TestPodBlacklist_HalfOpenProbeFailure_ReopensWithLongerBackoff(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "probe should be admitted")
	require.True(t, b.ClaimProbe(pod))
	b.RecordResult(pod, podAddr(pod), 400) // probe fails, and /health confirms unhealthy

	assert.True(t, b.IsBlacklisted(pod), "a failed probe confirmed by /health should reopen the breaker")

	state.mu.Lock()
	trip := state.tripCount
	openUntil := state.openUntil
	state.mu.Unlock()
	assert.Equal(t, 2, trip, "a failed probe should count as a second trip")
	assert.WithinDuration(t, time.Now().Add(backoffDuration(2)), openUntil, 2*time.Second,
		"the reopened window should reflect the doubled backoff for the second trip")
}

func TestPodBlacklist_HalfOpenProbeFailure_HealthyOverridesToClose(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "probe should be admitted")
	require.True(t, b.ClaimProbe(pod))

	// The probe request itself got a 400, but /health now reports the pod
	// healthy (e.g. the probe request was itself malformed).
	b.HealthCheck = alwaysHealthy
	b.RecordResult(pod, podAddr(pod), 400)

	assert.False(t, b.IsBlacklisted(pod), "a healthy /health check should close the breaker even if the probe request itself failed")
	state.mu.Lock()
	trip := state.tripCount
	state.mu.Unlock()
	assert.Equal(t, 0, trip, "closing via a healthy override should clear tripCount, same as a genuine probe success")
}

func TestPodBlacklist_ProbeTimeout_AllowsFreshProbe(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "pod is a candidate once its backoff elapses")
	assert.True(t, b.ClaimProbe(pod), "first probe claimed")
	assert.False(t, b.Allow(pod), "second caller blocked while probe is outstanding")
	assert.False(t, b.ClaimProbe(pod), "second probe refused while the first is outstanding")

	// Simulate the claimed probe request hanging forever (RecordResult is
	// never called for it) by fast-forwarding past its deadline.
	state.mu.Lock()
	state.probeDeadline = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "once the stuck probe's deadline lapses, the pod is a candidate again")
	assert.True(t, b.ClaimProbe(pod), "and a new caller can claim a fresh probe")
	assert.False(t, b.Allow(pod), "the fresh probe excludes the pod again")
}

// TestOnPodFlakyReAdd_ProactivelyOpensBreakerAndRecovers covers the fix for
// requests routing to a pod immediately after a flaky delete/re-add, before any
// health confirmation: OnPodFlakyReAdd (implementing cache.PodLifecycleListener)
// should open the breaker for flakyReAddOpenDuration, then let it recover through
// the exact same half-open probe path a reactive trip uses.
func TestOnPodFlakyReAdd_ProactivelyOpensBreakerAndRecovers(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	assert.True(t, b.Allow(pod), "pod has no entry yet and should be routable")

	b.OnPodFlakyReAdd(pod)
	assert.False(t, b.Allow(pod), "pod should be proactively excluded right after a flaky re-add")
	assert.True(t, b.IsBlacklisted(pod))

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	assert.Equal(t, circuitOpen, state.state)
	assert.Equal(t, 0, state.tripCount, "a proactive flaky-re-add open must not count as a confirmed-failure trip")
	openUntil := state.openUntil
	state.mu.Unlock()
	assert.WithinDuration(t, time.Now().Add(flakyReAddOpenDuration), openUntil, time.Second)

	// Fast-forward past the (short) proactive window without sleeping.
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod), "exactly one probe should be admitted once the window elapses")
	require.True(t, b.ClaimProbe(pod))
	b.RecordResult(pod, podAddr(pod), 200) // the probe request succeeds

	assert.False(t, b.IsBlacklisted(pod), "a successful probe should close the breaker")
	assert.True(t, b.Allow(pod), "closed breaker should admit normally again")
}

// TestOnPodFlakyReAdd_FailedProbeReopensWithNormalBackoff covers the other half
// of recovery: if the pod really is unhealthy, the probe RecordResult drives after
// OnPodFlakyReAdd behaves identically to a reactively-tripped breaker's probe --
// it re-opens with trip #1's normal backoff, not something special-cased.
func TestOnPodFlakyReAdd_FailedProbeReopensWithNormalBackoff(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	b.OnPodFlakyReAdd(pod)
	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()

	assert.True(t, b.Allow(pod))
	require.True(t, b.ClaimProbe(pod))
	b.RecordResult(pod, podAddr(pod), 400) // probe fails, /health confirms unhealthy

	assert.True(t, b.IsBlacklisted(pod))
	state.mu.Lock()
	trip := state.tripCount
	openUntil := state.openUntil
	state.mu.Unlock()
	assert.Equal(t, 1, trip, "the first failed probe after a proactive open is trip #1, same as any first reactive trip")
	assert.WithinDuration(t, time.Now().Add(backoffDuration(1)), openUntil, 2*time.Second)
}

// TestOnPodFlakyReAdd_DoesNotShortenExistingExclusion covers a pod that was
// already blacklisted for an unrelated, longer-running reason (a real failure
// streak) when the flaky-re-add signal arrives: PodBlacklist is keyed by pod
// name and untouched by the informer's delete/re-add cycle, so the two can
// legitimately overlap. The proactive open must never shorten an existing,
// later exclusion window.
func TestOnPodFlakyReAdd_DoesNotShortenExistingExclusion(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod) // openUntil = now + podBlacklistBaseDuration (1m), well past flakyReAddOpenDuration (3s)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	longOpenUntil := state.openUntil
	state.mu.Unlock()

	b.OnPodFlakyReAdd(pod)

	state.mu.Lock()
	afterOpenUntil := state.openUntil
	afterTripCount := state.tripCount
	state.mu.Unlock()
	assert.Equal(t, longOpenUntil, afterOpenUntil, "a shorter proactive window must not shorten an existing, longer exclusion")
	assert.Equal(t, 1, afterTripCount, "an already-tripped pod's trip count must be untouched by a flaky-re-add signal")
}

// TestOnPodFlakyReAdd_DoesNotDisturbInProgressProbe covers a pod that is
// mid-half-open-probe (a real caller already claimed the single probe slot) when
// the flaky-re-add signal arrives: resetting state here would let a second
// request slip through as an unaccounted-for extra probe.
func TestOnPodFlakyReAdd_DoesNotDisturbInProgressProbe(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)

	state := failureStateFor(t, &b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	assert.True(t, b.Allow(pod))
	require.True(t, b.ClaimProbe(pod), "claim the single half-open probe slot")

	state.mu.Lock()
	probeDeadline := state.probeDeadline
	state.mu.Unlock()

	b.OnPodFlakyReAdd(pod)

	state.mu.Lock()
	assert.Equal(t, circuitHalfOpen, state.state, "an in-progress probe must not be reset to circuitOpen")
	assert.Equal(t, probeDeadline, state.probeDeadline)
	state.mu.Unlock()
	assert.False(t, b.Allow(pod), "a second caller must still not get a concurrent probe")
}

func TestPodBlacklist_SweepStale_RemovesOnlyOldEntries(t *testing.T) {
	var b PodBlacklist // both calls stay well under threshold, so the health check is never consulted
	stalePod := blacklistTestPod("stale")
	freshPod := blacklistTestPod("fresh")

	b.RecordResult(stalePod, podAddr(stalePod), 400)
	b.RecordResult(freshPod, podAddr(freshPod), 400)

	staleState := failureStateFor(t, &b, stalePod)
	staleState.mu.Lock()
	staleState.lastUpdated = time.Now().Add(-podBlacklistEntryTTL - time.Minute)
	staleState.mu.Unlock()

	b.sweepStale(time.Now())

	_, staleStillPresent := b.pods.Load(PodKey(stalePod))
	_, freshStillPresent := b.pods.Load(PodKey(freshPod))
	assert.False(t, staleStillPresent, "an entry untouched for longer than podBlacklistEntryTTL should be pruned")
	assert.True(t, freshStillPresent, "a recently touched entry should survive the sweep")
}

func TestHTTPHealthCheck(t *testing.T) {
	t.Run("2xx response is healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, podHealthCheckPath, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		assert.True(t, httpHealthCheck(strings.TrimPrefix(srv.URL, "http://")))
	})

	t.Run("non-2xx response is unhealthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		assert.False(t, httpHealthCheck(strings.TrimPrefix(srv.URL, "http://")))
	})

	t.Run("connection failure is unhealthy", func(t *testing.T) {
		assert.False(t, httpHealthCheck("127.0.0.1:1")) // nothing listens on port 1
	})

	t.Run("empty address fails open (not blacklisted)", func(t *testing.T) {
		assert.True(t, httpHealthCheck(""))
	})
}

func TestHTTPListenerCheck(t *testing.T) {
	addrOf := func(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

	t.Run("any HTTP status shows a listener", func(t *testing.T) {
		for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			assert.True(t, httpListenerCheckWithin(addrOf(srv), time.Second), "status %d", status)
			srv.Close()
		}
	})

	t.Run("connection failure is not a listener", func(t *testing.T) {
		assert.False(t, httpListenerCheckWithin("127.0.0.1:1", time.Second)) // nothing listens on port 1
	})

	t.Run("empty address fails open (not blacklisted)", func(t *testing.T) {
		assert.True(t, httpListenerCheckWithin("", time.Second))
	})
}

// xLLM answers 404 on /health on every port while serving inference, so the
// probe must not call it dead for that; every other engine still needs a 2xx.
func TestProbe_EngineWithoutHealthRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	b := &PodBlacklist{}
	for _, engine := range []string{"xllm", "XLLM"} {
		result, failure := b.probe(addr, "m", engine, time.Second)
		assert.Equal(t, ProbeHealthy, result, "engine %q: %s", engine, failure)
	}
	for _, engine := range []string{"", "vllm", "sglang", "trtllm"} {
		result, _ := b.probe(addr, "", engine, time.Second)
		assert.Equal(t, ProbeDead, result, "engine %q must still need a 2xx /health", engine)
	}

	// An xLLM pod with nothing listening is still dead.
	result, _ := b.probe("127.0.0.1:1", "m", "xllm", time.Second)
	assert.Equal(t, ProbeDead, result)
}

func TestHTTPEngineServesModel(t *testing.T) {
	const model = "test-model"

	t.Run("model present in /v1/models is served", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, podModelsCheckPath, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other"},{"id":"test-model"}]}`))
		}))
		defer srv.Close()
		assert.True(t, httpEngineServesModel(strings.TrimPrefix(srv.URL, "http://"), model))
	})

	t.Run("model absent from /v1/models is not served", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other-model"}]}`))
		}))
		defer srv.Close()
		assert.False(t, httpEngineServesModel(strings.TrimPrefix(srv.URL, "http://"), model))
	})

	t.Run("non-2xx response is not served", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		assert.False(t, httpEngineServesModel(strings.TrimPrefix(srv.URL, "http://"), model))
	})

	t.Run("unparseable 2xx body fails open", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer srv.Close()
		assert.True(t, httpEngineServesModel(strings.TrimPrefix(srv.URL, "http://"), model))
	})

	t.Run("connection failure is not served", func(t *testing.T) {
		assert.False(t, httpEngineServesModel("127.0.0.1:1", model))
	})

	t.Run("empty address fails open", func(t *testing.T) {
		assert.True(t, httpEngineServesModel("", model))
	})

	t.Run("empty model fails open", func(t *testing.T) {
		assert.True(t, httpEngineServesModel("127.0.0.1:8000", ""))
	})
}

func TestHTTPEngineIsModelServer(t *testing.T) {
	const model = "test-model"

	serve := func(t *testing.T, h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}

	t.Run("model present in /v1/models is served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, podModelsCheckPath, r.URL.Path)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other"},{"id":"test-model"}]}`))
		})
		assert.True(t, httpEngineIsModelServer(addr, model))
	})

	t.Run("model absent is not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other-model"}]}`))
		})
		assert.False(t, httpEngineIsModelServer(addr, model))
	})

	// The stale-entry listener: 200 with no body, as seen on the recycled port.
	t.Run("empty 2xx body is another listener, not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		})
		assert.False(t, httpEngineIsModelServer(addr, model))
		assert.True(t, httpEngineServesModel(addr, model), "the lenient check still fails open on the same response")
	})

	t.Run("non-JSON 2xx body is not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		})
		assert.False(t, httpEngineIsModelServer(addr, model))
	})

	t.Run("non-2xx response is not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		assert.False(t, httpEngineIsModelServer(addr, model))
	})

	t.Run("connection failure is not served", func(t *testing.T) {
		assert.False(t, httpEngineIsModelServer("127.0.0.1:1", model))
	})

	t.Run("empty address or model fails open", func(t *testing.T) {
		assert.True(t, httpEngineIsModelServer("", model))
		assert.True(t, httpEngineIsModelServer("127.0.0.1:8000", ""))
	})
}

// captureKlog redirects klog into a buffer for the test; the returned func
// reads what was logged so far.
func captureKlog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
	})
	return func() string {
		klog.Flush()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A blacklisting log line has to say where the pod is reached and for which
// model, so it can be matched to a curl command.
func TestPodBlacklist_LogLinesNameTheAddressAndModel(t *testing.T) {
	const model = "test-model"
	newPod := func() *v1.Pod {
		pod := blacklistTestPod("1")
		pod.Labels = map[string]string{constants.ModelLabelName: model}
		return pod
	}
	wantTarget := "(addr=10.0.0.1:8000, model=" + model + ")"

	t.Run("tripped by failing requests", func(t *testing.T) {
		logs := captureKlog(t)
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := newPod()

		recordThreshold(&b, pod, 400, model)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" blacklisted for")
	})

	t.Run("re-blacklisted after a failed half-open probe", func(t *testing.T) {
		logs := captureKlog(t)
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := newPod()
		recordThreshold(&b, pod, 400, model)
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.openUntil = time.Now().Add(-time.Second)
		state.mu.Unlock()
		require.True(t, b.Allow(pod))
		require.True(t, b.ClaimProbe(pod))

		b.RecordResult(pod, podAddr(pod), 400, model)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" failed its half-open probe and its confirmation check reports it unhealthy")
	})

	t.Run("false alarm", func(t *testing.T) {
		logs := captureKlog(t)
		b := PodBlacklist{HealthCheck: alwaysHealthy}
		pod := newPod()

		recordThreshold(&b, pod, 400, model)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" hit 3 failures")
	})

	t.Run("recovered on a half-open probe", func(t *testing.T) {
		logs := captureKlog(t)
		b := PodBlacklist{HealthCheck: alwaysUnhealthy}
		pod := newPod()
		recordThreshold(&b, pod, 400, model)
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.openUntil = time.Now().Add(-time.Second)
		state.mu.Unlock()
		require.True(t, b.Allow(pod))
		require.True(t, b.ClaimProbe(pod))

		b.RecordResult(pod, podAddr(pod), 200, model)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" passed its half-open probe and is un-blacklisted")
	})

	// Background paths have no request, so the address and model come from the pod.
	t.Run("load outlier", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()
		pod.Labels[constants.ModelLabelPort] = "8000"

		b.SetLoadExcluded(pod, true)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" blacklisted: running+waiting request count")
	})

	t.Run("flaky re-add", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()
		pod.Labels[constants.ModelLabelPort] = "8000"

		b.OnPodFlakyReAdd(pod)

		assert.Regexp(t, `addr="10\.0\.0\.1:8000" model="test-model"`, logs())
	})
}

// PodLogFields is what lines outside this package use to name a pod, so it has
// to agree with the "pod ns/name (addr=..., model=...)" form the package's own
// lines use.
func TestPodLogFields(t *testing.T) {
	pod := blacklistTestPod("1")
	pod.Labels = map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}

	assert.Equal(t, []interface{}{"pod", "ns/1", "addr", "10.0.0.1:8000", "model", "test-model"}, PodLogFields(pod))
}

// A nil pod must not panic in a log helper; it just contributes no fields.
func TestPodLogFields_NilPod(t *testing.T) {
	assert.NotPanics(t, func() { assert.Nil(t, PodLogFields(nil)) })
}

// A broken-invariant error is still read by someone chasing a blacklisting
// problem, so it names the address and model like every other line.
func TestPodBlacklist_UnexpectedTypeLogNamesTheAddressAndModel(t *testing.T) {
	logs := captureKlog(t)
	var b PodBlacklist
	pod := blacklistTestPod("1")
	pod.Labels = map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}
	b.pods.Store(PodKey(pod), "not a *podFailureState")

	assert.True(t, b.Allow(pod))

	assert.Contains(t, logs(), "unexpected type string stored for pod ns/1 (addr=10.0.0.1:8000, model=test-model); treating as allowed")
}
