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
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/metrics"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
)

type requestEvent struct {
	Kind, RequestID, Model string
	TraceTerm              int64
}

// fakeCache is stateful because Gateway routing and lifecycle assertions read
// metrics/events at different points in one request. Each fixture owns a fresh
// instance so request counts, trace terms, and terminal events cannot leak
// between cases.
type fakeCache struct {
	mu             sync.Mutex
	models         map[string]bool
	podsByModel    map[string][]*corev1.Pod
	events         []requestEvent
	nextTraceTerm  int64
	metricValues   map[string]metrics.MetricValue
	metricReads    map[string]int
	inFlightEvents []int
}

var _ cache.Cache = (*fakeCache)(nil)
var _ extProcPb.ExternalProcessor_ProcessServer = (*fakeProcessStream)(nil)

func newFakeCache(pods []*corev1.Pod) *fakeCache {
	values := map[string]metrics.MetricValue{}
	for i, pod := range pods {
		base := 10.0
		if i == 0 {
			base = 1
		}
		values[pod.Name+"/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: base}
		values[pod.Name+"/"+metrics.KVCacheUsagePerc] = &metrics.SimpleMetricValue{Value: base}
		values[pod.Name+"/"+metrics.RealtimeRunningRequestsDrainRate1m] = &metrics.SimpleMetricValue{Value: 1 / base}
		values[pod.Name+"/"+metrics.RequestQueueTimeSeconds] = &metrics.SimpleMetricValue{Value: base}
		values[pod.Name+"/"+metrics.AvgPromptToksPerReq] = &metrics.SimpleMetricValue{Value: 10}
		values[pod.Name+"/"+metrics.AvgGenerationToksPerReq] = &metrics.SimpleMetricValue{Value: 10}
		values[pod.Name+"/"+metrics.RequestPrefillTimeSeconds] = &metrics.HistogramMetricValue{Sum: base, Count: 1}
		values[pod.Name+"/"+metrics.RequestDecodeTimeSeconds] = &metrics.HistogramMetricValue{Sum: base, Count: 1}
	}
	return &fakeCache{
		models:        map[string]bool{"llama2-7b": true},
		podsByModel:   map[string][]*corev1.Pod{"llama2-7b": append([]*corev1.Pod(nil), pods...)},
		nextTraceTerm: 1,
		metricValues:  values,
		metricReads:   make(map[string]int),
	}
}
func (c *fakeCache) HasModel(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models[model]
}
func (c *fakeCache) ListModels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.models))
	for m := range c.models {
		out = append(out, m)
	}
	return out
}
func (c *fakeCache) ListModelsByPod(_, _ string) ([]string, error) { return []string{}, nil }
func (c *fakeCache) ListPodsByModel(model string) (types.PodList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &podList{pods: append([]*corev1.Pod(nil), c.podsByModel[model]...)}, nil
}
func (c *fakeCache) GetPod(podName, podNamespace string) (*corev1.Pod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pods := range c.podsByModel {
		for _, pod := range pods {
			if pod.Namespace == podNamespace && pod.Name == podName {
				return pod.DeepCopy(), nil
			}
		}
	}
	return nil, fmt.Errorf("pod %s/%s not found", podNamespace, podName)
}
func (c *fakeCache) ModelBaseModel(string) (string, bool) { return "", false }
func (c *fakeCache) GetMetricValueByPod(pod, _, metric string) (metrics.MetricValue, error) {
	return c.metricValue(pod, metric), nil
}
func (c *fakeCache) GetMetricValueByPodModel(pod, _, _, metric string) (metrics.MetricValue, error) {
	return c.metricValue(pod, metric), nil
}
func (c *fakeCache) metricValue(pod, metric string) metrics.MetricValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metricReads[pod+"/"+metric]++
	if v, ok := c.metricValues[pod+"/"+metric]; ok {
		return v
	}
	return &metrics.SimpleMetricValue{}
}

