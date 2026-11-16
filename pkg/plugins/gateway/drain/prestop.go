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

// Package drain implements the gateway's pod-local preStop wait.
package drain

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"k8s.io/klog/v2"
)

const (
	inFlightMetric      = "gateway_in_flight_requests"
	maxMetricsBodyBytes = 8 << 20
)

var sampleValue = regexp.MustCompile(`^[+]?[0-9]+([.][0-9]+)?([eE][+-]?[0-9]+)?$`)

// Options configures the preStop budget, including the propagation delay.
type Options struct {
	MetricsURL       string
	PropagationDelay time.Duration
	Timeout          time.Duration
	PollInterval     time.Duration
}

// Validate rejects invalid options before any network requests or waits.
func (o Options) Validate() error {
	u, err := url.Parse(o.MetricsURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("metrics URL must be an absolute HTTP or HTTPS URL")
	}
	if o.PropagationDelay < 0 || o.Timeout <= o.PropagationDelay || o.PollInterval <= 0 {
		return fmt.Errorf("propagation delay must be nonnegative, timeout must exceed it, and poll interval must be positive")
	}
	if o.Timeout > 24*time.Hour || o.PollInterval > 24*time.Hour {
		return fmt.Errorf("durations must not exceed 24 hours")
	}
	return nil
}

// Wait accepts three consecutive valid zero samples or exhausts the hook budget.
// Exhausting that budget is a normal return: kubelet must proceed with SIGTERM.
// Cancellation by the caller is returned as an error.
func Wait(ctx context.Context, opts Options) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	budget, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	client := metricsClient()
	defer client.CloseIdleConnections()
	logger.Info("waiting for endpoint updates", "propagationDelay", opts.PropagationDelay, "timeout", opts.Timeout)
	if waitInterval(budget, opts.PropagationDelay) {
		zeros := 0
		for budget.Err() == nil {
			value, err := fetchInFlight(budget, client, opts.MetricsURL)
			if err != nil {
				zeros = 0
				logger.Error(err, "metrics fetch failed or sample missing or invalid; retrying")
			} else if budget.Err() == nil {
				if value == 0 {
					zeros++
					if zeros == 3 {
						logger.Info("in-flight is zero for three consecutive scrapes; drain complete")
						return nil
					}
				} else {
					zeros = 0
					logger.Info("requests still in flight", "inFlightRequests", value)
				}
			}
			if !waitInterval(budget, opts.PollInterval) {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Info("drain deadline reached; allowing SIGTERM (requests may remain)")
	return nil
}

func waitInterval(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func metricsClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// This endpoint belongs to the terminating Pod, regardless of proxy env vars.
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func fetchInFlight(ctx context.Context, client *http.Client, metricsURL string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("metrics endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetricsBodyBytes+1))
	if err != nil {
		return 0, err
	}
	if len(body) > maxMetricsBodyBytes {
		return 0, fmt.Errorf("metrics response exceeds %d bytes", maxMetricsBodyBytes)
	}
	return inFlightSample(body)
}

func inFlightSample(body []byte) (float64, error) {
	var sample []byte
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := string(fields[0])
		if name != inFlightMetric && !strings.HasPrefix(name, inFlightMetric+"{") {
			continue
		}
		// Preserve the shell hook's one-sample rule, lexical value checks and
		// rejection of timestamps. Unrelated metrics do not affect this decision.
		if sample != nil || len(fields) != 2 || !sampleValue.Match(fields[1]) {
			return 0, fmt.Errorf("expected exactly one valid %s sample without a timestamp", inFlightMetric)
		}
		sample = line
	}
	if sample == nil {
		return 0, fmt.Errorf("%s sample is missing", inFlightMetric)
	}
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(sample) + "\n"))
	if err != nil {
		return 0, err
	}
	family := families[inFlightMetric]
	if family == nil || len(family.Metric) != 1 || family.Metric[0].Untyped == nil {
		return 0, fmt.Errorf("invalid %s sample", inFlightMetric)
	}
	// Only the selected sample is parsed, so no TYPE declaration is required.
	value := family.Metric[0].Untyped.GetValue()
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Signbit(value) {
		return 0, fmt.Errorf("%s must be finite and nonnegative", inFlightMetric)
	}
	return value, nil
}
