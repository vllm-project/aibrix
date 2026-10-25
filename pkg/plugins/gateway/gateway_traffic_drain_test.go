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
	"io"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/internal/trafficdraintest"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/least"
	"github.com/vllm-project/aibrix/pkg/trafficdrain"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func trafficGatewayFixture(t *testing.T) (*miniredis.Miniredis, *redis.Client, *trafficdrain.Store, *v1.Pod, *Server, *Server) {
	t.Helper()
	m, client, _ := trafficdraintest.NewRedis(t)
	control := trafficdrain.New(client)
	t.Cleanup(func() { _ = control.Close() })
	epoch, err := control.Initialize(context.Background(), "protected-uid")
	require.NoError(t, err)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: "ns", UID: "protected-uid", Labels: map[string]string{constants.ModelLabelName: "test-model"}, Annotations: map[string]string{
		constants.TrafficScaleInMaxWaitAnnotationKey: "30s", constants.TrafficDrainProtocolAnnotationKey: constants.TrafficDrainProtocolVersion, constants.TrafficDrainEpochAnnotationKey: epoch,
	}}, Status: v1.PodStatus{PodIP: "10.0.0.1", Phase: v1.PodRunning, Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}}}
	newGateway := func() *Server {
		local := cache.NewWithPodsForTest([]*v1.Pod{pod.DeepCopy()}, "test-model")
		s := NewServerWithOptions(client, nil, nil, ServerOptions{Cache: local, DisableRateLimiting: true})
		t.Cleanup(s.Shutdown)
		return s
	}
	return m, client, control, pod, newGateway(), newGateway()
}

type trafficScoringCache struct {
	cache.Cache
	counts map[string]int64
}

func (c trafficScoringCache) GetPodsRunningRequests(_ []*v1.Pod) (map[string]int64, error) {
	return c.counts, nil
}
func (c trafficScoringCache) LatestPodReadySince() int64 { return 0 }
func (c trafficScoringCache) GetPodsReadySince(_ []*v1.Pod) (map[string]int64, error) {
	return nil, nil
}

func TestTrafficDrainRetriesWhenCandidateClosesAfterScoring(t *testing.T) {
	_, client, control, pod, _, _ := trafficGatewayFixture(t)
	other := pod.DeepCopy()
	other.Name = "other"
	other.UID = "other-uid"
	other.Status.PodIP = "10.0.0.2"
	epoch, err := control.Initialize(context.Background(), string(other.UID))
	require.NoError(t, err)
	other.Annotations[constants.TrafficDrainEpochAnnotationKey] = epoch
	local := cache.NewWithPodsForTest([]*v1.Pod{pod, other}, "test-model")
	scores := trafficScoringCache{Cache: local, counts: map[string]int64{"ns/protected": 0, "ns/other": 100}}
	s := &Server{redisClient: client, cache: scores}
	t.Cleanup(s.Shutdown)
	router := least.NewLeastRequestTopKRouterWithCache(scores)
	rc := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "retry", "")
	defer rc.Delete()
	rc.TrafficDrainRouting = true
	attempts := 0
	rc.SetTargetAdmitter(func(target *v1.Pod) error {
		attempts++
		if target.UID == pod.UID {
			_, err := control.Start(context.Background(), string(pod.UID), pod.Annotations[constants.TrafficDrainEpochAnnotationKey], time.Minute)
			require.NoError(t, err)
		}
		return s.admitTrafficAndReplica(rc, target, 0)
	})
	list := &utils.PodArray{Pods: []*v1.Pod{pod, other}}
	_, err = router.Route(rc, list)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)
	require.Equal(t, other.UID, rc.TargetPod().UID)
	require.Equal(t, 2, list.Len(), "retry must not mutate the cache-owned candidate slice")
	releaseTrafficReservation(rc)
}

func TestTrafficDrainRollsBackReservationWhenReplicaCapRejects(t *testing.T) {
	_, _, control, pod, _, _ := trafficGatewayFixture(t)
	legacy := &MockCache{}
	legacy.On("AdmitPodRunningRequest", pod.Name, pod.Namespace, int64(1)).Return(false, false, nil).Once()
	s := &Server{cache: legacy, trafficDrainStore: control}
	rc := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "cap", "")
	defer rc.Delete()
	require.ErrorIs(t, s.admitTrafficAndReplica(rc, pod, 1), types.ErrTargetReplicaInflightExceeded)
	state, err := control.Read(context.Background(), string(pod.UID), pod.Annotations[constants.TrafficDrainEpochAnnotationKey])
	require.NoError(t, err)
	require.Zero(t, state.Active)
	require.Nil(t, rc.TrafficDrainRelease)
	require.False(t, rc.TrafficDrainManaged)
	legacy.AssertExpectations(t)
}

