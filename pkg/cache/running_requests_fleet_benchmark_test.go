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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/utils"
)

// BenchmarkGetPodsRunningRequests_Redis measures the cross-gateway running-request read that
// least-request, load-balance, the load-imbalance gate and the inflight filter each make per
// routed request (RFC #3188). It needs a dedicated disposable Redis: set both
// AIBRIX_BENCH_REDIS_ADDR (for example localhost:6379) and AIBRIX_BENCH_REDIS_FLUSH=1. The
// benchmark flushes that Redis's current database.
func BenchmarkGetPodsRunningRequests_Redis(b *testing.B) {
	ctx := context.Background()
	client := connectDisposableBenchRedis(b, ctx)
	saved := runningRequestsReadTimeout
	runningRequestsReadTimeout = 10 * time.Second // measure the read, not the fallback
	b.Cleanup(func() { runningRequestsReadTimeout = saved })

	for _, n := range []int{50, 500, 2000} {
		if err := client.FlushDB(ctx).Err(); err != nil {
			b.Fatal(err)
		}
		pods := make([]*v1.Pod, n)
		for i := range pods {
			pods[i] = &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%04d", i), Namespace: "default"},
				Status: v1.PodStatus{
					PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
					Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
				},
			}
		}
		store := NewWithPodsForTest(pods, "m1")
		store.redisClient = client
		// Four live gateways, each with a few requests on every pod.
		now := float64(time.Now().UnixMilli())
		for g := 0; g < 4; g++ {
			gw := fmt.Sprintf("gw-%d", g)
			client.ZAdd(ctx, runningRequestsGatewaysKey, redis.Z{Score: now + float64(time.Hour.Milliseconds()), Member: gw})
			pipe := client.Pipeline()
			for i, pod := range pods {
				pipe.HSet(ctx, runningRequestsKey(pod.Namespace, pod.Name), gw, (i+g)%5)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
		}
		for _, k := range []int{n, 16} {
			if k > n {
				continue
			}
			subset := pods[:k]
			b.Run(fmt.Sprintf("N=%d/read=%d", n, k), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					counts, _ := store.GetPodsRunningRequests(subset)
					if len(counts) != k {
						b.Fatalf("got %d counts, want %d", len(counts), k)
					}
				}
			})
		}
	}
}

