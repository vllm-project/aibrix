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
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestZMQClientRecoveryStopsWithoutProgress(t *testing.T) {
	handler := &MockHandlerWithAssertions{}
	handler.Test(t)
	// A second clear fails immediately if recovery retries the malformed batch
	// forever, so removing the progress guard cannot hang this test.
	handler.On("HandleEvent", mock.AnythingOfType("*kvcache.AllBlocksClearedEvent")).Return(nil).Once()
	client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
	t.Cleanup(client.Stop)
	_, err := client.applyEventBatch(0, encodeFaultBatch(t), true)
	require.NoError(t, err)
	payload := []byte{0xc1} // Reserved MessagePack byte: decoding cannot advance.
	_, err = client.applyEventBatch(3, payload, true)
	require.NoError(t, err)
	client.replayWg.Wait()
	client.recoverStalledGap()
	require.Equal(t, int64(2), client.GetLastSequence())
	require.Equal(t, map[int64][]byte{3: payload}, client.pending)
	handler.AssertExpectations(t)
}

func TestZMQClientRecoveryLeavesNonGapsAlone(t *testing.T) {
	for _, seq := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprintf("pending_%d", seq), func(t *testing.T) {
			handler := &MockHandlerWithAssertions{}
			handler.Test(t) // No invalidation or batch dispatch is expected.
			client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
			t.Cleanup(client.Stop)
			client.setLastSequence(0)
			if seq >= 0 { // Absent, duplicate, or contiguous (not a gap).
				client.applyMu.Lock()
				require.True(t, client.bufferPendingLocked(seq, encodeFaultBatch(t)))
				client.applyMu.Unlock()
			}
			client.recoverStalledGap()
			require.Equal(t, int64(0), client.GetLastSequence())
			require.Equal(t, seq >= 0, len(client.pending) > 0)
		})
	}
}

// recoverStalledGap runs on the replay goroutine while live batches keep
// arriving. Run with -race: both sides must hold applyMu.
func TestZMQClientRecoveryRacesLiveBatches(t *testing.T) {
	handler := &MockHandlerWithAssertions{}
	handler.Test(t)
	handler.On("HandleEvent", mock.AnythingOfType("*kvcache.AllBlocksClearedEvent")).Return(nil).Maybe()
	client := NewZMQClient(DefaultZMQClientConfig(t.Name(), "127.0.0.1", "model"), handler)
	t.Cleanup(client.Stop)
	payload := encodeFaultBatch(t)
	_, err := client.applyEventBatch(0, payload, true)
	require.NoError(t, err)

	const lastSeq = 400
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for seq := int64(2); seq <= lastSeq; seq += 2 { // every batch leaves a gap
			_, _ = client.applyEventBatch(seq, payload, true)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < lastSeq/2; i++ {
			client.recoverStalledGap()
		}
	}()
	wg.Wait()
	client.replayWg.Wait()

	client.recoverStalledGap()
	require.Equal(t, int64(lastSeq), client.GetLastSequence())
	require.Empty(t, client.pending)
}

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

// silentReplayRouter binds a replay ROUTER that takes requests and never
// answers, like a publisher whose replay thread is stuck. It signals each
// request it receives on the returned channel.
func silentReplayRouter(t *testing.T, port int) <-chan struct{} {
	t.Helper()
	sock, err := zmq.NewSocket(zmq.ROUTER)
	require.NoError(t, err)
	require.NoError(t, sock.SetIpv6(true))
	require.NoError(t, sock.Bind(formatZMQBindEndpoint("::", port)))
	require.NoError(t, sock.SetRcvtimeo(50*time.Millisecond))

	requests := make(chan struct{}, 16)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := sock.RecvMessageBytes(0); err == nil {
				select {
				case requests <- struct{}{}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		_ = sock.Close()
	})
	return requests
}

func waitForReplayRequest(t *testing.T, requests <-chan struct{}) {
	t.Helper()
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("the replay request never reached the publisher")
	}
}

