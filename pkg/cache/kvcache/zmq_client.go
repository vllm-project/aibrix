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
	"fmt"
	"sync"
	"syscall"
	"time"

	zmq "github.com/pebbe/zmq4"
	"k8s.io/klog/v2"
)

// replayEndSeq is the sequence number of the message that ends a replay reply
// (ZmqEventPublisher.END_SEQ in vLLM's vllm/distributed/kv_events.py).
const replayEndSeq int64 = -1

// maxPendingBatches bounds the live batches held while a sequence gap is being
// repaired. When it is exceeded, the gap is treated as unrecoverable: the
// cached view is cleared and the stream continues from the newest batch.
var maxPendingBatches = 1024

// ZMQClient manages ZMQ connections to vLLM KV event publishers
type ZMQClient struct {
	config *ZMQClientConfig

	// ZMQ sockets
	subSocket    *zmq.Socket
	replaySocket *zmq.Socket

	// Event handler
	eventHandler EventHandler

	// State management
	mu              sync.RWMutex
	connected       bool
	lastSeq         int64
	reconnectDelay  time.Duration
	reconnectTicker *time.Ticker

	// applyMu serializes event batches so live and replayed batches are applied
	// in one order. It is held across decode + handler + sequence update.
	applyMu sync.Mutex
	// lastLiveSeq tracks observed live traffic independently of applied replay
	// progress, so a held gap or its replay is not counted more than once.
	// Guarded by applyMu.
	lastLiveSeq int64

	// replayInFlight prevents concurrent replay requests on the single replay
	// socket. Guarded by mu.
	replayInFlight bool

	// replayWg tracks background replay goroutines. stopped is set by Stop so
	// no new background replay starts once shutdown began. Both are guarded by
	// mu; Stop waits replayWg before closing the sockets.
	replayWg sync.WaitGroup
	stopped  bool

	// pending holds live batches that arrived ahead of lastSeq+1 until the
	// missing sequences are replayed or the gap is declared unrecoverable.
	// Guarded by applyMu.
	pending map[int64][]byte

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Metrics (to be implemented)
	metrics *ZMQClientMetrics
}

// NewZMQClient creates a new ZMQ client for a vLLM pod
func NewZMQClient(config *ZMQClientConfig, handler EventHandler) *ZMQClient {
	ctx, cancel := context.WithCancel(context.Background())

	return &ZMQClient{
		config:         config,
		eventHandler:   handler,
		lastSeq:        -1,
		lastLiveSeq:    -1,
		reconnectDelay: config.ReconnectDelay,
		ctx:            ctx,
		cancel:         cancel,
		metrics:        NewZMQClientMetrics(config.PodKey),
	}
}

