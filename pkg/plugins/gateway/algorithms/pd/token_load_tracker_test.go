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

package pd

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"k8s.io/klog/v2"
)

// testTokenLoadConfig keeps the arithmetic in the assertions easy to follow:
// weight 0.5, no fixed cost, one-minute TTL.
func testTokenLoadConfig() TokenLoadConfig {
	return TokenLoadConfig{KVWeight: 0.5, RequestCost: 0, TTL: time.Minute}
}

// fakeClock is an injectable clock for the janitor tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestTokenLoadTracker(t *testing.T, cfg TokenLoadConfig) (*TokenLoadTracker, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	return newTokenLoadTracker(cfg, clock.Now), clock
}

func assertLoad(t *testing.T, tr *TokenLoadTracker, pod string, wantActive, wantKV float64) {
	t.Helper()
	active, kv := tr.GetLoad(pod)
	assert.Equalf(t, wantActive, active, "%s active tokens", pod)
	assert.Equalf(t, wantKV, kv, "%s kv tokens", pod)
}

func TestTokenLoadTracker_Lifecycle(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	assertLoad(t, tr, "pod-a", 0, 0)
	assert.Equal(t, float64(0), tr.GetPriority("pod-a"), "unknown pod is idle")

	tr.AcquirePrefill("req-1", "pod-a", 1000)
	assertLoad(t, tr, "pod-a", 1000, 1000)
	assert.Equal(t, float64(1000+0.5*1000), tr.GetPriority("pod-a"))

	// Prefill returned: compute charge gone, KV still resident.
	tr.ReleaseTokens("req-1")
	assertLoad(t, tr, "pod-a", 0, 1000)
	assert.Equal(t, float64(0.5*1000), tr.GetPriority("pod-a"))

	// Request completed: KV charge gone too.
	tr.ReleaseKVCache("req-1")
	assertLoad(t, tr, "pod-a", 0, 0)
	_, tracked := tr.entries.Load("req-1")
	assert.False(t, tracked, "completed request must be forgotten")
}

func TestTokenLoadTracker_ChargesArePerPod(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	tr.AcquirePrefill("req-1", "pod-a", 8000)
	tr.AcquirePrefill("req-2", "pod-b", 100)
	tr.AcquirePrefill("req-3", "pod-b", 100)

	// One long prompt outweighs two short ones, which is the point of the policy.
	assertLoad(t, tr, "pod-a", 8000, 8000)
	assertLoad(t, tr, "pod-b", 200, 200)
	assert.Greater(t, tr.GetPriority("pod-a"), tr.GetPriority("pod-b"))

	tr.ReleaseAll("req-1")
	assertLoad(t, tr, "pod-a", 0, 0)
	assertLoad(t, tr, "pod-b", 200, 200)
}

func TestTokenLoadTracker_ReleasesAreIdempotent(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	tr.AcquirePrefill("req-1", "pod-a", 500)
	tr.AcquirePrefill("req-2", "pod-a", 500)

	tr.ReleaseTokens("req-1")
	tr.ReleaseTokens("req-1")
	assertLoad(t, tr, "pod-a", 500, 1000)

	tr.ReleaseKVCache("req-1")
	tr.ReleaseKVCache("req-1")
	tr.ReleaseAll("req-1")
	assertLoad(t, tr, "pod-a", 500, 500)

	// ReleaseAll twice on a request whose tokens were never released.
	tr.ReleaseAll("req-2")
	tr.ReleaseAll("req-2")
	assertLoad(t, tr, "pod-a", 0, 0)
}

func TestTokenLoadTracker_UnknownRequestIsNoop(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())
	tr.AcquirePrefill("req-1", "pod-a", 300)

	tr.ReleaseTokens("never-acquired")
	tr.ReleaseKVCache("never-acquired")
	tr.ReleaseAll("never-acquired")

	assertLoad(t, tr, "pod-a", 300, 300)
	assertLoad(t, tr, "never-seen-pod", 0, 0)
}

func TestTokenLoadTracker_CountersClampAtZero(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	// A negative delta with nothing charged must not produce a negative
	// counter that would make the pod look better than idle.
	assert.Equal(t, float64(0), addFloat(&tr.activeTokens, "pod-a", -42))
	assert.Equal(t, float64(0), loadFloat(&tr.activeTokens, "pod-a"))

	assert.Equal(t, float64(10), addFloat(&tr.activeTokens, "pod-a", 10))
	assert.Equal(t, float64(0), addFloat(&tr.activeTokens, "pod-a", -25))
}

