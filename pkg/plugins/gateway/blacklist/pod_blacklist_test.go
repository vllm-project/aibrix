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
	"github.com/vllm-project/aibrix/pkg/utils"
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

// alwaysUnhealthy/alwaysHealthy stub PodBlacklist.HealthCheck so tests never
// make a real network call: every probe of a pod finds it dead, respectively
// healthy.
func alwaysUnhealthy(string) bool { return false }
func alwaysHealthy(string) bool   { return true }

// tripBreaker opens pod's breaker the way the background sweep does: it finds
// the pod degraded podProbeFailureIterations sweeps in a row, which blocks it
// for the first backoff (a dead pod would be blocked for an hour instead).
func tripBreaker(b *PodBlacklist, pod *v1.Pod) {
	for i := 0; i < podProbeFailureIterations; i++ {
		b.RecordProbeResult(pod, ProbeDegraded, "GET /v1/models did not list the model")
	}
}

// failureStateFor returns the internal state tracked for pod, failing the test
// if nothing has touched it. Tests use this to fast-forward time-based
// transitions (backoff elapsed, entry gone stale) without actually sleeping.
func failureStateFor(t *testing.T, b *PodBlacklist, pod *v1.Pod) *podFailureState {
	t.Helper()
	v, ok := b.pods.Load(PodKey(pod))
	if !ok {
		t.Fatalf("no failure state tracked for pod %s", pod.Name)
	}
	return v.(*podFailureState)
}

// lapseBlock fast-forwards past the end of pod's block without sleeping.
func lapseBlock(t *testing.T, b *PodBlacklist, pod *v1.Pod) *podFailureState {
	t.Helper()
	state := failureStateFor(t, b, pod)
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	return state
}

// TestPodBlacklist_DisabledIsNoOp covers the production default: with the
// master switch off, nothing excludes pods or starts sweeps, so routing
// matches the pre-blacklist path.
func TestPodBlacklist_DisabledIsNoOp(t *testing.T) {
	prev := Enabled
	Enabled = false
	t.Cleanup(func() { Enabled = prev })

	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")
	other := blacklistTestPod("2")
	pods := []*v1.Pod{pod, other}

	assert.False(t, b.IsBlacklisted(pod))
	assert.True(t, b.Allow(pod))
	assert.Equal(t, pods, b.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = b.FilterBlacklisted(pods) }))

	assert.False(t, b.SetLoadExcluded(pod, true))
	assert.False(t, b.IsBlacklisted(pod))

	b.OnPodFlakyReAdd(pod)
	assert.False(t, b.IsBlacklisted(pod))

	for i := 0; i < podProbeFailureIterations; i++ {
		assert.False(t, b.RecordProbeResult(pod, ProbeDead, "GET /health failed"))
	}
	assert.False(t, b.IsQuarantined(pod))
	assert.False(t, b.IsBlacklisted(pod))
	assert.Zero(t, b.tracked.Load(), "disabled paths must not create entries")

	stop := make(chan struct{})
	close(stop)
	assert.NotPanics(t, func() {
		b.StartSweep(stop)
		b.StartLoadOutlierSweep(nil, nil, nil, stop)
		b.StartProbeSweep(nil, stop)
	})
}

