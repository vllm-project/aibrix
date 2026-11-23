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

package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vllm-project/aibrix/pkg/cache/discovery"
)

type discoveryTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *discoveryTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *discoveryTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// switchableCheck fails while its error is set.
type switchableCheck struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (s *switchableCheck) set(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *switchableCheck) check(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.err
}

func (s *switchableCheck) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newTestDiscoveryChecker(t *testing.T, opts DiscoveryCheckOptions) (*DiscoveryChecker, *discoveryTestClock) {
	t.Helper()
	checker, err := NewDiscoveryChecker(opts)
	require.NoError(t, err)
	clock := &discoveryTestClock{now: time.Unix(1_700_000_000, 0)}
	checker.now = clock.Now
	checker.started = clock.Now()
	checker.lastVerified = clock.Now()
	return checker, clock
}

func TestDiscoveryCheckerUnreachableAfterFailuresAndRecovers(t *testing.T) {
	reach := &switchableCheck{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second, Reachability: reach.check,
	})
	ctx := context.Background()

	require.True(t, checker.CheckOnce(ctx))
	assert.Zero(t, checker.ReachabilityAge())
	assert.False(t, checker.Unreachable())

	reach.set(errors.New("connection refused"))
	for i := 1; i <= 3; i++ {
		clock.Advance(10 * time.Second)
		require.False(t, checker.CheckOnce(ctx))
		assert.Equal(t, time.Duration(i)*10*time.Second, checker.ReachabilityAge())
	}
	assert.True(t, checker.Unreachable(), "30 s without a successful check reaches the threshold")
	assert.Equal(t, 3.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)))
	assert.Equal(t, 1.0, testutil.ToFloat64(checker.unreachable))
	assert.Equal(t, 30.0, testutil.ToFloat64(checker.age))

	reach.set(nil)
	clock.Advance(10 * time.Second)
	require.True(t, checker.CheckOnce(ctx))
	assert.Zero(t, checker.ReachabilityAge())
	assert.False(t, checker.Unreachable())
	assert.Equal(t, 0.0, testutil.ToFloat64(checker.unreachable))
	assert.Equal(t, 3.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)),
		"a success does not reset the failure counter")
}

func TestDiscoveryCheckerAgeGrowsWithoutChecks(t *testing.T) {
	// The age is computed when read, so a check goroutine that stopped
	// running still shows up as a growing age.
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: time.Second, UnreachableThreshold: 3 * time.Second, Reachability: (&switchableCheck{}).check,
	})
	clock.Advance(5 * time.Second)
	assert.Equal(t, 5.0, testutil.ToFloat64(checker.age))
	assert.Equal(t, 1.0, testutil.ToFloat64(checker.unreachable))
}

func TestDiscoveryCheckerBoundsEachCheck(t *testing.T) {
	var gotDeadline time.Duration
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, UnreachableThreshold: 3 * time.Second,
		Reachability: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			gotDeadline = time.Until(deadline)
			<-ctx.Done() // a hung API server
			return ctx.Err()
		},
	})
	require.NoError(t, err)

	started := time.Now()
	require.False(t, checker.CheckOnce(context.Background()))
	elapsed := time.Since(started)
	assert.LessOrEqual(t, gotDeadline, 500*time.Millisecond, "the deadline is half the interval")
	assert.GreaterOrEqual(t, elapsed, 400*time.Millisecond, "the check used its deadline")
	assert.Less(t, elapsed, time.Second, "a hung check ends before the next tick is due")
	assert.Equal(t, 1.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)))
}

func TestDiscoveryCheckerRunStopsAndDoesNotOverlap(t *testing.T) {
	var mu sync.Mutex
	running, maxRunning, calls := 0, 0, 0
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: 20 * time.Millisecond, UnreachableThreshold: time.Second,
		Reachability: func(ctx context.Context) error {
			mu.Lock()
			running++
			calls++
			maxRunning = max(maxRunning, running)
			mu.Unlock()
			<-ctx.Done() // every check uses its full deadline
			mu.Lock()
			running--
			mu.Unlock()
			return ctx.Err()
		},
	})
	require.NoError(t, err)

	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		checker.Run(stopCh)
		close(done)
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 3
	}, 2*time.Second, 5*time.Millisecond)
	close(stopCh)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after stopCh closed")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, maxRunning)
}

func TestDiscoveryCheckerShutdownIsNotAFailure(t *testing.T) {
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, UnreachableThreshold: 3 * time.Second,
		Reachability: func(ctx context.Context) error { return ctx.Err() },
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, checker.CheckOnce(ctx))
	assert.Equal(t, 0.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)))
}

