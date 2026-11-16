/*
Copyright 2026 The Aibrix Team.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreStopArguments(t *testing.T) {
	for _, args := range [][]string{
		{"extra"}, {"--unknown"}, {"--timeout=oops"}, {"--poll-interval=0s"},
		{"--propagation-delay=-1s"}, {"--timeout=10s"}, {"--timeout=25h"},
		{"--metrics-url=file:///metrics"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			var output bytes.Buffer
			require.Equal(t, 2, executePreStop(args, &output))
			require.NotEmpty(t, output.String())
		})
	}
	var output bytes.Buffer
	require.Zero(t, executePreStop([]string{"--help"}, &output))
	require.Contains(t, output.String(), "Usage: gateway-plugins prestop")
	require.Contains(t, output.String(), "default 5m0s")
}

func TestPreStopRunsWithoutGatewayStartup(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			value := 0
			if busy {
				value = 1
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, "gateway_in_flight_requests %d\n", value)
			}))
			defer server.Close()
			var output bytes.Buffer
			timeout := 3 * time.Second
			if busy {
				timeout = 50 * time.Millisecond
			}
			require.Zero(t, executePreStop([]string{
				"--metrics-url=" + server.URL, "--propagation-delay=0s", "--timeout=" + timeout.String(), "--poll-interval=1ms",
			}, &output))
			if busy {
				require.Contains(t, output.String(), "deadline reached")
			} else {
				require.Contains(t, output.String(), "drain complete")
			}
		})
	}
}
