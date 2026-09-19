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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestHandleRequestHeadersRateLimitingDisabledSeparatesAsyncJobOwner(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	server := &Server{
		redisClient:         client,
		rateLimitingEnabled: false,
	}
	header := &configPb.HeaderValue{Key: userKey, RawValue: []byte("unknown-user")}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					header,
					{Key: pathKey, RawValue: []byte(PathChatCompletions)},
				}},
			},
		},
	}

	resp, user, rpm, routingCtx, _ := server.HandleRequestHeaders(
		context.Background(), "request-1", trace.SpanFromContext(context.Background()), req,
	)

	require.NotNil(t, resp.GetRequestHeaders())
	assert.Equal(t, utils.User{}, user)
	assert.Zero(t, rpm)
	require.NotNil(t, routingCtx)
	assert.Nil(t, routingCtx.User)
	assert.Equal(t, asyncJobOwnerFromUserName("unknown-user"), routingCtx.AsyncJobOwner)
	assert.Equal(t, "unknown-user", string(header.RawValue), "the incoming header must be forwarded unchanged")
	assert.NotContains(t, resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetRemoveHeaders(), userKey)
}

func TestHandleRequestBodyRateLimitingDisabledSkipsModelRPSAndPreservesBody(t *testing.T) {
	for _, tt := range []struct {
		name        string
		profileJSON string
	}{
		{
			name:        "direct model RPS",
			profileJSON: `{"defaultProfile":"rps","profiles":{"rps":{"requestsPerSecond":1}}}`,
		},
		{
			name:        "per-replica RPS",
			profileJSON: `{"defaultProfile":"rps","profiles":{"rps":{"requestsPerSecondPerReplica":1}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mockCache := &MockCache{}
			mockModelLimiter := &mockRateLimiter{}
			body := []byte(`{"model":"test-model","stream":true,"user":"openai-user","messages":[{"role":"user","content":"hello"}]}`)
			pod := readyPod("pod-a", "default", "10.0.0.1")
			pod.Annotations = map[string]string{constants.ModelAnnoConfig: tt.profileJSON}
			pods := &utils.PodArray{Pods: []*v1.Pod{pod}}
			mockCache.On("HasModel", "test-model").Return(true).Times(2)
			mockCache.On("ListPodsByModel", "test-model").Return(pods, nil).Times(2)
			mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1)).Times(2)

			server := &Server{
				cache:               mockCache,
				modelRateLimiter:    mockModelLimiter,
				rateLimitingEnabled: false,
			}
			for i, requestID := range []string{"request-1", "request-2"} {
				routingCtx := types.NewRoutingContext(context.Background(), "", "", "", requestID, "")
				routingCtx.ReqPath = PathChatCompletions
				req := &extProcPb.ProcessingRequest{
					Request: &extProcPb.ProcessingRequest_RequestBody{
						RequestBody: &extProcPb.HttpBody{Body: body},
					},
				}

				resp, model, stream, term := server.HandleRequestBody(
					context.Background(), routingCtx, requestID, req, utils.User{Name: "ignored", Tpm: 1},
				)

				require.NotNil(t, resp.GetRequestBody())
				assert.Nil(t, resp.GetImmediateResponse(), "request %d must not be rate limited", i+1)
				assert.Equal(t, "test-model", model)
				assert.True(t, stream)
				assert.EqualValues(t, 1, term)
				assert.Equal(t, body, resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody())
				require.NotNil(t, routingCtx.ConfigProfile)
				assert.Zero(t, routingCtx.ConfigProfile.RequestsPerSecond)
			}
			mockModelLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
			mockCache.AssertExpectations(t)
		})
	}
}

func TestHandleRequestBodyRateLimitingDisabledDoesNotRollbackModelRPSOnRoutingFailure(t *testing.T) {
	cache.InitForTest()
	mockCache := &MockCache{Cache: cache.NewForTest()}
	mockRouter := new(mockRouter)
	mockModelLimiter := &mockRateLimiter{}
	registerTestRouter(mockRouter)

	profileJSON := `{"defaultProfile":"rps","profiles":{"rps":{"requestsPerSecond":1}}}`
	podA := readyPod("pod-a", "default", "10.0.0.1")
	podA.Annotations = map[string]string{constants.ModelAnnoConfig: profileJSON}
	podB := readyPod("pod-b", "default", "10.0.0.2")
	podB.Annotations = map[string]string{constants.ModelAnnoConfig: profileJSON}
	pods := &utils.PodArray{Pods: []*v1.Pod{podA, podB}}
	mockCache.On("HasModel", "test-model").Return(true).Once()
	mockCache.On("ListPodsByModel", "test-model").Return(pods, nil).Once()
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{
		"default/pod-a": 0,
		"default/pod-b": 0,
	}, nil).Once()
	mockRouter.On("Route", mock.Anything, mock.Anything).Return("", errors.New("route selection failed")).Once()

	server := &Server{
		cache:               mockCache,
		modelRateLimiter:    mockModelLimiter,
		rateLimitingEnabled: false,
		requestCountTracker: map[string]int{},
	}
	routingCtx := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
	routingCtx.ReqPath = PathChatCompletions
	routingCtx.ReqHeaders[HeaderRoutingStrategy] = string(TestRouterAlgorithm)
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)},
		},
	}

	resp, _, _, term := server.HandleRequestBody(
		context.Background(), routingCtx, "request-1", req, utils.User{Name: "ignored"},
	)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Zero(t, term)
	mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
	mockModelLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
	mockRouter.AssertExpectations(t)
	mockCache.AssertExpectations(t)
}

func TestHandleRequestBodyRateLimitingDisabledKeepsReplicaInflightAdmission(t *testing.T) {
	mockCache := &MockCache{}
	mockModelLimiter := &mockRateLimiter{}
	registerTestRouter(new(mockRouter))

	pod := podWithReplicaLimits("a", 5, 1)
	pods := &utils.PodArray{Pods: []*v1.Pod{pod}}
	mockCache.On("HasModel", "test-model").Return(true)
	mockCache.On("ListPodsByModel", "test-model").Return(pods, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "test-model").Return(int64(1)).Once()
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 0}, nil).Once()
	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(true, nil).Once()
	mockCache.On("GetMetricValueByPod", "a", "ns", metrics.RealtimeNumRequestsRunning).
		Return(&metrics.SimpleMetricValue{Value: 0}, nil).Once()
	mockCache.On("GetPodsRunningRequests", mock.Anything).Return(map[string]int64{"ns/a": 1}, nil).Once()

	server := &Server{
		cache:               mockCache,
		modelRateLimiter:    mockModelLimiter,
		rateLimitingEnabled: false,
	}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{Body: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`)},
		},
	}

	first := types.NewRoutingContext(context.Background(), "", "", "", "request-1", "")
	first.ReqPath = PathChatCompletions
	resp, _, _, term := server.HandleRequestBody(context.Background(), first, "request-1", req, utils.User{})
	require.Nil(t, resp.GetImmediateResponse())
	assert.EqualValues(t, 1, term)
	require.NotNil(t, first.ConfigProfile)
	assert.Equal(t, int64(1), first.ConfigProfile.RequestsInflight)
	assert.Zero(t, first.ConfigProfile.RequestsPerSecond)

	second := types.NewRoutingContext(context.Background(), "", "", "", "request-2", "")
	second.ReqPath = PathChatCompletions
	resp, _, _, term = server.HandleRequestBody(context.Background(), second, "request-2", req, utils.User{})
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeReplicaInflightExceeded)
	assert.Zero(t, term)
	mockModelLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
	mockCache.AssertExpectations(t)
}