func TestTrafficDrainRealSelectionRetriesOtherHealthyReplica(t *testing.T) {
	_, client, control, pod, _, _ := trafficGatewayFixture(t)
	other := pod.DeepCopy()
	other.Name = "other"
	other.UID = "other-uid"
	other.Status.PodIP = "10.0.0.2"
	epoch, err := control.Initialize(context.Background(), string(other.UID))
	require.NoError(t, err)
	other.Annotations[constants.TrafficDrainEpochAnnotationKey] = epoch
	local := cache.NewWithPodsForTest([]*v1.Pod{pod, other}, "test-model")
	scores := trafficScoringCache{Cache: local, counts: map[string]int64{"ns/protected": 0, "ns/other": 100}}
	s := NewServerWithOptions(client, nil, nil, ServerOptions{Cache: scores, DisableRateLimiting: true})
	t.Cleanup(s.Shutdown)
	_, err = control.Start(context.Background(), string(pod.UID), pod.Annotations[constants.TrafficDrainEpochAnnotationKey], time.Minute)
	require.NoError(t, err)
	rc, state, response := trafficGatewayRequest(t, s, "healthy-fallback")
	require.Nil(t, response.GetImmediateResponse(), "a stale, closed low-load Pod must not hide another healthy candidate")
	require.Equal(t, other.UID, rc.TargetPod().UID)
	releaseTrafficReservation(rc)
	s.finishRequestCount(state)
}

func TestTrafficDrainProcessWaitsPastSSEDone(t *testing.T) {
	_, _, control, pod, first, _ := trafficGatewayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headers := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestHeaders{RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
		{Key: methodKey, RawValue: []byte("POST")}, {Key: pathKey, RawValue: []byte(PathChatCompletions)}, {Key: userKey, RawValue: []byte("user")}, {Key: contentTypeKey, RawValue: []byte("application/json")}, {Key: HeaderRoutingStrategy, RawValue: []byte("least-request")},
	}}}}}
	body := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`), EndOfStream: true}}}
	stream := newBlockingProcessServer(ctx, headers, body)
	finished := make(chan error, 1)
	go func() { finished <- first.Process(stream) }()
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	require.Eventually(t, func() bool {
		s, e := control.Read(ctx, string(pod.UID), epoch)
		return e == nil && s.Active == 1 && len(stream.sentResponses()) >= 2
	}, 2*time.Second, 10*time.Millisecond)
	snapshot, err := control.Start(ctx, string(pod.UID), epoch, 30*time.Second)
	require.NoError(t, err)
	stream.msgs <- &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{Body: []byte("data: [DONE]\n\n")}}}
	require.Eventually(t, func() bool { return len(stream.sentResponses()) >= 3 }, time.Second, 10*time.Millisecond)
	select {
	case err := <-finished:
		t.Fatalf("Process returned before response EOS: %v", err)
	default:
	}
	active, err := control.Read(ctx, string(pod.UID), epoch)
	require.NoError(t, err)
	require.EqualValues(t, 1, active.Active)
	stream.msgs <- &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{EndOfStream: true}}}
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("Process did not finish at response EOS")
	}
	_, err = control.Seal(ctx, string(pod.UID), epoch, snapshot.Generation)
	require.NoError(t, err)
}

func TestTrafficDrainShippedExtProcFailsClosed(t *testing.T) {
	xds := loadGeneratedXDS(t)
	for _, pinned := range []bool{false, true} {
		_, filter := xds.resolve(t, PathChatCompletions, pinned)
		require.False(t, filter.TypedConfig.FailureModeAllow, "protected traffic cannot bypass the plugin on processing failure")
		require.Equal(t, "STREAMED", filter.TypedConfig.ProcessingMode.ResponseBodyMode)
	}
}

func TestTrafficDrainCommittedCancellationReleasesReservation(t *testing.T) {
	_, _, control, pod, first, _ := trafficGatewayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	headers := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestHeaders{RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
		{Key: methodKey, RawValue: []byte("POST")}, {Key: pathKey, RawValue: []byte(PathChatCompletions)}, {Key: userKey, RawValue: []byte("user")}, {Key: HeaderRoutingStrategy, RawValue: []byte("least-request")},
	}}}}}
	body := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`), EndOfStream: true}}}
	stream := newBlockingProcessServer(ctx, headers, body)
	finished := make(chan error, 1)
	go func() { finished <- first.Process(stream) }()
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	require.Eventually(t, func() bool {
		s, e := control.Read(context.Background(), string(pod.UID), epoch)
		return e == nil && s.Active == 1
	}, 2*time.Second, 10*time.Millisecond)
	snapshot, err := control.Start(context.Background(), string(pod.UID), epoch, 30*time.Second)
	require.NoError(t, err)
	cancel()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Process did not exit")
	}
	state, err := control.Read(context.Background(), string(pod.UID), epoch)
	require.NoError(t, err)
	require.Zero(t, state.Active)
	_, err = control.Seal(context.Background(), string(pod.UID), epoch, snapshot.Generation)
	require.NoError(t, err)
}

