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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	ctrlconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/blacklist"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Tests for the Server side of the pod blacklist: how request outcomes reach
// the breaker and how routing reads it. The breaker and its sweeps are tested
// in the blacklist package.

func blacklistTestPod(name string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status: v1.PodStatus{
			PodIP:      "10.0.0." + name,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// tripBreaker blacklists pod the way the probe sweep does, by reporting it
// degraded until the sweep's count trips the breaker.
func tripBreaker(b *blacklist.PodBlacklist, pod *v1.Pod) {
	for i := 0; i < 10 && !b.IsBlacklisted(pod); i++ {
		b.RecordProbeResult(pod, blacklist.ProbeDegraded, "GET /v1/models did not list the model")
	}
}

// captureKlogOutput redirects klog into a buffer for the test and returns a reader for it.
// It mutates process-global klog state, so tests that use it must not call t.Parallel().
func captureKlogOutput(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(klogWriterFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
	})
	return func() string {
		klog.Flush()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type klogWriterFunc func([]byte) (int, error)

func (f klogWriterFunc) Write(p []byte) (int, error) { return f(p) }

// engineQueueMock stubs the metrics podEngineQueueDepth reads for one pod serving
// the given models: running+waiting for each is running[i]+waiting[i].
func engineQueueMock(mc *MockCache, pod *v1.Pod, models []string, running, waiting []float64) {
	mc.On("ListModelsByPod", pod.Name, pod.Namespace).Return(models, nil)
	for i, model := range models {
		mc.On("GetMetricValueByPodModel", pod.Name, pod.Namespace, model, metrics.NumRequestsRunning).
			Return(&metrics.SimpleMetricValue{Value: running[i]}, nil)
		mc.On("GetMetricValueByPodModel", pod.Name, pod.Namespace, model, metrics.NumRequestsWaiting).
			Return(&metrics.SimpleMetricValue{Value: waiting[i]}, nil)
	}
}

// The recovery callback is invoked from the sweep with a pod it was handed. A nil one logs
// nothing and reads nothing from the cache (the mock has no expectations, so any call fails).
func TestLogPodUnBlacklisted_NilPodIsHarmless(t *testing.T) {
	server := &Server{cache: new(MockCache)}

	assert.NotPanics(t, func() { server.logPodUnBlacklisted(nil) })
	assert.Zero(t, server.getGlobalRunningRequestsByPod(nil))
	assert.Zero(t, server.podEngineTotal(nil))
}

// The un-blacklist line has to carry the same pod/addr/model fields as the
// "blacklisted" line it closes, so the two can be joined on a pod and model, and
// the gateway-wide and engine totals the pod was judged on.
func TestLogPodUnBlacklisted_NamesPodAddrModelAndTotals(t *testing.T) {
	logs := captureKlogOutput(t)
	pod := blacklistTestPod("3")
	pod.Labels = map[string]string{constants.ModelLabelName: "test-model", constants.ModelLabelPort: "8000"}
	mc := new(MockCache)
	mc.On("GetPodRunningRequests", "3", "ns").Return(int64(4), nil)
	engineQueueMock(mc, pod, []string{"test-model"}, []float64{7}, []float64{3})
	server := &Server{cache: mc}

	server.logPodUnBlacklisted(pod)

	out := logs()
	assert.Contains(t, out, `pod="ns/3"`)
	assert.Contains(t, out, `addr="10.0.0.3:8000"`)
	assert.Contains(t, out, `model="test-model"`)
	assert.Contains(t, out, "gateway_total=4")
	assert.Contains(t, out, "engine_total=10")
}

func TestAllBlacklistedLogThrottle(t *testing.T) {
	var th allBlacklistedLogThrottle
	t0 := time.Unix(1_000, 0)

	suppressed, ok := th.allow("model-a", t0)
	assert.True(t, ok, "first line for a model is always logged")
	assert.Equal(t, 0, suppressed)

	for i := 0; i < 2; i++ {
		_, ok = th.allow("model-a", t0.Add(time.Second))
		assert.False(t, ok, "inside the interval the line is dropped")
	}

	_, ok = th.allow("model-b", t0.Add(time.Second))
	assert.True(t, ok, "another model has its own budget")

	suppressed, ok = th.allow("model-a", t0.Add(allBlacklistedLogInterval))
	assert.True(t, ok, "logged again once the interval has elapsed")
	assert.Equal(t, 2, suppressed, "reports how many lines were dropped in between")

	suppressed, ok = th.allow("model-a", t0.Add(allBlacklistedLogInterval+time.Second))
	assert.False(t, ok)
	assert.Equal(t, 0, suppressed)
}

func TestLogAllPodsBlacklisted_NamesModelAndReasonsOncePerInterval(t *testing.T) {
	logs := captureKlogOutput(t)
	server := &Server{}
	pod := blacklistTestPod("1")
	tripBreaker(&server.podBlacklist, pod)
	require.True(t, server.podBlacklist.IsBlacklisted(pod))
	routeCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "req-1", "test-user")

	for i := 0; i < 3; i++ {
		server.logAllPodsBlacklisted(routeCtx, []*v1.Pod{pod})
	}

	out := logs()
	assert.Equal(t, 1, strings.Count(out, "all ready pods are blacklisted"), "throttled to one line per interval")
	assert.Contains(t, out, `model="test-model"`)
	assert.Contains(t, out, `requestID="req-1"`)
	assert.Contains(t, out, "1: ", "reasons name the blacklisted pod")
}

// TestPodEngineQueueDepth_ReadsPodModelScopedMetric exercises podEngineQueueDepth
// against a real cache.Store (not MockCache) to prove it reads the pod+model-scoped
// metric num_requests_running/num_requests_waiting are stored under
// (metrics.PodModelMetricScope -- see cache.NewWithPodsModelMetricsForTest), rather
// than the pod-only scope GetMetricValueByPod looks at, which always misses them and
// would silently contribute 0 for every pod.
func TestPodEngineQueueDepth_ReadsPodModelScopedMetric(t *testing.T) {
	pod := blacklistTestPod("1")
	store := cache.NewWithPodsModelMetricsForTest([]*v1.Pod{pod}, "test-model", map[string]map[string]metrics.MetricValue{
		"1": {
			metrics.NumRequestsRunning: &metrics.SimpleMetricValue{Value: 7},
			metrics.NumRequestsWaiting: &metrics.SimpleMetricValue{Value: 3},
		},
	})

	server := &Server{cache: store}
	load, ok := server.podEngineQueueDepth(pod, "test-model")
	assert.True(t, ok)
	assert.Equal(t, 10.0, load, "should read the pod+model-scoped metric, not silently contribute 0")

	// The exact miss that would make podEngineQueueDepth always read 0: the
	// pod-only accessor genuinely does not have this metric.
	_, err := store.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.NumRequestsRunning)
	assert.Error(t, err, "num_requests_running is pod+model scoped and must not be visible via the pod-only accessor")
}

// A missing metric is an unmeasured pod, not an idle one: the load sweep leaves it out
// instead of scoring it, or its peers, as load 0.
func TestPodEngineQueueDepth_MissingMetricIsUnmeasured(t *testing.T) {
	pod := blacklistTestPod("1")

	t.Run("neither metric", func(t *testing.T) {
		mc := new(MockCache)
		mc.On("GetMetricValueByPodModel", "1", "ns", "test-model", mock.Anything).
			Return(&metrics.SimpleMetricValue{}, fmt.Errorf("metric not found"))
		server := &Server{cache: mc}

		load, ok := server.podEngineQueueDepth(pod, "test-model")

		assert.False(t, ok)
		assert.Zero(t, load)
	})

	t.Run("only one of them", func(t *testing.T) {
		mc := new(MockCache)
		mc.On("GetMetricValueByPodModel", "1", "ns", "test-model", metrics.NumRequestsRunning).
			Return(&metrics.SimpleMetricValue{Value: 7}, nil)
		mc.On("GetMetricValueByPodModel", "1", "ns", "test-model", metrics.NumRequestsWaiting).
			Return(&metrics.SimpleMetricValue{}, fmt.Errorf("metric not found"))
		server := &Server{cache: mc}

		_, ok := server.podEngineQueueDepth(pod, "test-model")

		assert.False(t, ok, "half a measurement is not a measurement")
	})
}

// A pod that serves several models (a base model plus LoRA adapters) reports the
// engine's queue summed over all of them.
func TestPodEngineTotal_SumsEveryModelThePodServes(t *testing.T) {
	pod := blacklistTestPod("1")
	mc := new(MockCache)
	engineQueueMock(mc, pod, []string{"base", "adapter"}, []float64{7, 2}, []float64{3, 1})
	server := &Server{cache: mc}

	assert.Equal(t, 13.0, server.podEngineTotal(pod))
}

func TestPodEngineTotal_UnknownPodReadsAsZero(t *testing.T) {
	pod := blacklistTestPod("1")
	mc := new(MockCache)
	mc.On("ListModelsByPod", "1", "ns").Return([]string(nil), fmt.Errorf("pod not in cache"))
	server := &Server{cache: mc}

	assert.Equal(t, 0.0, server.podEngineTotal(pod))
}

func handleResponseHeadersStatusReq(status string) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{
						{Key: ":status", RawValue: []byte(status)},
					},
				},
			},
		},
	}
}

