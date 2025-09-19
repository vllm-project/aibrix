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
	"fmt"
	"net"
	"time"
)

// EventHandler processes received KV events
type EventHandler interface {
	HandleEvent(event KVEvent) error
}

// ZMQClientConfig contains configuration for the ZMQ client
type ZMQClientConfig struct {
	PodKey         string
	PodIP          string
	ModelName      string
	PubPort        int
	RouterPort     int
	PollTimeout    time.Duration
	ReplayTimeout  time.Duration
	ReconnectDelay time.Duration
}

// Constants for ZMQ client configuration
const (
	// Default ZMQ ports
	DefaultPubPort    = 5557
	DefaultRouterPort = 5558

	// Timeouts and intervals
	DefaultPollTimeout       = 100 * time.Millisecond
	DefaultReplayTimeout     = 5 * time.Second
	DefaultReconnectInterval = 1 * time.Second
	MaxReconnectInterval     = 30 * time.Second
	ReconnectBackoffFactor   = 2.0

	// Buffer sizes
	EventChannelBufferSize = 1000
)

// DefaultZMQClientConfig returns a default configuration
func DefaultZMQClientConfig(podKey, podIP, modelName string) *ZMQClientConfig {
	return &ZMQClientConfig{
		PodKey:         podKey,
		PodIP:          podIP,
		ModelName:      modelName,
		PubPort:        DefaultPubPort,
		RouterPort:     DefaultRouterPort,
		PollTimeout:    DefaultPollTimeout,
		ReplayTimeout:  DefaultReplayTimeout,
		ReconnectDelay: DefaultReconnectInterval,
	}
}

// ValidateConfig validates the ZMQ client configuration
func ValidateConfig(config *ZMQClientConfig) error {
	if config.PodIP == "" {
		return fmt.Errorf("pod IP is required")
	}

	// Validate IP address format
	if ip := net.ParseIP(config.PodIP); ip == nil {
		return fmt.Errorf("invalid IP address: %s", config.PodIP)
	}

	// Validate port ranges
	if config.PubPort <= 0 || config.PubPort > 65535 {
		return fmt.Errorf("invalid publisher port: %d", config.PubPort)
	}

	if config.RouterPort <= 0 || config.RouterPort > 65535 {
		return fmt.Errorf("invalid router port: %d", config.RouterPort)
	}

	return nil
}