// BenchmarkRemoteRunningReader_Redis measures one gateway's background tick cost against a real
// Redis at G=8, N=2000 and N=5000: legacy snapshot sync (Phase-1 write + SCAN + G*N HGETALL) vs
// the remote running reader sweep (ZRANGEBYSCORE + non-fresh N HGETALL) with 0% or 50% request-path
// half-interval overlap. Requires both AIBRIX_BENCH_REDIS_ADDR pointing at a dedicated disposable
// Redis and AIBRIX_BENCH_REDIS_FLUSH=1.
func BenchmarkRemoteRunningReader_Redis(b *testing.B) {
	ctx := context.Background()
	client := connectDisposableBenchRedis(b, ctx)

	const gCount = 8
	for _, n := range []int{2000, 5000} {
		pods, podMap := seedRealRedisFleet(b, ctx, client, gCount, n)

		b.Run(fmt.Sprintf("G=%d/N=%d/mode=snapshot_full_tick", gCount, n), func(b *testing.B) {
			// Keep the other G-1 gateways' snapshot keys alive for the benchmark loop while gw-0
			// runs the normal write+SCAN+HGETALL tick (appendGatewayPodSnapshotToPipeline sets 500ms TTL).
			ttlPipe := client.Pipeline()
			for g := 1; g < gCount; g++ {
				gw := fmt.Sprintf("gw-%d", g)
				for _, p := range pods {
					ttlPipe.PExpire(ctx, gatewayPodSnapshotKey(gw, p.Namespace, p.Name), 10*time.Minute)
				}
			}
			if _, err := ttlPipe.Exec(ctx); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				writePipe := client.Pipeline()
				for _, p := range podMap {
					appendGatewayPodSnapshotToPipeline(ctx, writePipe, p, "gw-0")
				}
				if _, err := writePipe.Exec(ctx); err != nil {
					b.Fatal(err)
				}
				keys, err := scanGatewaySnapshotKeys(ctx, client)
				if err != nil {
					b.Fatal(err)
				}
				raw, _, _, readErr := readGatewaySnapshots(ctx, client, keys, nil)
				if readErr != nil || len(raw) != gCount*n {
					b.Fatalf("read %d keys (want %d), err=%v", len(raw), gCount*n, readErr)
				}
			}
		})

		for _, refreshedPct := range []int{0, 50} {
			b.Run(fmt.Sprintf("G=%d/N=%d/mode=reader_sweep_refreshed=%d%%", gCount, n, refreshedPct), func(b *testing.B) {
				clk := newFakeClock()
				store := &Store{redisClient: client, snapshotClock: clk.now}
				for _, p := range podMap {
					store.metaPods.Store(utils.GeneratePodKey(p.Namespace, p.Name), p)
				}
				r := newRemoteRunningReader(store, time.Second)
				store.remoteRunningReader.Store(r)
				refreshedCount := len(pods) * refreshedPct / 100
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					clk.advance(2 * time.Second)
					sweepStart := clk.now()
					for j := 0; j < refreshedCount; j++ {
						r.publish(utils.GeneratePodKey(pods[j].Namespace, pods[j].Name), int64(j), sweepStart)
					}
					b.StartTimer()
					r.sweep()
					b.StopTimer()
					assertSweepRefreshedPods(b, r, pods[refreshedCount:], sweepStart)
					b.StartTimer()
				}
			})
		}

		b.Run(fmt.Sprintf("G=%d/N=%d/mode=reader_sweep_plus_phase1_write", gCount, n), func(b *testing.B) {
			clk := newFakeClock()
			store := &Store{redisClient: client, snapshotClock: clk.now}
			for _, p := range podMap {
				store.metaPods.Store(utils.GeneratePodKey(p.Namespace, p.Name), p)
			}
			r := newRemoteRunningReader(store, time.Second)
			store.remoteRunningReader.Store(r)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				clk.advance(2 * time.Second)
				sweepStart := clk.now()
				b.StartTimer()
				writePipe := client.Pipeline()
				for _, p := range podMap {
					appendGatewayPodSnapshotToPipeline(ctx, writePipe, p, "gw-0")
				}
				if _, err := writePipe.Exec(ctx); err != nil {
					b.Fatal(err)
				}
				r.sweep()
				b.StopTimer()
				assertSweepRefreshedPods(b, r, pods, sweepStart)
				b.StartTimer()
			}
		})
	}
}

func assertSweepRefreshedPods(tb testing.TB, r *remoteRunningReader, pods []*v1.Pod, sweepStart time.Time) {
	tb.Helper()
	for _, p := range pods {
		e, ok := r.entries.Load(utils.GeneratePodKey(p.Namespace, p.Name))
		if !ok || e.readAt.Before(sweepStart) {
			tb.Fatalf("incomplete reader sweep: pod %s/%s not updated at/after %v", p.Namespace, p.Name, sweepStart)
		}
	}
}

