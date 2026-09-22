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

package routingalgorithms

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeRouterQueue is a minimal types.RouterQueue used to drive queueRouter.
type fakeRouterQueue struct {
	mu    sync.Mutex
	items []*types.RoutingContext
}

func (q *fakeRouterQueue) Enqueue(ctx *types.RoutingContext, _ time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, ctx)
	return nil
}

func (q *fakeRouterQueue) Peek(_ time.Time, _ types.PodList) (*types.RoutingContext, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, types.ErrQueueEmpty
	}
	return q.items[0], nil
}

func (q *fakeRouterQueue) Dequeue(_ time.Time) (*types.RoutingContext, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, types.ErrQueueEmpty
	}
	ctx := q.items[0]
	q.items = q.items[1:]
	return ctx, nil
}

func (q *fakeRouterQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// fakeBackendRouter either completes the route by setting a target pod, or
// fails with the configured error so outcome classification can be exercised.
type fakeBackendRouter struct {
	err error
}

func (r fakeBackendRouter) Route(ctx *types.RoutingContext, _ types.PodList) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	ctx.SetTargetPod(newQueueTestPod())
	return "10.0.0.1:8000", nil
}

func newQueueTestPod() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "queue-test-pod", Namespace: "default"},
		Status:     v1.PodStatus{PodIP: "10.0.0.1"},
	}
}

type metricEmission struct {
	name        string
	value       float64
	labelNames  []string
	labelValues []string
}

// metricCapture records counter and gauge emissions while the hooks are installed.
type metricCapture struct {
	mu       sync.Mutex
	counters []metricEmission
	gauges   []metricEmission
	restore  func()
}

func startMetricCapture() *metricCapture {
	capture := &metricCapture{}
	originalCounter := metrics.IncrementCounterMetricFnForTest
	originalGauge := metrics.SetGaugeMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name string, _ string, value float64, labelNames []string, labelValues ...string) {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.counters = append(capture.counters, newMetricEmission(name, value, labelNames, labelValues))
	}
	metrics.SetGaugeMetricFnForTest = func(name string, _ string, value float64, labelNames []string, labelValues ...string) {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.gauges = append(capture.gauges, newMetricEmission(name, value, labelNames, labelValues))
	}
	capture.restore = func() {
		metrics.IncrementCounterMetricFnForTest = originalCounter
		metrics.SetGaugeMetricFnForTest = originalGauge
	}
	return capture
}

func newMetricEmission(name string, value float64, labelNames []string, labelValues []string) metricEmission {
	return metricEmission{
		name:        name,
		value:       value,
		labelNames:  append([]string(nil), labelNames...),
		labelValues: append([]string(nil), labelValues...),
	}
}

func (c *metricCapture) countersFor(metricName string) []metricEmission {
	c.mu.Lock()
	defer c.mu.Unlock()
	return filterEmissions(c.counters, metricName)
}

func (c *metricCapture) gaugesFor(metricName string) []metricEmission {
	c.mu.Lock()
	defer c.mu.Unlock()
	return filterEmissions(c.gauges, metricName)
}

func filterEmissions(emissions []metricEmission, metricName string) []metricEmission {
	var out []metricEmission
	for _, e := range emissions {
		if e.name == metricName {
			out = append(out, e)
		}
	}
	return out
}

func labelValue(e metricEmission, labelName string) string {
	for i, name := range e.labelNames {
		if name == labelName && i < len(e.labelValues) {
			return e.labelValues[i]
		}
	}
	return ""
}

// waitForEmissions polls until at least want emissions were recorded, so the
// test can observe emissions made by queueRouter's serve goroutine.
func waitForEmissions(t *testing.T, get func() []metricEmission, want int) []metricEmission {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := get()
	for len(got) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		got = get()
	}
	return got
}

var queueWaitBucketPattern = regexp.MustCompile(`^\d+(-\d+)?ms(\+)?$`)

