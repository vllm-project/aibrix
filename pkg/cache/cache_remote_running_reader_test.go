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
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const testReaderInterval = time.Second

// commandCounter counts the Redis commands a client sends, by name, whether alone or pipelined.
type commandCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (h *commandCounter) add(cmds ...redis.Cmder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts = map[string]int{}
	}
	for _, cmd := range cmds {
		h.counts[cmd.Name()]++
	}
}

func (h *commandCounter) snapshot() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.counts))
	for k, v := range h.counts {
		out[k] = v
	}
	return out
}

// since is the commands sent since before, a snapshot taken earlier.
func (h *commandCounter) since(before map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range h.snapshot() {
		if d := v - before[k]; d > 0 {
			out[k] = d
		}
	}
	return out
}

func (h *commandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *commandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.add(cmd); return next(ctx, cmd) }
}

func (h *commandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.add(cmds...); return next(ctx, cmds) }
}

// pipelineHook runs before for every pipeline the client sends; before returns an error to fail
// the pipeline without sending it, as a timeout does.
type pipelineHook struct {
	before func(ctx context.Context, cmds []redis.Cmder) error
}

func (h *pipelineHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h *pipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *pipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if err := h.before(ctx, cmds); err != nil {
			for _, c := range cmds {
				c.SetErr(err)
			}
			return err
		}
		return next(ctx, cmds)
	}
}

// isSweepBatch tells a sweep batch (HGETALLs only) from a request-path read, which starts with
// ZRANGEBYSCORE.
func isSweepBatch(cmds []redis.Cmder) bool { return len(cmds) > 0 && cmds[0].Name() == "hgetall" }

type readerUnderTest struct {
	store  *Store
	client *redis.Client
	clk    *fakeClock
	reader *remoteRunningReader
}

// newReaderUnderTest builds a store on miniredis with the reader on and the fake clock, and marks
// gw-a and gw-b live. The tests drive sweeps by calling sweep, so nothing runs in the background.
func newReaderUnderTest(t *testing.T) *readerUnderTest {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	clk := newFakeClock()
	store := &Store{redisClient: client, snapshotClock: clk.now}
	r := newRemoteRunningReader(store, testReaderInterval)
	store.remoteRunningReader.Store(r)
	u := &readerUnderTest{store: store, client: client, clk: clk, reader: r}
	u.heartbeat(t, "gw-a", "gw-b")
	return u
}

// heartbeat marks gateways live now, on the clock liveGatewaysCutoffMillis uses.
func (u *readerUnderTest) heartbeat(t *testing.T, gateways ...string) {
	t.Helper()
	for _, gw := range gateways {
		require.NoError(t, u.client.ZAdd(context.Background(), runningRequestsGatewaysKey,
			redis.Z{Score: float64(time.Now().UnixMilli()), Member: gw}).Err())
	}
}

func (u *readerUnderTest) setRRQ(t *testing.T, pod string, fields map[string]any) {
	t.Helper()
	require.NoError(t, u.client.HSet(context.Background(), runningRequestsKey(testNamespace, pod), fields).Err())
}

func (u *readerUnderTest) entry(pod string) (*remoteRunningEntry, bool) {
	return u.reader.entries.Load(utils.GeneratePodKey(testNamespace, pod))
}

func readerPodName(i int) string { return fmt.Sprintf("pod-%03d", i) }

