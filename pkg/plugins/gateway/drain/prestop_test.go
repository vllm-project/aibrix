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

package drain

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

func drainTestContext(output *bytes.Buffer) context.Context {
	logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Output(output)))
	return klog.NewContext(context.Background(), logger)
}

func sample(value string) string {
	return "gateway_in_flight_requests{gateway_pod=\"gateway-test\"} " + value + "\n"
}

func TestInFlightSample(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want float64
	}{
		{"idle", sample("0"), 0},
		{"busy", sample("2"), 2},
		{"scientific zero", sample("0e+00"), 0},
		{"positive zero", sample("+0.0"), 0},
		{"scientific busy", sample("1.5e+01"), 15},
		{"unlabeled", "gateway_in_flight_requests 0\n", 0},
		{"empty labels", "gateway_in_flight_requests{} 0\n", 0},
		{"other labels", "gateway_in_flight_requests{zone=\"a\"} 0\n", 0},
		{"extra labels", "gateway_in_flight_requests{zone=\"a\",gateway_pod=\"gateway-test\"} 0\n", 0},
		{"other metrics", "other_metric 123\n" + sample("0"), 0},
		{"unrelated invalid metric", "other_metric invalid\n" + sample("0"), 0},
		{"gauge declaration", "# TYPE gateway_in_flight_requests gauge\n" + sample("0"), 0},
		{"no final newline", strings.TrimSuffix(sample("0"), "\n"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := inFlightSample([]byte(tc.body))
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
	for name, body := range map[string]string{
		"missing":       "# no sample\n",
		"sibling name":  "gateway_in_flight_requests_extra 0\n",
		"duplicate":     sample("0") + sample("0"),
		"mixed values":  sample("0") + sample("1"),
		"another pod":   sample("0") + "gateway_in_flight_requests{gateway_pod=\"other\"} 0\n",
		"bad labels":    "gateway_in_flight_requests{gateway_pod=bad} 0\n",
		"bad timestamp": sample("0 123"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := inFlightSample([]byte(body))
			require.Error(t, err)
		})
	}
	for _, value := range []string{"NaN", "+Inf", "Inf", "-1", "-0", "-0.0", "oops", "1e9999", ".0", "0."} {
		t.Run("invalid value "+value, func(t *testing.T) {
			_, err := inFlightSample([]byte(sample(value)))
			require.Error(t, err)
		})
	}
}

func TestWaitForQuietSamples(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		want      int
	}{
		{"three zeros", []string{sample("0")}, 3},
		{"busy resets", []string{sample("0"), sample("2"), sample("0")}, 5},
		{"missing resets", []string{sample("0"), "# no sample\n", sample("0")}, 5},
		{"invalid resets", []string{sample("0"), sample("NaN"), sample("0")}, 5},
		{"duplicate resets", []string{sample("0"), sample("0") + sample("0"), sample("0")}, 5},
		{"fetch failure resets", []string{sample("0"), "unavailable", sample("0")}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				index := min(len(requests), len(tc.responses)-1)
				requests = append(requests, time.Now())
				mu.Unlock()
				if tc.responses[index] == "unavailable" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = fmt.Fprint(w, tc.responses[index])
			}))
			defer server.Close()
			opts := Options{MetricsURL: server.URL, PropagationDelay: 10 * time.Millisecond,
				Timeout: 3 * time.Second, PollInterval: 5 * time.Millisecond}
			var output bytes.Buffer
			started := time.Now()
			require.NoError(t, Wait(drainTestContext(&output), opts))
			require.Contains(t, output.String(), "drain complete")
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, requests, tc.want)
			require.GreaterOrEqual(t, requests[0].Sub(started), opts.PropagationDelay)
		})
	}
}

func TestWaitBoundsFetchAndBudget(t *testing.T) {
	for _, mode := range []string{"busy", "missing", "slow headers", "trickling body"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "busy":
					_, _ = fmt.Fprint(w, sample("1"))
				case "missing":
					_, _ = fmt.Fprint(w, "# no sample\n")
				case "slow headers":
					<-r.Context().Done()
				case "trickling body":
					ticker := time.NewTicker(5 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-r.Context().Done():
							return
						case <-ticker.C:
							_, _ = fmt.Fprint(w, "# keepalive\n")
							w.(http.Flusher).Flush()
						}
					}
				}
			}))
			defer server.Close()
			opts := Options{MetricsURL: server.URL, Timeout: 60 * time.Millisecond, PollInterval: 5 * time.Millisecond}
			var output bytes.Buffer
			started := time.Now()
			require.NoError(t, Wait(drainTestContext(&output), opts))
			require.GreaterOrEqual(t, time.Since(started), opts.Timeout)
			require.Less(t, time.Since(started), time.Second)
			require.Contains(t, output.String(), "deadline reached")
			require.NotContains(t, output.String(), "drain complete")
		})
	}
}

func TestWaitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Wait(ctx, Options{MetricsURL: "http://127.0.0.1:1/metrics", PropagationDelay: time.Hour,
		Timeout: 2 * time.Hour, PollInterval: time.Second})
	require.ErrorIs(t, err, context.Canceled)
}

func TestWaitRetriesTimedOutFetch(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		first := requests == 1
		mu.Unlock()
		if first {
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprint(w, sample("0"))
	}))
	defer server.Close()
	var output bytes.Buffer
	started := time.Now()
	err := Wait(drainTestContext(&output), Options{MetricsURL: server.URL, Timeout: 10 * time.Second,
		PollInterval: 5 * time.Millisecond})
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 5*time.Second)
	require.Contains(t, output.String(), "metrics fetch failed")
	require.Contains(t, output.String(), "drain complete")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 4, requests)
}

func TestFetchLimits(t *testing.T) {
	for _, mode := range []string{"redirect", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, "/other", http.StatusFound)
					return
				}
				_, _ = fmt.Fprint(w, sample("0"), strings.Repeat(" ", maxMetricsBodyBytes))
			}))
			defer server.Close()
			client := metricsClient()
			defer client.CloseIdleConnections()
			_, err := fetchInFlight(context.Background(), client, server.URL)
			require.Error(t, err)
		})
	}
}

func TestOptionsValidation(t *testing.T) {
	for name, opts := range map[string]Options{
		"empty url":        {Timeout: time.Second, PollInterval: time.Second},
		"bad scheme":       {MetricsURL: "file:///metrics", Timeout: time.Second, PollInterval: time.Second},
		"no host":          {MetricsURL: "http:///metrics", Timeout: time.Second, PollInterval: time.Second},
		"zero timeout":     {MetricsURL: "http://localhost/metrics", PollInterval: time.Second},
		"zero poll":        {MetricsURL: "http://localhost/metrics", Timeout: time.Second},
		"negative delay":   {MetricsURL: "http://localhost/metrics", Timeout: time.Second, PollInterval: time.Second, PropagationDelay: -1},
		"delay at budget":  {MetricsURL: "http://localhost/metrics", Timeout: time.Second, PollInterval: time.Second, PropagationDelay: time.Second},
		"excessive budget": {MetricsURL: "http://localhost/metrics", Timeout: 25 * time.Hour, PollInterval: time.Second},
		"excessive poll":   {MetricsURL: "http://localhost/metrics", Timeout: time.Hour, PollInterval: 25 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, opts.Validate())
		})
	}
}
