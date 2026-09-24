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
	"testing"

	miniredis "github.com/alicebob/miniredis/v2"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestNewServerWithOptionsConfiguresRateLimiting(t *testing.T) {
	for _, tt := range []struct {
		name                string
		disableRateLimiting bool
		wantUsage           int64
		wantKeys            int
	}{
		{name: "enabled by default", wantUsage: 1, wantKeys: 2},
		{name: "disabled", disableRateLimiting: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			redisServer := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })

			server := NewServerWithOptions(redisClient, nil, nil, ServerOptions{
				Cache:               &MockCache{},
				DisableRateLimiting: tt.disableRateLimiting,
			})
			t.Cleanup(server.Shutdown)

			assert.Same(t, redisClient, server.redisClient)
			assert.Equal(t, tt.disableRateLimiting, server.disableRateLimiting)
			_, routingAvailable := server.routerManager.Validate("least-request")
			assert.True(t, routingAvailable)

			userUsage, err := server.ratelimiter.Incr(context.Background(), "user_RPM_CURRENT", 1)
			require.NoError(t, err)
			modelUsage, err := server.modelRateLimiter.Incr(context.Background(), "model_MODEL_RPS_CURRENT", 1)
			require.NoError(t, err)
			assert.Equal(t, tt.wantUsage, userUsage)
			assert.Equal(t, tt.wantUsage, modelUsage)
			assert.Len(t, redisServer.Keys(), tt.wantKeys)
		})
	}
}

func TestHandleRequestHeadersSkipsUnknownUserWhenRateLimitingDisabled(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	server := NewServerWithOptions(redisClient, nil, nil, ServerOptions{
		Cache:               &MockCache{},
		DisableRateLimiting: true,
	})
	t.Cleanup(server.Shutdown)

	userHeader := &configPb.HeaderValue{Key: userKey, RawValue: []byte("unknown-user")}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					userHeader,
					{Key: pathKey, RawValue: []byte(PathChatCompletions)},
				}},
			},
		},
	}

	resp, user, rpm, routingCtx, _ := server.HandleRequestHeaders(
		context.Background(), "request-1", trace.SpanFromContext(context.Background()), req,
	)

	require.NotNil(t, resp.GetRequestHeaders())
	assert.Nil(t, resp.GetImmediateResponse())
	assert.Equal(t, utils.User{}, user)
	assert.Zero(t, rpm)
	require.NotNil(t, routingCtx)
	assert.Nil(t, routingCtx.User)
	assert.Equal(t, "unknown-user", string(userHeader.RawValue))
	assert.Empty(t, redisServer.Keys())
}

func TestRateLimitingDisabledKeepsReplicaInflight(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	mockCache := &MockCache{}
	server := NewServerWithOptions(redisClient, nil, nil, ServerOptions{
		Cache:               mockCache,
		DisableRateLimiting: true,
	})
	t.Cleanup(server.Shutdown)

	pod := podWithReplicaLimits("a", 1, 1)
	routingCtx := types.NewRoutingContext(context.Background(), "", "model", "", "request-1", "")
	applyConfigProfile(routingCtx, []*v1.Pod{pod})
	require.NotNil(t, routingCtx.ConfigProfile)
	assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsInflight)

	assert.Nil(t, server.enforceModelRPS(context.Background(), "model", routingCtx))
	assert.Nil(t, server.enforceModelRPS(context.Background(), "model", routingCtx))
	assert.Empty(t, redisServer.Keys())

	mockCache.On("AdmitPodRunningRequest", "a", "ns", int64(1)).Return(false, nil).Once()
	routingCtx.SetTargetPod(pod)
	resp := server.enforceReplicaInflight(context.Background(), "model", routingCtx)
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), ErrorCodeReplicaInflightExceeded)
	mockCache.AssertExpectations(t)
}