// recvErrorServer is a Server wired for handleRecvError tests: a cache that
// accepts the request-count release every exit path performs.
func recvErrorServer() *Server {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	return &Server{
		cache:               mc,
		requestCountTracker: map[string]int{},
	}
}

// routedState is the processState of a request already routed to pod.
func routedState(ctx context.Context, pod *v1.Pod, reqID string) *processState {
	routerCtx := types.NewRoutingContext(ctx, "random", "test-model", "", reqID, "test-user")
	routerCtx.SetTargetPod(pod)
	return &processState{ctx: ctx, requestID: reqID, model: "test-model", routerCtx: routerCtx}
}

// recvFailure is one way the ext_proc stream can end before the response completed.
type recvFailure struct {
	name    string
	recvErr error
	wantErr func(t *testing.T, err error)
}

var recvFailures = []recvFailure{
	{"EOF", io.EOF, func(t *testing.T, err error) { assert.Equal(t, io.EOF, err) }},
	{"Canceled", status.Error(codes.Canceled, "context canceled"), func(t *testing.T, err error) {
		assert.Equal(t, codes.Canceled, status.Code(err))
	}},
}

// fallbackCounts records the requests routed with the blacklist ignored, by reason.
type fallbackCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func captureFallbackCounts(t *testing.T) *fallbackCounts {
	t.Helper()
	return captureBlacklistMetricCounts(t, metrics.GatewayPodBlacklistFallbackTotal)
}

