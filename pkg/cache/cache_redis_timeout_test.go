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
	"net"
	"sync"
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

	// Exercise New's real client wiring and restore its singleton after the test.
	previous := store
	testStore := New(client, nil, nil)
	t.Cleanup(func() {
		testStore.Close()
		// Also clean up on the base revision, where Close leaves this client open.
		_ = testStore.admitRedisClient.Close()
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
