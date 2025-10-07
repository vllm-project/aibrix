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
	"strconv"
)

// formatZMQTCPEndpoint creates a properly formatted ZMQ TCP endpoint
// that correctly handles both IPv4 and IPv6 addresses.
//
// Examples:
//   - IPv4: "10.0.0.1" + 5557 -> "tcp://10.0.0.1:5557"
//   - IPv6: "2001:db8::1" + 5557 -> "tcp://[2001:db8::1]:5557"
//   - IPv6: "::1" + 5557 -> "tcp://[::1]:5557"
func formatZMQTCPEndpoint(host string, port int) string {
	// net.JoinHostPort correctly handles IPv6 addresses by adding brackets
	hostPort := net.JoinHostPort(host, strconv.Itoa(port))
	return fmt.Sprintf("tcp://%s", hostPort)
}

// formatZMQBindEndpoint creates a ZMQ bind endpoint (for servers)
// Special handling for wildcard addresses.
//
// Examples:
//   - "*" + 5557 -> "tcp://*:5557"
//   - "::" + 5557 -> "tcp://[::]:5557"
//   - "0.0.0.0" + 5557 -> "tcp://0.0.0.0:5557"
func formatZMQBindEndpoint(host string, port int) string {
	if host == "*" {
		// ZMQ wildcard syntax doesn't need brackets
		return fmt.Sprintf("tcp://*:%d", port)
	}
	return formatZMQTCPEndpoint(host, port)
}