// Start runs in the store's pod event handlers, before vLLM has loaded the
// model and bound its replay socket. It must not wait for the startup replay,
// and the replay must still land once the publisher binds (#3363).
func TestZMQClientStartReturnsBeforeReplay(t *testing.T) {
	// Nothing is bound on either port yet.
	client, handler := newReplayTestClient(25821, 25822, 2*time.Second)
	// Only the first attempt can deliver the replay within this test, so it
	// must not wait for ReconnectDelay.
	client.config.ReconnectDelay = time.Minute
	defer client.Stop()

	start := time.Now()
	require.NoError(t, client.Start())
	require.Less(t, time.Since(start), 500*time.Millisecond, "Start waited for the startup replay")

	// The replay socket comes up after Start returned. The request already
	// queued on the client's DEALER reaches it then.
	serveReplayOnce(t, 25822,
		replayMessage(0, encodeBlockStored(t, 100)),
		replayMessage(1, encodeBlockStored(t, 200)),
		replayMessage(mockReplayEndSeq, []byte{}))
	require.Eventually(t, func() bool { return client.GetLastSequence() == 1 },
		2*time.Second, 10*time.Millisecond, "the startup replay was not applied")
	require.Equal(t, []int64{100, 200}, storedHashes(handler))
}

// A publisher that binds its replay socket only after the first startup
// attempt timed out still gets asked again.
func TestZMQClientStartupReplayRetriesUntilPublisherBinds(t *testing.T) {
	client, handler := newReplayTestClient(25823, 25824, 300*time.Millisecond)
	client.config.ReconnectDelay = 700 * time.Millisecond
	defer client.Stop()
	require.NoError(t, client.Start())

	// The first attempt gives up after 0.3 s and drops its request with the
	// socket; the next one starts at about 1 s.
	time.Sleep(650 * time.Millisecond)
	serveReplayOnce(t, 25824,
		replayMessage(0, encodeBlockStored(t, 100)),
		replayMessage(mockReplayEndSeq, []byte{}))
	require.Eventually(t, func() bool { return client.GetLastSequence() == 0 },
		2*time.Second, 10*time.Millisecond, "the startup replay was not retried")
	require.Equal(t, []int64{100}, storedHashes(handler))
}

// Stop runs in the same handlers as Start (pod delete, and pod update on a
// model, LoRA or IP change). It must not wait out an unanswered replay (#3363).
func TestZMQClientStopDoesNotWaitForUnansweredReplay(t *testing.T) {
	requests := silentReplayRouter(t, 25826)
	client, _ := newReplayTestClient(25825, 25826, 10*time.Second)
	require.NoError(t, client.Connect())
	client.requestReplayAsync(0)
	waitForReplayRequest(t, requests)

	start := time.Now()
	client.Stop()
	require.Less(t, time.Since(start), time.Second, "Stop waited for the unanswered replay")
}

// A replay cut short by Stop did not fail. It must not count as a replay
// failure, recreate the replay socket, or clear the view for the gap it was
// filling.
func TestZMQClientCancelledReplayIsNotAFailure(t *testing.T) {
	requests := silentReplayRouter(t, 25828)
	client, handler := newReplayTestClient(25827, 25828, 10*time.Second)
	defer client.Stop()
	failures := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_replay_failures_total"}, nil)
	client.metrics.replayFailures = failures.WithLabelValues()
	require.NoError(t, client.Connect())
	socket := replaySocketOf(client)

	// Batch 0 is applied and live batch 2 is held: the replay from 1 goes out
	// and is never answered.
	_, err := client.applyEventBatch(0, encodeBlockStored(t, 100), true)
	require.NoError(t, err)
	_, err = client.applyEventBatch(2, encodeBlockStored(t, 300), false)
	require.NoError(t, err)
	waitForReplayRequest(t, requests)

	client.cancel()
	returned := make(chan struct{})
	go func() {
		client.replayWg.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("the replay did not return after the client was cancelled")
	}

	value, err := getMetricValue(failures)
	require.NoError(t, err)
	require.Zero(t, value, "the cancelled replay was counted as a failure")
	require.Same(t, socket, replaySocketOf(client), "the replay socket was recreated after a cancel")
	for _, event := range handler.GetEvents() {
		require.NotEqual(t, EventTypeAllCleared, event.GetType(), "the gap was cleared as unrecoverable after a cancel")
	}
	require.Equal(t, []int64{100}, storedHashes(handler))
	require.Equal(t, int64(0), client.GetLastSequence())
}