func TestNewDiscoveryCheckerValidates(t *testing.T) {
	reach := (&switchableCheck{}).check
	for _, tc := range []struct {
		name string
		opts DiscoveryCheckOptions
		want string
	}{
		{"zero interval", DiscoveryCheckOptions{UnreachableThreshold: time.Second, Reachability: reach}, "interval must be positive"},
		{"threshold not above interval", DiscoveryCheckOptions{Interval: time.Second, UnreachableThreshold: time.Second, Reachability: reach}, "at least twice"},
		{"threshold below twice interval", DiscoveryCheckOptions{Interval: 10 * time.Second, UnreachableThreshold: 11 * time.Second, Reachability: reach}, "at least twice"},
		{"no reachability", DiscoveryCheckOptions{Interval: time.Second, UnreachableThreshold: 3 * time.Second}, "reachability check is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDiscoveryChecker(tc.opts)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestDiscoveryCheckerMetricNames(t *testing.T) {
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, UnreachableThreshold: 3 * time.Second, Reachability: (&switchableCheck{}).check,
	})
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	require.NoError(t, checker.Register(reg))
	families, err := reg.Gather()
	require.NoError(t, err)
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	assert.ElementsMatch(t, []string{
		"aibrix_gateway_discovery_reachability_age_seconds",
		"aibrix_gateway_discovery_unreachable",
		"aibrix_gateway_discovery_verification_failures_total",
	}, names)
	n, err := testutil.GatherAndCount(reg, "aibrix_gateway_discovery_verification_failures_total")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the reachability series exists while consistency is off")
}

func TestPodListReachabilityListsOnePod(t *testing.T) {
	client := fake.NewSimpleClientset()
	var got metav1.ListOptions
	var namespace string
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		list := action.(k8stesting.ListActionImpl)
		got, namespace = list.ListOptions, list.Namespace
		return false, nil, nil
	})
	require.NoError(t, PodListReachability(client)(context.Background()))
	assert.Equal(t, int64(1), got.Limit)
	assert.Empty(t, got.ResourceVersion, "a consistent read, not the watch cache")
	assert.Equal(t, metav1.NamespaceAll, namespace)

	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	require.Error(t, PodListReachability(client)(context.Background()))
}

func TestStartDiscoveryCheckerOnlyForKubernetes(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	config := &rest.Config{Host: "https://127.0.0.1:1"}
	kubernetesProvider := discovery.NewKubernetesProvider(config)
	opts := InitOptions{DiscoveryCheckInterval: time.Second, DiscoveryUnreachableThreshold: 3 * time.Second}

	for _, tc := range []struct {
		name     string
		config   *rest.Config
		provider discovery.Provider
		opts     InitOptions
		started  bool
	}{
		{"kubernetes", config, kubernetesProvider, opts, true},
		{"disabled by flag", config, kubernetesProvider, InitOptions{DiscoveryUnreachableThreshold: 3 * time.Second}, false},
		{"standalone", nil, discovery.NewStaticProvider("unused.yaml"), opts, false},
		{"no rest config", nil, kubernetesProvider, opts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			checker := startDiscoveryChecker(&Store{}, tc.config, tc.provider, tc.opts, reg, stopCh)
			assert.Equal(t, tc.started, checker != nil)
			families, err := reg.Gather()
			require.NoError(t, err)
			if tc.started {
				assert.Len(t, families, 3)
			} else {
				assert.Empty(t, families, "no discovery metric is reported when the check does not run")
			}
		})
	}
}

func TestStartDiscoveryCheckerWiresTheModelListHealth(t *testing.T) {
	// With --model-list-discovery-max-age set, the checker must drive the
	// same ModelListHealth that /v1/models uses, and export its gauge.
	stopCh := make(chan struct{})
	defer close(stopCh)
	config := &rest.Config{Host: "https://127.0.0.1:1"}
	provider := discovery.NewKubernetesProvider(config).WithModelListHealth(time.Minute)
	health := provider.ModelListHealth()
	store := &Store{modelListHealth: health}
	reg := prometheus.NewRegistry()
	checker := startDiscoveryChecker(store, config, provider,
		InitOptions{DiscoveryCheckInterval: time.Second, DiscoveryUnreachableThreshold: 3 * time.Second}, reg, stopCh)
	require.NotNil(t, checker)
	assert.Same(t, health, checker.consistency, "the checker verifies the /v1/models instance")

	families, err := reg.Gather()
	require.NoError(t, err)
	assert.Len(t, families, 4)
	n, err := testutil.GatherAndCount(reg, "aibrix_gateway_discovery_verification_failures_total")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "reachability and consistency series")
	n, err = testutil.GatherAndCount(reg, "aibrix_gateway_discovery_consistency_last_verified_age_seconds")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// fakeVerifier is a scripted ConsistencyVerifier. Each EnsureVerified call
// returns the next scripted error (nil when the script is empty).
type fakeVerifier struct {
	mu        sync.Mutex
	errs      []error
	calls     int
	matched   time.Time
	deadlines []bool
}