// captureBlacklistMetricCounts records counts emitted to metricName, keyed by the reason label.
func captureBlacklistMetricCounts(t *testing.T, metricName string) *fallbackCounts {
	t.Helper()
	c := &fallbackCounts{counts: map[string]int{}}
	prev := metrics.IncrementCounterMetricFnForTest
	metrics.IncrementCounterMetricFnForTest = func(name, help string, value float64, names []string, values ...string) {
		if prev != nil {
			prev(name, help, value, names, values...)
		}
		if name != metricName {
			return
		}
		for i, n := range names {
			if n == "reason" {
				c.mu.Lock()
				c.counts[values[i]]++
				c.mu.Unlock()
			}
		}
	}
	t.Cleanup(func() { metrics.IncrementCounterMetricFnForTest = prev })
	return c
}

func (c *fallbackCounts) get(reason string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[reason]
}

// The blacklist may exclude at most AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT of the pods a
// request considers. Past that the pods are routed across as if none were excluded.
func Test_excludeBlacklisted_RoutesAcrossAllPodsOverTheCap(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(50))
	counts := captureFallbackCounts(t)
	pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3"), blacklistTestPod("4")}
	ctx := types.NewRoutingContext(context.Background(), routing.RouterRandom, "m", "", "r1", "")
	s := &Server{}

	assert.Equal(t, pods, s.excludeBlacklisted(ctx, pods), "nothing excluded")

	tripBreaker(&s.podBlacklist, pods[0])
	assert.Equal(t, pods[1:], s.excludeBlacklisted(ctx, pods), "a quarter excluded is within the cap")

	tripBreaker(&s.podBlacklist, pods[1])
	assert.Equal(t, pods[2:], s.excludeBlacklisted(ctx, pods), "half excluded is at the cap, not over it")
	assert.Zero(t, counts.get(fallbackOverCap))

	tripBreaker(&s.podBlacklist, pods[2])
	assert.Equal(t, pods, s.excludeBlacklisted(ctx, pods), "three quarters excluded is over the cap: every pod is routed across")
	assert.Equal(t, 1, counts.get(fallbackOverCap))
	assert.Zero(t, counts.get(fallbackAllExcluded))

	tripBreaker(&s.podBlacklist, pods[3])
	assert.Equal(t, pods, s.excludeBlacklisted(ctx, pods), "all excluded")
	assert.Equal(t, 1, counts.get(fallbackAllExcluded), "counted as such, not as over the cap")
}

// With the cap at 100 only the case where every pod is excluded falls back.
func Test_excludeBlacklisted_CapOffStillFallsBackWhenAllAreExcluded(t *testing.T) {
	t.Cleanup(blacklist.SetMaxExcludedPercentForTest(100))
	counts := captureFallbackCounts(t)
	pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")}
	ctx := types.NewRoutingContext(context.Background(), routing.RouterRandom, "m", "", "r1", "")
	s := &Server{}
	tripBreaker(&s.podBlacklist, pods[0])
	tripBreaker(&s.podBlacklist, pods[1])

	assert.Equal(t, pods[2:], s.excludeBlacklisted(ctx, pods), "two of three excluded, no cap")
	assert.Zero(t, counts.get(fallbackOverCap))

	tripBreaker(&s.podBlacklist, pods[2])
	assert.Equal(t, pods, s.excludeBlacklisted(ctx, pods))
	assert.Equal(t, 1, counts.get(fallbackAllExcluded))
}