func TestTokenLoadTracker_ReacquireSameRequestReplacesCharge(t *testing.T) {
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	defer func() {
		klog.Flush()
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
	}()

	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	// A re-acquire is a caller bug, but it must not leak the first charge.
	tr.AcquirePrefill("req-1", "pod-a", 100)
	tr.AcquirePrefill("req-1", "pod-b", 200)
	assertLoad(t, tr, "pod-a", 0, 0)
	assertLoad(t, tr, "pod-b", 200, 200)
	klog.Flush()
	assert.Contains(t, logs.String(), "re-acquire for request_id=req-1")

	// Only the latest charge is tracked; the release subtracts that one.
	tr.ReleaseAll("req-1")
	assertLoad(t, tr, "pod-a", 0, 0)
	assertLoad(t, tr, "pod-b", 0, 0)
	_, tracked := tr.entries.Load("req-1")
	assert.False(t, tracked)

	// A partially released charge gives back only what it still holds.
	tr.AcquirePrefill("req-2", "pod-a", 100)
	tr.ReleaseTokens("req-2")
	tr.AcquirePrefill("req-3", "pod-a", 50)
	assertLoad(t, tr, "pod-a", 50, 150)
	tr.AcquirePrefill("req-2", "pod-b", 10)
	assertLoad(t, tr, "pod-a", 50, 50)
	assertLoad(t, tr, "pod-b", 10, 10)
}

func TestTokenLoadTracker_KVReleasedBeforeTokens(t *testing.T) {
	tr, clock := newTestTokenLoadTracker(t, testTokenLoadConfig())

	// The request completes while its prefill call is still outstanding: the
	// KV part goes, the active part stays and the entry stays with it.
	tr.AcquirePrefill("req-1", "pod-a", 100)
	tr.ReleaseKVCache("req-1")
	assertLoad(t, tr, "pod-a", 100, 0)
	_, tracked := tr.entries.Load("req-1")
	assert.True(t, tracked, "entry must survive until the active part is released too")

	tr.ReleaseKVCache("req-1") // idempotent
	assertLoad(t, tr, "pod-a", 100, 0)

	tr.ReleaseTokens("req-1")
	assertLoad(t, tr, "pod-a", 0, 0)
	_, tracked = tr.entries.Load("req-1")
	assert.False(t, tracked, "entry is forgotten once both parts are released")

	// ReleaseAll after a KV-first release gives back only the active part.
	tr.AcquirePrefill("req-2", "pod-a", 100)
	tr.ReleaseKVCache("req-2")
	tr.ReleaseAll("req-2")
	assertLoad(t, tr, "pod-a", 0, 0)

	// The janitor does not subtract an already released KV part again.
	tr.AcquirePrefill("req-3", "pod-a", 100)
	tr.AcquirePrefill("req-4", "pod-a", 100)
	tr.ReleaseKVCache("req-3")
	assertLoad(t, tr, "pod-a", 200, 100)
	clock.Advance(2 * time.Minute)
	assert.Equal(t, 2, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 0, 0)
}

func TestTokenLoadTracker_PrefillCostAndEstimate(t *testing.T) {
	tr := newTokenLoadTracker(TokenLoadConfig{KVWeight: 0.3, RequestCost: 3500}, nil)

	assert.Equal(t, float64(3500), tr.PrefillCost(0), "empty prompt still costs the fixed part")
	assert.Equal(t, float64(3500+1200), tr.PrefillCost(1200))
	assert.Equal(t, float64(3500), tr.PrefillCost(-7), "negative token counts are treated as zero")

	assert.Equal(t, 0, EstimatePromptTokens(nil))
	assert.Equal(t, 0, EstimatePromptTokens([]byte("abc")))
	assert.Equal(t, 25, EstimatePromptTokens(bytes.Repeat([]byte("x"), 100)))

	// Defaults are wired into the public constructor.
	cfg := NewTokenLoadTrackerWithConfig(TokenLoadConfig{KVWeight: 0.3, RequestCost: 3500}).Config()
	assert.Equal(t, 0.3, cfg.KVWeight)
	assert.Equal(t, float64(3500), cfg.RequestCost)
	assert.Equal(t, time.Duration(0), cfg.TTL)
}