// TestRemoteRunningReader_RedisCommandStats runs G=8 concurrent gateways against a single-node
// Redis at N=2000 and N=5000 for 3s each and logs real Redis INFO commandstats & CPU usage diffs
// for reader=off (snapshot sync) vs reader=on (0% and 50% request-path half-interval overlap).
// Requires both AIBRIX_BENCH_REDIS_ADDR pointing at a dedicated disposable Redis and
// AIBRIX_BENCH_REDIS_FLUSH=1.
func TestRemoteRunningReader_RedisCommandStats(t *testing.T) {
	ctx := context.Background()
	admin := connectDisposableBenchRedis(t, ctx)

	const (
		gCount = 8
		window = 3 * time.Second
	)
	for _, n := range []int{2000, 5000} {
		pods, podMap := seedRealRedisFleet(t, ctx, admin, gCount, n)

		for _, tc := range []struct {
			name         string
			readerOn     bool
			refreshShare float64
		}{
			{name: "reader=off (snapshot 100ms)", readerOn: false},
			{name: "reader=on (1s sweep + phase1 write 100ms, 0% refresh)", readerOn: true},
			{name: "reader=on (1s sweep + phase1 write 100ms, 50% refresh)", readerOn: true, refreshShare: 0.5},
		} {
			t.Run(fmt.Sprintf("G=%d/N=%d/%s", gCount, n, tc.name), func(t *testing.T) {
				require.NoError(t, admin.FlushDB(ctx).Err())
				seedRealRedisKeysOnly(t, ctx, admin, gCount, pods)

				addr := admin.Options().Addr
				stopCh := make(chan struct{})
				var stopOnce sync.Once
				var readerWG sync.WaitGroup
				snapDones := make([]<-chan struct{}, 0, gCount)
				clients := make([]*redis.Client, 0, gCount)

				// Register global restore before worker cleanup so LIFO teardown stops and waits
				// for all reader and snapshot workers before closing Redis clients and restoring
				// gatewayPodName last.
				origPodName := gatewayPodName
				t.Cleanup(func() { gatewayPodName = origPodName })
				t.Cleanup(func() {
					stopOnce.Do(func() { close(stopCh) })
					readerWG.Wait()
					for _, d := range snapDones {
						<-d
					}
					for _, c := range clients {
						_ = c.Close()
					}
				})

				stores := make([]*Store, gCount)
				readers := make([]*remoteRunningReader, gCount)
				for g := 0; g < gCount; g++ {
					c := redis.NewClient(&redis.Options{Addr: addr})
					clients = append(clients, c)
					s := &Store{redisClient: c}
					for key, mp := range podMap {
						s.metaPods.Store(key, mp)
					}
					stores[g] = s
					if tc.readerOn {
						r := newRemoteRunningReader(s, time.Second)
						s.remoteRunningReader.Store(r)
						readers[g] = r
						readerWG.Add(1)
						go func(rd *remoteRunningReader) {
							defer readerWG.Done()
							rd.run(stopCh)
						}(r)
					}
					gatewayPodName = fmt.Sprintf("gw-%d", g)
					snapDones = append(snapDones, startGatewaySnapshotSync(s, stopCh))
				}

				// Let initial bursts settle, then diff INFO cpu and INFO commandstats over the window.
				time.Sleep(1200 * time.Millisecond)
				cpuBefore := readRedisCPUSecs(t, ctx, admin)
				statsBefore := readRedisCommandStatMap(t, ctx, admin)

				if tc.refreshShare > 0 {
					refreshN := int(float64(len(pods)) * tc.refreshShare)
					ticker := time.NewTicker(250 * time.Millisecond)
					deadline := time.After(window)
				loop:
					for {
						now := time.Now()
						for g := 0; g < gCount; g++ {
							for j := 0; j < refreshN; j++ {
								readers[g].publish(utils.GeneratePodKey(pods[j].Namespace, pods[j].Name), int64(j%4), now)
							}
						}
						select {
						case <-deadline:
							ticker.Stop()
							break loop
						case <-ticker.C:
						}
					}
				} else {
					time.Sleep(window)
				}

				cpuAfter := readRedisCPUSecs(t, ctx, admin)
				statsAfter := readRedisCommandStatMap(t, ctx, admin)
				cmdStats := formatRedisCommandStatDelta(statsBefore, statsAfter)

				snapEverPublished := 0
				for g := 0; g < gCount; g++ {
					if stores[g].gatewayRemoteRunning.Load() != nil {
						snapEverPublished++
					}
				}

				stopOnce.Do(func() { close(stopCh) })
				readerWG.Wait()
				for _, d := range snapDones {
					<-d
				}

				t.Logf("window=%s cpu_cores=%.3f snap_ever_published=%d/%d stats=%s",
					window, (cpuAfter-cpuBefore)/window.Seconds(), snapEverPublished, gCount, cmdStats)
			})
		}
	}
}

func connectDisposableBenchRedis(tb testing.TB, ctx context.Context) *redis.Client {
	tb.Helper()
	addr := os.Getenv("AIBRIX_BENCH_REDIS_ADDR")
	if addr == "" || os.Getenv("AIBRIX_BENCH_REDIS_FLUSH") != "1" {
		tb.Skip("set both AIBRIX_BENCH_REDIS_ADDR and AIBRIX_BENCH_REDIS_FLUSH=1 to run against a dedicated disposable Redis")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	tb.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		tb.Fatalf("connect bench Redis %s: %v", addr, err)
	}
	tb.Cleanup(func() { _ = client.FlushDB(context.Background()).Err() })
	return client
}