func (f *fakeVerifier) EnsureVerified(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	_, hasDeadline := ctx.Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeVerifier) LastMatched() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.matched
}

func (f *fakeVerifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// scriptedWait stands in for the consistency loop's wait: it records each
// requested duration and returns the next scripted result, or cancels once
// the script runs out, which ends the loop.
type scriptedWait struct {
	waits   []time.Duration
	results []waitResult
}

func (w *scriptedWait) wait(_ context.Context, d time.Duration, _ <-chan struct{}) waitResult {
	w.waits = append(w.waits, d)
	if len(w.results) == 0 {
		return waitCancelled
	}
	r := w.results[0]
	w.results = w.results[1:]
	return r
}

var errListFailed = errors.New("verify model discovery: connection refused")

func newLoopChecker(t *testing.T, verifier *fakeVerifier, script *scriptedWait) *DiscoveryChecker {
	t.Helper()
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second,
		Reachability: (&switchableCheck{}).check, Consistency: verifier,
	})
	require.NoError(t, err)
	checker.maxBackoff = 40 * time.Second
	checker.wait = script.wait
	checker.jitter = func() time.Duration { return 3 * time.Second }
	return checker
}

func elapsed(n int) []waitResult {
	r := make([]waitResult, n)
	for i := range r {
		r[i] = waitElapsed
	}
	return r
}

func TestConsistencyLoopBacksOffAndResets(t *testing.T) {
	verifier := &fakeVerifier{errs: []error{errListFailed, errListFailed, errListFailed, errListFailed, nil, errListFailed}}
	script := &scriptedWait{results: elapsed(6)}
	checker := newLoopChecker(t, verifier, script)
	checker.runConsistency(context.Background())

	// Each failure doubles the wait up to the 40 s cap; a match resets it.
	assert.Equal(t, []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 40 * time.Second,
		40 * time.Second, 10 * time.Second, 20 * time.Second}, script.waits)
	assert.Equal(t, 5.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckConsistency)))
	for _, hadDeadline := range verifier.deadlines {
		assert.False(t, hadDeadline, "the consistency check bounds itself; a checker deadline would skip its backoff")
	}
}

func TestConsistencyLoopDeferredIsNeitherSuccessNorFailure(t *testing.T) {
	// A retry-window result made no request: it must not be counted or move
	// the wait in either direction.
	verifier := &fakeVerifier{errs: []error{errListFailed, discovery.ErrModelListVerificationDeferred,
		discovery.ErrModelListVerificationDeferred, errListFailed}}
	script := &scriptedWait{results: elapsed(4)}
	checker := newLoopChecker(t, verifier, script)
	checker.runConsistency(context.Background())

	assert.Equal(t, []time.Duration{10 * time.Second, 20 * time.Second, 20 * time.Second, 20 * time.Second,
		40 * time.Second}, script.waits)
	assert.Equal(t, 2.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckConsistency)))
}

func TestConsistencyLoopReconnectResetsWaitWithoutImmediateCheck(t *testing.T) {
	verifier := &fakeVerifier{errs: []error{errListFailed, errListFailed, errListFailed}}
	script := &scriptedWait{results: []waitResult{waitElapsed, waitElapsed, waitWoken, waitElapsed}}
	checker := newLoopChecker(t, verifier, script)
	checker.runConsistency(context.Background())

	// After the wake the wait is one interval plus jitter, not the 40 s
	// outage-era backoff, and the wake itself runs no check: three checks for
	// three elapsed waits.
	assert.Equal(t, []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 13 * time.Second,
		26 * time.Second}, script.waits)
	assert.Equal(t, 3, verifier.callCount())
}

func TestReachabilityRecoveryWakesConsistencyOnce(t *testing.T) {
	reach := &switchableCheck{}
	checker, _ := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second,
		Reachability: reach.check, Consistency: &fakeVerifier{},
	})
	ctx := context.Background()
	woken := func() bool {
		select {
		case <-checker.reconnected:
			return true
		default:
			return false
		}
	}
	require.True(t, checker.CheckOnce(ctx))
	assert.False(t, woken(), "no wake without a prior failure")
	reach.set(errors.New("connection refused"))
	require.False(t, checker.CheckOnce(ctx))
	require.False(t, checker.CheckOnce(ctx))
	assert.False(t, woken())
	reach.set(nil)
	require.True(t, checker.CheckOnce(ctx))
	assert.True(t, woken(), "the first success after a failure wakes the consistency loop")
	require.True(t, checker.CheckOnce(ctx))
	assert.False(t, woken())
}