// Connect establishes ZMQ connections
func (c *ZMQClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	// Clean up any existing sockets
	c.cleanupSocketsLocked()

	// Create SUB socket
	subSocket, err := zmq.NewSocket(zmq.SUB)
	if err != nil {
		return fmt.Errorf("failed to create SUB socket: %w", err)
	}

	// Enable IPv6 for dual-stack support
	if err := subSocket.SetIpv6(true); err != nil {
		_ = subSocket.Close()
		return fmt.Errorf("failed to enable IPv6 on SUB socket: %w", err)
	}

	subEndpoint := formatZMQTCPEndpoint(c.config.PodIP, c.config.PubPort)
	if err := subSocket.Connect(subEndpoint); err != nil {
		_ = subSocket.Close()
		return fmt.Errorf("failed to connect to %s: %w", subEndpoint, err)
	}

	// Subscribe to all messages
	if err := subSocket.SetSubscribe(""); err != nil {
		_ = subSocket.Close()
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Create DEALER socket for replay (to communicate with ROUTER)
	replaySocket, err := c.newReplaySocket()
	if err != nil {
		_ = subSocket.Close()
		return err
	}

	c.subSocket = subSocket
	c.replaySocket = replaySocket
	c.connected = true

	// Reset reconnect delay on successful connection
	c.reconnectDelay = c.config.ReconnectDelay

	klog.Infof("Successfully connected to vLLM pod %s at %s", c.config.PodKey, c.config.PodIP)
	c.metrics.IncrementConnectionCount()

	return nil
}

// newReplaySocket creates the DEALER socket for replay requests and connects
// it to the publisher's ROUTER.
func (c *ZMQClient) newReplaySocket() (*zmq.Socket, error) {
	replaySocket, err := zmq.NewSocket(zmq.DEALER)
	if err != nil {
		return nil, fmt.Errorf("failed to create DEALER socket: %w", err)
	}

	// Enable IPv6 for dual-stack support
	if err := replaySocket.SetIpv6(true); err != nil {
		_ = replaySocket.Close()
		return nil, fmt.Errorf("failed to enable IPv6 on DEALER socket: %w", err)
	}

	replayEndpoint := formatZMQTCPEndpoint(c.config.PodIP, c.config.RouterPort)
	if err := replaySocket.Connect(replayEndpoint); err != nil {
		_ = replaySocket.Close()
		return nil, fmt.Errorf("failed to connect to replay endpoint: %w", err)
	}

	return replaySocket, nil
}

// Start begins event consumption
func (c *ZMQClient) Start() error {
	if err := c.Connect(); err != nil {
		return fmt.Errorf("initial connection failed: %w", err)
	}

	// Request full replay on startup
	if _, err := c.requestReplay(0); err != nil {
		klog.Warningf("Failed to request initial replay for %s: %v", c.config.PodKey, err)
		// Retry in the background: without the replay the index misses every
		// batch published before the subscription, and the consumer only
		// retries on reconnect.
		c.retryReplayAsync(0, 3)
	}

	// Start event consumption goroutine
	c.wg.Add(1)
	go c.consumeEventsWithReconnect()

	klog.Infof("ZMQ client started for pod %s", c.config.PodKey)
	return nil
}

// Stop gracefully shuts down the client
func (c *ZMQClient) Stop() {
	klog.Infof("Stopping ZMQ client for pod %s", c.config.PodKey)

	// Cancel context to signal shutdown
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()

	// Wait for goroutines to finish before closing the sockets they use.
	c.wg.Wait()
	c.replayWg.Wait()

	// Clean up sockets
	c.mu.Lock()
	c.cleanupSocketsLocked()
	c.mu.Unlock()

	// Clean up metrics
	c.metrics.Delete()

	klog.Infof("ZMQ client stopped for pod %s", c.config.PodKey)
}

// IsConnected returns the current connection status
func (c *ZMQClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// GetLastSequence returns the last processed sequence number
func (c *ZMQClient) GetLastSequence() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSeq
}

// consumeEventsWithReconnect is the main event loop with reconnection logic
func (c *ZMQClient) consumeEventsWithReconnect() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			// Check connection status
			if !c.IsConnected() {
				c.handleReconnect()
				continue
			}

			// Try to consume events
			if err := c.consumeEvents(); err != nil {
				klog.Errorf("Event consumption error for %s: %v", c.config.PodKey, err)
				c.markDisconnected()
				c.metrics.IncrementErrorCount("consume_events")
			}
		}
	}
}

// handleReconnect attempts to reconnect with exponential backoff
func (c *ZMQClient) handleReconnect() {
	klog.Infof("Attempting to reconnect to %s after %v", c.config.PodKey, c.reconnectDelay)

	// Wait for reconnect delay
	select {
	case <-time.After(c.reconnectDelay):
	case <-c.ctx.Done():
		return
	}

	// Attempt to reconnect
	if err := c.Connect(); err != nil {
		klog.Errorf("Reconnection failed for %s: %v", c.config.PodKey, err)
		c.metrics.IncrementErrorCount("reconnect")

		// Exponential backoff
		c.mu.Lock()
		c.reconnectDelay = time.Duration(float64(c.reconnectDelay) * ReconnectBackoffFactor)
		if c.reconnectDelay > MaxReconnectInterval {
			c.reconnectDelay = MaxReconnectInterval
		}
		c.mu.Unlock()
		return
	}

	// Replay from the next missing sequence. This also retries a first replay
	// that failed at startup, when lastSeq is still -1.
	from := c.GetLastSequence() + 1
	if from < 0 {
		from = 0
	}
	c.requestReplayAsync(from)
}