// Switch off: no reader exists, so GetPodsRunningRequestsApprox and the gauge take the other
// gateways' counts from the snapshot, exactly as before, the request path sends the same commands
// and publishes nothing, and nothing reads aibrix:rrq in the background.
func TestRemoteRunningReader_OffKeepsSnapshotPath(t *testing.T) {
	t.Setenv(remoteRunningReaderEnabledEnv, "")
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	counter := &commandCounter{}
	client.AddHook(counter)
	clk := newFakeClock()
	store := &Store{redisClient: client, snapshotClock: clk.now}

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })
	initRemoteRunningReader(store, stopCh)
	require.Nil(t, store.remoteRunningReader.Load(), "the reader must not start with the switch off")
	require.Empty(t, counter.snapshot(), "the reader must send no Redis command with the switch off")

	pod := addTestMetaPod(t, store, testPodName)
	metaPod, _ := store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	atomic.StoreInt32(&metaPod.runningRequests, 1)

	// Before the first snapshot the gauge is not set, as on main.
	store.syncRunningRequestsGlobally(metaPod)
	_, ok := metaPod.Metrics.Load(metrics.RealtimeNumRequestsRunning)
	require.False(t, ok, "the gauge must wait for the first snapshot with the switch off")

	// aibrix:rrq holds 40 for another live gateway, the snapshot 2: only the snapshot counts.
	require.NoError(t, client.ZAdd(context.Background(), runningRequestsGatewaysKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: "gw-a"}).Err())
	require.NoError(t, client.HSet(context.Background(), runningRequestsKey(testNamespace, testPodName), "gw-a", "40").Err())
	snapKey := gatewayPodSnapshotKey("gw-remote", testNamespace, testPodName)
	raw := map[string]map[string]string{snapKey: {
		"gateway_instance_id": "gw-remote", "namespace": testNamespace, "pod_name": testPodName, "requests_running": "2",
	}}
	store.gatewayRemoteRunning.Store(remoteRunningByPod(raw, map[string]time.Time{snapKey: clk.now()}, gatewayPodName))
	require.Equal(t, int64(3), approxRunning(store, pod))
	store.syncRunningRequestsGlobally(metaPod)
	got, ok := metaPod.Metrics.Load(metrics.RealtimeNumRequestsRunning)
	require.True(t, ok)
	require.Equal(t, 3.0, got.Value.GetSimpleValue())

	// The snapshot keeps its own 1 s limit.
	clk.advance(remoteRunningMaxAge + time.Nanosecond)
	require.Equal(t, int64(1), approxRunning(store, pod))

	before := counter.snapshot()
	counts := store.readPodsRunningRequests([]*v1.Pod{pod})
	require.Equal(t, int64(41), counts[utils.GeneratePodKey(testNamespace, testPodName)])
	require.Equal(t, map[string]int{"zrangebyscore": 1, "hgetall": 1}, counter.since(before))
}

// The switch and the interval are read from the environment at startup.
func TestRemoteRunningReader_OnReadsInterval(t *testing.T) {
	for _, tc := range []struct {
		interval string
		want     time.Duration
	}{
		{"", time.Second},
		{"250ms", 250 * time.Millisecond},
		{"0s", time.Second},
		{"250", time.Second},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			t.Setenv(remoteRunningReaderEnabledEnv, "true")
			t.Setenv(remoteRunningReaderIntervalEnv, tc.interval)
			store := &Store{redisClient: newTestRunningRequestsClient(t)}
			stopCh := make(chan struct{})
			t.Cleanup(func() { close(stopCh) })
			initRemoteRunningReader(store, stopCh)
			r := store.remoteRunningReader.Load()
			require.NotNil(t, r)
			require.Equal(t, tc.want, r.interval)
			require.Equal(t, 3*tc.want, r.maxAge)
		})
	}

	t.Setenv(remoteRunningReaderEnabledEnv, "true")
	store := &Store{}
	initRemoteRunningReader(store, nil)
	require.Nil(t, store.remoteRunningReader.Load(), "no reader without Redis")
}

// A sweep sums, per pod, the live gateways' positive integer counts other than this gateway's,
// and stamps the entry with the start of the read that produced it.
func TestRemoteRunningReader_SweepSumsLiveOtherPositiveFields(t *testing.T) {
	u := newReaderUnderTest(t)
	require.NoError(t, u.client.ZAdd(context.Background(), runningRequestsGatewaysKey,
		redis.Z{Score: 1, Member: "gw-dead"}).Err())
	addTestMetaPod(t, u.store, "mixed")
	addTestMetaPod(t, u.store, "saturated")
	addTestMetaPod(t, u.store, "no-hash")
	u.setRRQ(t, "mixed", map[string]any{
		"gw-a":                           "3",
		"gw-b":                           "4",
		"gw-dead":                        "100",
		runningRequestsGatewayInstanceID: "50",
		"gw-unknown":                     "7",
	})
	u.heartbeat(t, runningRequestsGatewayInstanceID, "gw-c", "gw-d", "gw-e", "gw-f", "gw-g")
	u.setRRQ(t, "mixed", map[string]any{"gw-c": "x", "gw-d": "-2", "gw-e": "1.5", "gw-f": "NaN", "gw-g": ""})
	max := strconv.FormatInt(math.MaxInt64, 10)
	u.setRRQ(t, "saturated", map[string]any{"gw-a": max, "gw-b": max})

	// The clock moves while a batch is in flight: the read time is when the read started.
	start := u.clk.now()
	u.client.AddHook(&pipelineHook{before: func(context.Context, []redis.Cmder) error {
		u.clk.advance(10 * time.Millisecond)
		return nil
	}})
	u.reader.sweep()

	for pod, want := range map[string]int64{"mixed": 7, "saturated": math.MaxInt64, "no-hash": 0} {
		e, ok := u.entry(pod)
		require.True(t, ok, pod)
		require.Equal(t, want, e.count, pod)
		require.Equal(t, start, e.readAt, pod)
	}
}

