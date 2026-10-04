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
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	zmq "github.com/pebbe/zmq4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// MockEventHandler implements EventHandler for testing
type MockEventHandler struct {
	mu           sync.Mutex
	events       []KVEvent
	handleErrors map[int]error // Map of call index to error
	handleDelay  time.Duration
	callCount    int
}

func NewMockEventHandler() *MockEventHandler {
	return &MockEventHandler{
		events:       []KVEvent{},
		handleErrors: make(map[int]error),
		callCount:    0,
	}
}

func (m *MockEventHandler) HandleEvent(event KVEvent) error {
	if m.handleDelay > 0 {
		time.Sleep(m.handleDelay)
	}

	m.mu.Lock()
	callIndex := m.callCount
	m.callCount++
	err, hasError := m.handleErrors[callIndex]

	if !hasError {
		// Only add event if no error is configured
		m.events = append(m.events, event)
	}
	m.mu.Unlock()

	if hasError {
		return err
	}
	return nil
}

func (m *MockEventHandler) GetEvents() []KVEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := make([]KVEvent, len(m.events))
	copy(events, m.events)
	return events
}

func (m *MockEventHandler) SetHandleError(callIndex int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handleErrors[callIndex] = err
}

func TestZMQClientConfig(t *testing.T) {
	config := DefaultZMQClientConfig("test-pod", "10.0.0.1", "test-model")

	assert.Equal(t, "test-pod", config.PodKey)
	assert.Equal(t, "10.0.0.1", config.PodIP)
	assert.Equal(t, "test-model", config.ModelName)
	assert.Equal(t, DefaultPubPort, config.PubPort)
	assert.Equal(t, DefaultRouterPort, config.RouterPort)
	assert.Equal(t, DefaultPollTimeout, config.PollTimeout)
	assert.Equal(t, DefaultReplayTimeout, config.ReplayTimeout)
	assert.Equal(t, DefaultReconnectInterval, config.ReconnectDelay)
}

func TestNewZMQClient(t *testing.T) {
	config := DefaultZMQClientConfig("test-pod", "10.0.0.1", "test-model")
	handler := NewMockEventHandler()

	client := NewZMQClient(config, handler)

	assert.NotNil(t, client)
	assert.Equal(t, config, client.config)
	assert.Equal(t, handler, client.eventHandler)
	assert.Equal(t, int64(-1), client.lastSeq)
	assert.False(t, client.connected)
	assert.NotNil(t, client.ctx)
	assert.NotNil(t, client.cancel)
	assert.NotNil(t, client.metrics)
}

func TestZMQClientLifecycle(t *testing.T) {
	config := DefaultZMQClientConfig("test-pod", "10.0.0.1", "test-model")
	handler := NewMockEventHandler()

	client := NewZMQClient(config, handler)

	// Test initial state
	assert.False(t, client.IsConnected())
	assert.Equal(t, int64(-1), client.GetLastSequence())

	// Test Stop without Start
	client.Stop()

	// Verify clean shutdown
	select {
	case <-client.ctx.Done():
		// Context should be cancelled
	default:
		t.Fatal("Context should be cancelled after Stop")
	}
}

func TestZMQClientReconnectDelay(t *testing.T) {
	config := DefaultZMQClientConfig("test-pod", "10.0.0.1", "test-model")
	config.ReconnectDelay = 100 * time.Millisecond
	handler := NewMockEventHandler()

	client := NewZMQClient(config, handler)

	// Test exponential backoff
	assert.Equal(t, config.ReconnectDelay, client.reconnectDelay)

	// Simulate failed reconnection
	client.mu.Lock()
	client.reconnectDelay = time.Duration(float64(client.reconnectDelay) * ReconnectBackoffFactor)
	client.mu.Unlock()

	assert.Equal(t, 200*time.Millisecond, client.reconnectDelay)

	// Test max reconnect interval
	client.mu.Lock()
	client.reconnectDelay = MaxReconnectInterval * 2
	if client.reconnectDelay > MaxReconnectInterval {
		client.reconnectDelay = MaxReconnectInterval
	}
	client.mu.Unlock()

	assert.Equal(t, MaxReconnectInterval, client.reconnectDelay)
}