func TestDisabledVTCBasicFallsBackWithoutResolvedUser(t *testing.T) {
	cache.InitForTest()
	routing.Init()
	server := &Server{rateLimitingEnabled: false}
	pods := &utils.PodArray{Pods: []*v1.Pod{
		readyPod("pod-a", "default", "10.0.0.1"),
		readyPod("pod-b", "default", "10.0.0.2"),
	}}
	routingCtx := types.NewRoutingContext(context.Background(), "vtc-basic", "test-model", "hello", "request-1", "")
	require.Nil(t, routingCtx.User)

	addr, err := server.selectTargetPod(context.Background(), routingCtx, pods, "")

	require.NoError(t, err)
	assert.Contains(t, addr, "10.0.0.")
	assert.NotNil(t, routingCtx.TargetPod())
}

func TestHandleResponseBodyRateLimitingDisabledSkipsUserTPM(t *testing.T) {
	mockLimiter := &mockRateLimiter{}
	server := &Server{
		ratelimiter:         mockLimiter,
		rateLimitingEnabled: false,
	}
	routingCtx := types.NewRoutingContext(context.Background(), "", "test-model", "", "request-1", "")
	routingCtx.ReqPath = PathChatCompletions
	routingCtx.RequestTime = time.Now()
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model":"test-model","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete, usage := server.HandleResponseBody(
		context.Background(), routingCtx, "request-1", req, utils.User{Name: "ignored"}, 42, "test-model", false, false,
	)

	assert.True(t, complete)
	assert.Equal(t, int64(15), usage.TotalTokens)
	headers := resp.GetResponseBody().GetResponse().GetHeaderMutation().GetSetHeaders()
	for _, header := range headers {
		assert.NotEqual(t, HeaderUpdateRPM, header.GetHeader().GetKey())
		assert.NotEqual(t, HeaderUpdateTPM, header.GetHeader().GetKey())
	}
	mockLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
}