// A single pod is the whole model: excluding it leaves nothing, so it is routed to anyway.
func Test_excludeBlacklisted_ASinglePodIsAlwaysRoutedTo(t *testing.T) {
	counts := captureFallbackCounts(t)
	only := blacklistTestPod("only")
	ctx := types.NewRoutingContext(context.Background(), routing.RouterRandom, "m", "", "r1", "")
	s := &Server{}
	tripBreaker(&s.podBlacklist, only)

	assert.Equal(t, []*v1.Pod{only}, s.excludeBlacklisted(ctx, []*v1.Pod{only}))
	assert.Equal(t, 1, counts.get(fallbackAllExcluded))
}

// respondWith delivers an upstream response with the given status to a request routed to pod,
// through the same step Process uses when the response headers arrive.
func respondWith(t *testing.T, server *Server, pod *v1.Pod, status, reqID string) {
	t.Helper()
	st := routedState(context.Background(), pod, reqID)
	_, err := server.handleProcessingRequest(st, handleResponseHeadersStatusReq(status))
	require.NoError(t, err)
}

// Upstream failure statuses, as the gateway sees them in the response headers, block the pod
// that served them once enough arrive in a row: a 502, 503 or 504, from the engine or from a proxy
// in front of it. (Envoy's own local replies never get here; see recordPodResponse.)
func Test_UpstreamFailureStatusesBlacklistThePodThatServedThem(t *testing.T) {
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(5)
	failing, bystander := blacklistTestPod("1"), blacklistTestPod("2")

	for i, code := range []string{"502", "503", "504", "503"} {
		respondWith(t, server, failing, code, fmt.Sprintf("fail-%d", i))
		assert.False(t, server.podBlacklist.IsBlacklisted(failing), "%d failures is one short of the threshold", i+1)
	}
	respondWith(t, server, failing, "503", "fail-last")

	assert.True(t, server.podBlacklist.IsBlacklisted(failing))
	assert.False(t, server.podBlacklist.Allow(failing))
	assert.True(t, server.podBlacklist.Allow(bystander), "only the pod that served the failures is blocked")
	assert.Contains(t, server.podBlacklist.BlacklistReason(failing), "5 upstream failures in a row, last status 503")
}

// A response that is not a failure between two failures starts the count over, so a pod that
// only now and then returns a 5xx, however often, is not blocked for it.
func Test_UpstreamFailuresBlockOnlyInARow(t *testing.T) {
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(3)
	pod := blacklistTestPod("1")

	for i := 0; i < 20; i++ {
		respondWith(t, server, pod, "503", fmt.Sprintf("a-%d", i))
		respondWith(t, server, pod, "503", fmt.Sprintf("b-%d", i))
		respondWith(t, server, pod, "200", fmt.Sprintf("ok-%d", i))
	}
	assert.False(t, server.podBlacklist.IsBlacklisted(pod))

	respondWith(t, server, pod, "503", "c-1")
	respondWith(t, server, pod, "503", "c-2")
	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	respondWith(t, server, pod, "503", "c-3")
	assert.True(t, server.podBlacklist.IsBlacklisted(pod))
}

// The status says whose fault a failure is. A client error, a rate limit, a route the engine does
// not implement and a bare 500 (the engine's answer to a request that made it throw, which a client
// can send on purpose) are not evidence against the pod, and a pod that answers them is alive; none
// blocks it, with the threshold at its lowest, and none creates an entry. A :status the gateway
// cannot parse becomes a 500, so it does not count either.
func Test_StatusesThatAreNotThePodsFaultDoNotBlacklistIt(t *testing.T) {
	routing.Init()
	pod := blacklistTestPod("1")
	pods := []*v1.Pod{pod}
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(1)

	for i := 0; i < 10; i++ {
		for _, code := range []string{"200", "204", "301", "400", "401", "404", "413", "422", "429", "500", "501", "505", "not-a-status"} {
			respondWith(t, server, pod, code, fmt.Sprintf("req-%s-%d", code, i))
		}
	}

	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	assert.True(t, server.podBlacklist.Allow(pod))
	assert.Equal(t, pods, server.podBlacklist.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = server.podBlacklist.FilterBlacklisted(pods) }),
		"none of them creates an entry, so the healthy-fleet fast path is intact")
}