func trafficGatewayRequest(t *testing.T, s *Server, id string) (*types.RoutingContext, *processState, *extProcPb.ProcessingResponse) {
	t.Helper()
	rc := types.NewRoutingContext(context.Background(), "", "", "", id, "user")
	t.Cleanup(rc.Delete)
	rc.ReqPath = PathChatCompletions
	rc.ReqHeaders[HeaderRoutingStrategy] = "least-request"
	req := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_RequestBody{RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)}}}
	response, model, stream, term := s.HandleRequestBody(context.Background(), rc, id, req, utils.User{Name: "user"})
	return rc, &processState{ctx: context.Background(), routerCtx: rc, requestID: id, model: model, stream: stream, traceTerm: term, user: utils.User{Name: "user"}}, response
}

func TestTrafficDrainTwoGatewaysAndTransportCompletion(t *testing.T) {
	_, _, control, pod, first, second := trafficGatewayFixture(t)
	rc, state, response := trafficGatewayRequest(t, first, "first")
	require.Nil(t, response.GetImmediateResponse())
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	snapshot, err := control.Start(context.Background(), string(pod.UID), epoch, 30*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 1, snapshot.Active)
	_, _, rejected := trafficGatewayRequest(t, second, "stale-second")
	require.NotNil(t, rejected.GetImmediateResponse())
	require.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, rejected.GetImmediateResponse().GetStatus().GetCode())
	done := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{Body: []byte("data: [DONE]\n\n"), EndOfStream: false}}}
	_, err = first.handleProcessingRequest(state, done)
	require.NoError(t, err)
	require.False(t, state.completed)
	observed, err := control.Read(context.Background(), string(pod.UID), epoch)
	require.NoError(t, err)
	require.EqualValues(t, 1, observed.Active)
	last := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{ResponseBody: &extProcPb.HttpBody{EndOfStream: true}}}
	_, err = first.handleProcessingRequest(state, last)
	require.NoError(t, err)
	require.True(t, state.completed)
	require.Nil(t, rc.TrafficDrainRelease)
	sealed, err := control.Seal(context.Background(), string(pod.UID), epoch, snapshot.Generation)
	require.NoError(t, err)
	require.Equal(t, trafficdrain.Sealed, sealed.Phase)
	_, _, rejected = trafficGatewayRequest(t, second, "after-seal")
	require.NotNil(t, rejected.GetImmediateResponse())
}

func TestTrafficDrainAmbiguousEOFDoesNotRelease(t *testing.T) {
	_, _, control, pod, first, _ := trafficGatewayFixture(t)
	_, state, response := trafficGatewayRequest(t, first, "uncertain")
	require.Nil(t, response.GetImmediateResponse())
	epoch := pod.Annotations[constants.TrafficDrainEpochAnnotationKey]
	snapshot, err := control.Start(context.Background(), string(pod.UID), epoch, 30*time.Second)
	require.NoError(t, err)
	require.ErrorIs(t, first.handleRecvError(state, io.EOF), io.EOF)
	current, err := control.Read(context.Background(), string(pod.UID), epoch)
	require.NoError(t, err)
	require.EqualValues(t, 1, current.Active)
	_, err = control.Seal(context.Background(), string(pod.UID), epoch, snapshot.Generation)
	require.ErrorIs(t, err, trafficdrain.ErrConflict)
}

func TestTrafficDrainStateLossAndUnsupportedRoutingFailClosed(t *testing.T) {
	m, _, _, pod, first, _ := trafficGatewayFixture(t)
	m.FlushAll()
	_, _, response := trafficGatewayRequest(t, first, "lost-state")
	require.NotNil(t, response.GetImmediateResponse())
	for _, algorithm := range []types.RoutingAlgorithm{"pd", "random", "least-request:1,prefix-cache:1"} {
		rc := types.NewRoutingContext(context.Background(), algorithm, "test-model", "", "unsupported", "")
		rc.ReqPath = PathChatCompletions
		_, err := prepareTrafficRouting(rc, []*v1.Pod{pod}, algorithm)
		require.Error(t, err)
		rc.Delete()
	}
	rc := types.NewRoutingContext(context.Background(), "least-request", "test-model", "", "video", "")
	defer rc.Delete()
	rc.ReqPath = "/v1/videos"
	_, err := prepareTrafficRouting(rc, []*v1.Pod{pod}, "least-request")
	require.Error(t, err)
}
