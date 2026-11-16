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
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

// Accept connections but never reply, including during Redis connection initialization.
func newSilentRedisListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() {
		_ = ln.Close()
		close(stop)
		wg.Wait()
	})
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				<-stop
				_ = c.Close()
			}(conn)
		}
	}()
	return ln
}

func newTestSilentRedisStore(t *testing.T) *Store {
	t.Helper()
	// Bootstrap the real factory on healthy Redis so its startup PING succeeds.
	mr := miniredis.RunT(t)
	host, port, err := net.SplitHostPort(mr.Addr())
	require.NoError(t, err)
	t.Setenv("REDIS_HOST", host)
	t.Setenv("REDIS_PORT", port)
	t.Setenv("REDIS_PASSWORD", "")
	shared := utils.GetRedisClient()
	require.NotNil(t, shared)
	t.Cleanup(func() { _ = shared.Close() })

	options := *shared.Options()
	options.Addr = newSilentRedisListener(t).Addr().String()
	client := redis.NewClient(&options)
	t.Cleanup(func() { _ = client.Close() })
	require.False(t, client.Options().ContextTimeoutEnabled)

	return newTestRedisStore(t, client)
}

func newTestRedisStore(t *testing.T, client *redis.Client) *Store {
	t.Helper()
	// Exercise New's real client wiring and restore its singleton after the test.
	previous := store
	testStore := New(client, nil, nil)
	t.Cleanup(func() {
		testStore.Close()
		close(testStore.podMetricsJobs)
		store = previous
	})
	return testStore
}

func TestAdmitRunningRequestReadBudgetOnSilentTCPConnection(t *testing.T) {
	store := newTestSilentRedisStore(t)
	require.Zero(t, store.admitRedisClient.Options().MaxRetries)

	start := time.Now()
	admitted, counted, running := store.admitRunningRequest(testNamespace, testPodName, 1, 0)
	elapsed := time.Since(start)
	t.Logf("elapsed=%s budget=%s admitted=%t counted=%t running=%d",
		elapsed, runningRequestsReadTimeout, admitted, counted, running)
	require.True(t, admitted, "Redis failure falls back to localRunning=0 < limit=1")
	require.True(t, counted, "a timeout cannot prove the script did not run")
	require.Zero(t, running)
	require.Less(t, elapsed, runningRequestsReadTimeout+time.Second,
		"the admission budget must bound a blocked Redis socket, with scheduling slack")
}

func TestReadPodRunningRequestsReadBudgetOnSilentTCPConnection(t *testing.T) {
	store := newTestSilentRedisStore(t)

	start := time.Now()
	count, ok := store.readPodRunningRequests(testNamespace, testPodName)
	elapsed := time.Since(start)
	t.Logf("elapsed=%s budget=%s count=%d ok=%t", elapsed, runningRequestsReadTimeout, count, ok)
	require.Zero(t, count)
	require.False(t, ok, "Redis failure leaves the caller to use its local count")
	require.Less(t, elapsed, runningRequestsReadTimeout+time.Second,
		"the running-requests read budget must bound a blocked Redis socket, with scheduling slack")
}

func TestReadPodsSharedRoutingStateReadBudgetOnSilentTCPConnection(t *testing.T) {
	store := newTestSilentRedisStore(t)
	pod := newTestPod(4)

	start := time.Now()
	counts, ledger := store.readPodsSharedRoutingState([]*v1.Pod{pod.Pod}, true)
	elapsed := time.Since(start)
	t.Logf("elapsed=%s budget=%s counts=%v ledger=%v", elapsed, runningRequestsReadTimeout, counts, ledger)
	require.Nil(t, counts, "Redis failure leaves the caller to use local counts")
	require.Nil(t, ledger, "Redis failure supplies no remote decode load")
	require.Less(t, elapsed, runningRequestsReadTimeout+time.Second,
		"the decode-ledger read budget must bound a blocked Redis socket, with scheduling slack")
}

func TestStoreClose_ClosesDerivedRedisClients(t *testing.T) {
	shared := newTestRunningRequestsClient(t)
	testStore := newTestRedisStore(t, shared)
	ctx := context.Background()
	require.NoError(t, testStore.readRedisClient.Ping(ctx).Err())
	require.NoError(t, testStore.admitRedisClient.Ping(ctx).Err())

	for i := 0; i < 2; i++ {
		testStore.Close()
		require.ErrorIs(t, testStore.readRedisClient.Ping(ctx).Err(), redis.ErrClosed)
		require.ErrorIs(t, testStore.admitRedisClient.Ping(ctx).Err(), redis.ErrClosed)
		require.NoError(t, shared.Ping(ctx).Err(), "the shared client is owned by the caller")
	}
}

// redisTestProxy forwards to miniredis, optionally resetting the first TCP connection.
// replyDelay delays one subsequent server write, after a test has warmed the client.
type redisTestProxy struct {
	addr        string
	connections atomic.Int64
	replyDelay  atomic.Int64
}

