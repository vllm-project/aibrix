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

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vllm-project/aibrix/pkg/metrics"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = DescribeTable("routing strategy regressions", Label("gateway", "integration"),
	func(strategy string) {
		// Every entry exercises the common contract: real Server.Process,
		// strategy-specific state, effective routing output, and cleanup.
		fixture := newGatewayFixtureWithRequest(twoReadyPods(), strategy, "", "")
		configureStrategyMetrics(fixture.cache, strategy)
		if strategy == "prefix-cache" {
			// Keep both candidates through the gateway load gate, then seed the
			// fixture-local table so the real prefix scorer can select other.
			fixture.cache.metricValues["target/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
			fixture.cache.metricValues["other/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
			fixture.prefixIndexer.AddPrefix(fixture.prefixIndexer.GetPrefixHashes([]byte("hello")), "llama2-7b", "other")
		}
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "routing-strategy", strategy)
		switch strategy {
		case "random":
			// Random has no stable target contract; only its ready candidate set is stable.
			expectHeaderOneOf(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000", "10.0.0.3:8000")
		case "prefix-cache":
			expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
		default:
			expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
		}
		expectSuccessfulLifecycle(fixture)
	},
	Entry("random", "random"),
	Entry("least-request", "least-request"),
	Entry("least-kv-cache", "least-kv-cache"),
	Entry("least-latency", "least-latency"),
	Entry("load-balance", "load-balance"),
	Entry("prefix-cache", "prefix-cache"),
)

// The base prefix case covers fallback; the configuration regression below
// reuses one fixture/server/local table to prove miss-to-hit without shared state.

var _ = Describe("routing configuration regressions", Label("gateway", "integration"), func() {
	It("retains a prefix mapping across requests in the fixture-local indexer", func() {
		strategy := "prefix-cache:1,least-request:0,load-balance:0"
		fixture := newGatewayFixtureWithRequest(twoReadyPods(), strategy, "", "")
		// Keep both pods through the gateway imbalance gate (gap < 8), while
		// making the real prefix router's miss fallback choose other.
		fixture.cache.metricValues["target/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 8}
		fixture.cache.metricValues["other/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 1}

		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		first := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(first.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
		firstMatch, _ := fixture.prefixIndexer.MatchPrefix(
			[]byte("hello"), "llama2-7b",
			map[string]struct{}{"target": {}, "other": {}},
		)
		Expect(firstMatch["other"]).To(BeNumerically(">", 0), fixture.diagnostics())
		expectSuccessfulLifecycle(fixture)

		secondID := fmt.Sprintf("%032x", fixtureSequence.Add(1))
		fixture.requestID = secondID
		fixture.stream = newFakeProcessStream(
			context.Background(),
			requestHeadersRequest(secondID, strategy, "", ""),
			requestBodyRequest(), responseHeadersRequest(), responseBodyRequest(),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		second := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(second.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
		secondMatch, _ := fixture.prefixIndexer.MatchPrefix(
			[]byte("hello"), "llama2-7b",
			map[string]struct{}{"target": {}, "other": {}},
		)
		Expect(secondMatch["other"]).To(BeNumerically(">", 0), fixture.diagnostics())
		expectSuccessfulLifecycle(fixture)
	})

	It("rejects an invalid strategy", func() {
		fixture := newGatewayFixtureWithRequest([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)}, "not-a-strategy", "", "")
		err := fixture.run()
		Expect(err).To(HaveOccurred(), fixture.diagnostics())
		Expect(errors.Is(err, io.EOF)).To(BeTrue(), fixture.diagnostics())
		immediateResponse := findResponseWithImmediate(fixture.stream.responses())
		Expect(immediateResponse).NotTo(BeNil(), fixture.diagnostics())
		immediate := immediateResponse.GetImmediateResponse()
		Expect(immediate.GetStatus().GetCode()).To(Equal(envoyTypePb.StatusCode_BadRequest), fixture.diagnostics())
		Expect(immediate.GetBody()).To(ContainSubstring("incorrect routing strategy"), fixture.diagnostics())
		expectErrorLifecycle(fixture)
	})

	It("ignores a zero-weight strategy", func() {
		// This is a representative configuration case, not a complete scorer-
		// exclusion matrix, which belongs in broader routing coverage.
		fixture := newGatewayFixtureWithRequest(twoReadyPods(), "least-request:0,least-kv-cache:1,load-balance:0", "", "")
		configureZeroWeightMetrics(fixture.cache)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(
			response.GetHeaderMutation().GetSetHeaders(),
			"routing-strategy", "least-request:0,least-kv-cache:1,load-balance:0",
		)
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
		// Balanced running counts make least-request prefer target while the
		// enabled KV scorer prefers other; KV reads prove scorer participation.
		Expect(fixture.cache.metricReadCount(metrics.KVCacheUsagePerc)).To(BeNumerically(">=", 2), fixture.diagnostics())
		expectSuccessfulLifecycle(fixture)

		// Full scorer-exclusion A/B coverage is a follow-up: the gateway's
		// load-imbalance gate and auto-blend can observe disabled metrics.
	})

	It("uses the multi-strategy routing configuration", func() {
		// Keep one representative real composition here; exhaustive weight causality
		// is intentionally outside this focused integration suite.
		fixture := newGatewayFixtureWithRequest(twoReadyPods(), "least-request:3,least-latency:1", "", "")
		configureMultiStrategyMetrics(fixture.cache)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "least-request:3,least-latency:1")
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
		// A complete weight-by-weight causal matrix is a follow-up; this case
		// verifies the real combined router path and effective target.
		expectSuccessfulLifecycle(fixture)
	})

	It("filters candidates by readiness", func() {
		fixture := newGatewayFixtureWithRequest(
			[]*corev1.Pod{
				readyPod("target", "10.0.0.2", nil),
				notReadyPod("offline", "10.0.0.3"),
			}, "random", "", "",
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		expectHeader(
			findRequestBodyResponse(fixture.stream.responses()).GetHeaderMutation().GetSetHeaders(),
			"target-pod", "10.0.0.2:8000",
		)
		expectSuccessfulLifecycle(fixture)
	})

	It("applies the external label filter before routing", func() {
		fixture := newGatewayFixtureWithRequest(
			[]*corev1.Pod{
				readyPod("target", "10.0.0.2", map[string]string{"gpu": "true"}),
				readyPod("other", "10.0.0.3", map[string]string{"gpu": "false"}),
			}, "random", "", "gpu=true",
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		expectHeader(
			findRequestBodyResponse(fixture.stream.responses()).GetHeaderMutation().GetSetHeaders(),
			"target-pod", "10.0.0.2:8000",
		)
		expectSuccessfulLifecycle(fixture)
	})

	It("lets a request routing header override the selected profile", func() {
		pods := twoReadyPods()
		profileConfig := `{"defaultProfile":"default","profiles":` +
			`{"default":{"routingStrategy":"least-kv-cache:1,load-balance:0"}}}`
		for _, pod := range pods {
			pod.Annotations = map[string]string{
				"model.aibrix.ai/config": profileConfig,
			}
		}
		fixture := newGatewayFixtureWithRequest(pods, "least-request:1,load-balance:0", "default", "")
		fixture.cache.metricValues["target/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 10}
		fixture.cache.metricValues["other/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 1}
		fixture.cache.metricValues["target/"+metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: 0.1}
		fixture.cache.metricValues["other/"+metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: 0.9}
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "least-request:1,load-balance:0")
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
		expectSuccessfulLifecycle(fixture)
	})
})

func readyPod(name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func notReadyPod(name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
}

func twoReadyPods() []*corev1.Pod {
	return []*corev1.Pod{readyPod("target", "10.0.0.2", nil), readyPod("other", "10.0.0.3", nil)}
}

func expectSuccessfulLifecycle(fixture *gatewayFixture) {
	events := fixture.cache.eventsSnapshotForRequest(fixture.requestID)
	Expect(events).To(HaveLen(2), fixture.diagnostics())
	Expect(findEvent(events, "add")).NotTo(BeNil(), fixture.diagnostics())
	Expect(findEvent(events, "done-trace")).NotTo(BeNil(), fixture.diagnostics())
	Expect(findEvent(events, "done")).To(BeNil(), fixture.diagnostics())
	Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
}

func (c *fakeCache) eventsSnapshotForRequest(requestID string) []requestEvent {
	events := c.eventsSnapshot()
	filtered := make([]requestEvent, 0, len(events))
	for _, event := range events {
		if event.RequestID == requestID {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func expectErrorLifecycle(fixture *gatewayFixture) {
	events := fixture.cache.eventsSnapshot()
	Expect(events).To(HaveLen(1), fixture.diagnostics())
	Expect(findEvent(events, "done")).NotTo(BeNil(), fixture.diagnostics())
	Expect(findEvent(events, "done-trace")).To(BeNil(), fixture.diagnostics())
	Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
}

func configureStrategyMetrics(c *fakeCache, strategy string) {
	set := func(pod, metric string, value metrics.MetricValue) { c.metricValues[pod+"/"+metric] = value }
	switch strategy {
	case "least-request":
		set("target", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 2})
		set("other", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 17})
	case "least-kv-cache":
		set("target", metrics.KVCacheUsagePerc, &metrics.SimpleMetricValue{Value: .15})
		set("target", metrics.CPUCacheUsagePerc, &metrics.SimpleMetricValue{Value: .05})
		set("other", metrics.KVCacheUsagePerc, &metrics.SimpleMetricValue{Value: .85})
		set("other", metrics.CPUCacheUsagePerc, &metrics.SimpleMetricValue{Value: .75})
	case "least-latency":
		set("target", metrics.RequestQueueTimeSeconds, &metrics.SimpleMetricValue{Value: .02})
		set("other", metrics.RequestQueueTimeSeconds, &metrics.SimpleMetricValue{Value: .9})
		set("target", metrics.RequestPrefillTimeSeconds, &metrics.HistogramMetricValue{Sum: .1, Count: 1})
		set("target", metrics.RequestDecodeTimeSeconds, &metrics.HistogramMetricValue{Sum: .1, Count: 1})
		set("other", metrics.RequestPrefillTimeSeconds, &metrics.HistogramMetricValue{Sum: 4, Count: 1})
		set("other", metrics.RequestDecodeTimeSeconds, &metrics.HistogramMetricValue{Sum: 4, Count: 1})
	case "load-balance":
		set("target", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 3})
		set("target", metrics.RealtimeRunningRequestsDrainRate1m, &metrics.SimpleMetricValue{Value: 3})
		set("other", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 12})
		set("other", metrics.RealtimeRunningRequestsDrainRate1m, &metrics.SimpleMetricValue{Value: 1})
	case "prefix-cache":
		set("target", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 4})
		set("other", metrics.RealtimeNumRequestsRunning, &metrics.SimpleMetricValue{Value: 15})
	}
}

func configureZeroWeightMetrics(c *fakeCache) {
	c.metricValues["target/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 1}
	c.metricValues["other/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 3}
	c.metricValues["target/"+metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: .9}
	c.metricValues["target/"+metrics.CPUCacheUsagePerc] = &metrics.SimpleMetricValue{Value: .9}
	c.metricValues["other/"+metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: .1}
	c.metricValues["other/"+metrics.CPUCacheUsagePerc] = &metrics.SimpleMetricValue{Value: .1}
}

func configureMultiStrategyMetrics(c *fakeCache) {
	c.metricValues["target/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 1}
	c.metricValues["other/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 10}
	c.metricValues["target/"+metrics.RequestQueueTimeSeconds] = &metrics.SimpleMetricValue{Value: 10}
	c.metricValues["other/"+metrics.RequestQueueTimeSeconds] = &metrics.SimpleMetricValue{Value: 1}
	c.metricValues["target/"+metrics.RequestPrefillTimeSeconds] = &metrics.HistogramMetricValue{Sum: 10, Count: 1}
	c.metricValues["target/"+metrics.RequestDecodeTimeSeconds] = &metrics.HistogramMetricValue{Sum: 10, Count: 1}
	c.metricValues["other/"+metrics.RequestPrefillTimeSeconds] = &metrics.HistogramMetricValue{Sum: 1, Count: 1}
	c.metricValues["other/"+metrics.RequestDecodeTimeSeconds] = &metrics.HistogramMetricValue{Sum: 1, Count: 1}
}

func expectHeaderOneOf(headers []*configPb.HeaderValueOption, key string, values ...string) {
	for _, header := range headers {
		if header.GetHeader().GetKey() == key {
			Expect(values).To(ContainElement(string(header.GetHeader().GetRawValue())))
			return
		}
	}
	Fail(fmt.Sprintf("missing header %q", key))
}