func TestTokenLoadTracker_DefaultConfigFromEnv(t *testing.T) {
	t.Setenv("AIBRIX_TOKEN_LOAD_KV_WEIGHT", "0.7")
	t.Setenv("AIBRIX_TOKEN_LOAD_REQUEST_COST", "100")
	t.Setenv("AIBRIX_TOKEN_LOAD_TTL_SECONDS", "42")
	t.Setenv("AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS", "7")
	t.Setenv("AIBRIX_TOKEN_LOAD_MAX_SESSIONS", "500")

	cfg := DefaultTokenLoadConfig()
	assert.Equal(t, 0.7, cfg.KVWeight)
	assert.Equal(t, float64(100), cfg.RequestCost)
	assert.Equal(t, 42*time.Second, cfg.TTL)
	assert.Equal(t, 7*time.Second, cfg.SessionTTL)
	assert.Equal(t, 500, cfg.MaxSessions)

	t.Setenv("AIBRIX_TOKEN_LOAD_KV_WEIGHT", "not-a-number")
	t.Setenv("AIBRIX_TOKEN_LOAD_REQUEST_COST", "")
	t.Setenv("AIBRIX_TOKEN_LOAD_TTL_SECONDS", "0")
	t.Setenv("AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS", "-1")
	t.Setenv("AIBRIX_TOKEN_LOAD_MAX_SESSIONS", "0")
	cfg = DefaultTokenLoadConfig()
	assert.Equal(t, DefaultTokenLoadKVWeight, cfg.KVWeight, "invalid value falls back to the default")
	assert.Equal(t, float64(DefaultTokenLoadRequestCost), cfg.RequestCost, "empty value falls back to the default")
	assert.Equal(t, DefaultTokenLoadTTLSeconds*time.Second, cfg.TTL, "non-positive value falls back to the default")
	assert.Equal(t, DefaultTokenLoadSessionTTLSeconds*time.Second, cfg.SessionTTL, "negative value falls back to the default")
	assert.Equal(t, DefaultTokenLoadMaxSessions, cfg.MaxSessions, "non-positive value falls back to the default")

	// 0 is the documented way to turn session tracking off.
	t.Setenv("AIBRIX_TOKEN_LOAD_SESSION_TTL_SECONDS", " 0 ")
	assert.Equal(t, time.Duration(0), DefaultTokenLoadConfig().SessionTTL, "0 disables session tracking")

	// A config without MaxSessions gets the default bound.
	assert.Equal(t, DefaultTokenLoadMaxSessions, NewTokenLoadTrackerWithConfig(TokenLoadConfig{}).Config().MaxSessions)
}

// sessionTestConfig enables session tracking on top of testTokenLoadConfig.
func sessionTestConfig() TokenLoadConfig {
	cfg := testTokenLoadConfig()
	cfg.SessionTTL = 30 * time.Minute
	return cfg
}