// TestMockZMQPublisher tests with a mock ZMQ publisher
func TestMockZMQPublisher(t *testing.T) {
	// Skip if ZMQ is not available
	ctx, err := zmq.NewContext()
	if err != nil {
		t.Skip("ZMQ not available:", err)
	}
	defer func() { _ = ctx.Term() }()

	// Create mock publisher
	publisher, err := zmq.NewSocket(zmq.PUB)
	require.NoError(t, err)
	defer func() { _ = publisher.Close() }()

	// Enable IPv6 for dual-stack support
	err = publisher.SetIpv6(true)
	require.NoError(t, err)

	err = publisher.Bind("tcp://127.0.0.1:5557")
	require.NoError(t, err)

	// Allow time for binding
	time.Sleep(100 * time.Millisecond)

	// Create client
	config := DefaultZMQClientConfig("test-pod", "127.0.0.1", "test-model")
	config.PollTimeout = 50 * time.Millisecond
	handler := NewMockEventHandler()
	client := NewZMQClient(config, handler)

	// Connect should work
	require.NoError(t, client.Connect())
	assert.True(t, client.IsConnected())

	// Prepare test event
	now := time.Now().UTC().Truncate(time.Second)
	testEvent := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		BlockHashes: []int64{123, 456},
		TokenIDs: [][]byte{
			tokenIDsToBytes([]uint32{1, 2}),
			tokenIDsToBytes([]uint32{3, 4}),
		},
	}

	testBatch := &EventBatch{
		Timestamp: now,
		Events:    []KVEvent{testEvent},
	}

	// Encode batch
	payload, err := EncodeEventBatch(testBatch)
	require.NoError(t, err)

	// Start client before publishing to avoid race condition
	require.NoError(t, client.Start())

	// Wait for client to start consuming
	time.Sleep(100 * time.Millisecond)

	// Publish message after client has started
	seq := make([]byte, 8)
	binary.BigEndian.PutUint64(seq, 1)
	_, err = publisher.SendBytes([]byte("test-topic"), zmq.SNDMORE)
	require.NoError(t, err)
	_, err = publisher.SendBytes(seq, zmq.SNDMORE)
	require.NoError(t, err)
	_, err = publisher.SendBytes(payload, 0)
	require.NoError(t, err)

	// Wait for event to be processed
	time.Sleep(200 * time.Millisecond)

	// Stop client
	client.Stop()

	// Check received events
	events := handler.GetEvents()
	require.Len(t, events, 1, "expected exactly one event")

	receivedEvent, ok := events[0].(*BlockStoredEvent)
	require.True(t, ok, "event type mismatch")

	fmt.Println("Type:", receivedEvent.Type)
	fmt.Println("BlockHashes:", receivedEvent.BlockHashes)
	fmt.Println("TokenIDs:", receivedEvent.TokenIDs)
	fmt.Println("Timestamp:", receivedEvent.Timestamp)

	assert.Equal(t, testEvent.Type, receivedEvent.Type)
	assert.Equal(t, testEvent.BlockHashes, receivedEvent.BlockHashes)
	assert.Equal(t, testEvent.TokenIDs, receivedEvent.TokenIDs)
	assert.Equal(t, now, receivedEvent.Timestamp)
	assert.Equal(t, "test-model", receivedEvent.ModelName)
	assert.Equal(t, "test-pod", receivedEvent.PodName)
}

func TestMetricsTracking(t *testing.T) {
	config := DefaultZMQClientConfig("test-metrics-pod", "10.0.0.1", "test-model")
	handler := NewMockEventHandler()

	client := NewZMQClient(config, handler)

	// Test connection metrics
	client.mu.Lock()
	client.connected = true
	client.mu.Unlock()
	client.metrics.IncrementConnectionCount()

	// Test disconnection metrics
	client.markDisconnected()
	assert.False(t, client.IsConnected())

	// Test event metrics
	client.metrics.IncrementEventCount(string(EventTypeBlockStored))
	client.metrics.RecordEventProcessingLatency(1 * time.Millisecond)

	// Test error metrics
	client.metrics.IncrementErrorCount("test_error")

	// Test missed events
	client.metrics.IncrementMissedEvents(5)

	// Cleanup metrics
	client.metrics.Delete()
}