func seedRealRedisFleet(tb testing.TB, ctx context.Context, client *redis.Client, gCount, n int) ([]*v1.Pod, map[string]*Pod) {
	tb.Helper()
	if err := client.FlushDB(ctx).Err(); err != nil {
		tb.Fatal(err)
	}
	pods := make([]*v1.Pod, n)
	podMap := make(map[string]*Pod, n)
	for i := range pods {
		p := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%04d", i), Namespace: "default"},
			Status: v1.PodStatus{
				PodIP:      fmt.Sprintf("10.0.%d.%d", i/256, i%256),
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
			},
		}
		pods[i] = p
		podMap[utils.GeneratePodKey(p.Namespace, p.Name)] = &Pod{Pod: p}
	}
	seedRealRedisKeysOnly(tb, ctx, client, gCount, pods)
	return pods, podMap
}

func seedRealRedisKeysOnly(tb testing.TB, ctx context.Context, client *redis.Client, gCount int, pods []*v1.Pod) {
	tb.Helper()
	nowMs := float64(time.Now().UnixMilli() + int64(time.Hour/time.Millisecond))
	for g := 0; g < gCount; g++ {
		gw := fmt.Sprintf("gw-%d", g)
		if err := client.ZAdd(ctx, runningRequestsGatewaysKey, redis.Z{Score: nowMs, Member: gw}).Err(); err != nil {
			tb.Fatal(err)
		}
		pipe := client.Pipeline()
		for i, p := range pods {
			pipe.HSet(ctx, runningRequestsKey(p.Namespace, p.Name), gw, (i+g)%5+1)
			appendGatewayPodSnapshotToPipeline(ctx, pipe, &Pod{Pod: p}, gw)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			tb.Fatal(err)
		}
	}
}

func readRedisCPUSecs(t *testing.T, ctx context.Context, client *redis.Client) float64 {
	t.Helper()
	info, err := client.Info(ctx, "cpu").Result()
	require.NoError(t, err)
	var total float64
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "used_cpu_sys:") || strings.HasPrefix(line, "used_cpu_user:") {
			parts := strings.SplitN(line, ":", 2)
			v, err := strconv.ParseFloat(parts[1], 64)
			require.NoError(t, err)
			total += v
		}
	}
	return total
}

type redisCmdStat struct {
	calls         int64
	usec          int64
	rejectedCalls int64
	failedCalls   int64
}

func readRedisCommandStatMap(t *testing.T, ctx context.Context, client *redis.Client) map[string]redisCmdStat {
	t.Helper()
	info, err := client.Info(ctx, "commandstats").Result()
	require.NoError(t, err)
	out := make(map[string]redisCmdStat)
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "cmdstat_") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		var st redisCmdStat
		for _, kv := range strings.Split(parts[1], ",") {
			pair := strings.SplitN(kv, "=", 2)
			if len(pair) != 2 {
				continue
			}
			v, err := strconv.ParseInt(pair[1], 10, 64)
			if err != nil {
				continue
			}
			switch pair[0] {
			case "calls":
				st.calls = v
			case "usec":
				st.usec = v
			case "rejected_calls":
				st.rejectedCalls = v
			case "failed_calls":
				st.failedCalls = v
			}
		}
		out[parts[0]] = st
	}
	return out
}

func formatRedisCommandStatDelta(before, after map[string]redisCmdStat) string {
	names := make([]string, 0, len(after))
	for k := range after {
		names = append(names, k)
	}
	sort.Strings(names)
	var lines []string
	for _, k := range names {
		a := after[k]
		b := before[k]
		dCalls := a.calls - b.calls
		if dCalls == 0 && a.rejectedCalls == b.rejectedCalls && a.failedCalls == b.failedCalls {
			continue
		}
		dUsec := a.usec - b.usec
		usecPerCall := 0.0
		if dCalls > 0 {
			usecPerCall = float64(dUsec) / float64(dCalls)
		}
		lines = append(lines, fmt.Sprintf("%s:calls=%d,usec=%d,usec_per_call=%.2f,rejected_calls=%d,failed_calls=%d",
			k, dCalls, dUsec, usecPerCall, a.rejectedCalls-b.rejectedCalls, a.failedCalls-b.failedCalls))
	}
	return strings.Join(lines, " | ")
}