// How the ext_proc stream ends is not recorded. Envoy half-closes and resets the stream
// together when a downstream client leaves, so an EOF or a cancel cannot be told from an upstream
// failure: a client that gives up, a stream that closes early and a gRPC error all leave the pod
// routable, with the threshold at its lowest, whether the stream context is live or cancelled.
func Test_StreamOutcomesDoNotBlacklistAPod(t *testing.T) {
	pod := blacklistTestPod("1")
	pods := []*v1.Pod{pod}
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(1)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 10; i++ {
		for _, tc := range recvFailures {
			for _, ctx := range []context.Context{context.Background(), cancelled} {
				tc.wantErr(t, server.handleRecvError(routedState(ctx, pod, fmt.Sprintf("req-%s-%d", tc.name, i)), tc.recvErr))
			}
		}
		unavailable := status.Error(codes.Unavailable, "upstream reset")
		assert.Error(t, server.handleRecvError(routedState(context.Background(), pod, fmt.Sprintf("req-unavailable-%d", i)), unavailable))
	}

	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	assert.Equal(t, pods, server.podBlacklist.FilterBlacklisted(pods))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = server.podBlacklist.FilterBlacklisted(pods) }))
}

// recordPodResponse blames only the pod a request was routed to. A request that has no pod yet has
// none to blame, and asking its context for one would wait for a pod that is never set.
func Test_recordPodResponse_NeedsARoutedPod(t *testing.T) {
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(1)

	notRouted := &processState{
		ctx:         context.Background(),
		routerCtx:   types.NewRoutingContext(context.Background(), "random", "test-model", "", "req-unrouted", "test-user"),
		isRespError: true, respErrorCode: 503,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.recordPodResponse(notRouted)
		server.recordPodResponse(&processState{isRespError: true, respErrorCode: 503}) // no routing context at all
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recordPodResponse waited for a pod that was never set")
	}
}

// The probe sweep's ignored pods (a StormService's, prefill, decode and encode pods, ModelClaim
// pods) are never blamed, whatever their requests return: the PD router handles failures for
// them per role, and a ModelClaim pod runs one engine per model on one pod.
func Test_UpstreamFailuresDoNotBlacklistIgnoredPods(t *testing.T) {
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(1)

	storm := blacklistTestPod("1")
	storm.Labels = map[string]string{ctrlconstants.StormServiceNameLabelKey: "svc"}
	decode := blacklistTestPod("2")
	decode.Labels = map[string]string{pd.RoleLabel: pd.RoleDecode}
	claim := blacklistTestPod("3")
	claim.Annotations = map[string]string{constants.ModelClaimPodAnnotationPrefix + "claim-a": `{"model":"test-model","port":9001,"state":"active"}`}
	plain := blacklistTestPod("4")

	for i, pod := range []*v1.Pod{storm, decode, claim, plain} {
		respondWith(t, server, pod, "503", fmt.Sprintf("req-%d", i))
	}
	assert.False(t, server.podBlacklist.IsBlacklisted(storm))
	assert.False(t, server.podBlacklist.IsBlacklisted(decode))
	assert.False(t, server.podBlacklist.IsBlacklisted(claim))
	assert.True(t, server.podBlacklist.IsBlacklisted(plain), "the plain pod, same threshold, is blocked")
}

// The request-failure source is opt-in: a Server whose blacklist was never given a threshold, in a
// process without AIBRIX_POD_BLACKLIST_REQUEST_FAILURE_THRESHOLD, never blocks a pod for what its
// requests return. A threshold of 0 turns it off without touching the other sources, and the
// master switch turns every source off.
func Test_UpstreamFailureSourceIsOptIn(t *testing.T) {
	if _, set := os.LookupEnv(blacklist.EnvPodBlacklistRequestFailureThreshold); set {
		t.Skipf("%s is set in this environment", blacklist.EnvPodBlacklistRequestFailureThreshold)
	}
	untouched := recvErrorServer() // no threshold set: the shipped default
	off := recvErrorServer()
	off.podBlacklist.SetRequestFailureThreshold(0)
	pod := blacklistTestPod("1")

	for i := 0; i < 20; i++ {
		respondWith(t, untouched, pod, "503", fmt.Sprintf("default-%d", i))
		respondWith(t, off, pod, "503", fmt.Sprintf("off-%d", i))
	}
	assert.False(t, untouched.podBlacklist.IsBlacklisted(pod), "enabling the blacklist does not enable request ejection")
	assert.False(t, off.podBlacklist.IsBlacklisted(pod))
	tripBreaker(&off.podBlacklist, pod)
	assert.True(t, off.podBlacklist.IsBlacklisted(pod), "the probe source still works with the request source off")

	on := recvErrorServer()
	on.podBlacklist.SetRequestFailureThreshold(1)
	prev := blacklist.Enabled
	blacklist.Enabled = false
	defer func() { blacklist.Enabled = prev }()
	other := blacklistTestPod("2")
	respondWith(t, on, other, "503", "disabled")
	assert.False(t, on.podBlacklist.IsBlacklisted(other), "the master switch is off")
}