func (c *fakeCache) metricReadCount(metric string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for key, reads := range c.metricReads {
		if strings.HasSuffix(key, "/"+metric) {
			count += reads
		}
	}
	return count
}
func (c *fakeCache) AddSubscriber(metrics.MetricSubscriber)      {}
func (c *fakeCache) RegisterRequestTracker(cache.RequestTracker) {}
func (c *fakeCache) GetModelProfileByPod(*corev1.Pod, string) (*cache.ModelGPUProfile, error) {
	return nil, nil
}
func (c *fakeCache) GetModelProfileByDeploymentName(string, string) (*cache.ModelGPUProfile, error) {
	return nil, nil
}
func (c *fakeCache) GetOutputPredictor(string) (types.OutputPredictor, error) {
	return fakeOutputPredictor{}, nil
}
func (c *fakeCache) GetRouter(*types.RoutingContext) (types.Router, error) { return nil, nil }
func (c *fakeCache) AddRequestCount(_ *types.RoutingContext, requestID, model string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	term := c.nextTraceTerm
	c.nextTraceTerm++
	c.events = append(c.events, requestEvent{"add", requestID, model, term})
	return term
}
func (c *fakeCache) DoneRequestCount(_ *types.RoutingContext, requestID, model string, term int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, requestEvent{"done", requestID, model, term})
}
func (c *fakeCache) DoneRequestTrace(_ *types.RoutingContext, requestID, model string, _, _, term int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, requestEvent{"done-trace", requestID, model, term})
}
func (c *fakeCache) eventsSnapshot() []requestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]requestEvent(nil), c.events...)
}
func (c *fakeCache) finalizationCount(requestID string) int {
	n := 0
	for _, e := range c.eventsSnapshot() {
		if e.RequestID == requestID && (e.Kind == "done" || e.Kind == "done-trace") {
			n++
		}
	}
	return n
}
func (c *fakeCache) inFlightSnapshot() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.inFlightEvents...)
}

func (c *fakeCache) inFlightValue() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := 0
	for _, delta := range c.inFlightEvents {
		value += delta
	}
	return value
}

type podList struct{ pods []*corev1.Pod }

func (p *podList) Len() int             { return len(p.pods) }
func (p *podList) At(i int) *corev1.Pod { return p.pods[i] }

func (p *podList) All() []*corev1.Pod                { return append([]*corev1.Pod(nil), p.pods...) }
func (p *podList) Indexes() []string                 { return nil }
func (p *podList) ListByIndex(string) []*corev1.Pod  { return nil }
func (p *podList) ListPortsForPod() map[string][]int { return nil }

type fakeOutputPredictor struct{}

func (fakeOutputPredictor) AddTrace(int, int, int32) {}
func (fakeOutputPredictor) Predict(n int) int        { return n }

type fakeProcessStream struct {
	ctx               context.Context
	inputs            []*extProcPb.ProcessingRequest
	outputs           []*extProcPb.ProcessingResponse
	inputPos          int
	mu                sync.Mutex
	blockOnExhaustion bool
	recvStarted       chan struct{}
	recvOnce          sync.Once
}

// fakeProcessStream drives the ordered ext_proc callback sequence consumed by
// Gateway.Process. Response-body callbacks are protocol boundaries only: Envoy
// owns forwarding the upstream body bytes.
func newFakeProcessStream(ctx context.Context, inputs ...*extProcPb.ProcessingRequest) *fakeProcessStream {
	return &fakeProcessStream{ctx: ctx, inputs: inputs}
}
func (s *fakeProcessStream) Recv() (*extProcPb.ProcessingRequest, error) {
	s.mu.Lock()
	if s.inputPos >= len(s.inputs) {
		if s.blockOnExhaustion {
			s.recvOnce.Do(func() {
				if s.recvStarted != nil {
					close(s.recvStarted)
				}
			})
			s.mu.Unlock()
			<-s.ctx.Done()
			if s.ctx.Err() == context.DeadlineExceeded {
				return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
			}
			return nil, status.Error(codes.Canceled, "context canceled")
		}
		s.mu.Unlock()
		return nil, io.EOF
	}
	req := s.inputs[s.inputPos]
	s.inputPos++
	s.mu.Unlock()
	return req, nil
}
func (s *fakeProcessStream) Send(resp *extProcPb.ProcessingResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outputs = append(s.outputs, proto.Clone(resp).(*extProcPb.ProcessingResponse))
	return nil
}
func (s *fakeProcessStream) Context() context.Context { return s.ctx }
func (s *fakeProcessStream) SetHeader(metadata.MD) error {
	return fmt.Errorf("fake ext_proc stream does not support SetHeader")
}
func (s *fakeProcessStream) SendHeader(metadata.MD) error {
	return fmt.Errorf("fake ext_proc stream does not support SendHeader")
}

// SetTrailer is required by grpc.ServerStream but is outside this fake's
// supported Recv/Send/Context surface; Process never calls it.
func (s *fakeProcessStream) SetTrailer(metadata.MD) {}
func (s *fakeProcessStream) SendMsg(any) error {
	return fmt.Errorf("fake ext_proc stream does not support SendMsg")
}
func (s *fakeProcessStream) RecvMsg(any) error {
	return fmt.Errorf("fake ext_proc stream does not support RecvMsg")
}
func (s *fakeProcessStream) responses() []*extProcPb.ProcessingResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*extProcPb.ProcessingResponse(nil), s.outputs...)
}