// TestEnvPodBlacklistEnabled_DefaultsOff documents the LoadEnvBool contract
// for the master switch: unset / empty / false / 0 all mean off.
func TestEnvPodBlacklistEnabled_DefaultsOff(t *testing.T) {
	t.Setenv(EnvPodBlacklistEnabled, "")
	assert.False(t, utils.LoadEnvBool(EnvPodBlacklistEnabled, false))

	t.Setenv(EnvPodBlacklistEnabled, "false")
	assert.False(t, utils.LoadEnvBool(EnvPodBlacklistEnabled, false))

	t.Setenv(EnvPodBlacklistEnabled, "0")
	assert.False(t, utils.LoadEnvBool(EnvPodBlacklistEnabled, false))

	t.Setenv(EnvPodBlacklistEnabled, "true")
	assert.True(t, utils.LoadEnvBool(EnvPodBlacklistEnabled, false))

	t.Setenv(EnvPodBlacklistEnabled, "1")
	assert.True(t, utils.LoadEnvBool(EnvPodBlacklistEnabled, false))
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
	assert.Contains(t, reason, "background probe failed")
	assert.NotContains(t, reason, "load outlier")

	b.SetLoadExcluded(loadExcluded, true)
	assert.Equal(t, "load outlier: running+waiting request count is a statistical outlier among its replicas", b.BlacklistReason(loadExcluded))

	tripBreaker(&b, both)
	b.SetLoadExcluded(both, true)
	combined := b.BlacklistReason(both)
	assert.Contains(t, combined, "load outlier")
	assert.Contains(t, combined, "background probe failed")

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

// Entries are created by the sweeps and pruned by sweepStale on different goroutines. However
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
	var b PodBlacklist
	pod1, pod2 := blacklistTestPod("1"), blacklistTestPod("2")

	b.RecordProbeResult(pod1, ProbeDegraded, "x")
	b.RecordProbeResult(pod1, ProbeDegraded, "x")
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

// recountExcluded counts the stored entries that can exclude their pod, the number
// PodBlacklist.excluded must equal.
func recountExcluded(b *PodBlacklist) int64 {
	var n int64
	b.pods.Range(func(_, v any) bool {
		state := v.(*podFailureState)
		state.mu.RLock()
		if state.excludingLocked() {
			n++
		}
		state.mu.RUnlock()
		return true
	})
	return n
}

// FilterBlacklisted skips every lookup while excluded reads zero, so excluded has to follow
// every way a pod becomes, or stops being, excluded: an undercount would let an excluded
// pod through.
func TestPodBlacklist_ExcludedCountFollowsEveryTransition(t *testing.T) {
	var b PodBlacklist
	loaded, probed, flapped := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	check := func(want int64, when string) {
		t.Helper()
		assert.Equal(t, want, b.excluded.Load(), when)
		assert.Equal(t, recountExcluded(&b), b.excluded.Load(), when+" (against a recount)")
	}
	check(0, "nothing tracked")

	b.RecordProbeResult(probed, ProbeDegraded, "x")
	check(0, "an entry that excludes nothing is not counted")

	b.SetLoadExcluded(loaded, true)
	check(1, "load-excluded")
	b.SetLoadExcluded(loaded, true)
	check(1, "flagging again counts nothing more")

	tripBreaker(&b, probed)
	check(2, "breaker opened by the sweep")
	b.OnPodFlakyReAdd(flapped)
	check(3, "flaky re-add")

	b.SetLoadExcluded(loaded, false)
	check(2, "load exclusion cleared")

	lapseBlock(t, &b, probed)
	check(2, "a block that has ended is counted until something closes the breaker")
	b.RecordProbeResult(probed, ProbeHealthy, "")
	check(1, "a healthy sweep closes a lapsed breaker")

	for i := 0; i < podProbeRecoveryIterations; i++ {
		b.RecordProbeResult(flapped, ProbeHealthy, "")
	}
	check(0, "healthy sweeps lift a running block")

	b.SetLoadExcluded(loaded, true)
	tripBreaker(&b, probed)
	check(2, "excluded again")
	b.sweepStale(time.Now().Add(2 * podBlacklistEntryTTL))
	check(0, "pruned entries are taken off")
	assert.Zero(t, b.tracked.Load())
}

// A writer that loaded an entry's pointer just before sweepStale deleted it updates a state
// that is no longer stored. It must not leave excluded counting a pod nothing stores.
func TestPodBlacklist_ExcludedCountIgnoresAWriterHoldingADeletedEntry(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	b.SetLoadExcluded(pod, true)
	orphan := failureStateFor(t, &b, pod)
	require.Equal(t, int64(1), b.excluded.Load())

	b.sweepStale(time.Now().Add(2 * podBlacklistEntryTTL))
	require.Zero(t, b.excluded.Load())

	orphan.mu.Lock() // the late writer
	orphan.loadExcluded = true
	b.syncCountLocked(orphan)
	orphan.mu.Unlock()

	assert.Zero(t, b.excluded.Load(), "a deleted entry is never counted again")
	assert.Zero(t, recountExcluded(&b))
}

// The same invariant while writers of every kind race with the sweep that prunes.
func TestPodBlacklist_ExcludedCountStaysExactWhileSweepRacesWithWriters(t *testing.T) {
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
			for i := 0; i < 400; i++ {
				pod := pods[(w+i)%len(pods)]
				switch i % 5 {
				case 0:
					b.SetLoadExcluded(pod, true)
				case 1:
					b.SetLoadExcluded(pod, false)
				case 2:
					b.OnPodFlakyReAdd(pod)
				case 3:
					b.RecordProbeResult(pod, ProbeDead, "x")
				default:
					b.RecordProbeResult(pod, ProbeHealthy, "")
				}
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

	assert.Equal(t, recountExcluded(&b), b.excluded.Load())
}

// A pod that has failed one probe has an entry that excludes nothing. It must not put the
// fleet on the per-pod path: FilterBlacklisted still costs no lookup and no allocation.
func TestPodBlacklist_FilterBlacklisted_AClosedEntryKeepsTheFastPath(t *testing.T) {
	var b PodBlacklist
	pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")}
	b.RecordProbeResult(pods[0], ProbeDegraded, "x")
	require.Equal(t, int64(1), b.tracked.Load())

	assert.Equal(t, pods, b.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = b.FilterBlacklisted(pods) }))
}