func TestTokenLoadTracker_NewTokensWithoutSession(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, sessionTestConfig())

	cases := []struct {
		name         string
		promptTokens int
		matchPct     int
		wantTokens   int
		wantSource   string
	}{
		{"no match info charges the whole prompt", 1000, -1, 1000, NewTokensSourcePrompt},
		{"zero match charges the whole prompt", 1000, 0, 1000, NewTokensSourcePrefixMatch},
		{"partial match charges the uncached part", 1000, 60, 400, NewTokensSourcePrefixMatch},
		{"full match charges nothing", 1000, 100, 0, NewTokensSourcePrefixMatch},
		{"match above 100 is clamped", 1000, 150, 0, NewTokensSourcePrefixMatch},
		{"negative prompt is treated as empty", -5, -1, 0, NewTokensSourcePrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := tr.NewTokens("model", "", tc.promptTokens, tc.matchPct)
			assert.Equal(t, tc.wantTokens, got)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

func TestTokenLoadTracker_NewTokensSessionDelta(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, sessionTestConfig())

	// First turn: nothing to diff against, so the prefix-match rule applies.
	got, source := tr.NewTokens("model", "s1", 1000, 40)
	assert.Equal(t, 600, got)
	assert.Equal(t, NewTokensSourcePrefixMatch, source)

	// Second turn resends the history plus 500 new tokens: only the growth is
	// charged, whatever the prefix cache says.
	got, source = tr.NewTokens("model", "s1", 1500, 0)
	assert.Equal(t, 500, got)
	assert.Equal(t, NewTokensSourceSession, source)

	// A repeated prompt has no growth to charge; fall back to the match rule.
	got, source = tr.NewTokens("model", "s1", 1500, 90)
	assert.Equal(t, 150, got)
	assert.Equal(t, NewTokensSourcePrefixMatch, source)

	// A shorter prompt (history trimmed, new conversation under the same ID)
	// falls back too and becomes the new baseline.
	got, source = tr.NewTokens("model", "s1", 800, -1)
	assert.Equal(t, 800, got)
	assert.Equal(t, NewTokensSourcePrompt, source)
	got, source = tr.NewTokens("model", "s1", 1000, -1)
	assert.Equal(t, 200, got)
	assert.Equal(t, NewTokensSourceSession, source)

	// The same session ID against another model is a different conversation.
	got, source = tr.NewTokens("other-model", "s1", 1000, -1)
	assert.Equal(t, 1000, got)
	assert.Equal(t, NewTokensSourcePrompt, source)
}

func TestTokenLoadTracker_NewTokensSessionDisabled(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig()) // SessionTTL 0

	got, _ := tr.NewTokens("model", "s1", 1000, -1)
	assert.Equal(t, 1000, got)
	got, source := tr.NewTokens("model", "s1", 1500, -1)
	assert.Equal(t, 1500, got, "sessions are not remembered when SessionTTL is 0")
	assert.Equal(t, NewTokensSourcePrompt, source)
}

func TestTokenLoadTracker_NewTokensIgnoresOverlongSessionID(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, sessionTestConfig())

	longID := strings.Repeat("s", maxTokenLoadSessionIDLen+1)
	tr.NewTokens("model", longID, 1000, -1)
	got, source := tr.NewTokens("model", longID, 1500, -1)
	assert.Equal(t, 1500, got, "an over-long session ID is treated as no session")
	assert.Equal(t, NewTokensSourcePrompt, source)
	assert.Equal(t, int64(0), tr.sessionCount.Load(), "nothing is recorded for it")

	maxID := strings.Repeat("s", maxTokenLoadSessionIDLen)
	tr.NewTokens("model", maxID, 1000, -1)
	got, source = tr.NewTokens("model", maxID, 1500, -1)
	assert.Equal(t, 500, got, "an ID at the limit is recorded")
	assert.Equal(t, NewTokensSourceSession, source)
	assert.Equal(t, int64(1), tr.sessionCount.Load())
}

func TestTokenLoadTracker_SessionTableIsBounded(t *testing.T) {
	cfg := sessionTestConfig()
	cfg.MaxSessions = 2
	tr, clock := newTestTokenLoadTracker(t, cfg)

	tr.NewTokens("model", "s1", 1000, -1)
	tr.NewTokens("model", "s2", 1000, -1)
	assert.Equal(t, int64(2), tr.sessionCount.Load())

	// The table is full: s3 is not recorded and keeps being charged by the
	// other rules, while the live sessions still get their delta.
	for i := 0; i < 3; i++ {
		got, source := tr.NewTokens("model", "s3", 1000+500*i, 40)
		assert.Equal(t, (1000+500*i)*60/100, got, "turn %d of an unadmitted session is charged by the prefix rule", i)
		assert.Equal(t, NewTokensSourcePrefixMatch, source)
	}
	assert.Equal(t, int64(2), tr.sessionCount.Load())
	got, source := tr.NewTokens("model", "s1", 1500, 40)
	assert.Equal(t, 500, got)
	assert.Equal(t, NewTokensSourceSession, source)

	// Once the janitor sweeps an idle session, a new one can be admitted.
	clock.Advance(cfg.SessionTTL + time.Minute)
	tr.NewTokens("model", "s1", 1600, -1) // keep s1 alive
	tr.sweepExpired()
	assert.Equal(t, int64(1), tr.sessionCount.Load(), "s2 was swept")
	tr.NewTokens("model", "s3", 1000, -1)
	got, source = tr.NewTokens("model", "s3", 1500, -1)
	assert.Equal(t, 500, got, "s3 is admitted into the freed slot")
	assert.Equal(t, NewTokensSourceSession, source)
	assert.Equal(t, int64(2), tr.sessionCount.Load())
}