func TestEventHandlerErrors(t *testing.T) {
	handler := NewMockEventHandler()

	// Configure error for the first call (when events array is empty)
	handler.SetHandleError(0, errors.New("test error"))

	event := &BlockStoredEvent{
		Type:      EventTypeBlockStored,
		Timestamp: time.Now(),
	}

	// First event should return error
	err := handler.HandleEvent(event)
	assert.Error(t, err)
	assert.Equal(t, "test error", err.Error())

	// Check that event was NOT added due to error
	events := handler.GetEvents()
	assert.Len(t, events, 0)

	// Second event should succeed (no error configured for index 0 when events is still empty)
	// The index is still 0 because no events were added
	err = handler.HandleEvent(event)
	assert.NoError(t, err)

	events = handler.GetEvents()
	assert.Len(t, events, 1)
}

// TestZMQClientEventProcessingFull tests complete event processing flow
func TestZMQClientEventProcessingFull(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25557, 25558)
	defer publisher.Close()

	// Give publisher time to bind
	time.Sleep(100 * time.Millisecond)

	handler := NewMockEventHandler()
	config := &ZMQClientConfig{
		PodKey:         "test-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "test-model",
		PubPort:        25557,
		RouterPort:     25558,
		PollTimeout:    100 * time.Millisecond,
		ReplayTimeout:  1 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	client := NewZMQClient(config, handler)
	defer client.Stop()

	// Start client
	err := client.Start()
	require.NoError(t, err)

	// Give client time to connect and request replay
	time.Sleep(200 * time.Millisecond)

	// Test various event types
	parentHash := int64(9999)
	events := []KVEvent{
		&BlockStoredEvent{
			Type:        EventTypeBlockStored,
			BlockHashes: []int64{1234, 5678},
			TokenIDs: [][]byte{
				tokenIDsToBytes([]uint32{1, 2, 3}),
				tokenIDsToBytes([]uint32{4, 5, 6}),
			},
			ParentBlockHash: &parentHash,
		},
		&BlockRemovedEvent{
			Type:        EventTypeBlockRemoved,
			BlockHashes: []int64{1234},
		},
		&AllBlocksClearedEvent{
			Type: EventTypeAllCleared,
		},
	}

	// Publish events
	for _, event := range events {
		err = publisher.PublishEvent(event)
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Verify all events were received
	receivedEvents := handler.GetEvents()
	assert.Len(t, receivedEvents, 3)

	// Verify pod name was set on all events
	for _, event := range receivedEvents {
		switch e := event.(type) {
		case *BlockStoredEvent:
			assert.Equal(t, []int64{1234, 5678}, e.BlockHashes)
			assert.Equal(t, parentHash, *e.ParentBlockHash)
			assert.Equal(t, "test-pod", e.PodName)
		case *BlockRemovedEvent:
			assert.Equal(t, "test-pod", e.PodName)
			assert.Equal(t, []int64{1234}, e.BlockHashes)
		case *AllBlocksClearedEvent:
			assert.Equal(t, "test-pod", e.PodName)
		}
	}
}

// TestZMQClientReconnectionFlow tests complete reconnection flow
func TestZMQClientReconnectionFlow(t *testing.T) {
	skipIfZMQUnavailable(t)

	// Start publisher
	publisher := createMockPublisher(t, 25559, 25560)

	handler := NewMockEventHandler()
	config := &ZMQClientConfig{
		PodKey:         "test-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "test-model",
		PubPort:        25559,
		RouterPort:     25560,
		PollTimeout:    100 * time.Millisecond,
		ReplayTimeout:  1 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	client := NewZMQClient(config, handler)
	defer client.Stop()

	// Start client
	err := client.Start()
	require.NoError(t, err)

	// Give client time to connect
	time.Sleep(200 * time.Millisecond)
	assert.True(t, client.IsConnected())

	// Publish first event
	testEvent1 := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		BlockHashes: []int64{1000},
		TokenIDs: [][]byte{
			tokenIDsToBytes([]uint32{10}),
		},
	}
	err = publisher.PublishEvent(testEvent1)
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// Stop publisher to simulate connection loss
	publisher.Close()
	time.Sleep(100 * time.Millisecond)

	// Client should detect disconnection
	time.Sleep(500 * time.Millisecond)

	// Restart publisher
	publisher = createMockPublisher(t, 25559, 25560)
	defer publisher.Close()

	// Wait for reconnection
	time.Sleep(1 * time.Second)

	// Should be reconnected
	assert.True(t, client.IsConnected())

	// Publish second event
	testEvent2 := &BlockRemovedEvent{
		Type:        EventTypeBlockRemoved,
		BlockHashes: []int64{1000},
	}
	err = publisher.PublishEvent(testEvent2)
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Verify both events were received
	events := handler.GetEvents()
	assert.Len(t, events, 2)
}

// TestZMQClientSequenceHandling tests sequence number tracking and gap detection
func TestZMQClientSequenceHandling(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25561, 25562)
	defer publisher.Close()

	// Give publisher time to bind
	time.Sleep(100 * time.Millisecond)

	handler := NewMockEventHandler()
	config := &ZMQClientConfig{
		PodKey:         "test-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "test-model",
		PubPort:        25561,
		RouterPort:     25562,
		PollTimeout:    100 * time.Millisecond,
		ReplayTimeout:  1 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	client := NewZMQClient(config, handler)
	defer client.Stop()

	// Start client
	err := client.Start()
	require.NoError(t, err)

	// Give client time to connect
	time.Sleep(200 * time.Millisecond)

	// Publish events with gaps in sequence
	for i := 0; i < 10; i++ {
		if i == 5 || i == 6 {
			// Skip sequences 5 and 6 to create a gap
			publisher.sequence += 2
			continue
		}

		testEvent := &BlockStoredEvent{
			Type:        EventTypeBlockStored,
			BlockHashes: []int64{int64(i * 100)},
			TokenIDs: [][]byte{
				tokenIDsToBytes([]uint32{uint32(i)}),
			},
		}
		err = publisher.PublishEvent(testEvent)
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Should have received 8 events (10 - 2 skipped)
	events := handler.GetEvents()
	assert.Len(t, events, 8)

	// Verify sequence tracking
	lastSeq := client.GetLastSequence()
	assert.GreaterOrEqual(t, lastSeq, int64(9))
}

// TestZMQClientStartAppliesReplayedBatches tests that a client that subscribes
// after the engine has published batches gets them from the initial replay
func TestZMQClientStartAppliesReplayedBatches(t *testing.T) {
	skipIfZMQUnavailable(t)

	tests := []struct {
		name         string
		pubPort      int
		routerPort   int
		withoutTopic bool
	}{
		{name: "vLLM v0.26+ framing", pubPort: 25581, routerPort: 25582},
		{name: "vLLM v0.25 and earlier framing", pubPort: 25583, routerPort: 25584, withoutTopic: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := createMockPublisher(t, tt.pubPort, tt.routerPort)
			defer publisher.Close()
			publisher.mu.Lock()
			publisher.replayWithoutTopic = tt.withoutTopic
			publisher.mu.Unlock()

			// Published before the client connects, so they only reach it by replay
			publishBlockStored(t, publisher, 100, 200, 300)

			client, handler := newReplayTestClient(tt.pubPort, tt.routerPort, time.Second)
			defer client.Stop()

			// Start requests the replay from seq 0 and returns when it is done
			require.NoError(t, client.Start())

			assert.Equal(t, []int64{100, 200, 300}, storedHashes(handler))
			assert.Equal(t, int64(3), client.GetLastSequence())
			requireNoPendingReplayFrames(t, client)
		})
	}
}

// TestZMQClientReplaySkipsAppliedBatches tests that replayed batches at or
// below lastSeq are not applied again
func TestZMQClientReplaySkipsAppliedBatches(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25585, 25586)
	defer publisher.Close()
	publishBlockStored(t, publisher, 100, 200, 300)

	client, handler := newReplayTestClient(25585, 25586, time.Second)
	defer client.Stop()
	require.NoError(t, client.Connect())

	client.mu.Lock()
	client.lastSeq = 2
	client.mu.Unlock()

	require.NoError(t, client.requestReplay(0))

	assert.Equal(t, []int64{300}, storedHashes(handler))
	assert.Equal(t, int64(3), client.GetLastSequence())
	requireNoPendingReplayFrames(t, client)
}