// consumeEvents is the core event processing loop
func (c *ZMQClient) consumeEvents() error {
	c.mu.RLock()
	socket := c.subSocket
	c.mu.RUnlock()

	if socket == nil {
		return fmt.Errorf("socket is nil")
	}

	poller := zmq.NewPoller()
	poller.Add(socket, zmq.POLLIN)

	for {
		select {
		case <-c.ctx.Done():
			return nil
		default:
			// Poll with timeout
			polled, err := poller.Poll(c.config.PollTimeout)
			if err != nil {
				return fmt.Errorf("poll error: %w", err)
			}

			if len(polled) == 0 {
				// No data available, continue
				continue
			}

			// Process message
			if err := c.processMessage(); err != nil {
				return fmt.Errorf("failed to process message: %w", err)
			}
		}
	}
}

// processMessage reads and processes a single message
func (c *ZMQClient) processMessage() error {
	c.mu.RLock()
	socket := c.subSocket
	c.mu.RUnlock()

	if socket == nil {
		return fmt.Errorf("socket is nil")
	}

	// Receive multipart message: [topic, sequence, payload]
	topic, err := socket.RecvBytes(0)
	if err != nil {
		return fmt.Errorf("failed to receive topic: %w", err)
	}

	seqBytes, err := socket.RecvBytes(0)
	if err != nil {
		return fmt.Errorf("failed to receive sequence: %w", err)
	}

	payload, err := socket.RecvBytes(0)
	if err != nil {
		return fmt.Errorf("failed to receive payload: %w", err)
	}

	// Parse sequence number
	if len(seqBytes) != 8 {
		return fmt.Errorf("invalid sequence bytes length: %d", len(seqBytes))
	}
	seq := int64(binary.BigEndian.Uint64(seqBytes))

	numEvents, err := c.applyEventBatch(seq, payload, false)
	if err != nil {
		return err
	}

	klog.V(5).Infof("Processed event batch %d from %s (topic=%s, %d events)",
		seq, c.config.PodKey, string(topic), numEvents)

	return nil
}

// applyEventBatch decodes an event batch, hands its events to the event
// handler and records seq as the last processed sequence. A batch that fails
// to decode is not applied and leaves lastSeq unchanged.
//
// The apply is guarded so the event view stays consistent with the publisher:
// a replayed batch at or below lastSeq is a duplicate and is dropped, a live
// batch at sequence 0 after prior traffic conservatively resets the view,
// and a live batch beyond lastSeq+1 asks for the missing batches, including
// at cold start. Replay can begin at the publisher buffer's oldest batch when
// no batch has been applied yet. Sequence numbers advance within each stream.
func (c *ZMQClient) applyEventBatch(seq int64, payload []byte, replay bool) (int, error) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	lastSeq := c.GetLastSequence()
	if !replay && seq == 0 && (lastSeq >= 0 || c.lastLiveSeq >= 0) {
		// vLLM uses count() starting at zero. Without a publisher epoch,
		// a duplicated live zero cannot be distinguished from a restart;
		// clearing is safer than retaining the previous engine's view.
		// Prior traffic includes cold-start batches held pending replay.
		c.resetEventStreamLocked()
		lastSeq = -1
	} else if lastSeq >= 0 && seq <= lastSeq {
		klog.V(5).Infof("Skipping duplicate or stale event batch %d from %s (last=%d)",
			seq, c.config.PodKey, lastSeq)
		return 0, nil
	}
	if !replay {
		observed := max(lastSeq, c.lastLiveSeq)
		if seq > observed+1 {
			missedCount := seq - observed - 1
			klog.Warningf("Missed %d events for %s (last live/applied=%d, current=%d)",
				missedCount, c.config.PodKey, observed, seq)
			c.metrics.IncrementMissedEvents(missedCount)
		}
		c.lastLiveSeq = max(c.lastLiveSeq, seq)
	}
	if seq > lastSeq+1 && (!replay || lastSeq >= 0) {
		if !c.bufferPendingLocked(seq, payload) {
			// The gap outgrew the buffer. Clear the untrusted view and
			// continue from this batch instead of dropping every later one.
			klog.Warningf("Sequence gap for %s outgrew the buffer; resetting the cached view",
				c.config.PodKey)
			c.clearCachedViewLocked()
			c.setLastSequence(seq - 1)
			c.pending = nil
		} else {
			c.requestReplayAsync(lastSeq + 1)
			return 0, nil
		}
	}

	numEvents, err := c.handleEventBatch(payload)
	if err != nil {
		return 0, err
	}
	c.setLastSequence(seq)
	numEvents += c.drainPendingLocked()
	return numEvents, nil
}