// With an exclusion somewhere, a call whose pods are all admitted still allocates nothing,
// and one that drops a pod allocates once.
func TestPodBlacklist_FilterBlacklisted_AllocatesOnlyToDropAPod(t *testing.T) {
	var b PodBlacklist
	excluded := blacklistTestPod("x")
	b.SetLoadExcluded(excluded, true)
	healthy := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")}

	assert.Equal(t, healthy, b.FilterBlacklisted(healthy))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = b.FilterBlacklisted(healthy) }))

	mixed := []*v1.Pod{healthy[0], excluded, healthy[1], healthy[2]}
	assert.Equal(t, healthy, b.FilterBlacklisted(mixed), "order is kept")
	assert.Equal(t, []*v1.Pod{healthy[0], healthy[1]}, b.FilterBlacklisted([]*v1.Pod{healthy[0], healthy[1], excluded}))
	assert.Equal(t, []*v1.Pod{healthy[0], healthy[1]}, b.FilterBlacklisted([]*v1.Pod{excluded, healthy[0], healthy[1]}))
	assert.Empty(t, b.FilterBlacklisted([]*v1.Pod{excluded}))
	assert.EqualValues(t, 1, testing.AllocsPerRun(100, func() { _ = b.FilterBlacklisted(mixed) }))
}

// Allow is a read: it must not count as a touch. An entry that routing keeps looking at but
// nothing changes ages out like any other, so one old failure does not pin a pod's entry,
// and with it the per-pod path, for as long as the pod is routable.
func TestPodBlacklist_Allow_DoesNotKeepAnEntryFromAgingOut(t *testing.T) {
	var b PodBlacklist
	inspected, untouched := blacklistTestPod("inspected"), blacklistTestPod("untouched")
	b.RecordProbeResult(inspected, ProbeDegraded, "x")
	b.RecordProbeResult(untouched, ProbeDegraded, "x")
	for _, pod := range []*v1.Pod{inspected, untouched} {
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		state.lastUpdated = time.Now().Add(-podBlacklistEntryTTL + 5*time.Minute)
		state.mu.Unlock()
	}

	assert.True(t, b.Allow(inspected))
	b.sweepStale(time.Now().Add(10 * time.Minute)) // past the TTL for an entry nothing changed

	_, inspectedKept := b.pods.Load(PodKey(inspected))
	_, untouchedKept := b.pods.Load(PodKey(untouched))
	assert.False(t, untouchedKept)
	assert.False(t, inspectedKept, "looking at an entry does not keep it")
	assert.Zero(t, b.tracked.Load())
}