// TestZMQClientReplayEndMarkerOnly tests a replay with nothing to resend
func TestZMQClientReplayEndMarkerOnly(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25587, 25588)
	defer publisher.Close()

	client, handler := newReplayTestClient(25587, 25588, time.Second)
	defer client.Stop()
	require.NoError(t, client.Connect())

	require.NoError(t, client.requestReplay(0))

	assert.Empty(t, handler.GetEvents())
	assert.Equal(t, int64(-1), client.GetLastSequence())
	requireNoPendingReplayFrames(t, client)
}

// TestZMQClientReplayTimeoutRecreatesSocket tests that a reply cut off by
// ReplayTimeout keeps the batches applied so far, and that its late frames are
// not read as the reply to the next request
func TestZMQClientReplayTimeoutRecreatesSocket(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25589, 25590)
	defer publisher.Close()
	publishBlockStored(t, publisher, 100, 200, 300)

	// Hold the first reply after its first batch until the client gave up
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReply := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseReply()
	stalled := false
	publisher.mu.Lock()
	publisher.beforeReplayBatch = func(n int) {
		if n == 1 && !stalled {
			stalled = true
			<-release
		}
	}
	publisher.mu.Unlock()

	client, handler := newReplayTestClient(25589, 25590, 500*time.Millisecond)
	defer client.Stop()
	require.NoError(t, client.Connect())
	firstSocket := replaySocketOf(client)

	err := client.requestReplay(0)
	require.Error(t, err, "requestReplay returned before the end of the replay")
	assert.Contains(t, err.Error(), "timed out")
	assert.Equal(t, []int64{100}, storedHashes(handler), "the batch received before the timeout stays applied")
	assert.Equal(t, int64(1), client.GetLastSequence())
	assert.NotSame(t, firstSocket, replaySocketOf(client), "replay socket was not recreated after the timeout")

	// The rest of the first reply now goes to the closed socket
	releaseReply()

	require.NoError(t, client.requestReplay(client.GetLastSequence()+1))
	assert.Equal(t, []int64{100, 200, 300}, storedHashes(handler))
	assert.Equal(t, int64(3), client.GetLastSequence())
	requireNoPendingReplayFrames(t, client)
}