// bufferPendingLocked holds an out-of-order batch until the missing sequences
// arrive. It reports false when the buffer is full. Callers hold applyMu.
func (c *ZMQClient) bufferPendingLocked(seq int64, payload []byte) bool {
	if c.pending == nil {
		c.pending = make(map[int64][]byte)
	}
	if len(c.pending) >= maxPendingBatches {
		return false
	}
	c.pending[seq] = payload
	return true
}

// drainPendingLocked applies buffered batches that became contiguous once
// lastSeq advanced, and drops entries the replay already applied. Callers hold
// applyMu.
func (c *ZMQClient) drainPendingLocked() int {
	if len(c.pending) == 0 {
		return 0
	}
	applied := 0
	for {
		lastSeq := c.GetLastSequence()
		for seq := range c.pending {
			if seq <= lastSeq {
				delete(c.pending, seq)
			}
		}
		payload, ok := c.pending[lastSeq+1]
		if !ok {
			return applied
		}
		numEvents, err := c.handleEventBatch(payload)
		if err != nil {
			klog.Errorf("Failed to apply buffered event batch %d from %s: %v",
				lastSeq+1, c.config.PodKey, err)
			return applied
		}
		delete(c.pending, lastSeq+1)
		c.setLastSequence(lastSeq + 1)
		applied += numEvents
	}
}

// recoverStalledGap runs after a replay: if a gap is still open, the publisher
// could not supply the missing batches. Clear the cached view, because it can
// no longer be trusted, and continue from the oldest buffered batch.
func (c *ZMQClient) recoverStalledGap() {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	if len(c.pending) == 0 {
		return
	}
	minSeq := int64(-1)
	for seq := range c.pending {
		if minSeq < 0 || seq < minSeq {
			minSeq = seq
		}
	}
	if minSeq <= c.GetLastSequence()+1 {
		return
	}
	klog.Warningf("Sequence gap for %s could not be replayed; resetting the cached view",
		c.config.PodKey)
	c.clearCachedViewLocked()
	c.setLastSequence(minSeq - 1)
	c.drainPendingLocked()
}

// clearCachedViewLocked tells the handler to drop every prefix cached for this
// pod. Callers hold applyMu.
func (c *ZMQClient) clearCachedViewLocked() {
	event := &AllBlocksClearedEvent{
		Type:      EventTypeAllCleared,
		Timestamp: time.Now(),
		ModelName: c.config.ModelName,
		PodName:   c.config.PodKey,
	}
	if err := c.eventHandler.HandleEvent(event); err != nil {
		klog.Errorf("Failed to clear the cached view of %s: %v", c.config.PodKey, err)
	}
}

// resetEventStreamLocked starts a new sequence space after a publisher restart.
// Callers hold applyMu.
func (c *ZMQClient) resetEventStreamLocked() {
	klog.Warningf("KV event publisher %s restarted; resetting sequence and cached view",
		c.config.PodKey)
	c.pending = nil
	c.lastLiveSeq = -1
	c.clearCachedViewLocked()
	c.setLastSequence(-1)
}