// Test_selectTargetPod_ExcludesBlacklistedPod verifies selectTargetPod drops a
// blacklisted pod from the candidate set handed to the router, but still
// routes normally among the remaining pods.
func Test_selectTargetPod_ExcludesBlacklistedPod(t *testing.T) {
	routing.Init()

	blacklisted := blacklistTestPod("blacklisted")
	healthy1 := blacklistTestPod("healthy1")
	healthy2 := blacklistTestPod("healthy2")

	server := &Server{}
	tripBreaker(&server.podBlacklist, blacklisted)
	require.True(t, server.podBlacklist.IsBlacklisted(blacklisted))

	mockR := new(mockRouter)
	algo := types.RoutingAlgorithm("test-blacklist-router")
	routing.Register(algo, func() (types.Router, error) {
		return mockR, nil
	})
	mockR.On("Route", mock.Anything, mock.MatchedBy(func(pods types.PodList) bool {
		all := pods.All()
		if len(all) != 2 {
			return false
		}
		for _, p := range all {
			if p.Name == "blacklisted" {
				return false
			}
		}
		return true
	})).Return("10.0.0.healthy1:8000", nil).Once()
	routing.Init()

	routeCtx := types.NewRoutingContext(context.Background(), algo, "test-model", "test-message", "test-request", "test-user")
	podIP, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: []*v1.Pod{blacklisted, healthy1, healthy2}}, "")

	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.healthy1:8000", podIP)
	mockR.AssertExpectations(t)
}

// A pod blocked by its own responses is kept out of routing like one blocked by a sweep: the
// router is never handed it while healthy pods remain.
func Test_selectTargetPod_ExcludesAPodBlockedByUpstreamFailures(t *testing.T) {
	failing, healthy1, healthy2 := blacklistTestPod("failing"), blacklistTestPod("healthy1"), blacklistTestPod("healthy2")
	server := recvErrorServer()
	server.podBlacklist.SetRequestFailureThreshold(3)
	for i := 0; i < 3; i++ {
		respondWith(t, server, failing, "503", fmt.Sprintf("fail-%d", i))
	}
	require.True(t, server.podBlacklist.IsBlacklisted(failing))

	routed := routedPodNames(t, server, []*v1.Pod{failing, healthy1, healthy2}, map[string]int64{})
	assert.Equal(t, []string{"healthy1", "healthy2"}, routed)
}

// routedPodNames returns the names of the pods selectTargetPod hands the router
// for a request over pods, in sorted order. running is each pod's current
// running-request count, the number the load-imbalance gate reads.
func routedPodNames(t *testing.T, server *Server, pods []*v1.Pod, running map[string]int64) []string {
	t.Helper()
	mockCache := &MockCache{Cache: cache.NewForTest()}
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(running, nil)
	server.cache = mockCache

	var routed []string
	mockR := new(mockRouter)
	algo := types.RoutingAlgorithm("test-blacklist-gate-router")
	routing.Register(algo, func() (types.Router, error) { return mockR, nil })
	mockR.On("Route", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		for _, p := range args.Get(1).(types.PodList).All() {
			routed = append(routed, p.Name)
		}
	}).Return("10.0.0.any:8000", nil).Once()
	routing.Init()

	routeCtx := types.NewRoutingContext(context.Background(), algo, "test-model", "test-message", "test-request", "test-user")
	_, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	sort.Strings(routed)
	return routed
}