func TestApplyConfigProfileRateLimitingDisabled(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{"default":{
			"routingStrategy":"throughput",
			"requestsPerSecond":7,
			"requestsPerSecondPerReplica":3
		}}
	}`
	pod := readyPod("pod-a", "default", "10.0.0.1")
	pod.Annotations = map[string]string{constants.ModelAnnoConfig: profileJSON}
	routingCtx := types.NewRoutingContext(context.Background(), "", "model", "", "request-1", "")

	applyConfigProfile(routingCtx, []*v1.Pod{pod}, false)

	require.NotNil(t, routingCtx.ConfigProfile)
	assert.Equal(t, "throughput", routingCtx.ConfigProfile.RoutingStrategy)
	assert.Zero(t, routingCtx.ConfigProfile.RequestsPerSecond)
	assert.Zero(t, routingCtx.ConfigProfile.RateWindowSeconds)
}

func TestApplyConfigProfileRateLimitingDisabledPreservesInflight(t *testing.T) {
	profileJSON := `{
		"defaultProfile":"default",
		"profiles":{"default":{
			"routingStrategy":"throughput",
			"requestsPerSecondPerReplica":3,
			"requestsInflight":2
		}}
	}`
	pod := readyPod("pod-a", "default", "10.0.0.1")
	pod.Annotations = map[string]string{constants.ModelAnnoConfig: profileJSON}
	routingCtx := types.NewRoutingContext(context.Background(), "", "model", "", "request-1", "")

	applyConfigProfile(routingCtx, []*v1.Pod{pod}, false)

	require.NotNil(t, routingCtx.ConfigProfile)
	assert.Equal(t, int64(2), routingCtx.ConfigProfile.RequestsInflight)
	assert.Equal(t, string(routing.RouterLeastRequest), routingCtx.ConfigProfile.RoutingStrategy)
	assert.Zero(t, routingCtx.ConfigProfile.RequestsPerSecond)
}

func TestVideoFollowUpRateLimitingDisabledSkipsModelRPS(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })

			mockCache := new(MockCache)
			mockModelLimiter := &mockRateLimiter{}
			pod := readyPod("pod-a", "default", "10.0.0.1")
			pod.Annotations = map[string]string{
				constants.ModelAnnoConfig: `{"defaultProfile":"default","profiles":{"default":{"requestsPerSecond":1,"requestsInflight":1}}}`,
			}
			writerRegistry := newRedisAsyncJobRegistry(asyncJobStoreClient(redisClient), mockCache)
			record, err := writerRegistry.Register(context.Background(), AsyncJobRegistration{
				JobType:      asyncJobTypeVideo,
				Owner:        asyncJobOwnerShared,
				Model:        "video-model",
				BackendJobID: "backend-video-1",
				Pod:          pod,
			})
			require.NoError(t, err)
			readerRegistry := newRedisAsyncJobRegistry(asyncJobStoreClient(redisClient), mockCache)
			server := &Server{
				cache:               mockCache,
				redisClient:         redisClient,
				asyncJobs:           readerRegistry,
				modelRateLimiter:    mockModelLimiter,
				rateLimitingEnabled: false,
			}
			mockCache.On("GetPod", "pod-a", "default").Return(pod, nil).Once()
			mockCache.On("AddRequestCount", mock.Anything, "request-1", "video-model").Return(int64(1)).Once()
			req := &extProcPb.ProcessingRequest{
				Request: &extProcPb.ProcessingRequest_RequestHeaders{
					RequestHeaders: &extProcPb.HttpHeaders{
						Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
							{Key: pathKey, RawValue: []byte(PathVideos + "/" + record.PublicJobID)},
							{Key: methodKey, RawValue: []byte(method)},
						}},
						EndOfStream: true,
					},
				},
			}

			resp, _, _, routingCtx, term := server.HandleRequestHeaders(
				context.Background(), "request-1", trace.SpanFromContext(context.Background()), req,
			)

			require.NotNil(t, resp.GetRequestHeaders())
			assert.EqualValues(t, 1, term)
			require.NotNil(t, routingCtx.ConfigProfile)
			assert.Zero(t, routingCtx.ConfigProfile.RequestsPerSecond)
			assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsInflight)
			mockModelLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
			keys := redisServer.Keys()
			assert.Contains(t, keys, asyncJobRecordKey(asyncJobOwnerShared, record.PublicJobID),
				"the follow-up must resolve the non-rate async-job record from Redis")
			for _, key := range keys {
				assert.False(t, strings.Contains(key, "_MODEL_RPS_CURRENT"), "disabled mode must not create model-RPS keys")
			}
			mockCache.AssertExpectations(t)
		})
	}
}

func TestVideoFollowUpRateLimitingDisabledFailureDoesNotChargeModelRPS(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	mockCache := new(MockCache)
	mockModelLimiter := &mockRateLimiter{}
	pod := readyPod("pod-a", "default", "10.0.0.1")
	writerRegistry := newRedisAsyncJobRegistry(asyncJobStoreClient(redisClient), mockCache)
	record, err := writerRegistry.Register(context.Background(), AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        asyncJobOwnerShared,
		Model:        "video-model",
		BackendJobID: "backend-video-1",
		Pod:          pod,
	})
	require.NoError(t, err)
	pod.Status.Conditions[0].Status = v1.ConditionFalse
	mockCache.On("GetPod", "pod-a", "default").Return(pod, nil).Once()
	server := &Server{
		cache:               mockCache,
		redisClient:         redisClient,
		asyncJobs:           newRedisAsyncJobRegistry(asyncJobStoreClient(redisClient), mockCache),
		modelRateLimiter:    mockModelLimiter,
		rateLimitingEnabled: false,
	}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos + "/" + record.PublicJobID)},
					{Key: methodKey, RawValue: []byte(http.MethodGet)},
				}},
				EndOfStream: true,
			},
		},
	}

	resp, _, _, _, term := server.HandleRequestHeaders(
		context.Background(), "request-1", trace.SpanFromContext(context.Background()), req,
	)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Zero(t, term)
	assert.Contains(t, redisServer.Keys(), asyncJobRecordKey(asyncJobOwnerShared, record.PublicJobID),
		"a transient pod failure must retain the async-job record")
	mockCache.AssertNotCalled(t, "AddRequestCount", mock.Anything, mock.Anything, mock.Anything)
	mockModelLimiter.AssertNotCalled(t, "Incr", mock.Anything, mock.Anything, mock.Anything)
	mockCache.AssertExpectations(t)
}

func TestAPIKeyAuthenticationIsIndependentOfRateLimiting(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		mode := "disabled"
		if enabled {
			mode = "enabled"
		}
		for _, tt := range []struct {
			name   string
			header string
			want   envoyTypePb.StatusCode
		}{
			{name: "valid", header: "Bearer secret", want: envoyTypePb.StatusCode_OK},
			{name: "missing", want: envoyTypePb.StatusCode_Unauthorized},
			{name: "invalid", header: "Bearer wrong", want: envoyTypePb.StatusCode_Unauthorized},
		} {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				server := &Server{
					apiKeyAuth:          &apiKeyAuthConfig{token: "secret"},
					rateLimitingEnabled: enabled,
				}
				headers := []*configPb.HeaderValue{{Key: pathKey, RawValue: []byte(PathCompletions)}}
				if tt.header != "" {
					headers = append(headers, &configPb.HeaderValue{Key: authorizationKey, RawValue: []byte(tt.header)})
				}
				req := &extProcPb.ProcessingRequest{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{Headers: &configPb.HeaderMap{Headers: headers}},
					},
				}

				resp, _, _, routingCtx, _ := server.HandleRequestHeaders(
					context.Background(), "request-1", trace.SpanFromContext(context.Background()), req,
				)

				if tt.want == envoyTypePb.StatusCode_OK {
					assert.Nil(t, resp.GetImmediateResponse())
					assert.NotNil(t, routingCtx)
				} else {
					assert.Equal(t, tt.want, resp.GetImmediateResponse().GetStatus().GetCode())
					assert.Nil(t, routingCtx)
				}
			})
		}
	}
}

func TestDisabledProfileSourceIsNotMutated(t *testing.T) {
	profileJSON := `{"defaultProfile":"default","profiles":{"default":{"requestsPerSecond":3}}}`
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{constants.ModelAnnoConfig: profileJSON}}}
	routingCtx := &types.RoutingContext{}

	applyConfigProfile(routingCtx, []*v1.Pod{pod}, false)

	assert.Equal(t, profileJSON, pod.Annotations[constants.ModelAnnoConfig])
}
