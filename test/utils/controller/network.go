// Copyright 2026 The Aibrix Team.
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

package controller

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// FindBindableNonLoopbackIPv4 returns a non-loopback IPv4 address that can bind port.
func FindBindableNonLoopbackIPv4(port int) string {
	ginkgo.GinkgoHelper()

	interfaces, err := net.Interfaces()
	if err != nil {
		ginkgo.Fail(fmt.Sprintf(
			"list network interfaces while looking for a non-loopback IPv4 address that can bind port %d: %v",
			port, err,
		))
		return ""
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch addr := addr.(type) {
			case *net.IPNet:
				ip = addr.IP
			case *net.IPAddr:
				ip = addr.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}

			listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
			if err != nil {
				continue
			}
			_ = listener.Close()
			return ip.String()
		}
	}

	ginkgo.Fail(fmt.Sprintf(
		"no non-loopback IPv4 address can bind port %d; "+
			"check local network interfaces and whether the port is already in use",
		port,
	))
	return ""
}

// StartFixedPortHTTPServer starts handler on ip:port, retrying temporary port conflicts.
func StartFixedPortHTTPServer(
	ip string,
	port int,
	handler http.Handler,
	timeout time.Duration,
	interval time.Duration,
) *httptest.Server {
	ginkgo.GinkgoHelper()
	gomega.Expect(ip).NotTo(gomega.BeEmpty(), "need a non-loopback local IPv4 for fixed-port HTTP server")

	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()

	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)
	for {
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			server.Listener = listener
			server.Start()
			return server
		}
		if time.Now().After(deadline) {
			ginkgo.Fail(fmt.Sprintf("listen on fixed-port HTTP server address %s within %s: %v", addr, timeout, err))
			return nil
		}
		time.Sleep(interval)
	}
}
