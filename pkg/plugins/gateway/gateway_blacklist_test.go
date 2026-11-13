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
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
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

// alwaysUnhealthy/alwaysHealthy stub blacklist.PodBlacklist.HealthCheck so
// tests never make a real network call when the breaker confirms a trip.
func alwaysUnhealthy(string) bool { return false }
func alwaysHealthy(string) bool   { return true }

// tripBreaker reports blacklist.FailureThreshold failures for pod, the way
// HandleResponseHeaders does.
func tripBreaker(b *blacklist.PodBlacklist, pod *v1.Pod) {
	for i := 0; i < blacklist.FailureThreshold; i++ {
		b.RecordResult(pod, pod.Status.PodIP+":8000", 400)
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
	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy}}
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
	assert.Equal(t, 10.0, server.podEngineQueueDepth(pod, "test-model"),
		"should read the pod+model-scoped metric, not silently contribute 0")

	// The exact miss that would make podEngineQueueDepth always read 0: the
	// pod-only accessor genuinely does not have this metric.
	_, err := store.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.NumRequestsRunning)
	assert.Error(t, err, "num_requests_running is pod+model scoped and must not be visible via the pod-only accessor")
}

// A missing metric reads as 0: the load sweep only ever excludes a pod for being
// too high, never for being unmeasured.
func TestPodEngineQueueDepth_MissingMetricReadsAsZero(t *testing.T) {
	pod := blacklistTestPod("1")
	mc := new(MockCache)
	mc.On("GetMetricValueByPodModel", "1", "ns", "test-model", mock.Anything).
		Return(&metrics.SimpleMetricValue{}, fmt.Errorf("metric not found"))
	server := &Server{cache: mc}

	assert.Equal(t, 0.0, server.podEngineQueueDepth(pod, "test-model"))
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

// Test_HandleResponseHeaders_TripsPodBlacklist verifies HandleResponseHeaders
// feeds each request's upstream status code (and target address) into the
// server's pod blacklist, and that blacklist.FailureThreshold 400s from the same
// pod, confirmed unhealthy, trip it.
func Test_HandleResponseHeaders_TripsPodBlacklist(t *testing.T) {
	routing.Init()
	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy}}
	pod := blacklistTestPod("1")

	for i := 0; i < blacklist.FailureThreshold-1; i++ {
		routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", fmt.Sprintf("req-%d", i), "test-user")
		routerCtx.SetTargetPod(pod)
		server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, "test-model", handleResponseHeadersStatusReq("400"))
		assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "req-final", "test-user")
	routerCtx.SetTargetPod(pod)
	server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, "test-model", handleResponseHeadersStatusReq("400"))
	assert.True(t, server.podBlacklist.IsBlacklisted(pod))
}

// A failing streak on a pod that confirmation finds healthy is a false alarm (a
// client-side 400, say), so the pod stays in rotation.
func Test_HandleResponseHeaders_FalseAlarmWhenPodConfirmedHealthy(t *testing.T) {
	routing.Init()
	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysHealthy}}
	pod := blacklistTestPod("1")

	for i := 0; i < blacklist.FailureThreshold+2; i++ {
		routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", fmt.Sprintf("req-%d", i), "test-user")
		routerCtx.SetTargetPod(pod)
		server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, "test-model", handleResponseHeadersStatusReq("400"))
	}

	assert.False(t, server.podBlacklist.IsBlacklisted(pod), "a pod whose health check passes is not blacklisted by a streak of failures")
}

// A request that was never routed to a pod has nobody to blame.
func Test_HandleResponseHeaders_UnroutedRequestRecordsNothing(t *testing.T) {
	routing.Init()
	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy}}
	pod := blacklistTestPod("1")

	for i := 0; i < blacklist.FailureThreshold+1; i++ {
		routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", fmt.Sprintf("req-%d", i), "test-user")
		server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, "test-model", handleResponseHeadersStatusReq("500"))
	}

	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
	assert.Zero(t, server.podBlacklist.FailureCount(pod))
}