// handleEventBatch decodes one batch and hands its events to the handler
// without touching the sequence state.
func (c *ZMQClient) handleEventBatch(payload []byte) (int, error) {
	batch, err := DecodeEventBatch(payload, c.config.ModelName, c.config.PodKey)
	if err != nil {
		c.metrics.IncrementErrorCount("decode")
		return 0, fmt.Errorf("failed to decode event batch: %w", err)
	}

	for _, event := range batch.Events {
		startTime := time.Now()
		if err := c.eventHandler.HandleEvent(event); err != nil {
			klog.Errorf("Failed to handle event from %s: %v", c.config.PodKey, err)
			c.metrics.IncrementErrorCount("handle_event")
		} else {
			c.metrics.RecordEventProcessingLatency(time.Since(startTime))
			c.metrics.IncrementEventCount(string(event.GetType()))
		}
	}
	return len(batch.Events), nil
}

func (c *ZMQClient) setLastSequence(seq int64) {
	c.mu.Lock()
	c.lastSeq = seq
	c.mu.Unlock()
}

// requestReplayAsync asks the publisher for the batches missing after fromSeq
// without blocking the event path.
func (c *ZMQClient) requestReplayAsync(fromSeq int64) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.replayWg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.replayWg.Done()
		_, ran, err := c.runReplay(fromSeq)
		if err != nil {
			klog.Warningf("Replay from seq %d failed for %s: %v",
				fromSeq, c.config.PodKey, err)
		}
		if ran {
			// The replay may have applied the missing batches or may have
			// found nothing; only the guard knows whether a gap is still open.
			c.recoverStalledGap()
		}
	}()
}

// retryReplayAsync runs retryReplay in a tracked goroutine so Stop cannot close
// the replay socket under it.
func (c *ZMQClient) retryReplayAsync(fromSeq int64, attempts int) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.replayWg.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.replayWg.Done()
		c.retryReplay(fromSeq, attempts)
	}()
}

// runReplay serializes replay requests on the single replay socket. ran reports
// whether this call owned the socket; a call that found another replay in
// flight returns ran=false and must not interpret applied=0.
func (c *ZMQClient) runReplay(fromSeq int64) (applied int, ran bool, err error) {
	c.mu.Lock()
	if c.replayInFlight || c.replaySocket == nil {
		c.mu.Unlock()
		return 0, false, nil
	}
	c.replayInFlight = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.replayInFlight = false
		c.mu.Unlock()
	}()

	applied, err = c.requestReplay(fromSeq)
	return applied, true, err
}

// retryReplay retries a failed replay with the reconnect delay between
// attempts, so a first replay that failed at startup is not lost.
func (c *ZMQClient) retryReplay(fromSeq int64, attempts int) {
	for i := 0; i < attempts; i++ {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(c.config.ReconnectDelay):
		}
		_, ran, err := c.runReplay(fromSeq)
		if err == nil && ran {
			c.recoverStalledGap()
			return
		}
		if err != nil {
			klog.Warningf("Replay retry %d/%d from seq %d failed for %s: %v",
				i+1, attempts, fromSeq, c.config.PodKey, err)
			continue
		}
		// Another replay owned the socket; this attempt did not run, so keep
		// retrying instead of reporting a success that never happened.
		klog.V(4).Infof("Replay retry %d/%d from seq %d skipped for %s: replay in flight",
			i+1, attempts, fromSeq, c.config.PodKey)
	}
}