// A pod's remote count is used while it was read within 3 x the interval, the limit included,
// and not after that: the pod then falls back to this gateway's own count, in the estimate and
// in the gauge alike.
func TestRemoteRunningReader_AgeLimit(t *testing.T) {
	u := newReaderUnderTest(t)
	pod := addTestMetaPod(t, u.store, testPodName)
	metaPod, _ := u.store.metaPods.Load(utils.GeneratePodKey(testNamespace, testPodName))
	atomic.StoreInt32(&metaPod.runningRequests, 1)
	u.setRRQ(t, testPodName, map[string]any{"gw-a": "5"})
	u.reader.sweep()
	gauge := func() float64 {
		u.store.syncRunningRequestsGlobally(metaPod)
		got, ok := metaPod.Metrics.Load(metrics.RealtimeNumRequestsRunning)
		require.True(t, ok)
		return got.Value.GetSimpleValue()
	}

	require.Equal(t, int64(6), approxRunning(u.store, pod))
	u.clk.advance(3 * testReaderInterval)
	require.Equal(t, int64(6), approxRunning(u.store, pod), "a count exactly at the limit still counts")
	require.Equal(t, 6.0, gauge())
	u.clk.advance(time.Nanosecond)
	require.Equal(t, int64(1), approxRunning(u.store, pod), "a count past the limit must not be added")
	require.Equal(t, 1.0, gauge())
}

// A sweep that runs out of time publishes the batches it read; the pods it did not reach keep
// their previous entry and read time, and age out.
func TestRemoteRunningReader_PartialSweepKeepsUnreadPodsAging(t *testing.T) {
	u := newReaderUnderTest(t)
	// A short interval, so the timed-out batch waits only briefly for the sweep's deadline. The
	// age limit is judged on the fake clock.
	u.reader.interval = 50 * time.Millisecond
	n := 2*remoteRunningReaderBatchSize + 50
	pods := make([]*v1.Pod, n)
	for i := range n {
		pods[i] = addTestMetaPod(t, u.store, readerPodName(i))
		u.setRRQ(t, readerPodName(i), map[string]any{"gw-a": "3"})
	}
	u.reader.sweep()
	t0 := u.clk.now()
	for i := range n {
		e, ok := u.entry(readerPodName(i))
		require.True(t, ok)
		require.Equal(t, int64(3), e.count)
	}

	// The second batch runs into the deadline; the batch before it is read.
	tail := remoteRunningReaderBatchSize + 50
	hook := &tailTimeoutHook{
		tailKey: runningRequestsKey(testNamespace, readerPodName(tail)),
		slow:    make(chan struct{}),
		release: make(chan struct{}),
	}
	u.client.AddHook(hook)
	t.Cleanup(func() { close(hook.release) })
	close(hook.slow)
	for i := range n {
		u.setRRQ(t, readerPodName(i), map[string]any{"gw-a": "0"})
	}
	u.clk.advance(time.Millisecond)
	t1 := u.clk.now()
	u.reader.sweep()

	for i := range n {
		e, ok := u.entry(readerPodName(i))
		require.True(t, ok)
		if i < remoteRunningReaderBatchSize {
			require.Equal(t, remoteRunningEntry{count: 0, readAt: t1}, *e, "a batch that was read is published")
		} else {
			require.Equal(t, remoteRunningEntry{count: 3, readAt: t0}, *e, "an unread pod keeps its previous read time")
		}
	}
	u.clk.advance(u.reader.maxAge - t1.Sub(t0))
	require.Equal(t, int64(3), approxRunning(u.store, pods[n-1]), "an unread pod's count is used up to the limit")
	u.clk.advance(time.Nanosecond)
	require.Equal(t, int64(0), approxRunning(u.store, pods[n-1]), "an unread pod must age out")
	require.Equal(t, int64(0), approxRunning(u.store, pods[0]))
}