// Allow only takes the entry's read lock: requests do not queue on one another, or on a
// reader that is slow, for a pod they all consider.
func TestPodBlacklist_Allow_ReadersDoNotBlockEachOther(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	b.RecordProbeResult(pod, ProbeDegraded, "x")
	state := failureStateFor(t, &b, pod)

	state.mu.RLock() // another request is inside Allow
	defer state.mu.RUnlock()
	done := make(chan bool, 1)
	go func() { done <- b.Allow(pod) }()

	select {
	case allowed := <-done:
		assert.True(t, allowed)
	case <-time.After(2 * time.Second):
		t.Fatal("Allow waited for another reader of the same entry")
	}
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

func TestPodBlacklist_LoadExcludedIndependentOfTheBreaker(t *testing.T) {
	b := PodBlacklist{HealthCheck: alwaysUnhealthy}
	pod := blacklistTestPod("1")

	// Open the breaker, then also flag it as a load outlier.
	tripBreaker(&b, pod)
	assert.True(t, b.IsBlacklisted(pod))

	b.SetLoadExcluded(pod, true)
	assert.True(t, b.IsBlacklisted(pod))

	// Clearing the load-outlier flag alone must not resurrect a pod whose
	// breaker is still open.
	b.SetLoadExcluded(pod, false)
	assert.True(t, b.IsBlacklisted(pod), "pod should remain blacklisted while its breaker is still open")
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

func TestPodBlacklist_BackoffDurationDoublesAndCaps(t *testing.T) {
	assert.Equal(t, podBlacklistBaseDuration, backoffDuration(1))
	assert.Equal(t, 2*podBlacklistBaseDuration, backoffDuration(2))
	assert.Equal(t, 4*podBlacklistBaseDuration, backoffDuration(3))
	assert.Equal(t, podBlacklistMaxDuration, backoffDuration(100), "backoff must cap at podBlacklistMaxDuration")
}

// TestPodBlacklist_BlockLapsesWithoutAProbeRequest covers recovery now that no
// request reports its outcome: once a block ends the pod is a routing candidate
// again and nothing claims or settles a probe.
func TestPodBlacklist_BlockLapsesWithoutAProbeRequest(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	require.False(t, b.Allow(pod))

	state := lapseBlock(t, &b, pod)

	assert.True(t, b.Allow(pod), "a pod whose block has ended is a candidate again")
	assert.False(t, b.IsBlacklisted(pod))
	assert.Empty(t, b.BlacklistReason(pod))
	assert.Equal(t, []*v1.Pod{pod}, b.FilterBlacklisted([]*v1.Pod{pod}))
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, 1, state.tripCount, "Allow neither reads nor changes the trip count")
}

// A healthy sweep never lifts a block that is still running, but once the block
// has ended it starts the backoff over, which is what a successful half-open
// request used to do.
func TestPodBlacklist_ASingleHealthySweepRestartsTheBackoffOnlyOnceTheBlockHasEnded(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	state := failureStateFor(t, &b, pod)

	b.RecordProbeResult(pod, ProbeHealthy, "")
	assert.True(t, b.IsBlacklisted(pod), "one healthy probe must not lift a running block")
	state.mu.Lock()
	assert.Equal(t, 1, state.tripCount)
	state.mu.Unlock()

	lapseBlock(t, &b, pod)
	b.RecordProbeResult(pod, ProbeHealthy, "")

	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, circuitClosed, state.state)
	assert.Equal(t, 0, state.tripCount, "a pod that probes healthy after its block is not punished for the earlier one")
}

