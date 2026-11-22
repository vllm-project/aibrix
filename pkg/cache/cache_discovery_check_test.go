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
	checker.lastVerified = clock.Now()
	checker.consistencyVerified = clock.Now()
	return checker, clock
}

func TestDiscoveryCheckerStaleAfterFailuresAndRecovers(t *testing.T) {
	reach := &switchableCheck{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, StaleThreshold: 30 * time.Second, Reachability: reach.check,
	})
	ctx := context.Background()

	require.True(t, checker.CheckOnce(ctx))
	assert.Zero(t, checker.Age())
	assert.False(t, checker.Stale())

	reach.set(errors.New("connection refused"))
	for i := 1; i <= 3; i++ {
		clock.Advance(10 * time.Second)
		require.False(t, checker.CheckOnce(ctx))
		assert.Equal(t, time.Duration(i)*10*time.Second, checker.Age())
	}
	assert.True(t, checker.Stale(), "30 s without a successful check reaches the threshold")
	assert.Equal(t, 3.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)))
	assert.Equal(t, 1.0, testutil.ToFloat64(checker.stale))
	assert.Equal(t, 30.0, testutil.ToFloat64(checker.age))

	reach.set(nil)
	clock.Advance(10 * time.Second)
	require.True(t, checker.CheckOnce(ctx))
	assert.Zero(t, checker.Age())
	assert.False(t, checker.Stale())
	assert.Equal(t, 0.0, testutil.ToFloat64(checker.stale))
	assert.Equal(t, 3.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)),
		"a success does not reset the failure counter")
}

func TestDiscoveryCheckerAgeGrowsWithoutChecks(t *testing.T) {
	// The age is computed when read, so a check goroutine that stopped
	// running still shows up as a growing age.
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: time.Second, StaleThreshold: 3 * time.Second, Reachability: (&switchableCheck{}).check,
	})
	clock.Advance(5 * time.Second)
	assert.Equal(t, 5.0, testutil.ToFloat64(checker.age))
	assert.Equal(t, 1.0, testutil.ToFloat64(checker.stale))
}

func TestDiscoveryCheckerConsistencyDoesNotDriveStale(t *testing.T) {
	// The consistency check is exact equality, so churn during its LIST can
	// fail it on a healthy replica. It is reported on its own gauge and must
	// not make the replica stale while reachability succeeds.
	reach := &switchableCheck{}
	consistency := &switchableCheck{}
	checker, clock := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, StaleThreshold: 30 * time.Second,
		Reachability: reach.check, Consistency: consistency.check,
	})
	ctx := context.Background()

	consistency.set(discovery.ErrModelListDiscoveryUnavailable)
	for i := 0; i < 6; i++ {
		clock.Advance(10 * time.Second)
		require.True(t, checker.CheckOnce(ctx))
		require.False(t, checker.CheckConsistencyOnce(ctx))
	}
	assert.False(t, checker.Stale(), "a minute of consistency failures with reachability fine is not stale")
	assert.Zero(t, checker.Age())
	assert.Equal(t, 60*time.Second, checker.ConsistencyAge())
	assert.Equal(t, 60.0, testutil.ToFloat64(checker.consistencyAge))
	assert.Equal(t, 6.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckConsistency)))
	assert.Equal(t, 0.0, testutil.ToFloat64(checker.failures.WithLabelValues(DiscoveryCheckReachability)))

	consistency.set(nil)
	clock.Advance(10 * time.Second)
	require.True(t, checker.CheckConsistencyOnce(ctx))
	assert.Zero(t, checker.ConsistencyAge())
}

func TestDiscoveryCheckerConsistencyHasNoCheckerDeadline(t *testing.T) {
	// A checker deadline would end a slow LIST as if its caller had gone
	// away, and ModelListHealth skips its backoff for that case (review D1).
	var hadDeadline bool
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, StaleThreshold: 3 * time.Second,
		Reachability: (&switchableCheck{}).check,
		Consistency: func(ctx context.Context) error {
			_, hadDeadline = ctx.Deadline()
			return nil
		},
	})
	require.NoError(t, err)
	require.True(t, checker.CheckConsistencyOnce(context.Background()))
	assert.False(t, hadDeadline, "the consistency check must bound itself")
}

func TestDiscoveryCheckerConsistencyBacksOff(t *testing.T) {
	// A consistency check that keeps failing (for example a full LIST slower
	// than its budget on a large cluster) must not run every interval.
	var mu sync.Mutex
	var calls []time.Time
	fail := true
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: 10 * time.Millisecond, StaleThreshold: time.Hour,
		Reachability: (&switchableCheck{}).check,
		Consistency: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, time.Now())
			if fail {
				return discovery.ErrModelListDiscoveryUnavailable
			}
			return nil
		},
	})
	require.NoError(t, err)
	checker.maxBackoff = 80 * time.Millisecond

	stopCh := make(chan struct{})
	go checker.Run(stopCh)
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	failing := len(calls)
	fail = false
	mu.Unlock()
	// Waits of 10, 20, 40, 80, 80... ms give about 8 calls in 500 ms; without
	// backoff it would be about 50.
	assert.LessOrEqual(t, failing, 12, "failures back off")
	assert.GreaterOrEqual(t, failing, 4, "failures are still retried")

	time.Sleep(300 * time.Millisecond)
	close(stopCh)
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, len(calls)-failing, 5, "after a success the interval resets")
}