// Sweeps that each run out of time after one batch still read every pod, because each starts
// where the last one stopped. With a fixed start, the same tail would be missed every time.
func TestRemoteRunningReader_RotatingStartReadsEveryPod(t *testing.T) {
	u := newReaderUnderTest(t)
	n := 3 * remoteRunningReaderBatchSize
	for i := range n {
		addTestMetaPod(t, u.store, readerPodName(i))
		u.setRRQ(t, readerPodName(i), map[string]any{"gw-a": "1"})
	}
	// Each sweep has time for one batch: later batches fail as if the deadline had passed.
	var batches atomic.Int32
	u.client.AddHook(&pipelineHook{before: func(_ context.Context, cmds []redis.Cmder) error {
		if isSweepBatch(cmds) && batches.Add(1) > 1 {
			return context.DeadlineExceeded
		}
		return nil
	}})
	for range 3 {
		batches.Store(0)
		u.reader.sweep()
	}
	for i := range n {
		_, ok := u.entry(readerPodName(i))
		require.True(t, ok, "%s was never read", readerPodName(i))
	}
}

// A batch that fails on every sweep must not stop the sweeps from reading the other pods.
func TestRemoteRunningReader_FailingBatchDoesNotStallSweep(t *testing.T) {
	u := newReaderUnderTest(t)
	n := 3 * remoteRunningReaderBatchSize
	for i := range n {
		addTestMetaPod(t, u.store, readerPodName(i))
	}
	poison := runningRequestsKey(testNamespace, readerPodName(remoteRunningReaderBatchSize))
	u.client.AddHook(&pipelineHook{before: func(_ context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			if len(c.Args()) > 1 && c.Args()[1] == poison {
				return context.DeadlineExceeded
			}
		}
		return nil
	}})
	for range 4 {
		u.reader.sweep()
	}
	for _, i := range []int{0, 2*remoteRunningReaderBatchSize + 1, n - 1} {
		_, ok := u.entry(readerPodName(i))
		require.True(t, ok, "%s was never read", readerPodName(i))
	}
}

// A successful exact read refreshes the entries of the pods it read with the other gateways'
// share, at no extra Redis command. A failed read publishes nothing.
func TestRemoteRunningReader_RequestPathRefreshesWithoutExtraCommands(t *testing.T) {
	u := newReaderUnderTest(t)
	counter := &commandCounter{}
	u.client.AddHook(counter)
	pods := []*v1.Pod{addTestMetaPod(t, u.store, "a"), addTestMetaPod(t, u.store, "b"), addTestMetaPod(t, u.store, "c")}
	metaPod, _ := u.store.metaPods.Load(utils.GeneratePodKey(testNamespace, "a"))
	atomic.StoreInt32(&metaPod.runningRequests, 2)
	u.setRRQ(t, "a", map[string]any{"gw-a": "3", "gw-b": "4", runningRequestsGatewayInstanceID: "9"})
	u.setRRQ(t, "b", map[string]any{"gw-a": "1"})

	readAt := u.clk.now()
	before := counter.snapshot()
	counts, _ := u.store.readPodsSharedRoutingState(pods, true)
	require.Equal(t, int64(9), counts[utils.GeneratePodKey(testNamespace, "a")], "the exact total is unchanged")
	// One ZRANGEBYSCORE plus an rrq and a decode-ledger HGETALL per pod, as with the reader off.
	require.Equal(t, map[string]int{"zrangebyscore": 1, "hgetall": 6}, counter.since(before))
	for pod, want := range map[string]int64{"a": 7, "b": 1, "c": 0} {
		e, ok := u.entry(pod)
		require.True(t, ok, pod)
		require.Equal(t, remoteRunningEntry{count: want, readAt: readAt}, *e, pod)
	}

	// A failed read leaves every entry as it was.
	u.setRRQ(t, "a", map[string]any{"gw-a": "30"})
	u.clk.advance(time.Millisecond)
	u.client.AddHook(&pipelineHook{before: func(context.Context, []redis.Cmder) error { return context.DeadlineExceeded }})
	counts, _ = u.store.readPodsSharedRoutingState(pods, false)
	require.Nil(t, counts)
	e, _ := u.entry("a")
	require.Equal(t, remoteRunningEntry{count: 7, readAt: readAt}, *e, "a failed read must publish nothing")
}