func TestQueueWaitBucketLabel(t *testing.T) {
	cases := []struct {
		wait time.Duration
		want string
	}{
		{0, "0-10ms"},
		{-time.Second, "0-10ms"},
		{9 * time.Millisecond, "0-10ms"},
		{10 * time.Millisecond, "10-50ms"},
		{49 * time.Millisecond, "10-50ms"},
		{50 * time.Millisecond, "50-100ms"},
		{1500 * time.Millisecond, "1000-5000ms"},
		{9 * time.Second, "5000-10000ms"},
		{10 * time.Second, "10000-30000ms"},
		{30 * time.Second, "30000-60000ms"},
		{60 * time.Second, "60000ms+"},
		{5 * time.Minute, "60000ms+"},
	}
	for _, tc := range cases {
		if got := queueWaitBucketLabel(tc.wait); got != tc.want {
			t.Errorf("queueWaitBucketLabel(%v) = %q, want %q", tc.wait, got, tc.want)
		}
	}
}

func TestQueueOutcomeLabel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"routed", nil, "routed"},
		{"slo failure", cache.ErrorSLOFailureRequest, "slo_failure"},
		{"wrapped slo failure", fmt.Errorf("route: %w", cache.ErrorSLOFailureRequest), "slo_failure"},
		{"capacity reached", cache.ErrorLoadCapacityReached, "capacity_reached"},
		{"unexpected error", errors.New("no candidate"), "error"},
	}
	for _, tc := range cases {
		if got := queueOutcomeLabel(tc.err); got != tc.want {
			t.Errorf("%s: queueOutcomeLabel(%v) = %q, want %q", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestQueueMetricsRegistered(t *testing.T) {
	for _, name := range []string{
		metrics.GatewayQueueWaitTimeBucketTotal,
		metrics.GatewayQueuePendingRequests,
		metrics.GatewayQueueOutcomeTotal,
		metrics.GatewayQueueFIFOFallbackTotal,
	} {
		if _, ok := metrics.Metrics[name]; !ok {
			t.Errorf("gateway queue metric %s is not registered", name)
		}
	}
}

func TestQueueRouterEmitsQueueMetricsByOutcome(t *testing.T) {
	cases := []struct {
		name     string
		routeErr error
		want     string
	}{
		{"routed", nil, "routed"},
		{"slo failure", cache.ErrorSLOFailureRequest, "slo_failure"},
		{"capacity reached", cache.ErrorLoadCapacityReached, "capacity_reached"},
		{"unexpected error", errors.New("backend selection failed"), "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache.InitForTest()
			capture := startMetricCapture()
			defer capture.restore()

			router, err := NewQueueRouter(fakeBackendRouter{err: tc.routeErr}, &fakeRouterQueue{})
			if err != nil {
				t.Fatalf("NewQueueRouter() error = %v", err)
			}

			ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm("slo"), "test-model", "hello world", "req-"+tc.name, "")
			pods := newMockPodList([]*v1.Pod{newQueueTestPod()}, nil)

			if _, err := router.Route(ctx, pods); !errors.Is(err, tc.routeErr) {
				t.Fatalf("Route() error = %v, want %v", err, tc.routeErr)
			}

			outcomes := waitForEmissions(t, func() []metricEmission {
				return capture.countersFor(metrics.GatewayQueueOutcomeTotal)
			}, 1)
			if len(outcomes) != 1 {
				t.Fatalf("expected 1 outcome emission, got %d", len(outcomes))
			}
			if got := labelValue(outcomes[0], "outcome"); got != tc.want {
				t.Fatalf("outcome label = %q, want %q", got, tc.want)
			}
			if got := labelValue(outcomes[0], "model"); got != "test-model" {
				t.Fatalf("model label = %q, want %q", got, "test-model")
			}

			waits := waitForEmissions(t, func() []metricEmission {
				return capture.countersFor(metrics.GatewayQueueWaitTimeBucketTotal)
			}, 1)
			if len(waits) != 1 {
				t.Fatalf("expected 1 wait bucket emission, got %d", len(waits))
			}
			if bucket := labelValue(waits[0], "bucket"); !queueWaitBucketPattern.MatchString(bucket) {
				t.Fatalf("wait bucket label = %q, want a low-highms bucket", bucket)
			}

			pending := waitForEmissions(t, func() []metricEmission {
				return capture.gaugesFor(metrics.GatewayQueuePendingRequests)
			}, 2)
			if len(pending) != 2 {
				t.Fatalf("expected 2 pending gauge emissions, got %d", len(pending))
			}
			if pending[0].value != 1 || pending[1].value != 0 {
				t.Fatalf("pending gauge values = [%v %v], want [1 0]", pending[0].value, pending[1].value)
			}
		})
	}
}