// engineModelsServer serves GET /v1/models with the given OpenAI-style body
// and 200 on every other path (including /health), so a 404 confirmation
// that still consulted /health or the routing cache would not trip.
func engineModelsServer(t *testing.T, body string) (string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	ip, portStr, err := net.SplitHostPort(host)
	assert.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	assert.NoError(t, err)
	return ip, port
}

func send404Streak(t *testing.T, server *Server, pod *v1.Pod, port int, model string) {
	t.Helper()
	for i := 0; i < blacklist.FailureThreshold-1; i++ {
		routerCtx := types.NewRoutingContext(context.Background(), "random", model, "", fmt.Sprintf("req-%d", i), "test-user")
		routerCtx.SetTargetPod(pod)
		routerCtx.SetTargetPort(port)
		server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, model, handleResponseHeadersStatusReq("404"))
		assert.False(t, server.podBlacklist.IsBlacklisted(pod), "should not blacklist before threshold (i=%d)", i)
	}
	routerCtx := types.NewRoutingContext(context.Background(), "random", model, "", "req-final", "test-user")
	routerCtx.SetTargetPod(pod)
	routerCtx.SetTargetPort(port)
	server.HandleResponseHeaders(context.Background(), routerCtx, routerCtx.RequestID, model, handleResponseHeadersStatusReq("404"))
}

// Test_HandleResponseHeaders_404_TripsWhenEngineNoLongerListsModel is the
// production path for 404 confirmation: HandleResponseHeaders always passes
// the request model, and the zero-value blacklist confirms against GET
// /v1/models on the routed address -- not the gateway cache that selected
// the pod. /health is 200 on this server, so a leftover cache-membership
// veto (or a /health fallback) would fail to trip.
func Test_HandleResponseHeaders_404_TripsWhenEngineNoLongerListsModel(t *testing.T) {
	routing.Init()
	ip, port := engineModelsServer(t, `{"object":"list","data":[{"id":"other-model"}]}`)
	pod := blacklistTestPod("1")
	pod.Status.PodIP = ip
	server := &Server{}

	send404Streak(t, server, pod, port, "test-model")
	assert.True(t, server.podBlacklist.IsBlacklisted(pod), "a 404 streak must trip once GET /v1/models confirms the engine no longer lists the requested model, even though /health is 200")
}

// Test_HandleResponseHeaders_404_FalseAlarmWhenEngineStillListsModel is the
// converse production path: the engine still lists the model, so consecutive
// 404s are a false alarm and must not blacklist the pod.
func Test_HandleResponseHeaders_404_FalseAlarmWhenEngineStillListsModel(t *testing.T) {
	routing.Init()
	ip, port := engineModelsServer(t, `{"object":"list","data":[{"id":"test-model"}]}`)
	pod := blacklistTestPod("1")
	pod.Status.PodIP = ip
	server := &Server{}

	send404Streak(t, server, pod, port, "test-model")
	assert.False(t, server.podBlacklist.IsBlacklisted(pod), "a 404 streak must not trip when GET /v1/models still lists the requested model")
}

// recvErrorServer is a Server wired for handleRecvError tests: a breaker that
// confirms every trip as unhealthy, and a cache that accepts the request-count
// release every exit path performs.
func recvErrorServer() *Server {
	mc := &MockCache{}
	mc.On("DoneRequestCount", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	return &Server{
		podBlacklist:        blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy},
		cache:               mc,
		requestCountTracker: map[string]int{},
	}
}

