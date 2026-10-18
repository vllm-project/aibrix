//go:build zmq

// Copyright 2025 The AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kvcache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	zmq "github.com/pebbe/zmq4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestZMQClientZeroBasedPublisherRestart(t *testing.T) {
	for _, previousLast := range []int64{0, 3} {
		t.Run(fmt.Sprintf("previous_last_%d", previousLast), func(t *testing.T) {
			h := newGuardedViewHarness(t)
			oldPrefix, newPrefix := faultPrefix('A'), faultPrefix('B')
			for seq := int64(0); seq <= previousLast; seq++ {
				h.apply(t, faultArrival{seq: seq, replay: true, events: storedEvents([]int64{101, 102}, oldPrefix)})
			}
			require.True(t, h.fullHit(oldPrefix))
			h.apply(t, faultArrival{seq: 0, events: storedEvents([]int64{201, 202}, newPrefix)})
			require.False(t, h.fullHit(oldPrefix), "the first live batch of a restarted publisher must invalidate the old view")
			require.True(t, h.fullHit(newPrefix))
			require.Equal(t, int64(0), h.client.GetLastSequence())
		})
	}
}

func TestZMQClientColdStartHoldsLiveBatchUntilReplay(t *testing.T) {
	handler := NewMockEventHandler()
	client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
	defer client.Stop()
	_, err := client.applyEventBatch(3, encodeBlockStored(t, 400), false)
	require.NoError(t, err)
	require.Equal(t, int64(-1), client.GetLastSequence(), "a failed first replay must not let live traffic discard older history")
	require.Empty(t, handler.GetEvents())
	for seq := int64(0); seq < 3; seq++ {
		_, err = client.applyEventBatch(seq, encodeBlockStored(t, 100*(seq+1)), true)
		require.NoError(t, err)
	}
	require.Equal(t, []int64{100, 200, 300, 400}, storedHashes(handler))
	require.Equal(t, int64(3), client.GetLastSequence())
	require.Empty(t, client.pending)
}

func TestZMQClientRestartDiscardsPendingColdStartBatch(t *testing.T) {
	h := newGuardedViewHarness(t)
	oldPrefix, newPrefix := faultPrefix('A'), faultPrefix('B')
	h.apply(t, faultArrival{seq: 5, events: storedEvents([]int64{101, 102}, oldPrefix)})
	require.False(t, h.fullHit(oldPrefix))
	// Restart before the initial replay has applied anything. The old live
	// batch must not be drained into the new publisher's sequence space.
	for seq := int64(0); seq < 5; seq++ {
		h.apply(t, faultArrival{seq: seq, events: storedEvents([]int64{201, 202}, newPrefix)})
	}
	require.False(t, h.fullHit(oldPrefix), "pending traffic from the previous publisher leaked into the new view")
	require.True(t, h.fullHit(newPrefix))
	require.Equal(t, int64(4), h.client.GetLastSequence())
	require.Empty(t, h.client.pending)
}

func TestZMQClientCountsEachLiveGapOnce(t *testing.T) {
	handler := NewMockEventHandler()
	client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
	defer client.Stop()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_missing_batches_total"}, nil)
	client.metrics.missedEvents = counter.WithLabelValues()

	sender, err := zmq.NewSocket(zmq.PAIR)
	require.NoError(t, err)
	defer func() { _ = sender.Close() }()
	receiver, err := zmq.NewSocket(zmq.PAIR)
	require.NoError(t, err)
	client.subSocket = receiver // Stop owns the receiver, just as in Connect.
	require.NoError(t, receiver.SetRcvtimeo(time.Second))
	endpoint := "inproc://" + t.Name()
	require.NoError(t, sender.Bind(endpoint))
	require.NoError(t, receiver.Connect(endpoint))
	_, err = client.applyEventBatch(0, encodeBlockStored(t, 100), true)
	require.NoError(t, err)

	send := func(seq int64) {
		t.Helper()
		_, err := sender.SendMessage("", encodeSeq(seq), encodeBlockStored(t, seq*100))
		require.NoError(t, err)
		require.NoError(t, client.processMessage())
	}
	value := func() float64 {
		t.Helper()
		v, err := getMetricValue(counter)
		require.NoError(t, err)
		return v
	}

	send(3) // Missing live batches 1 and 2.
	require.Equal(t, float64(2), value(), "processMessage and applyEventBatch must not both count the same gap")
	send(3) // Duplicate of a held batch.
	require.Equal(t, float64(2), value())
	send(4) // Still the same gap, followed by a contiguous live batch.
	require.Equal(t, float64(2), value())
	_, err = client.applyEventBatch(1, encodeBlockStored(t, 200), true)
	require.NoError(t, err)
	_, err = client.applyEventBatch(2, encodeBlockStored(t, 300), true)
	require.NoError(t, err)
	require.Equal(t, int64(4), client.GetLastSequence())
	require.Equal(t, float64(2), value(), "replay must not count the gap again")
	send(6) // A distinct missing live batch 5.
	require.Equal(t, float64(3), value())
}