func TestConsistencyDoesNotDriveUnreachable(t *testing.T) {
	verifier := &fakeVerifier{}
	reach := &switchableCheck{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second,
		Reachability: reach.check, Consistency: verifier,
	})
	for i := 0; i < 6; i++ {
		verifier.errs = append(verifier.errs, discovery.ErrModelListDiscoveryUnavailable)
	}
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		clock.Advance(10 * time.Second)
		require.True(t, checker.CheckOnce(ctx))
		checker.CheckConsistencyOnce(ctx)
	}
	assert.False(t, checker.Unreachable(), "a minute of consistency failures with reachability fine is not stale")
	assert.Equal(t, 60*time.Second, checker.ConsistencyAge())
	assert.Equal(t, 6.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckConsistency)))
}

func TestConsistencyAgeReadsTheVerifier(t *testing.T) {
	verifier := &fakeVerifier{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second,
		Reachability: (&switchableCheck{}).check, Consistency: verifier,
	})
	clock.Advance(15 * time.Second)
	assert.Equal(t, 15*time.Second, checker.ConsistencyAge(), "counted from gateway start before any match")

	verifier.mu.Lock()
	verifier.matched = clock.Now().Add(-5 * time.Second)
	verifier.mu.Unlock()
	assert.Equal(t, 5*time.Second, checker.ConsistencyAge())
	assert.Equal(t, 5.0, testutil.ToFloat64(checker.consistencyAge))

	// A check that reused a fresh proof (nil, no new match) does not lower it.
	clock.Advance(10 * time.Second)
	checker.CheckConsistencyOnce(context.Background())
	assert.Equal(t, 15*time.Second, checker.ConsistencyAge())
}

func TestReachabilityFreshnessCountsFromCheckStart(t *testing.T) {
	// A LIST may answer from the moment it started, so a check that takes
	// 4 s and succeeds leaves the age at 4 s, not 0.
	var clock *discoveryTestClock
	slow := func(context.Context) error {
		clock.Advance(4 * time.Second)
		return nil
	}
	checker, c := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 20 * time.Second, Reachability: slow,
	})
	clock = c
	clock.Advance(10 * time.Second)
	require.True(t, checker.CheckOnce(context.Background()))
	assert.Equal(t, 4*time.Second, checker.ReachabilityAge())
}

func TestHealthySlowCheckIsNeverUnreachableAtTheMinimumThreshold(t *testing.T) {
	// Review probe: with interval 10 s, a check that takes the full 5 s
	// budget can be scraped at an age of 15 s. The minimum threshold, twice
	// the interval, must still read healthy.
	var clock *discoveryTestClock
	var midCheckUnreachable []bool
	var checker *DiscoveryChecker
	slow := func(context.Context) error {
		clock.Advance(5 * time.Second)
		midCheckUnreachable = append(midCheckUnreachable, checker.Unreachable())
		return nil
	}
	ch, c := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 20 * time.Second, Reachability: slow,
	})
	checker, clock = ch, c
	// The ticker fires every 10 s from the previous tick, so after a 5 s check
	// the next one starts 5 s later. The worst scrape is mid-check: 15 s.
	clock.Advance(10 * time.Second)
	for i := 0; i < 5; i++ {
		require.True(t, checker.CheckOnce(context.Background()))
		assert.False(t, checker.Unreachable())
		clock.Advance(5 * time.Second)
	}
	assert.Equal(t, []bool{false, false, false, false, false}, midCheckUnreachable)
	assert.Equal(t, 5*time.Second+5*time.Second, checker.ReachabilityAge(), "just before the next tick")
}

func TestFirstReachabilityFailureOfAStreakIsLoggedOnce(t *testing.T) {
	reach := &switchableCheck{}
	checker, _ := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second, Reachability: reach.check,
	})
	logged := 0
	checker.logFirstFailure = func(error) { logged++ }
	ctx := context.Background()

	reach.set(errors.New("connection refused"))
	checker.CheckOnce(ctx)
	checker.CheckOnce(ctx)
	assert.Equal(t, 1, logged, "only the first failure of a streak is logged")

	// A cancel at shutdown neither extends the streak nor logs again.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	checker.CheckOnce(cancelled)
	assert.Equal(t, 1, logged)
	assert.Equal(t, 2, checker.failureStreak)

	reach.set(nil)
	checker.CheckOnce(ctx)
	reach.set(errors.New("connection refused"))
	checker.CheckOnce(ctx)
	assert.Equal(t, 2, logged, "a success ends the streak, so the next failure is logged")
}

func TestConsistencyAgeKeepsAMatchFromBeforeTheChecker(t *testing.T) {
	// /v1/models can verify before the checker starts; that match is real.
	verifier := &fakeVerifier{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, UnreachableThreshold: 30 * time.Second,
		Reachability: (&switchableCheck{}).check, Consistency: verifier,
	})
	verifier.mu.Lock()
	verifier.matched = clock.Now().Add(-20 * time.Second)
	verifier.mu.Unlock()
	assert.Equal(t, 20*time.Second, checker.ConsistencyAge())
}