// Newest read wins: a sweep batch that started before a request-path read must not overwrite
// it when it finishes later, and the other way round.
func TestRemoteRunningReader_NewestReadWins(t *testing.T) {
	t.Run("older sweep batch, newer request read", func(t *testing.T) {
		u := newReaderUnderTest(t)
		pod := addTestMetaPod(t, u.store, testPodName)
		u.setRRQ(t, testPodName, map[string]any{"gw-a": "3"})
		sweepStart := u.clk.now()
		var once sync.Once
		u.client.AddHook(&pipelineHook{before: func(_ context.Context, cmds []redis.Cmder) error {
			if isSweepBatch(cmds) {
				// The batch is in flight: a request reads a newer value and finishes first.
				once.Do(func() {
					u.clk.advance(time.Millisecond)
					u.setRRQ(t, testPodName, map[string]any{"gw-a": "5"})
					u.store.readPodsRunningRequests([]*v1.Pod{pod})
				})
			}
			return nil
		}})
		u.reader.sweep()
		e, _ := u.entry(testPodName)
		require.Equal(t, remoteRunningEntry{count: 5, readAt: sweepStart.Add(time.Millisecond)}, *e)
	})

	t.Run("older request read, newer sweep batch", func(t *testing.T) {
		u := newReaderUnderTest(t)
		pod := addTestMetaPod(t, u.store, testPodName)
		u.setRRQ(t, testPodName, map[string]any{"gw-a": "3"})
		readStart := u.clk.now()
		var once sync.Once
		u.client.AddHook(&pipelineHook{before: func(_ context.Context, cmds []redis.Cmder) error {
			if !isSweepBatch(cmds) {
				once.Do(func() {
					u.clk.advance(time.Millisecond)
					u.setRRQ(t, testPodName, map[string]any{"gw-a": "5"})
					u.reader.sweep()
				})
			}
			return nil
		}})
		u.store.readPodsRunningRequests([]*v1.Pod{pod})
		e, _ := u.entry(testPodName)
		require.Equal(t, remoteRunningEntry{count: 5, readAt: readStart.Add(time.Millisecond)}, *e)
	})
}

// Entries of pods this gateway no longer knows are dropped at the next sweep, and the freshness
// gauges count the known pods by state.
func TestRemoteRunningReader_PrunesDeletedPodsAndRecordsFreshness(t *testing.T) {
	u := newReaderUnderTest(t)
	addTestMetaPod(t, u.store, "kept")
	addTestMetaPod(t, u.store, "deleted")
	u.reader.sweep()
	u.store.metaPods.Delete(utils.GeneratePodKey(testNamespace, "deleted"))
	u.reader.publish(utils.GeneratePodKey(testNamespace, "stale"), 1, u.clk.now().Add(-time.Hour))
	addTestMetaPod(t, u.store, "stale")
	addTestMetaPod(t, u.store, "unread")
	// The second sweep reads nothing: "kept" keeps its first read, 2 s old, within the limit.
	u.client.AddHook(&pipelineHook{before: func(context.Context, []redis.Cmder) error { return context.DeadlineExceeded }})
	u.clk.advance(2 * time.Second)
	u.reader.sweep()

	_, ok := u.entry("deleted")
	require.False(t, ok, "a deleted pod's entry must be dropped")
	require.Equal(t, 1.0, testutil.ToFloat64(remoteRunningReaderPods.WithLabelValues(remoteRunningPodFresh)))
	require.Equal(t, 1.0, testutil.ToFloat64(remoteRunningReaderPods.WithLabelValues(remoteRunningPodStale)))
	require.Equal(t, 1.0, testutil.ToFloat64(remoteRunningReaderPods.WithLabelValues(remoteRunningPodUnread)))
	require.InDelta(t, (time.Hour + 2*time.Second).Seconds(), testutil.ToFloat64(remoteRunningReaderOldestAge), 1e-6)
}

// The reader, the request path and the consumers run at once; run with -race.
func TestRemoteRunningReader_Concurrent(t *testing.T) {
	u := newReaderUnderTest(t)
	pods := make([]*v1.Pod, 50)
	for i := range pods {
		pods[i] = addTestMetaPod(t, u.store, readerPodName(i))
		u.setRRQ(t, readerPodName(i), map[string]any{"gw-a": strconv.Itoa(i)})
	}
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				f()
			}
		}()
	}
	run(u.reader.sweep)
	run(func() { u.store.readPodsRunningRequests(pods[:10]) })
	run(func() { u.store.readPodsRunningRequests(pods[40:]) })
	run(func() { u.store.GetPodsRunningRequestsApprox(pods) })
	run(func() {
		metaPod, _ := u.store.metaPods.Load(utils.GeneratePodKey(testNamespace, readerPodName(1)))
		u.store.syncRunningRequestsGlobally(metaPod)
	})
	run(func() { u.clk.advance(time.Millisecond) })
	wg.Wait()
	require.Equal(t, int64(49), approxRunning(u.store, pods[49]))
}