func newTestRedisProxy(t *testing.T, backendAddr string, resetFirst bool) *redisTestProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &redisTestProxy{addr: ln.Addr().String()}
	accepted := make(chan struct{})
	var conns []net.Conn
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		<-accepted // No more connections or copy goroutines can be added.
		for _, conn := range conns {
			_ = conn.Close()
		}
		wg.Wait()
	})
	go func() {
		defer close(accepted)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if proxy.connections.Add(1) == 1 && resetFirst {
				if err := conn.(*net.TCPConn).SetLinger(0); err != nil {
					t.Errorf("reset first proxy connection: %v", err)
				}
				_ = conn.Close()
				continue
			}
			backend, err := net.DialTimeout("tcp", backendAddr, time.Second)
			if err != nil {
				_ = conn.Close()
				t.Errorf("dial proxy backend: %v", err)
				return
			}
			conns = append(conns, conn, backend)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = io.Copy(backend, conn)
				_ = backend.Close()
			}()
			go func() {
				defer wg.Done()
				_, _ = io.Copy(delayedRedisReplyWriter{Writer: conn, delay: &proxy.replyDelay}, backend)
				_ = conn.Close()
			}()
		}
	}()
	return proxy
}

type delayedRedisReplyWriter struct {
	io.Writer
	delay *atomic.Int64
}

func (w delayedRedisReplyWriter) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(w.delay.Swap(0)))
	return w.Writer.Write(p)
}

func TestRedisRoutingReads_SlowHealthyReply(t *testing.T) {
	backend := newTestRunningRequestsClient(t)
	markLive(t, backend, "gw-remote")
	require.NoError(t, backend.HSet(context.Background(), runningRequestsKey(testNamespace, testPodName), "gw-remote", 7).Err())
	setLedgerFields(t, backend, map[string]string{"gw-remote": formatLedgerValue(100, 1, 1)})
	proxy := newTestRedisProxy(t, backend.Options().Addr, false)
	shared := redis.NewClient(&redis.Options{Addr: proxy.addr})
	t.Cleanup(func() { _ = shared.Close() })
	testStore := newTestRedisStore(t, shared)
	// Keep initialization outside the read being measured.
	require.NoError(t, testStore.readRedisClient.Ping(context.Background()).Err())

	for _, path := range []string{"single", "fleet and ledger"} {
		t.Run(path, func(t *testing.T) {
			delay := runningRequestsReadTimeout / 2
			proxy.replyDelay.Store(int64(delay))
			start := time.Now()
			if path == "single" {
				count, ok := testStore.readPodRunningRequests(testNamespace, testPodName)
				require.True(t, ok, "a delayed healthy read must not fall back")
				require.EqualValues(t, 7, count)
			} else {
				counts, ledger := testStore.readPodsSharedRoutingState([]*v1.Pod{newTestPod(0).Pod}, true)
				require.EqualValues(t, 7, counts[testLedgerPodKey])
				require.Equal(t, 100.0, ledger[testLedgerPodKey].Tokens)
			}
			elapsed := time.Since(start)
			t.Logf("elapsed=%s delay=%s budget=%s", elapsed, delay, runningRequestsReadTimeout)
			require.GreaterOrEqual(t, elapsed, delay, "the proxy must delay the reply")
			require.Zero(t, proxy.replyDelay.Load(), "the read consumed the injected delay")
		})
	}
}

func TestRedisClients_RetryAfterConnectionReset(t *testing.T) {
	for _, path := range []string{"single", "fleet and ledger", "admit"} {
		t.Run(path, func(t *testing.T) {
			backend := newTestRunningRequestsClient(t)
			markLive(t, backend, "gw-remote")
			require.NoError(t, backend.HSet(context.Background(), runningRequestsKey(testNamespace, testPodName), "gw-remote", 7).Err())
			setLedgerFields(t, backend, map[string]string{"gw-remote": formatLedgerValue(100, 1, 1)})
			proxy := newTestRedisProxy(t, backend.Options().Addr, true)
			shared := redis.NewClient(&redis.Options{
				Addr: proxy.addr, MaxRetries: 1, MinRetryBackoff: -1, MaxRetryBackoff: -1,
			})
			t.Cleanup(func() { _ = shared.Close() })
			testStore := newTestRedisStore(t, shared)

			switch path {
			case "single":
				count, ok := testStore.readPodRunningRequests(testNamespace, testPodName)
				require.True(t, ok)
				require.EqualValues(t, 7, count)
			case "fleet and ledger":
				counts, ledger := testStore.readPodsSharedRoutingState([]*v1.Pod{newTestPod(0).Pod}, true)
				require.EqualValues(t, 7, counts[testLedgerPodKey])
				require.Equal(t, 100.0, ledger[testLedgerPodKey].Tokens)
			case "admit":
				admitted, _, running := testStore.admitRunningRequest(testNamespace, testPodName, 10, 0)
				require.True(t, admitted)
				require.Zero(t, running, "the failed single attempt must fall back to localRunning")
				require.EqualValues(t, 1, proxy.connections.Load(), "admission must not retry")
				return
			}
			require.EqualValues(t, 2, proxy.connections.Load(), "the read must retry the reset connection once")
		})
	}
}