// routedState is the processState of a request already routed to pod.
func routedState(ctx context.Context, pod *v1.Pod, reqID string, headersSeen bool) *processState {
	routerCtx := types.NewRoutingContext(ctx, "random", "test-model", "", reqID, "test-user")
	routerCtx.SetTargetPod(pod)
	return &processState{ctx: ctx, requestID: reqID, model: "test-model", routerCtx: routerCtx, respHeadersSeen: headersSeen}
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

// Test_handleRecvError_StreamClosedBeforeHeaders_TripsPodBlacklist verifies that an
// ext_proc stream that closes with no ResponseHeaders ever seen (a stale
// registration or an unreachable pod) feeds the pod blacklist the same way a
// tracked HTTP status would, and that blacklist.FailureThreshold occurrences,
// confirmed unhealthy, trip it.
func Test_handleRecvError_StreamClosedBeforeHeaders_TripsPodBlacklist(t *testing.T) {
	for _, tc := range recvFailures {
		t.Run(tc.name, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			for i := 0; i < blacklist.FailureThreshold-1; i++ {
				tc.wantErr(t, server.handleRecvError(routedState(context.Background(), pod, fmt.Sprintf("req-%d", i), false), tc.recvErr))
				assert.False(t, server.podBlacklist.IsBlacklisted(pod))
			}

			tc.wantErr(t, server.handleRecvError(routedState(context.Background(), pod, "req-final", false), tc.recvErr))
			assert.True(t, server.podBlacklist.IsBlacklisted(pod), "a pod that never returns a response at all should trip the breaker just like consecutive 400/404s")
		})
	}
}

// Test_handleRecvError_ClosedAfterHeaders_RecordsIncompleteStream verifies that the
// stream closing after ResponseHeaders, with the client still connected, is an
// upstream reset: it records blacklist.StatusIncompleteStream (distinct from the
// 200 already recorded at header time) rather than being ignored.
func Test_handleRecvError_ClosedAfterHeaders_RecordsIncompleteStream(t *testing.T) {
	for _, tc := range recvFailures {
		t.Run(tc.name, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			for i := 0; i < blacklist.FailureThreshold-1; i++ {
				tc.wantErr(t, server.handleRecvError(routedState(context.Background(), pod, fmt.Sprintf("req-%d", i), true), tc.recvErr))
				assert.False(t, server.podBlacklist.IsBlacklisted(pod))
			}

			tc.wantErr(t, server.handleRecvError(routedState(context.Background(), pod, "req-final", true), tc.recvErr))
			assert.True(t, server.podBlacklist.IsBlacklisted(pod), "a close after 200 headers must accumulate as incomplete-stream failures")
		})
	}
}

// A client that left after the headers arrived is not the pod's fault, and the
// headers already recorded the status.
func Test_handleRecvError_ClientAbortAfterHeaders_DoesNotBlacklist(t *testing.T) {
	for _, tc := range recvFailures {
		t.Run(tc.name, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for i := 0; i < blacklist.FailureThreshold+2; i++ {
				tc.wantErr(t, server.handleRecvError(routedState(ctx, pod, fmt.Sprintf("req-%d", i), true), tc.recvErr))
			}

			assert.False(t, server.podBlacklist.IsBlacklisted(pod), "client abort after ResponseHeaders must not incomplete-blacklist the pod")
			assert.Zero(t, server.podBlacklist.FailureCount(pod))
		})
	}
}

// A client that gave up before any response headers arrived (a timeout while the pod is
// still queuing or prefilling) says nothing about the pod: counting it would blacklist
// a busy pod for the very load that made the clients time out.
func Test_handleRecvError_ClientAbortBeforeHeaders_DoesNotBlacklist(t *testing.T) {
	for _, tc := range recvFailures {
		t.Run(tc.name, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			for i := 0; i < blacklist.FailureThreshold+2; i++ {
				tc.wantErr(t, server.handleRecvError(routedState(ctx, pod, fmt.Sprintf("req-%d", i), false), tc.recvErr))
			}

			assert.False(t, server.podBlacklist.IsBlacklisted(pod), "client abort before ResponseHeaders must not blacklist the pod")
			assert.Zero(t, server.podBlacklist.FailureCount(pod))
		})
	}
}