func TestDiscoveryCheckerFreshnessCountsFromCheckStart(t *testing.T) {
	// A LIST may answer from the moment it started, so a check that takes
	// 15 s and succeeds leaves the age at 15 s, not 0.
	var clock *discoveryTestClock
	slow := func(context.Context) error {
		clock.Advance(15 * time.Second)
		return nil
	}
	checker, c := newTestDiscoveryChecker(t, DiscoveryCheckOptions{
		Interval: 10 * time.Second, StaleThreshold: 30 * time.Second,
		Reachability: slow, Consistency: slow,
	})
	clock = c
	clock.Advance(10 * time.Second)
	require.True(t, checker.CheckOnce(context.Background()))
	assert.Equal(t, 15*time.Second, checker.Age())
	require.True(t, checker.CheckConsistencyOnce(context.Background()))
	assert.Equal(t, 15*time.Second, checker.ConsistencyAge())
}

func TestDiscoveryCheckerBoundsEachCheck(t *testing.T) {
	var gotDeadline time.Duration
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, StaleThreshold: 3 * time.Second,
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
		Interval: 20 * time.Millisecond, StaleThreshold: time.Second,
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
		Interval: time.Second, StaleThreshold: 3 * time.Second,
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
		{"zero interval", DiscoveryCheckOptions{StaleThreshold: time.Second, Reachability: reach}, "interval must be positive"},
		{"threshold not above interval", DiscoveryCheckOptions{Interval: time.Second, StaleThreshold: time.Second, Reachability: reach}, "must be greater than"},
		{"no reachability", DiscoveryCheckOptions{Interval: time.Second, StaleThreshold: 2 * time.Second}, "reachability check is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDiscoveryChecker(tc.opts)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestDiscoveryCheckerMetricNames(t *testing.T) {
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, StaleThreshold: 3 * time.Second, Reachability: (&switchableCheck{}).check,
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
		"aibrix_gateway_discovery_last_verified_age_seconds",
		"aibrix_gateway_discovery_stale",
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
	opts := InitOptions{DiscoveryCheckInterval: time.Second, DiscoveryStaleThreshold: 3 * time.Second}

	for _, tc := range []struct {
		name     string
		config   *rest.Config
		provider discovery.Provider
		opts     InitOptions
		started  bool
	}{
		{"kubernetes", config, kubernetesProvider, opts, true},
		{"disabled by flag", config, kubernetesProvider, InitOptions{DiscoveryStaleThreshold: 3 * time.Second}, false},
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

func TestDiscoveryCheckerConsistencyMetrics(t *testing.T) {
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: time.Second, StaleThreshold: 3 * time.Second,
		Reachability: (&switchableCheck{}).check, Consistency: (&switchableCheck{}).check,
	})
	require.NoError(t, err)
	reg := prometheus.NewRegistry()
	require.NoError(t, checker.Register(reg))
	n, err := testutil.GatherAndCount(reg, "aibrix_gateway_discovery_consistency_last_verified_age_seconds")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = testutil.GatherAndCount(reg, "aibrix_gateway_discovery_verification_failures_total")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both check series exist while consistency is on")
}

func TestDiscoveryCheckerConsistencyRunsWhenReachabilityRecovers(t *testing.T) {
	// Consistency failures during an outage grow its backoff. Once the API
	// server answers again, the consistency gauge must not lag by that
	// backoff: the loop checks at once and resets its wait.
	reach := &switchableCheck{}
	consistency := &switchableCheck{}
	consistency.set(discovery.ErrModelListDiscoveryUnavailable)
	checker, err := NewDiscoveryChecker(DiscoveryCheckOptions{
		Interval: 20 * time.Millisecond, StaleThreshold: time.Hour,
		Reachability: reach.check, Consistency: consistency.check,
	})
	require.NoError(t, err)
	checker.maxBackoff = time.Hour

	stopCh := make(chan struct{})
	defer close(stopCh)
	go checker.Run(stopCh)

	// Waits of 20, 40, 80, 160 ms: after 400 ms the next one is 320 ms away.
	time.Sleep(400 * time.Millisecond)
	reach.set(errors.New("connection refused"))
	time.Sleep(60 * time.Millisecond)
	consistency.set(nil)
	before := consistency.callCount()
	reach.set(nil)
	recoveredAt := time.Now()

	require.Eventually(t, func() bool { return consistency.callCount() > before },
		150*time.Millisecond, 5*time.Millisecond, "consistency did not run soon after reachability recovered")
	assert.Less(t, time.Since(recoveredAt), 200*time.Millisecond)
	require.Eventually(t, func() bool { return checker.ConsistencyAge() < 100*time.Millisecond },
		time.Second, 5*time.Millisecond)
}