// requestReplay asks the publisher to resend its buffered batches from fromSeq
// on and applies them. It returns the number of batches applied. vLLM's
// ZmqEventPublisher._service_replay answers with one message per buffered batch
// with sequence >= fromSeq, then an end marker with sequence END_SEQ (-1).
func (c *ZMQClient) requestReplay(fromSeq int64) (int, error) {
	c.mu.RLock()
	socket := c.replaySocket
	c.mu.RUnlock()

	if socket == nil {
		return 0, fmt.Errorf("replay socket is nil")
	}

	// Prepare replay request
	// DEALER-ROUTER pattern requires: [empty_delimiter, payload]
	// The DEALER socket will automatically prepend the identity frame
	reqData := make([]byte, 8)
	binary.BigEndian.PutUint64(reqData, uint64(fromSeq))

	// Send replay request as multipart message: [empty_delimiter, start_seq_bytes]
	if _, err := socket.SendMessage([]byte{}, reqData); err != nil {
		c.metrics.IncrementReplayFailure()
		return 0, fmt.Errorf("failed to send replay request: %w", err)
	}
	c.metrics.IncrementReplayCount()

	applied, err := c.receiveReplay(socket)
	if err != nil {
		c.metrics.IncrementReplayFailure()
		// The rest of this reply can still arrive. Replace the socket so that
		// it is not read as the reply to the next request.
		c.resetReplaySocket(socket)
		return applied, fmt.Errorf("replay from seq %d stopped after %d batches: %w", fromSeq, applied, err)
	}

	klog.Infof("Replayed %d event batches from seq %d for %s", applied, fromSeq, c.config.PodKey)
	c.metrics.IncrementReplaySuccess()
	return applied, nil
}

// receiveReplay reads replay messages until the end marker and applies each
// batch newer than lastSeq, in order. The whole reply must arrive within
// ReplayTimeout. It returns the number of batches applied.
func (c *ZMQClient) receiveReplay(socket *zmq.Socket) (int, error) {
	deadline := time.Now().Add(c.config.ReplayTimeout)
	applied := 0

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return applied, fmt.Errorf("timed out after %v waiting for the end of the replay",
				c.config.ReplayTimeout)
		}
		_ = socket.SetRcvtimeo(remaining)

		frames, err := socket.RecvMessageBytes(0)
		if err != nil {
			if zmq.AsErrno(err) == zmq.Errno(syscall.EAGAIN) {
				// Receive timeout; the deadline check above reports it
				continue
			}
			return applied, fmt.Errorf("failed to receive replay message: %w", err)
		}

		// vLLM v0.26 and later send [empty_delimiter, topic, sequence, payload];
		// earlier versions send [empty_delimiter, sequence, payload]
		if len(frames) != 3 && len(frames) != 4 {
			return applied, fmt.Errorf("invalid replay message: %d frames", len(frames))
		}
		seqBytes, payload := frames[len(frames)-2], frames[len(frames)-1]
		if len(seqBytes) != 8 {
			return applied, fmt.Errorf("invalid sequence bytes length: %d", len(seqBytes))
		}
		seq := int64(binary.BigEndian.Uint64(seqBytes))
		if seq == replayEndSeq {
			return applied, nil
		}

		if lastSeq := c.GetLastSequence(); seq <= lastSeq {
			klog.V(5).Infof("Skipping replayed event batch %d from %s (last=%d)",
				seq, c.config.PodKey, lastSeq)
			continue
		}

		if _, err := c.applyEventBatch(seq, payload, true); err != nil {
			return applied, err
		}
		applied++
	}
}

// resetReplaySocket closes the replay socket after a failed replay and opens
// a new one. Frames of the abandoned reply that arrive later are dropped with
// the old socket.
func (c *ZMQClient) resetReplaySocket(old *zmq.Socket) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.replaySocket != old {
		// Already closed or replaced by Connect or Stop
		return
	}

	// Linger 0 also drops the request if the publisher has not taken it yet
	_ = old.SetLinger(0)
	_ = old.Close()
	c.replaySocket = nil

	replaySocket, err := c.newReplaySocket()
	if err != nil {
		klog.Errorf("Failed to recreate replay socket for %s: %v", c.config.PodKey, err)
		return
	}
	c.replaySocket = replaySocket
}

// markDisconnected marks the client as disconnected
func (c *ZMQClient) markDisconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.metrics.IncrementDisconnectionCount()
}

// cleanupSocketsLocked closes and cleans up sockets (must be called with lock held)
func (c *ZMQClient) cleanupSocketsLocked() {
	if c.subSocket != nil {
		_ = c.subSocket.Close()
		c.subSocket = nil
	}
	if c.replaySocket != nil {
		_ = c.replaySocket.Close()
		c.replaySocket = nil
	}
	c.connected = false
}
