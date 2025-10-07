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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	zmq "github.com/pebbe/zmq4"
	msgpack "github.com/shamaton/msgpack/v2"
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
	err = client.Connect()
	assert.NoError(t, err)
	assert.True(t, client.IsConnected())

	// Prepare test event
	now := time.Now().UTC().Truncate(time.Second)
	testEvent := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		Timestamp:   now,
		BlockHashes: []int64{123, 456},
		TokenIDs:    [][]int32{{1, 2}, {3, 4}},
		ModelName:   "test-model",
	}

	// Create event batch
	batch := map[string]interface{}{
		"events": []interface{}{
			map[string]interface{}{
				"type":         string(testEvent.Type),
				"timestamp":    testEvent.Timestamp.Unix(),
				"block_hashes": []interface{}{int64(123), int64(456)},
				"token_ids":    []interface{}{[]interface{}{int32(1), int32(2)}, []interface{}{int32(3), int32(4)}},
				"model_name":   testEvent.ModelName,
			},
		},
	}

	payload, err := msgpack.Marshal(batch)
	require.NoError(t, err)

	// Start client before publishing to avoid race conditions in CI
	// where messages sent before client starts might still be buffered
	// and received, causing the test to receive 2 events instead of 1
	err = client.Start()
	assert.NoError(t, err)

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
	assert.Len(t, events, 1)

	if len(events) > 0 {
		receivedEvent, ok := events[0].(*BlockStoredEvent)
		assert.True(t, ok)
		assert.Equal(t, testEvent.Type, receivedEvent.Type)
		assert.Equal(t, testEvent.BlockHashes, receivedEvent.BlockHashes)
		assert.Equal(t, "test-pod", receivedEvent.PodName)
	}
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
			Type:            EventTypeBlockStored,
			Timestamp:       time.Now(),
			BlockHashes:     []int64{1234, 5678},
			TokenIDs:        [][]int32{{1, 2, 3}, {4, 5, 6}},
			ParentBlockHash: &parentHash,
			ModelName:       "test-model",
		},
		&BlockRemovedEvent{
			Type:        EventTypeBlockRemoved,
			Timestamp:   time.Now(),
			BlockHashes: []int64{1234},
			ModelName:   "test-model",
		},
		&AllBlocksClearedEvent{
			Type:      EventTypeAllCleared,
			Timestamp: time.Now(),
			ModelName: "test-model",
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
			assert.Equal(t, "test-pod", e.PodName)
		case *BlockRemovedEvent:
			assert.Equal(t, "test-pod", e.PodName)
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
	event1 := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		Timestamp:   time.Now(),
		BlockHashes: []int64{1000},
		TokenIDs:    [][]int32{{10}},
		ModelName:   "test-model",
	}
	err = publisher.PublishEvent(event1)
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
	event2 := &BlockRemovedEvent{
		Type:        EventTypeBlockRemoved,
		Timestamp:   time.Now(),
		BlockHashes: []int64{1000},
		ModelName:   "test-model",
	}
	err = publisher.PublishEvent(event2)
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

		event := &BlockStoredEvent{
			Type:        EventTypeBlockStored,
			Timestamp:   time.Now(),
			BlockHashes: []int64{int64(i * 100)},
			TokenIDs:    [][]int32{{int32(i)}},
			ModelName:   "test-model",
		}
		err = publisher.PublishEvent(event)
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

// Mock publisher helper implementation
type mockPublisher struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pubSock  *zmq.Socket
	repSock  *zmq.Socket
	sequence int64
	mu       sync.Mutex
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
		ctx:     ctx,
		cancel:  cancel,
		pubSock: pubSock,
		repSock: repSock,
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
	return err
}

func (mp *mockPublisher) Close() {
	mp.cancel()
	_ = mp.pubSock.Close()
	_ = mp.repSock.Close()
}

func (mp *mockPublisher) handleReplay() {
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

			if len(msg) >= 2 {
				// Send acknowledgment
				_, _ = mp.repSock.SendMessage(msg[0], "OK")
			}
		}
	}
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
		event := &BlockStoredEvent{
			Type:        EventTypeBlockStored,
			Timestamp:   time.Now(),
			BlockHashes: []int64{int64(i)},
			TokenIDs:    [][]int32{{int32(i)}},
			ModelName:   "bench-model",
		}

		err := publisher.PublishEvent(event)
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

	// Publish test event
	blockStored := &BlockStoredEvent{
		Type:        EventTypeBlockStored,
		Timestamp:   time.Now(),
		BlockHashes: []int64{1234, 5678},
		TokenIDs:    [][]int32{{1, 2, 3}, {4, 5, 6}},
		ModelName:   "test-model",
	}

	err = publisher.PublishEvent(blockStored)
	require.NoError(t, err)

	// Wait for processing
	time.Sleep(300 * time.Millisecond)

	// Verify handler was called
	handler.AssertExpectations(t)
}
