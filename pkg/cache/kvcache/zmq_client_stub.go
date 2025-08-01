//go:build !zmq

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
	"time"
)

// ZMQClient stub implementation when ZMQ is not available
type ZMQClient struct {
	config *ZMQClientConfig
	ctx    context.Context
	cancel context.CancelFunc
}

// NewZMQClient creates a stub ZMQ client
func NewZMQClient(config *ZMQClientConfig, handler EventHandler) *ZMQClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &ZMQClient{
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Connect returns an error indicating ZMQ is not available
func (c *ZMQClient) Connect() error {
	return fmt.Errorf("ZMQ support not compiled in (build with -tags=zmq)")
}

// Subscribe returns an error
func (c *ZMQClient) Subscribe(handler EventHandler) error {
	return fmt.Errorf("ZMQ support not compiled in (build with -tags=zmq)")
}

// Start is an alias for Connect
func (c *ZMQClient) Start() error {
	return c.Connect()
}

// Stop closes the client
func (c *ZMQClient) Stop() {
	c.cancel()
}

// Close is a no-op
func (c *ZMQClient) Close() {
	c.Stop()
}

// ReplayFrom returns an error
func (c *ZMQClient) ReplayFrom(timestamp int64) error {
	return fmt.Errorf("ZMQ support not compiled in (build with -tags=zmq)")
}

// GetStats returns empty stats
func (c *ZMQClient) GetStats() (connected bool, msgsReceived uint64) {
	return false, 0
}

// SetLogLevel is a no-op
func (c *ZMQClient) SetLogLevel(level string) {}

// Reconnect returns an error
func (c *ZMQClient) Reconnect() error {
	return fmt.Errorf("ZMQ support not compiled in (build with -tags=zmq)")
}

// WaitForConnection returns an error
func (c *ZMQClient) WaitForConnection(timeout time.Duration) error {
	return fmt.Errorf("ZMQ support not compiled in (build with -tags=zmq)")
}
