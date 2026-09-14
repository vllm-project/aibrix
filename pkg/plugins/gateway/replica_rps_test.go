/*
Copyright 2024 The Aibrix Team.

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
	"time"

	"github.com/alicebob/miniredis/v2"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
)

func TestRpsToLimitWindow(t *testing.T) {
	tests := []struct {
		name          string
		rps           float64
		wantLimit     int64
		wantWindowSec int64
	}{
		{"zero disables", 0, 0, 0},
		{"negative disables", -5, 0, 0},
		{"rps at or above 1 uses a 1s window", 5, 5, 1},
		{"fractional rps rounds to nearest at exactly 1", 1, 1, 1},
		{"0.18 rps floors to 1 request every 5s", 0.18, 1, 5},
		{"0.5 rps floors to 1 request every 2s", 0.5, 1, 2},
		{"0.99 rps floors to 1 request every 1s", 0.99, 1, 1},
		{"tiny rps clamps to the max window", 0.0001, 1, maxRateWindowSeconds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, window := rpsToLimitWindow(tt.rps)
			assert.Equal(t, tt.wantLimit, limit)
			assert.Equal(t, tt.wantWindowSec, window)
		})
	}
}

// podWithReplicaRPS returns a pod whose model.aibrix.ai/config annotation sets
// requestsPerSecondPerReplica on its default profile.
func podWithReplicaRPS(name string, rps float64) *v1.Pod {
	return podWithReplicaLimits(name, rps, 0)
}

func TestApplyConfigProfile_FractionalModelReplicaRPS(t *testing.T) {
	// 0.18 replica rps * 1 routable replica = 0.18 aggregate rps -> 1 request every 5s.
	pods := []*v1.Pod{podWithReplicaRPS("a", 0.18)}
	routingCtx := &types.RoutingContext{}

	applyConfigProfile(routingCtx, pods)

	assert.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, int64(5), routingCtx.ConfigProfile.RateWindowSeconds)
	assert.Equal(t, string(routing.RouterLeastRequest), routingCtx.ConfigProfile.RoutingStrategy)
}

func TestApplyConfigProfile_IntegerModelReplicaRPSUnchanged(t *testing.T) {
	// 5 replica rps * 2 routable replicas = 10 aggregate rps, still a 1s window.
	pods := []*v1.Pod{podWithReplicaRPS("a", 5), podWithReplicaRPS("b", 5)}
	routingCtx := &types.RoutingContext{}

	applyConfigProfile(routingCtx, pods)

	assert.Equal(t, int64(10), routingCtx.ConfigProfile.RequestsPerSecond)
	assert.Equal(t, int64(1), routingCtx.ConfigProfile.RateWindowSeconds)
}

// TestModelReplicaRPS_Half_AllowsOneRequestEveryTwoSeconds exercises the full
// requestsPerSecondPerReplica=0.5 path end to end against a real (miniredis-backed) rate
// limiter: applyConfigProfile resolves it to "1 request per 2s", and enforceModelRPS should
// then admit one request, reject an immediate second one, and admit a third only once the
// 2-second window has actually elapsed.
func TestModelReplicaRPS_Half_AllowsOneRequestEveryTwoSeconds(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	pods := []*v1.Pod{podWithReplicaRPS("a", 0.5)}
	routingCtx := &types.RoutingContext{}
	applyConfigProfile(routingCtx, pods)
	require.Equal(t, int64(1), routingCtx.ConfigProfile.RequestsPerSecond)
	require.Equal(t, int64(2), routingCtx.ConfigProfile.RateWindowSeconds)

	s := &Server{modelRateLimiter: ratelimiter.NewRedisAccountRateLimiter("aibrix_model_test", client, time.Second)}
	ctx := context.Background()
	const model = "half-rps-model"

	assert.Nil(t, s.enforceModelRPS(ctx, model, routingCtx), "first request in the window should be allowed")

	resp := s.enforceModelRPS(ctx, model, routingCtx)
	if assert.NotNil(t, resp, "a second request within the same 2s window should be rejected") {
		imm := resp.GetImmediateResponse()
		require.NotNil(t, imm)
		assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, imm.GetStatus().GetCode())
	}

	time.Sleep(2100 * time.Millisecond)
	assert.Nil(t, s.enforceModelRPS(ctx, model, routingCtx), "a request after the 2s window elapses should be allowed again")
}