// TestTokenLoadTracker_SweptSessionIsNotRefreshed covers the writer side of
// the sweep race: a writer that loaded a session pointer just before the
// janitor removed it must not refresh the orphan but start a new session.
func TestTokenLoadTracker_SweptSessionIsNotRefreshed(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, sessionTestConfig())

	tr.NewTokens("model", "s1", 1000, -1)
	v, ok := tr.sessions.Load(sessionKey("model", "s1"))
	require.True(t, ok)
	orphan := v.(*tokenLoadSession)

	// What sweepExpired does once it finds the session idle.
	orphan.mu.Lock()
	orphan.deleted = true
	tr.sessions.Delete(sessionKey("model", "s1"))
	tr.sessionCount.Add(-1)
	orphan.mu.Unlock()

	got, source := tr.NewTokens("model", "s1", 1500, -1)
	assert.Equal(t, 1500, got, "the swept baseline is gone")
	assert.Equal(t, NewTokensSourcePrompt, source)
	v, ok = tr.sessions.Load(sessionKey("model", "s1"))
	require.True(t, ok, "a fresh session was inserted")
	assert.NotSame(t, orphan, v.(*tokenLoadSession))
	assert.Equal(t, int64(1), tr.sessionCount.Load())
	got, source = tr.NewTokens("model", "s1", 2000, -1)
	assert.Equal(t, 500, got, "the fresh session is the new baseline")
	assert.Equal(t, NewTokensSourceSession, source)
}

// TestTokenLoadTracker_SessionCountSurvivesConcurrentSweeps races writers
// against the janitor and checks the live-session count against the map.
func TestTokenLoadTracker_SessionCountSurvivesConcurrentSweeps(t *testing.T) {
	cfg := sessionTestConfig()
	cfg.SessionTTL = time.Millisecond
	cfg.MaxSessions = 8
	tr := newTokenLoadTracker(cfg, time.Now)

	stop := make(chan struct{})
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for {
			select {
			case <-stop:
				return
			default:
				tr.sweepExpired()
			}
		}
	}()
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 2000; i++ {
				tr.NewTokens("model", fmt.Sprintf("s-%d", (w*7+i)%16), 1000+i, -1)
			}
		}(w)
	}
	writers.Wait()
	close(stop)
	<-sweeperDone

	live := 0
	tr.sessions.Range(func(_, _ any) bool { live++; return true })
	assert.Equal(t, int64(live), tr.sessionCount.Load(), "count matches the map after concurrent admissions and sweeps")
	assert.LessOrEqual(t, live, cfg.MaxSessions)
}

func TestTokenLoadTracker_JanitorForgetsIdleSessions(t *testing.T) {
	cfg := sessionTestConfig()
	cfg.TTL = 0 // charges are never swept here; only sessions are
	tr, clock := newTestTokenLoadTracker(t, cfg)

	tr.AcquirePrefill("req-1", "pod-a", 1000)
	tr.NewTokens("model", "fresh", 1000, -1)
	tr.NewTokens("model", "stale", 1000, -1)
	clock.Advance(20 * time.Minute)
	tr.NewTokens("model", "fresh", 1200, -1) // touches the session

	clock.Advance(15 * time.Minute) // stale idle for 35 min, fresh for 15
	assert.Equal(t, 0, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 1000, 1000)

	got, source := tr.NewTokens("model", "fresh", 1500, -1)
	assert.Equal(t, 300, got)
	assert.Equal(t, NewTokensSourceSession, source)
	got, source = tr.NewTokens("model", "stale", 1500, -1)
	assert.Equal(t, 1500, got, "an idle session is forgotten and starts over")
	assert.Equal(t, NewTokensSourcePrompt, source)
}