// TestZMQClientReplayRejectsMalformedReply tests that a malformed replay
// message fails the replay without applying it or anything after it
func TestZMQClientReplayRejectsMalformedReply(t *testing.T) {
	skipIfZMQUnavailable(t)

	end := replayMessage(mockReplayEndSeq, []byte{})
	tests := []struct {
		name        string
		routerPort  int
		reply       [][][]byte
		wantHashes  []int64
		wantLastSeq int64
	}{
		{
			name:       "undecodable payload",
			routerPort: 25591,
			reply: [][][]byte{
				replayMessage(1, encodeBlockStored(t, 100)),
				replayMessage(2, []byte{0xc1}), // 0xc1 is never used in msgpack
				replayMessage(3, encodeBlockStored(t, 300)),
				end,
			},
			wantHashes:  []int64{100},
			wantLastSeq: 1,
		},
		{
			name:        "too few frames",
			routerPort:  25592,
			reply:       [][][]byte{{{}, encodeSeq(1)}, end},
			wantLastSeq: -1,
		},
		{
			name:        "short sequence frame",
			routerPort:  25593,
			reply:       [][][]byte{{{}, {}, {0, 0, 0, 1}, encodeBlockStored(t, 100)}, end},
			wantLastSeq: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serveReplayOnce(t, tt.routerPort, tt.reply...)

			client, handler := newReplayTestClient(25599, tt.routerPort, time.Second)
			defer client.Stop()
			require.NoError(t, client.Connect())
			firstSocket := replaySocketOf(client)

			err := client.requestReplay(0)
			require.Error(t, err, "malformed replay reply was accepted")
			assert.Equal(t, tt.wantHashes, storedHashes(handler))
			assert.Equal(t, tt.wantLastSeq, client.GetLastSequence())
			assert.NotSame(t, firstSocket, replaySocketOf(client), "replay socket was not recreated after the error")
		})
	}
}