func TestZMQClientPendingCleanupAfterReplay(t *testing.T) {
	handler := NewMockEventHandler()
	client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
	defer client.Stop()
	_, err := client.applyEventBatch(5, encodeBlockStored(t, 500), false)
	require.NoError(t, err)
	require.Empty(t, handler.GetEvents())
	// While the first replay was unavailable, its finite publisher buffer
	// advanced past the earlier live batch. The reply starts at its oldest
	// remaining batch; the obsolete pending entry must be discarded.
	_, err = client.applyEventBatch(10, encodeBlockStored(t, 1000), true)
	require.NoError(t, err)
	require.Equal(t, []int64{1000}, storedHashes(handler))
	require.Equal(t, int64(10), client.GetLastSequence())
	require.Empty(t, client.pending, "replayed batches must not remain in the bounded pending buffer")
}

type retryIterationContext struct {
	context.Context
	iterations chan struct{}
}

func (c *retryIterationContext) Done() <-chan struct{} {
	select {
	case c.iterations <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestZMQClientRetryContinuesAfterSkippedReplay(t *testing.T) {
	publisher := createMockPublisher(t, 25811, 25812)
	defer publisher.Close()
	publishBlockStored(t, publisher, 100)
	client, handler := newReplayTestClient(25811, 25812, time.Second)
	defer client.Stop()
	require.NoError(t, client.Connect())
	iterations := make(chan struct{}, 4)
	client.ctx = &retryIterationContext{Context: client.ctx, iterations: iterations}
	client.mu.Lock()
	client.replayInFlight = true
	client.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.retryReplay(0, 3)
	}()
	defer func() {
		client.cancel()
		<-done
	}()
	// Done is evaluated at each retry's select. Reaching the second select
	// proves the first attempt was skipped rather than reported as success.
	for i := 0; i < 2; i++ {
		select {
		case <-iterations:
		case <-done:
			t.Fatal("a replay owned by another caller was treated as a successful retry")
		case <-time.After(2 * time.Second):
			t.Fatal("retry did not advance to its next attempt")
		}
	}
	client.mu.Lock()
	client.replayInFlight = false
	client.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not finish after the replay socket became available")
	}
	require.Equal(t, []int64{100}, storedHashes(handler))
}

func TestZMQClientSuccessfulRetryRecoversUnreplayableGap(t *testing.T) {
	publisher := createMockPublisher(t, 25815, 25816)
	defer publisher.Close() // Empty history: replay succeeds with only END_SEQ.
	h := newGuardedViewHarness(t)
	oldPrefix, newPrefix := faultPrefix('A'), faultPrefix('B')
	h.apply(t, faultArrival{seq: 0, events: storedEvents([]int64{101, 102}, oldPrefix)})
	h.apply(t, faultArrival{seq: 3, events: storedEvents([]int64{201, 202}, newPrefix)})
	// The initial asynchronous attempt has no socket. Wait for it to finish
	// before providing the publisher, so this test isolates retryReplay.
	h.client.replayWg.Wait()
	require.True(t, h.fullHit(oldPrefix))
	require.False(t, h.fullHit(newPrefix))
	h.client.config.PodIP = "127.0.0.1"
	h.client.config.PubPort, h.client.config.RouterPort = 25815, 25816
	h.client.config.ReplayTimeout = time.Second
	require.NoError(t, h.client.Connect())
	h.client.retryReplay(1, 1)
	require.False(t, h.fullHit(oldPrefix), "a successful but incomplete replay must invalidate the old view")
	require.True(t, h.fullHit(newPrefix))
	require.Equal(t, int64(3), h.client.GetLastSequence())
	require.Empty(t, h.client.pending)
}

type blockedReplayHandler struct {
	*MockEventHandler
	entered chan struct{}
	release chan struct{}
}

func (h *blockedReplayHandler) HandleEvent(event KVEvent) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return h.MockEventHandler.HandleEvent(event)
}

func TestZMQClientStopWaitsForReplayHandler(t *testing.T) {
	publisher := createMockPublisher(t, 25813, 25814)
	defer publisher.Close()
	publishBlockStored(t, publisher, 100)
	handler := &blockedReplayHandler{MockEventHandler: NewMockEventHandler(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	config := DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model")
	config.PubPort, config.RouterPort = 25813, 25814
	config.ReplayTimeout = time.Second
	client := NewZMQClient(config, handler)
	defer client.Stop()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(handler.release) }) }
	defer release()
	require.NoError(t, client.Connect())
	client.requestReplayAsync(0)
	select {
	case <-handler.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background replay never reached its handler")
	}
	stopped := make(chan struct{})
	go func() { client.Stop(); close(stopped) }()
	require.Eventually(t, func() bool {
		client.mu.RLock()
		defer client.mu.RUnlock()
		return client.stopped
	}, time.Second, time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("Stop returned while a replay handler still owned the socket")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish after the replay handler returned")
	}
	require.Equal(t, []int64{100}, storedHashes(handler.MockEventHandler))
}