// A success whose response headers end the message (a 204, an empty-body 200) has no body
// callback to mark it complete. It must complete on the headers, or the stream closing
// afterwards is recorded as an upstream hang-up against a pod that answered correctly.
func Test_HeadersOnlySuccess_IsCompleteAndDoesNotBlacklist(t *testing.T) {
	for _, statusCode := range []string{"200", "204"} {
		t.Run(statusCode, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			for i := 0; i < blacklist.FailureThreshold+2; i++ {
				st := routedState(context.Background(), pod, fmt.Sprintf("req-%d", i), false)
				req := handleResponseHeadersStatusReq(statusCode)
				req.GetResponseHeaders().EndOfStream = true

				_, err := server.handleProcessingRequest(st, req)
				require.NoError(t, err)
				assert.True(t, st.completed, "a headers-only success is complete: no body callback will follow")

				_ = server.handleRecvError(st, io.EOF)
			}

			assert.False(t, server.podBlacklist.IsBlacklisted(pod))
			assert.Zero(t, server.podBlacklist.FailureCount(pod))
		})
	}
}

// With a body still to come, the response headers alone do not complete the request.
func Test_ResponseHeaders_WithBodyToFollowIsNotComplete(t *testing.T) {
	pod := blacklistTestPod("1")
	server := recvErrorServer()
	st := routedState(context.Background(), pod, "req-body", false)

	_, err := server.handleProcessingRequest(st, handleResponseHeadersStatusReq("200"))

	require.NoError(t, err)
	assert.False(t, st.completed)
}

// A request that already completed ended normally; the stream closing afterwards says
// nothing about the pod.
func Test_handleRecvError_AfterCompletion_DoesNotBlacklist(t *testing.T) {
	for _, tc := range recvFailures {
		t.Run(tc.name, func(t *testing.T) {
			pod := blacklistTestPod("1")
			server := recvErrorServer()

			for i := 0; i < blacklist.FailureThreshold+2; i++ {
				st := routedState(context.Background(), pod, fmt.Sprintf("req-%d", i), true)
				st.completed = true
				_ = server.handleRecvError(st, tc.recvErr)
			}

			assert.False(t, server.podBlacklist.IsBlacklisted(pod))
			assert.Zero(t, server.podBlacklist.FailureCount(pod))
		})
	}
}

// A stream that was never routed (rejected before a pod was picked) has no pod to blame.
func Test_handleRecvError_UnroutedRequest_DoesNotBlacklist(t *testing.T) {
	pod := blacklistTestPod("1")
	server := recvErrorServer()

	for i := 0; i < blacklist.FailureThreshold+2; i++ {
		routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", fmt.Sprintf("req-%d", i), "test-user")
		st := &processState{ctx: context.Background(), requestID: routerCtx.RequestID, model: "test-model", routerCtx: routerCtx}
		assert.Equal(t, io.EOF, server.handleRecvError(st, io.EOF))
	}

	assert.False(t, server.podBlacklist.IsBlacklisted(pod))
}

// Test_selectTargetPod_ExcludesBlacklistedPod verifies selectTargetPod drops a
// blacklisted pod from the candidate set handed to the router, but still
// routes normally among the remaining pods.
func Test_selectTargetPod_ExcludesBlacklistedPod(t *testing.T) {
	routing.Init()

	blacklisted := blacklistTestPod("blacklisted")
	healthy1 := blacklistTestPod("healthy1")
	healthy2 := blacklistTestPod("healthy2")

	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy}}
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

// Test_selectTargetPod_AllBlacklistedFallsBack verifies that when every ready
// pod is currently blacklisted, selectTargetPod still routes (falls back to
// the unfiltered set) instead of failing every request outright.
func Test_selectTargetPod_AllBlacklistedFallsBack(t *testing.T) {
	routing.Init()

	pod := blacklistTestPod("only")

	server := &Server{podBlacklist: blacklist.PodBlacklist{HealthCheck: alwaysUnhealthy}}
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
	server.podBlacklist.HealthCheck = alwaysUnhealthy
	tripBreaker(&server.podBlacklist, other)
	require.True(t, server.podBlacklist.IsBlacklisted(other))
	assert.False(t, fake.checker.IsQuarantined(other), "an ordinary trip keeps its metrics")
}