// Match vLLM's ZmqEventPublisher: the default buffer_steps and END_SEQ
const (
	mockReplayBufferSteps       = 10000
	mockReplayEndSeq      int64 = -1
)

// Mock publisher helper implementation
type mockPublisher struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pubSock  *zmq.Socket
	repSock  *zmq.Socket
	sequence int64
	// buffer holds the published batches for replay, oldest first
	buffer []bufferedBatch
	// replayWithoutTopic makes replay replies use the framing of vLLM v0.25
	// and earlier, which had no topic frame
	replayWithoutTopic bool
	// beforeReplayBatch, if set, runs before each batch of a replay reply is
	// sent; n is the number of batches already sent in that reply
	beforeReplayBatch func(n int)
	// replayDone is closed when handleReplay returns
	replayDone chan struct{}
	mu         sync.Mutex
}

type bufferedBatch struct {
	seq     int64
	payload []byte
}

func createMockPublisher(t testing.TB, pubPort, repPort int) *mockPublisher {
	ctx, cancel := context.WithCancel(context.Background())

	// Create PUB socket
	pubSock, err := zmq.NewSocket(zmq.PUB)
	require.NoError(t, err)

	// Enable IPv6 for dual-stack support
	err = pubSock.SetIpv6(true)
	require.NoError(t, err)

	// Use IPv6 wildcard :: which also listens on IPv4
	err = pubSock.Bind(formatZMQBindEndpoint("::", pubPort))
	require.NoError(t, err)

	// Create ROUTER socket for replay
	repSock, err := zmq.NewSocket(zmq.ROUTER)
	require.NoError(t, err)

	// Enable IPv6 for dual-stack support
	err = repSock.SetIpv6(true)
	require.NoError(t, err)

	// Use IPv6 wildcard :: which also listens on IPv4
	err = repSock.Bind(formatZMQBindEndpoint("::", repPort))
	require.NoError(t, err)

	mp := &mockPublisher{
		ctx:        ctx,
		cancel:     cancel,
		pubSock:    pubSock,
		repSock:    repSock,
		replayDone: make(chan struct{}),
	}

	// Start replay handler
	go mp.handleReplay()

	return mp
}

func (mp *mockPublisher) PublishEvent(event KVEvent) error {
	// Encode event
	batch := &EventBatch{Events: []KVEvent{event}}
	data, err := EncodeEventBatch(batch)
	if err != nil {
		return err
	}

	// Send multipart message
	mp.mu.Lock()
	mp.sequence++
	seq := mp.sequence
	mp.mu.Unlock()

	seqBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seqBytes, uint64(seq))

	_, err = mp.pubSock.SendMessage("", seqBytes, data)

	mp.mu.Lock()
	mp.buffer = append(mp.buffer, bufferedBatch{seq: seq, payload: data})
	if len(mp.buffer) > mockReplayBufferSteps {
		mp.buffer = mp.buffer[1:]
	}
	mp.mu.Unlock()
	return err
}

func (mp *mockPublisher) Close() {
	mp.cancel()
	// ZMQ sockets are not thread safe: let handleReplay stop using repSock
	// before it is closed
	<-mp.replayDone
	_ = mp.pubSock.Close()
	_ = mp.repSock.Close()
}

func (mp *mockPublisher) handleReplay() {
	defer close(mp.replayDone)
	for {
		select {
		case <-mp.ctx.Done():
			return
		default:
			// Handle replay requests with non-blocking receive
			msg, err := mp.repSock.RecvMessageBytes(zmq.DONTWAIT)
			if err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}

			mp.serviceReplay(msg)
		}
	}
}