// A block that is still running is lifted by podProbeRecoveryIterations healthy sweeps in a
// row: a pod that has been answering correctly is not kept out for the rest of the hour.
func TestPodBlacklist_HealthySweepsInARowLiftARunningBlock(t *testing.T) {
	t.Run("a quarantine ends and the cache sees it", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		for i := 0; i < podProbeFailureIterations; i++ {
			b.RecordProbeResult(pod, ProbeDead, "GET /health failed")
		}
		require.True(t, b.IsQuarantined(pod))
		require.False(t, b.Allow(pod))

		for i := 0; i < podProbeRecoveryIterations-1; i++ {
			b.RecordProbeResult(pod, ProbeHealthy, "")
			assert.True(t, b.IsQuarantined(pod), "still quarantined after %d healthy sweeps", i+1)
		}
		b.RecordProbeResult(pod, ProbeHealthy, "")

		assert.False(t, b.IsQuarantined(pod), "the cache scrapes the pod again")
		assert.False(t, b.IsBlacklisted(pod))
		assert.True(t, b.Allow(pod))
		state := failureStateFor(t, &b, pod)
		state.mu.Lock()
		defer state.mu.Unlock()
		assert.Equal(t, circuitClosed, state.state)
		assert.Zero(t, state.tripCount, "a recovered pod is not punished for its earlier block")
		assert.True(t, state.quarantinedUntil.IsZero())
	})

	t.Run("a degraded backoff ends too", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		tripBreaker(&b, pod)
		require.True(t, b.IsBlacklisted(pod))

		for i := 0; i < podProbeRecoveryIterations; i++ {
			b.RecordProbeResult(pod, ProbeHealthy, "")
		}

		assert.False(t, b.IsBlacklisted(pod))
		assert.True(t, b.Allow(pod))
	})

	t.Run("a failing sweep in between starts the count over", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		for i := 0; i < podProbeFailureIterations; i++ {
			b.RecordProbeResult(pod, ProbeDead, "GET /health failed")
		}

		for i := 0; i < podProbeRecoveryIterations-1; i++ {
			b.RecordProbeResult(pod, ProbeHealthy, "")
		}
		b.RecordProbeResult(pod, ProbeDead, "GET /health failed")
		b.RecordProbeResult(pod, ProbeHealthy, "")

		assert.True(t, b.IsQuarantined(pod), "a flapping pod does not leave on one healthy sweep after a failing one")
	})

	t.Run("a load exclusion is not a sweep block", func(t *testing.T) {
		var b PodBlacklist
		pod := blacklistTestPod("1")
		b.SetLoadExcluded(pod, true)

		for i := 0; i < 2*podProbeRecoveryIterations; i++ {
			b.RecordProbeResult(pod, ProbeHealthy, "")
		}

		assert.True(t, b.IsBlacklisted(pod), "only the next load sweep lifts a load exclusion")
	})
}

// A pod that is still degraded once its block has ended is blocked again after
// another podProbeFailureIterations sweeps, for the next, longer backoff.
func TestPodBlacklist_DegradedSweepsAfterTheBlockEscalateTheBackoff(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	state := lapseBlock(t, &b, pod)

	for i := 0; i < podProbeFailureIterations-1; i++ {
		assert.False(t, b.RecordProbeResult(pod, ProbeDegraded, "x"))
		assert.True(t, b.Allow(pod), "a lapsed pod stays routable while the sweeps build their count (i=%d)", i)
	}
	require.True(t, b.RecordProbeResult(pod, ProbeDegraded, "x"))

	assert.False(t, b.Allow(pod))
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, 2, state.tripCount)
	assert.WithinDuration(t, time.Now().Add(backoffDuration(2)), state.openUntil, 2*time.Second)
}

// TestOnPodFlakyReAdd_ProactivelyOpensBreakerAndRecovers covers requests routing
// to a pod immediately after a flaky delete/re-add: OnPodFlakyReAdd (implementing
// cache.PodLifecycleListener) opens the breaker for flakyReAddOpenDuration, after
// which the pod is a candidate again.
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
	assert.Equal(t, 0, state.tripCount, "a proactive flaky-re-add open must not count as a trip")
	openUntil := state.openUntil
	state.mu.Unlock()
	assert.WithinDuration(t, time.Now().Add(flakyReAddOpenDuration), openUntil, time.Second)

	// Fast-forward past the (short) proactive window without sleeping.
	lapseBlock(t, &b, pod)

	assert.True(t, b.Allow(pod), "the pod is a candidate again once the window elapses")
	assert.False(t, b.IsBlacklisted(pod))
}