// Test_selectTargetPod_LoadImbalanceGateRunsWithBlacklistOn verifies that turning the
// blacklist on does not switch the load-imbalance gate off. The gate is a separate,
// per-request safeguard on live running counts; the blacklist only removes the pods it
// excludes before the gate sees the rest. p1 is busy and the other pods are idle. Four
// pods, so that two or more survive both filters and the router is still called: a lone
// survivor is returned without consulting the router, and the test could not see it.
func Test_selectTargetPod_LoadImbalanceGateRunsWithBlacklistOn(t *testing.T) {
	require.True(t, blacklist.Enabled, "the package's TestMain turns the blacklist on")
	p0, p1, p2, p3 := blacklistTestPod("p0"), blacklistTestPod("p1"), blacklistTestPod("p2"), blacklistTestPod("p3")
	pods := []*v1.Pod{p0, p1, p2, p3}
	running := map[string]int64{"ns/p0": 0, "ns/p1": 30, "ns/p2": 0, "ns/p3": 0}

	t.Run("nothing excluded: the gate still drops the busy pod", func(t *testing.T) {
		server := &Server{}
		require.False(t, server.podBlacklist.IsBlacklisted(p0))

		assert.Equal(t, []string{"p0", "p2", "p3"}, routedPodNames(t, server, pods, running),
			"with no exclusion the blacklist changes nothing, so the gate narrows exactly as it does with the blacklist off")
	})

	t.Run("one pod excluded: the gate narrows what is left", func(t *testing.T) {
		server := &Server{}
		require.True(t, server.podBlacklist.SetLoadExcluded(p0, true))
		require.True(t, server.podBlacklist.IsBlacklisted(p0))

		assert.Equal(t, []string{"p2", "p3"}, routedPodNames(t, server, pods, running),
			"the blacklist drops p0, then the gate drops the busy p1 from the remaining three")
	})
}

// Test_selectTargetPod_AllBlacklistedFallsBack verifies that when every ready
// pod is currently blacklisted, selectTargetPod still routes (falls back to
// the unfiltered set) instead of failing every request outright.
func Test_selectTargetPod_AllBlacklistedFallsBack(t *testing.T) {
	routing.Init()

	pod := blacklistTestPod("only")

	server := &Server{}
	tripBreaker(&server.podBlacklist, pod)
	assert.True(t, server.podBlacklist.IsBlacklisted(pod))

	mockR := new(mockRouter)
	algo := types.RoutingAlgorithm("test-blacklist-fallback-router")
	routing.Register(algo, func() (types.Router, error) {
		return mockR, nil
	})
	mockR.On("Route", mock.Anything, mock.Anything).Return("10.0.0.only:8000", nil).Once()
	routing.Init()

	routeCtx := types.NewRoutingContext(context.Background(), algo, "test-model", "test-message", "test-request", "test-user")
	podIP, err := server.selectTargetPod(context.Background(), routeCtx, &utils.PodArray{Pods: []*v1.Pod{pod}}, "")

	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.only:8000", podIP)
}

// quarantineRegistryCache is a cache that accepts a PodQuarantineChecker, as
// *cache.Store does.
type quarantineRegistryCache struct {
	cache.Cache
	checker cache.PodQuarantineChecker
}

func (c *quarantineRegistryCache) SetPodQuarantineChecker(checker cache.PodQuarantineChecker) {
	c.checker = checker
}

func (c *quarantineRegistryCache) ClearPodQuarantineChecker(cache.PodQuarantineChecker) {
	c.checker = nil
}

func TestRegisterPodQuarantine_CacheSeesTheBlacklistsQuarantine(t *testing.T) {
	fake := &quarantineRegistryCache{}
	server := &Server{cache: fake}
	pod := blacklistTestPod("1")

	server.registerPodQuarantine()
	require.NotNil(t, fake.checker, "the blacklist must be registered with a cache that takes a checker")
	assert.False(t, fake.checker.IsQuarantined(pod))

	for i := 0; i < 3; i++ {
		server.podBlacklist.RecordProbeResult(pod, blacklist.ProbeDead, "GET /health failed")
	}
	assert.True(t, fake.checker.IsQuarantined(pod), "the cache must see the pod the sweep found dead")

	other := blacklistTestPod("2")
	tripBreaker(&server.podBlacklist, other)
	require.True(t, server.podBlacklist.IsBlacklisted(other))
	assert.False(t, fake.checker.IsQuarantined(other), "an ordinary trip keeps its metrics")
}

func TestRegisterPodQuarantine_CacheWithoutARegistryIsLeftAlone(t *testing.T) {
	server := &Server{cache: struct{ cache.Cache }{}}

	assert.NotPanics(t, server.registerPodQuarantine)
}

// The server is the cache's PodLifecycleListener: a flaky re-add opens the breaker for a
// short hold.
func TestOnPodFlakyReAdd_ExcludesThePodForAShortHold(t *testing.T) {
	server := &Server{}
	pod := blacklistTestPod("1")
	var _ cache.PodLifecycleListener = server

	server.OnPodFlakyReAdd(pod)

	assert.True(t, server.podBlacklist.IsBlacklisted(pod))
}

// wiringSpyCache records what NewServerWithOptions registers with the cache.
type wiringSpyCache struct {
	*cache.Store
	listeners []cache.PodLifecycleListener
	checker   cache.PodQuarantineChecker
}