// serviceReplay answers a replay request the way vLLM's
// ZmqEventPublisher._service_replay (vllm/distributed/kv_events.py) does: one
// [identity, "", topic, seq, payload] message per buffered batch with
// seq >= the requested start, then [identity, "", "", END_SEQ, ""]. vLLM v0.25
// and earlier sent the same messages without the topic frame.
func (mp *mockPublisher) serviceReplay(frame [][]byte) {
	if len(frame) != 3 {
		// vLLM logs "Invalid replay request" and sends nothing
		return
	}
	clientID := frame[0]
	startSeq := int64(binary.BigEndian.Uint64(frame[2]))

	mp.mu.Lock()
	buffer := append([]bufferedBatch(nil), mp.buffer...)
	hook := mp.beforeReplayBatch
	withoutTopic := mp.replayWithoutTopic
	mp.mu.Unlock()

	// The mock publishes with an empty topic, so the topic frame is empty
	send := func(seq int64, payload []byte) {
		if withoutTopic {
			_, _ = mp.repSock.SendMessage(clientID, []byte{}, encodeSeq(seq), payload)
		} else {
			_, _ = mp.repSock.SendMessage(clientID, []byte{}, []byte{}, encodeSeq(seq), payload)
		}
	}

	sent := 0
	for _, b := range buffer {
		if b.seq >= startSeq {
			if hook != nil {
				hook(sent)
			}
			send(b.seq, b.payload)
			sent++
		}
	}
	send(mockReplayEndSeq, []byte{})
}

func encodeSeq(seq int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(seq))
	return b
}

// replayMessage is one message of a vLLM replay reply as the DEALER receives
// it: [empty_delimiter, topic, sequence, payload]
func replayMessage(seq int64, payload []byte) [][]byte {
	return [][]byte{{}, {}, encodeSeq(seq), payload}
}

// serveReplayOnce binds a ROUTER that answers the first replay request with
// the given messages, for replies the mock publisher does not produce
func serveReplayOnce(t *testing.T, port int, messages ...[][]byte) {
	t.Helper()

	sock, err := zmq.NewSocket(zmq.ROUTER)
	require.NoError(t, err)
	require.NoError(t, sock.SetIpv6(true))
	require.NoError(t, sock.Bind(formatZMQBindEndpoint("::", port)))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sock.SetRcvtimeo(5 * time.Second)
		request, err := sock.RecvMessageBytes(0)
		if err != nil {
			return
		}
		for _, message := range messages {
			_, _ = sock.SendMessage(request[0], message)
		}
	}()
	t.Cleanup(func() {
		<-done
		_ = sock.Close()
	})
}

func newReplayTestClient(pubPort, routerPort int, replayTimeout time.Duration) (*ZMQClient, *MockEventHandler) {
	handler := NewMockEventHandler()
	config := &ZMQClientConfig{
		PodKey:         "test-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "test-model",
		PubPort:        pubPort,
		RouterPort:     routerPort,
		PollTimeout:    100 * time.Millisecond,
		ReplayTimeout:  replayTimeout,
		ReconnectDelay: 100 * time.Millisecond,
	}
	return NewZMQClient(config, handler), handler
}

func blockStoredEvent(hash int64) *BlockStoredEvent {
	return &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		BlockHashes: []int64{hash},
		TokenIDs:    [][]byte{tokenIDsToBytes([]uint32{uint32(hash)})},
	}
}

func encodeBlockStored(t *testing.T, hash int64) []byte {
	payload, err := EncodeEventBatch(&EventBatch{Events: []KVEvent{blockStoredEvent(hash)}})
	require.NoError(t, err)
	return payload
}

// publishBlockStored publishes one batch per hash, with seq 1, 2, ...
func publishBlockStored(t *testing.T, publisher *mockPublisher, hashes ...int64) {
	for _, hash := range hashes {
		require.NoError(t, publisher.PublishEvent(blockStoredEvent(hash)))
	}
}

// storedHashes returns the block hashes of the BlockStored events the
// handler received, in order
func storedHashes(handler *MockEventHandler) []int64 {
	var hashes []int64
	for _, event := range handler.GetEvents() {
		if e, ok := event.(*BlockStoredEvent); ok {
			hashes = append(hashes, e.BlockHashes...)
		}
	}
	return hashes
}

func replaySocketOf(client *ZMQClient) *zmq.Socket {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return client.replaySocket
}

// requireNoPendingReplayFrames fails if a message is left in the replay
// socket, where the next replay request would read it as its reply
func requireNoPendingReplayFrames(t *testing.T, client *ZMQClient) {
	t.Helper()
	socket := replaySocketOf(client)
	require.NotNil(t, socket)

	// Give frames still in flight time to arrive
	time.Sleep(100 * time.Millisecond)
	frames, err := socket.RecvMessageBytes(zmq.DONTWAIT)
	require.Error(t, err, "replay socket holds an unread message: %q", frames)
}