func TestTokenLoadTracker_JanitorReleasesStaleCharges(t *testing.T) {
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	defer func() {
		klog.Flush()
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
	}()

	tr, clock := newTestTokenLoadTracker(t, testTokenLoadConfig())

	tr.AcquirePrefill("stale", "pod-a", 1000)
	clock.Advance(30 * time.Second)
	tr.AcquirePrefill("fresh", "pod-a", 10)
	assertLoad(t, tr, "pod-a", 1010, 1010)

	// Nothing is older than the TTL yet.
	assert.Equal(t, 0, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 1010, 1010)

	// 61s after "stale", 31s after "fresh": only the former is over the TTL.
	clock.Advance(31 * time.Second)
	assert.Equal(t, 1, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 10, 10)
	_, tracked := tr.entries.Load("stale")
	assert.False(t, tracked)
	_, tracked = tr.entries.Load("fresh")
	assert.True(t, tracked)

	klog.Flush()
	assert.Contains(t, logs.String(), "force-releasing stale charge")
	assert.Contains(t, logs.String(), "request_id=stale")
	assert.Contains(t, logs.String(), "pod_name=pod-a")

	// A sweep after the normal release of "fresh" finds nothing.
	tr.ReleaseAll("fresh")
	clock.Advance(time.Hour)
	assert.Equal(t, 0, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 0, 0)
}

func TestTokenLoadTracker_JanitorDoesNotDoubleReleaseTokens(t *testing.T) {
	tr, clock := newTestTokenLoadTracker(t, testTokenLoadConfig())

	tr.AcquirePrefill("req-1", "pod-a", 1000)
	tr.AcquirePrefill("req-2", "pod-a", 1000)
	// req-1's prefill returned long ago, only its KV charge is outstanding.
	tr.ReleaseTokens("req-1")
	assertLoad(t, tr, "pod-a", 1000, 2000)

	clock.Advance(2 * time.Minute)
	assert.Equal(t, 2, tr.sweepExpired())
	// req-2's active charge and both KV charges go; req-1's active charge
	// must not be subtracted a second time.
	assertLoad(t, tr, "pod-a", 0, 0)
}

func TestTokenLoadTracker_CloseStopsJanitor(t *testing.T) {
	tr := NewTokenLoadTrackerWithConfig(testTokenLoadConfig())
	require.NotNil(t, tr.janitorDone, "a positive TTL starts the janitor")

	done := make(chan struct{})
	go func() {
		tr.Close()
		tr.Close() // idempotent
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: janitor still running")
	}

	// The tracker stays usable after Close.
	tr.AcquirePrefill("req-1", "pod-a", 1000)
	assertLoad(t, tr, "pod-a", 1000, 1000)

	// Close on a tracker without a janitor returns immediately.
	noJanitor, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())
	noJanitor.Close()
}

func TestTokenLoadTracker_JanitorDisabledWithZeroTTL(t *testing.T) {
	cfg := testTokenLoadConfig()
	cfg.TTL = 0
	tr, clock := newTestTokenLoadTracker(t, cfg)

	tr.AcquirePrefill("req-1", "pod-a", 1000)
	clock.Advance(365 * 24 * time.Hour)
	assert.Equal(t, 0, tr.sweepExpired())
	assertLoad(t, tr, "pod-a", 1000, 1000)
}

// tokenLoadSeriesPublished reports whether the default registry currently
// exports a series of metricName for pod.
func tokenLoadSeriesPublished(t *testing.T, metricName, pod string) bool {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "pod_name" && label.GetValue() == pod {
					return true
				}
			}
		}
	}
	return false
}

func assertPodTracked(t *testing.T, tr *TokenLoadTracker, pod string, want bool) {
	t.Helper()
	_, active := tr.activeTokens.Load(pod)
	_, kv := tr.kvTokens.Load(pod)
	assert.Equal(t, want, active, "active counter for %s tracked", pod)
	assert.Equal(t, want, kv, "kv counter for %s tracked", pod)
	assert.Equal(t, want, tokenLoadSeriesPublished(t, metrics.PDTokenLoadActiveTokens, pod), "active series for %s", pod)
	assert.Equal(t, want, tokenLoadSeriesPublished(t, metrics.PDTokenLoadKVTokens, pod), "kv series for %s", pod)
}