func (c *wiringSpyCache) RegisterPodLifecycleListener(l cache.PodLifecycleListener) {
	c.listeners = append(c.listeners, l)
}

func (c *wiringSpyCache) UnregisterPodLifecycleListener(l cache.PodLifecycleListener) {
	kept := c.listeners[:0]
	for _, existing := range c.listeners {
		if existing != l {
			kept = append(kept, existing)
		}
	}
	c.listeners = kept
}

func (c *wiringSpyCache) SetPodQuarantineChecker(checker cache.PodQuarantineChecker) {
	c.checker = checker
}

// ClearPodQuarantineChecker empties the single slot only if checker still holds it,
// as cache.Store does.
func (c *wiringSpyCache) ClearPodQuarantineChecker(checker cache.PodQuarantineChecker) {
	if c.checker == checker {
		c.checker = nil
	}
}

// NewServerWithOptions wires the server into the cache: it registers as the lifecycle
// listener and offers its blacklist as the quarantine checker.
func TestNewServerWithOptions_WiresBlacklistIntoTheCache(t *testing.T) {
	spy := &wiringSpyCache{Store: cache.NewForTest()}

	server := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: spy})
	t.Cleanup(server.Shutdown)

	require.Len(t, spy.listeners, 1)
	assert.Same(t, server, spy.listeners[0], "the server itself is the cache's lifecycle listener")
	require.NotNil(t, spy.checker)

	pod := blacklistTestPod("1")
	assert.False(t, spy.checker.IsQuarantined(pod))
	for i := 0; i < 3; i++ {
		server.podBlacklist.RecordProbeResult(pod, blacklist.ProbeDead, "GET /health failed")
	}
	assert.True(t, spy.checker.IsQuarantined(pod), "the checker is the server's own blacklist")
}

// With AIBRIX_POD_BLACKLIST_ENABLED off (production default), NewServerWithOptions
// must not start sweeps or register with the cache, so a healthy fleet matches
// pre-blacklist behaviour.
func TestNewServerWithOptions_SkipsBlacklistWiringWhenDisabled(t *testing.T) {
	prev := blacklist.Enabled
	blacklist.Enabled = false
	t.Cleanup(func() { blacklist.Enabled = prev })

	spy := &wiringSpyCache{Store: cache.NewForTest()}
	server := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: spy})
	t.Cleanup(server.Shutdown)

	assert.Empty(t, spy.listeners, "flaky re-add listener is not registered while disabled")
	assert.Nil(t, spy.checker, "quarantine checker is not registered while disabled")

	pod := blacklistTestPod("1")
	for i := 0; i < 10; i++ {
		server.podBlacklist.RecordProbeResult(pod, blacklist.ProbeDead, "GET /health failed")
	}
	assert.False(t, server.podBlacklist.SetLoadExcluded(pod, true))
	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	assert.Equal(t, []*v1.Pod{pod}, server.podBlacklist.FilterBlacklisted([]*v1.Pod{pod}))
}

// A server that shuts down takes itself out of the cache again, so a cache that outlives it
// neither keeps it alive nor keeps asking its blacklist about pods.
func TestShutdown_UnregistersFromTheCache(t *testing.T) {
	spy := &wiringSpyCache{Store: cache.NewForTest()}
	server := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: spy})
	require.Len(t, spy.listeners, 1)
	require.NotNil(t, spy.checker)

	server.Shutdown()
	assert.Empty(t, spy.listeners, "the server is no longer a lifecycle listener")
	assert.Nil(t, spy.checker, "the server's blacklist is no longer the quarantine checker")

	server.Shutdown() // a second call is harmless
}

// The quarantine slot is shared and the last server to set it wins. An earlier server shutting
// down afterwards must not remove the later one's registration.
func TestShutdown_LeavesALaterServersRegistrationAlone(t *testing.T) {
	spy := &wiringSpyCache{Store: cache.NewForTest()}
	first := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: spy})
	second := NewServerWithOptions(nil, nil, nil, ServerOptions{Cache: spy})
	t.Cleanup(second.Shutdown)
	require.Len(t, spy.listeners, 2)
	require.Same(t, &second.podBlacklist, spy.checker)

	first.Shutdown()

	require.Len(t, spy.listeners, 1)
	assert.Same(t, second, spy.listeners[0])
	assert.Same(t, &second.podBlacklist, spy.checker, "the first server cleared only what was its own")
}

// A cache that offers neither registry (a stub or a mock) is simply not registered with.
func TestShutdown_CacheWithoutRegistriesIsFine(t *testing.T) {
	server := &Server{cache: new(MockCache), shutdown: make(chan struct{})}

	assert.NotPanics(t, server.Shutdown)
}
