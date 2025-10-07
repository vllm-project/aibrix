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
	"time"

	zmq "github.com/pebbe/zmq4"
	"k8s.io/klog/v2"
)

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
	replaySocket, err := zmq.NewSocket(zmq.DEALER)
	if err != nil {
		_ = subSocket.Close()
		return fmt.Errorf("failed to create DEALER socket: %w", err)
	}

	// Enable IPv6 for dual-stack support
	if err := replaySocket.SetIpv6(true); err != nil {
		_ = subSocket.Close()
		_ = replaySocket.Close()
		return fmt.Errorf("failed to enable IPv6 on DEALER socket: %w", err)
	}

	replayEndpoint := formatZMQTCPEndpoint(c.config.PodIP, c.config.RouterPort)
	if err := replaySocket.Connect(replayEndpoint); err != nil {
		_ = subSocket.Close()
		_ = replaySocket.Close()
		return fmt.Errorf("failed to connect to replay endpoint: %w", err)
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

// Start begins event consumption
func (c *ZMQClient) Start() error {
	if err := c.Connect(); err != nil {
		return fmt.Errorf("initial connection failed: %w", err)
	}

	// Request full replay on startup
	if err := c.requestReplay(0); err != nil {
		klog.Warningf("Failed to request initial replay for %s: %v", c.config.PodKey, err)
		// Don't fail startup if replay fails
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
	c.cancel()

	// Wait for goroutines to finish
	c.wg.Wait()

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

	// Request replay from last known sequence
	lastSeq := c.GetLastSequence()
	if lastSeq >= 0 {
		if err := c.requestReplay(lastSeq + 1); err != nil {
			klog.Warningf("Failed to request replay after reconnect for %s: %v", c.config.PodKey, err)
		}
	}
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

	// Check for missed events
	c.mu.RLock()
	lastSeq := c.lastSeq
	c.mu.RUnlock()

	if lastSeq >= 0 && seq > lastSeq+1 {
		missedCount := seq - lastSeq - 1
		klog.Warningf("Missed %d events for %s (last=%d, current=%d)",
			missedCount, c.config.PodKey, lastSeq, seq)
		c.metrics.IncrementMissedEvents(missedCount)
	}

	// Decode and process events
	batch, err := DecodeEventBatch(payload)
	if err != nil {
		c.metrics.IncrementErrorCount("decode")
		return fmt.Errorf("failed to decode event batch: %w", err)
	}

	// Process each event
	for _, event := range batch.Events {
		// Add pod information
		switch e := event.(type) {
		case *BlockStoredEvent:
			e.PodName = c.config.PodKey
			if e.ModelName == "" {
				e.ModelName = c.config.ModelName
			}
		case *BlockRemovedEvent:
			e.PodName = c.config.PodKey
			if e.ModelName == "" {
				e.ModelName = c.config.ModelName
			}
		case *AllBlocksClearedEvent:
			e.PodName = c.config.PodKey
			if e.ModelName == "" {
				e.ModelName = c.config.ModelName
			}
		}

		// Handle the event
		startTime := time.Now()
		if err := c.eventHandler.HandleEvent(event); err != nil {
			klog.Errorf("Failed to handle event from %s: %v", c.config.PodKey, err)
			c.metrics.IncrementErrorCount("handle_event")
		} else {
			c.metrics.RecordEventProcessingLatency(time.Since(startTime))
			c.metrics.IncrementEventCount(string(event.GetType()))
		}
	}

	// Update sequence
	c.mu.Lock()
	c.lastSeq = seq
	c.mu.Unlock()

	klog.V(5).Infof("Processed event batch %d from %s (topic=%s, %d events)",
		seq, c.config.PodKey, string(topic), len(batch.Events))

	return nil
}

// requestReplay requests event replay from a specific sequence
func (c *ZMQClient) requestReplay(fromSeq int64) error {
	c.mu.RLock()
	socket := c.replaySocket
	c.mu.RUnlock()

	if socket == nil {
		return fmt.Errorf("replay socket is nil")
	}

	// Prepare replay request
	reqData := make([]byte, 8)
	binary.BigEndian.PutUint64(reqData, uint64(fromSeq))

	// Send replay request
	if _, err := socket.SendBytes(reqData, 0); err != nil {
		return fmt.Errorf("failed to send replay request: %w", err)
	}

	// Set receive timeout
	_ = socket.SetRcvtimeo(c.config.ReplayTimeout)

	// Receive response
	resp, err := socket.RecvBytes(0)
	if err != nil {
		return fmt.Errorf("failed to receive replay response: %w", err)
	}

	klog.Infof("Successfully requested replay from seq %d for %s (response: %d bytes)",
		fromSeq, c.config.PodKey, len(resp))

	c.metrics.IncrementReplayCount()
	return nil
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