// Helper function to skip tests if ZMQ is not available
func skipIfZMQUnavailable(t testing.TB) {
	ctx, err := zmq.NewContext()
	if err != nil {
		t.Skip("ZMQ not available:", err)
	}
	defer func() { _ = ctx.Term() }()
}

// Benchmark tests
func BenchmarkZMQClientEventProcessing(b *testing.B) {
	skipIfZMQUnavailable(b)

	publisher := createMockPublisher(b, 25571, 25572)
	defer publisher.Close()

	time.Sleep(100 * time.Millisecond)

	handler := &benchmarkHandler{
		count: new(int32),
	}

	config := &ZMQClientConfig{
		PodKey:         "bench-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "bench-model",
		PubPort:        25571,
		RouterPort:     25572,
		PollTimeout:    10 * time.Millisecond,
		ReplayTimeout:  1 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	client := NewZMQClient(config, handler)
	defer client.Stop()

	err := client.Start()
	require.NoError(b, err)

	time.Sleep(200 * time.Millisecond)

	b.ResetTimer()

	// Publish events
	for i := 0; i < b.N; i++ {
		testEvent := &BlockStoredEvent{
			Type:        EventTypeBlockStored,
			BlockHashes: []int64{int64(i)},
			TokenIDs: [][]byte{
				tokenIDsToBytes([]uint32{uint32(i)}),
			},
		}
		err := publisher.PublishEvent(testEvent)
		if err != nil {
			b.Fatal(err)
		}
	}

	// Wait for all events to be processed
	deadline := time.Now().Add(30 * time.Second)
	for atomic.LoadInt32(handler.count) < int32(b.N) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	processed := atomic.LoadInt32(handler.count)
	if processed < int32(b.N) {
		b.Fatalf("Only processed %d/%d events", processed, b.N)
	}
}

type benchmarkHandler struct {
	count *int32
}

func (h *benchmarkHandler) HandleEvent(event KVEvent) error {
	atomic.AddInt32(h.count, 1)
	return nil
}

// MockHandlerWithAssertions implements EventHandler with mock.Mock for testing
type MockHandlerWithAssertions struct {
	mock.Mock
}

func (m *MockHandlerWithAssertions) HandleEvent(event KVEvent) error {
	args := m.Called(event)
	return args.Error(0)
}

// TestZMQClientWithMockHandler tests using testify mock
func TestZMQClientWithMockHandler(t *testing.T) {
	skipIfZMQUnavailable(t)

	publisher := createMockPublisher(t, 25563, 25564)
	defer publisher.Close()

	// Give publisher time to bind
	time.Sleep(100 * time.Millisecond)

	handler := new(MockHandlerWithAssertions)
	config := &ZMQClientConfig{
		PodKey:         "test-pod",
		PodIP:          "127.0.0.1",
		ModelName:      "test-model",
		PubPort:        25563,
		RouterPort:     25564,
		PollTimeout:    100 * time.Millisecond,
		ReplayTimeout:  1 * time.Second,
		ReconnectDelay: 100 * time.Millisecond,
	}
	client := NewZMQClient(config, handler)
	defer client.Stop()

	// Set up handler expectations
	handler.On("HandleEvent", mock.MatchedBy(func(e KVEvent) bool {
		bs, ok := e.(*BlockStoredEvent)
		return ok && len(bs.BlockHashes) == 2 && bs.PodName == "test-pod"
	})).Return(nil).Once()

	// Start client
	err := client.Start()
	require.NoError(t, err)

	// Give client time to connect
	time.Sleep(200 * time.Millisecond)

	testEvent := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		BlockHashes: []int64{1234, 5678},
		TokenIDs: [][]byte{
			tokenIDsToBytes([]uint32{1, 2, 3}),
			tokenIDsToBytes([]uint32{4, 5, 6}),
		},
	}

	err = publisher.PublishEvent(testEvent)
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Verify handler was called
	handler.AssertExpectations(t)
}
