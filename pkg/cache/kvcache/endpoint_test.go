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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatZMQTCPEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		port     int
		expected string
	}{
		{
			name:     "IPv4 address",
			host:     "192.168.1.1",
			port:     5557,
			expected: "tcp://192.168.1.1:5557",
		},
		{
			name:     "IPv6 loopback",
			host:     "::1",
			port:     5557,
			expected: "tcp://[::1]:5557",
		},
		{
			name:     "IPv6 full address",
			host:     "2001:db8::1",
			port:     5558,
			expected: "tcp://[2001:db8::1]:5558",
		},
		{
			name:     "IPv6 with zone",
			host:     "fe80::1%eth0",
			port:     5557,
			expected: "tcp://[fe80::1%eth0]:5557",
		},
		{
			name:     "Hostname",
			host:     "localhost",
			port:     5557,
			expected: "tcp://localhost:5557",
		},
		{
			name:     "IPv4 localhost",
			host:     "127.0.0.1",
			port:     8080,
			expected: "tcp://127.0.0.1:8080",
		},
		{
			name:     "IPv6 any address",
			host:     "::",
			port:     5557,
			expected: "tcp://[::]:5557",
		},
		{
			name:     "IPv4 any address",
			host:     "0.0.0.0",
			port:     5557,
			expected: "tcp://0.0.0.0:5557",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatZMQTCPEndpoint(tt.host, tt.port)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatZMQBindEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		port     int
		expected string
	}{
		{
			name:     "ZMQ wildcard",
			host:     "*",
			port:     5557,
			expected: "tcp://*:5557",
		},
		{
			name:     "IPv6 any address",
			host:     "::",
			port:     5557,
			expected: "tcp://[::]:5557",
		},
		{
			name:     "IPv4 any address",
			host:     "0.0.0.0",
			port:     5557,
			expected: "tcp://0.0.0.0:5557",
		},
		{
			name:     "IPv4 specific address",
			host:     "192.168.1.1",
			port:     5558,
			expected: "tcp://192.168.1.1:5558",
		},
		{
			name:     "IPv6 specific address",
			host:     "2001:db8::1",
			port:     5558,
			expected: "tcp://[2001:db8::1]:5558",
		},
		{
			name:     "Hostname bind",
			host:     "localhost",
			port:     5559,
			expected: "tcp://localhost:5559",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatZMQBindEndpoint(tt.host, tt.port)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDualStackEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		description string
		host        string
		port        int
		expected    string
	}{
		{
			name:        "IPv6 wildcard for dual-stack",
			description: ":: should be used for dual-stack wildcard binding",
			host:        "::",
			port:        5557,
			expected:    "tcp://[::]:5557",
		},
		{
			name:        "IPv4-mapped IPv6 address",
			description: "IPv4-mapped IPv6 addresses should be properly formatted",
			host:        "::ffff:192.168.1.1",
			port:        5557,
			expected:    "tcp://[::ffff:192.168.1.1]:5557",
		},
		{
			name:        "Link-local IPv6",
			description: "Link-local IPv6 addresses should work",
			host:        "fe80::1",
			port:        5557,
			expected:    "tcp://[fe80::1]:5557",
		},
		{
			name:        "Dual-stack hostname",
			description: "Hostnames work in dual-stack environments",
			host:        "dual-stack-host.local",
			port:        5557,
			expected:    "tcp://dual-stack-host.local:5557",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatZMQTCPEndpoint(tt.host, tt.port)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *ZMQClientConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid IPv4 config",
			config: &ZMQClientConfig{
				PodIP:      "192.168.1.1",
				PubPort:    5557,
				RouterPort: 5558,
			},
			wantErr: false,
		},
		{
			name: "valid IPv6 config",
			config: &ZMQClientConfig{
				PodIP:      "2001:db8::1",
				PubPort:    5557,
				RouterPort: 5558,
			},
			wantErr: false,
		},
		{
			name: "empty pod IP",
			config: &ZMQClientConfig{
				PodIP:      "",
				PubPort:    5557,
				RouterPort: 5558,
			},
			wantErr: true,
			errMsg:  "pod IP is required",
		},
		{
			name: "invalid IP address",
			config: &ZMQClientConfig{
				PodIP:      "not-an-ip",
				PubPort:    5557,
				RouterPort: 5558,
			},
			wantErr: true,
			errMsg:  "invalid IP address",
		},
		{
			name: "invalid pub port - zero",
			config: &ZMQClientConfig{
				PodIP:      "192.168.1.1",
				PubPort:    0,
				RouterPort: 5558,
			},
			wantErr: true,
			errMsg:  "invalid publisher port",
		},
		{
			name: "invalid pub port - too high",
			config: &ZMQClientConfig{
				PodIP:      "192.168.1.1",
				PubPort:    65536,
				RouterPort: 5558,
			},
			wantErr: true,
			errMsg:  "invalid publisher port",
		},
		{
			name: "invalid router port - negative",
			config: &ZMQClientConfig{
				PodIP:      "192.168.1.1",
				PubPort:    5557,
				RouterPort: -1,
			},
			wantErr: true,
			errMsg:  "invalid router port",
		},
		{
			name: "IPv6 loopback",
			config: &ZMQClientConfig{
				PodIP:      "::1",
				PubPort:    5557,
				RouterPort: 5558,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConfig(tt.config)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