func TestRegisterPodQuarantine_CacheWithoutARegistryIsLeftAlone(t *testing.T) {
	server := &Server{cache: struct{ cache.Cache }{}}

	assert.NotPanics(t, server.registerPodQuarantine)
}

// The server is the cache's PodLifecycleListener: a flaky re-add opens the breaker so
// the first request after it is the half-open probe.
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

// The half-open probe belongs to the request routed to the pod, not to every
// candidate the filter admits. Two pods are recovering (their backoff has
// elapsed) next to a healthy one. A request the router sends to the healthy pod
// must leave both recovering pods available to the next request, and a request
// the router sends to a recovering pod must make it, and only it, that pod's
// probe.
func TestHandleRequestBody_ClaimsTheHalfOpenProbeOnlyForThePickedPod(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	router := new(mockRouter)
	registerTestRouter(router)

	recoveringA := readyPod("recovering-a", "ns", "1.2.3.1")
	recoveringB := readyPod("recovering-b", "ns", "1.2.3.2")
	healthy := readyPod("healthy", "ns", "1.2.3.3")
	pods := []*v1.Pod{recoveringA, recoveringB, healthy}

	server := &Server{cache: mockCache}
	// A flaky re-add opens the breaker for a few seconds without confirming anything.
	server.podBlacklist.OnPodFlakyReAdd(recoveringA)
	server.podBlacklist.OnPodFlakyReAdd(recoveringB)
	require.True(t, server.podBlacklist.IsBlacklisted(recoveringA))
	// Allow only reports eligibility, so waiting on it does not spend the probe.
	require.Eventually(t, func() bool {
		return server.podBlacklist.Allow(recoveringA) && server.podBlacklist.Allow(recoveringB)
	}, 10*time.Second, 50*time.Millisecond, "the backoffs should elapse")

	mockCache.On("HasModel", "test-model").Return(true)
	mockCache.On("ListPodsByModel", "test-model").Return(&utils.PodArray{Pods: pods}, nil)
	mockCache.On("GetMetricValueByPod", mock.Anything, mock.Anything, mock.Anything).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Maybe()
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1))

	var picked *v1.Pod
	var candidates []*v1.Pod
	router.On("Route", mock.Anything, mock.Anything).Return("1.2.3.4:8000", nil).
		Run(func(args mock.Arguments) {
			candidates = args.Get(1).(types.PodList).All()
			args.Get(0).(*types.RoutingContext).SetTargetPod(picked)
		})

	send := func(requestID string) {
		t.Helper()
		req := &extProcPb.ProcessingRequest{
			Request: &extProcPb.ProcessingRequest_RequestBody{
				RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","prompt":"hi"}`)},
			},
		}
		routingCtx := types.NewRoutingContext(context.Background(), "", "", "", requestID, "test-user")
		routingCtx.ReqPath = PathCompletions
		routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
		resp, _, _, _ := server.HandleRequestBody(context.Background(), routingCtx, requestID, req, utils.User{Name: "test-user"})
		require.NotNil(t, resp)
		require.Nil(t, resp.GetImmediateResponse(), "the request must be routed")
	}

	picked = healthy
	send("passes-over-the-recovering-pods")
	assert.ElementsMatch(t, pods, candidates, "both recovering pods were candidates")
	assert.True(t, server.podBlacklist.Allow(recoveringA), "a pod the router passed over still has its probe to give")
	assert.True(t, server.podBlacklist.Allow(recoveringB), "a pod the router passed over still has its probe to give")

	picked = recoveringA
	send("probes-recovering-a")
	assert.False(t, server.podBlacklist.Allow(recoveringA), "the request routed to it is its half-open probe")
	assert.True(t, server.podBlacklist.Allow(recoveringB), "claiming one pod's probe leaves the other's alone")

	// The probe reports back like any request, and a success closes the breaker.
	server.podBlacklist.RecordResult(recoveringA, "1.2.3.1:8000", 200)
	assert.True(t, server.podBlacklist.Allow(recoveringA))
	assert.False(t, server.podBlacklist.IsBlacklisted(recoveringA))
}