func TestTokenLoadTracker_JanitorPrunesIdlePods(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	tr.AcquirePrefill("req-1", "prune-a", 100)
	tr.AcquirePrefill("req-2", "prune-b", 100)
	tr.ReleaseAll("req-1")
	assertPodTracked(t, tr, "prune-a", true)
	assertPodTracked(t, tr, "prune-b", true)

	// prune-a was written since the last sweep, so it is only observed idle;
	// prune-b still carries a charge.
	assert.Equal(t, 0, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-a", true)

	// Traffic between sweeps keeps a pod alive even if it is back at zero.
	tr.AcquirePrefill("req-3", "prune-a", 10)
	tr.ReleaseAll("req-3")
	assert.Equal(t, 0, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-a", true)

	// A full quiet interval at zero gets the pod pruned; a pod with a
	// charge outstanding is never pruned, however long it stays untouched.
	assert.Equal(t, 1, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-a", false)
	assertPodTracked(t, tr, "prune-b", true)
	assertLoad(t, tr, "prune-a", 0, 0)
	assertLoad(t, tr, "prune-b", 100, 100)

	// The next charge re-creates the pod from zero.
	tr.AcquirePrefill("req-4", "prune-a", 7)
	assertPodTracked(t, tr, "prune-a", true)
	assertLoad(t, tr, "prune-a", 7, 7)

	// A pod whose KV part is still resident is not pruned either.
	tr.ReleaseTokens("req-2")
	assert.Equal(t, 0, tr.pruneIdle())
	assert.Equal(t, 0, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-b", true)
	tr.ReleaseKVCache("req-2")
	assert.Equal(t, 0, tr.pruneIdle())
	assert.Equal(t, 1, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-b", false)
}

func TestTokenLoadTracker_JanitorPrunesAfterForceRelease(t *testing.T) {
	tr, clock := newTestTokenLoadTracker(t, testTokenLoadConfig())

	// A leaked charge on a pod that is gone: the sweep releases it, and the
	// pod is pruned once nothing has touched it for another interval.
	tr.AcquirePrefill("leaked", "prune-c", 100)
	clock.Advance(2 * time.Minute)
	assert.Equal(t, 1, tr.sweepExpired())
	assert.Equal(t, 0, tr.pruneIdle())
	assert.Equal(t, 1, tr.pruneIdle())
	assertPodTracked(t, tr, "prune-c", false)
}

func TestTokenLoadTracker_PruneRacesWithCharges(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	const (
		workers   = 8
		perWorker = 500
		cost      = 3.0
	)
	stop := make(chan struct{})
	var pruner sync.WaitGroup
	pruner.Add(1)
	go func() {
		defer pruner.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tr.pruneIdle()
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				id := fmt.Sprintf("race-w%d-r%d", w, i)
				tr.AcquirePrefill(id, "prune-race", cost)
				tr.ReleaseTokens(id)
				tr.ReleaseKVCache(id)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	pruner.Wait()

	// Every charge was balanced by its releases whether or not the pruner
	// dropped the counters in between, so nothing is left on the pod, and
	// at most two more quiet sweeps drop it (the pruner may already have).
	assertLoad(t, tr, "prune-race", 0, 0)
	tr.pruneIdle()
	tr.pruneIdle()
	assertPodTracked(t, tr, "prune-race", false)
}

func TestTokenLoadTracker_ConcurrentChargesBalance(t *testing.T) {
	tr, _ := newTestTokenLoadTracker(t, testTokenLoadConfig())

	const (
		workers    = 16
		perWorker  = 200
		cost       = 7.0
		totalCount = workers * perWorker
	)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tr.AcquirePrefill(fmt.Sprintf("w%d-r%d", w, i), "pod-a", cost)
			}
		}(w)
	}
	wg.Wait()
	assertLoad(t, tr, "pod-a", totalCount*cost, totalCount*cost)

	// Release tokens and KV from different goroutines, some of them twice.
	for w := 0; w < workers; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tr.ReleaseTokens(fmt.Sprintf("w%d-r%d", w, i))
				tr.ReleaseTokens(fmt.Sprintf("w%d-r%d", w, i))
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tr.ReleaseAll(fmt.Sprintf("w%d-r%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	assertLoad(t, tr, "pod-a", 0, 0)

	count := 0
	tr.entries.Range(func(_, _ any) bool { count++; return true })
	require.Equal(t, 0, count, "every request must be forgotten after release")
}