// A flap on a pod whose earlier block has already ended is a fresh exclusion,
// reported as the flap and not under the old block's reason.
func TestOnPodFlakyReAdd_AfterALapsedBlockIsAFreshExclusion(t *testing.T) {
	var b PodBlacklist
	pod := blacklistTestPod("1")
	tripBreaker(&b, pod)
	state := lapseBlock(t, &b, pod)

	b.OnPodFlakyReAdd(pod)

	assert.False(t, b.Allow(pod))
	assert.Contains(t, b.BlacklistReason(pod), "flaky delete/re-add")
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.Equal(t, 1, state.tripCount, "a flap leaves the trip count alone")
	assert.WithinDuration(t, time.Now().Add(flakyReAddOpenDuration), state.openUntil, time.Second)
}

// TestOnPodFlakyReAdd_DoesNotShortenExistingExclusion covers a pod that was
// already blacklisted for an unrelated, longer-running reason (a failed probe
// sweep) when the flaky-re-add signal arrives: PodBlacklist is keyed by pod
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

func TestPodBlacklist_SweepStale_RemovesOnlyOldEntries(t *testing.T) {
	var b PodBlacklist // one failed sweep each stays well under the trip count
	stalePod := blacklistTestPod("stale")
	freshPod := blacklistTestPod("fresh")

	b.RecordProbeResult(stalePod, ProbeDegraded, "x")
	b.RecordProbeResult(freshPod, ProbeDegraded, "x")

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

func TestHTTPHealthCheckWithin(t *testing.T) {
	t.Run("2xx response is healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, podHealthCheckPath, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		assert.True(t, httpHealthCheckWithin(strings.TrimPrefix(srv.URL, "http://"), time.Second))
	})

	t.Run("non-2xx response is unhealthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		assert.False(t, httpHealthCheckWithin(strings.TrimPrefix(srv.URL, "http://"), time.Second))
	})

	t.Run("connection failure is unhealthy", func(t *testing.T) {
		assert.False(t, httpHealthCheckWithin("127.0.0.1:1", time.Second)) // nothing listens on port 1
	})

	t.Run("empty address fails open (not blacklisted)", func(t *testing.T) {
		assert.True(t, httpHealthCheckWithin("", time.Second))
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

// Only an engine whose /health is known to be a 2xx while it is up (vLLM, SGLang) is
// required to answer one. xLLM answers 404 on /health on every port while serving
// inference and xDiT has no such route, so for those, and for a pod whose engine is not
// known, the probe must not call the pod dead for that: it settles for an answer.
func TestProbe_EngineWithoutHealthRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	b := &PodBlacklist{}
	for _, engine := range []string{"xllm", "XLLM", "xdit", "trtllm", ""} {
		result, failure := b.probe(addr, "m", engine, time.Second)
		assert.Equal(t, ProbeHealthy, result, "engine %q answers, so it is alive: %s", engine, failure)
	}
	for _, engine := range []string{"vllm", "VLLM", "sglang"} {
		result, _ := b.probe(addr, "", engine, time.Second)
		assert.Equal(t, ProbeDead, result, "engine %q must still need a 2xx /health", engine)
	}

	// A pod with nothing listening is dead whatever its engine.
	for _, engine := range []string{"xllm", "xdit", "", "vllm"} {
		result, _ := b.probe("127.0.0.1:1", "m", engine, time.Second)
		assert.Equal(t, ProbeDead, result, "engine %q", engine)
	}
}

// The probe must not go through a proxy taken from the environment: a proxy that cannot
// reach the pod network answers like a dead pod, and the sweep would block every pod for
// an hour. (httptest listens on loopback, which Go never proxies, so the transport is
// checked directly.)
func TestPodHealthHTTPClient_DoesNotUseTheEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	transport, ok := podHealthHTTPClient.Transport.(*http.Transport)
	require.True(t, ok, "the probe client has a transport of its own, not http.DefaultTransport")

	req, err := http.NewRequest(http.MethodGet, "http://10.1.2.3:8000/health", nil)
	require.NoError(t, err)
	if transport.Proxy != nil {
		proxy, err := transport.Proxy(req)
		require.NoError(t, err)
		assert.Nil(t, proxy, "no proxy is used for a probe")
	}
	assert.NotSame(t, http.DefaultTransport, podHealthHTTPClient.Transport)
}