type gatewayFixture struct {
	server        *gatewayplugin.Server
	cache         *fakeCache
	prefixIndexer *prefixcacheindexer.PrefixHashTable
	stream        *fakeProcessStream
	requestID     string
}

var fixtureSequence atomic.Uint64

func newGatewayFixture(pods []*corev1.Pod) *gatewayFixture {
	return newGatewayFixtureWithRequest(pods, "random", "", "")
}

func newGatewayFixtureWithRequest(pods []*corev1.Pod, strategy, profile, externalFilter string) *gatewayFixture {
	c := newFakeCache(pods)
	requestID := fmt.Sprintf("%032x", fixtureSequence.Add(1))
	prefixIndexer := prefixcacheindexer.NewPrefixHashTable()
	f := &gatewayFixture{cache: c, prefixIndexer: prefixIndexer, requestID: requestID}
	// Both dependencies stay local so cases cannot observe process-global
	// routing/cache registries or shared prefix state.
	routerManager := routingalgorithms.NewRouterManagerWithCacheAndPrefixIndexer(c, prefixIndexer)
	f.server = gatewayplugin.NewServerWithOptions(
		nil, nil, nil,
		gatewayplugin.ServerOptions{
			Cache:         c,
			RouterManager: routerManager,
			InFlightObserver: func(delta int) {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.inFlightEvents = append(c.inFlightEvents, delta)
			},
		},
	)
	inputs := []*extProcPb.ProcessingRequest{
		requestHeadersRequest(requestID, strategy, profile, externalFilter),
		requestBodyRequest(),
	}
	_, valid := routerManager.Validate(strategy)
	if hasReadyPod(pods) && (valid || profile != "") {
		inputs = append(inputs, responseHeadersRequest(), responseBodyRequest())
	}
	f.stream = newFakeProcessStream(context.Background(), inputs...)
	return f
}

func (f *gatewayFixture) run() error { return f.server.Process(f.stream) }
func (f *gatewayFixture) diagnostics() string {
	inputs := make([]string, 0, len(f.stream.inputs))
	for _, input := range f.stream.inputs {
		inputs = append(inputs, fmt.Sprintf("%T", input.Request))
	}
	outputs := make([]string, 0, len(f.stream.responses()))
	for _, output := range f.stream.responses() {
		status := ""
		target := ""
		strategy := ""
		if immediate := output.GetImmediateResponse(); immediate != nil {
			status = fmt.Sprintf("immediate:%d %s", immediate.GetStatus().GetCode(), immediate.GetBody())
		}
		if body := output.GetRequestBody(); body != nil && body.GetResponse() != nil {
			for _, h := range body.GetResponse().GetHeaderMutation().GetSetHeaders() {
				if h.GetHeader().GetKey() == "target-pod" {
					target = string(h.GetHeader().GetRawValue())
				}
				if h.GetHeader().GetKey() == "routing-strategy" {
					strategy = string(h.GetHeader().GetRawValue())
				}
			}
		}
		outputs = append(outputs, fmt.Sprintf(
			"%T status=%s target=%s strategy=%s",
			output.Response, status, target, strategy,
		))
	}
	return fmt.Sprintf(
		"request_id=%s inputs=%v outputs=%v events=%v",
		f.requestID, inputs, outputs, f.cache.eventsSnapshot(),
	)
}
func requestHeadersRequest(requestID, strategy, profile, externalFilter string) *extProcPb.ProcessingRequest {
	headers := []*configPb.HeaderValue{
		{Key: ":method", RawValue: []byte("POST")},
		{Key: ":path", RawValue: []byte("/v1/chat/completions")},
		{Key: "content-type", RawValue: []byte("application/json")},
		{Key: "routing-strategy", RawValue: []byte(strategy)},
		{Key: "traceparent", RawValue: []byte("00-" + requestID + "-00f067aa0ba902b7-01")},
	}
	if profile != "" {
		headers = append(headers, &configPb.HeaderValue{Key: "config-profile", RawValue: []byte(profile)})
	}
	if externalFilter != "" {
		headers = append(headers, &configPb.HeaderValue{Key: "external-filter", RawValue: []byte(externalFilter)})
	}
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: headers},
			},
		},
	}
}
func requestBodyRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model":"llama2-7b","messages":[{"role":"user","content":"hello"}],"stream":false}`),
				EndOfStream: true,
			},
		},
	}
}
func responseHeadersRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{
					Headers: []*configPb.HeaderValue{{
						Key: ":status", RawValue: []byte("200"),
					}},
				},
			},
		},
	}
}
func responseBodyRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body: []byte(
					`{"model":"llama2-7b","choices":[],"usage":` +
						`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				),
				EndOfStream: true,
			},
		},
	}
}

func hasReadyPod(pods []*corev1.Pod) bool {
	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}