// A pod whose /v1/models body is slow is degraded, which costs it a minute's backoff at
// worst, not dead, which would block and quarantine it for an hour.
func TestProbe_ASlowModelListIsDegradedNotDead(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == podHealthCheckPath {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	var b PodBlacklist
	result, failure := b.probe(strings.TrimPrefix(srv.URL, "http://"), "test-model", "vllm", 150*time.Millisecond)

	assert.Equal(t, ProbeDegraded, result, failure)
}

func TestFetchModelListWithin(t *testing.T) {
	const model = "test-model"

	serve := func(t *testing.T, h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	fetch := func(addr string) modelListState { return fetchModelListWithin(addr, model, time.Second) }

	t.Run("model present in /v1/models is served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, podModelsCheckPath, r.URL.Path)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other"},{"id":"test-model"}]}`))
		})
		assert.Equal(t, modelListServed, fetch(addr))
	})

	t.Run("model absent is not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"other-model"}]}`))
		})
		assert.Equal(t, modelListNotServed, fetch(addr))
	})

	// The stale-entry listener: 200 with no body, as seen on the recycled port.
	t.Run("empty 2xx body is another listener", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		})
		assert.Equal(t, modelListUnparseable, fetch(addr))
	})

	t.Run("non-JSON 2xx body is another listener", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		})
		assert.Equal(t, modelListUnparseable, fetch(addr))
	})

	t.Run("non-2xx response is not served", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		assert.Equal(t, modelListNotServed, fetch(addr))
	})

	t.Run("connection failure is not served", func(t *testing.T) {
		assert.Equal(t, modelListNotServed, fetch("127.0.0.1:1"))
	})

	// A valid body that arrives after the wait ran out is a slow pod, not another process
	// on the address: it must read as "not served" (degraded), never as "unparseable" (dead,
	// an hour).
	t.Run("a body that does not arrive in time is not another listener", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "64")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		assert.Equal(t, modelListNotServed, fetchModelListWithin(addr, model, 150*time.Millisecond))
	})

	t.Run("a body cut off by a broken connection is not another listener", func(t *testing.T) {
		addr := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "64")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-m`))
		})
		assert.Equal(t, modelListNotServed, fetch(addr))
	})

	t.Run("empty address or model fails open", func(t *testing.T) {
		assert.Equal(t, modelListServed, fetch(""))
		assert.Equal(t, modelListServed, fetchModelListWithin("127.0.0.1:8000", "", time.Second))
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
		pod.Labels = map[string]string{constants.ModelLabelName: model, constants.ModelLabelPort: "8000"}
		return pod
	}
	wantTarget := "(addr=10.0.0.1:8000, model=" + model + ")"

	t.Run("degraded for several sweeps", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()

		tripBreaker(&b, pod)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" blacklisted for")
		assert.Contains(t, logs(), "background probe failed 3 sweeps in a row")
	})

	t.Run("dead for several sweeps", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()

		for i := 0; i < podProbeFailureIterations; i++ {
			b.RecordProbeResult(pod, ProbeDead, "GET /health failed")
		}

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" blacklisted for 1h0m0s: background probe found it dead")
	})

	// The address and model come from the pod: no request is involved.
	t.Run("load outlier", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()

		b.SetLoadExcluded(pod, true)

		assert.Contains(t, logs(), "pod ns/1 "+wantTarget+" blacklisted: running+waiting request count")
	})

	t.Run("flaky re-add", func(t *testing.T) {
		logs := captureKlog(t)
		var b PodBlacklist
		pod := newPod()

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
