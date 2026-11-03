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
	"time"

	"github.com/alicebob/miniredis/v2"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/ratelimiter"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func TestApplyDefaultUserLimits(t *testing.T) {
	tests := []struct {
		name    string
		user    utils.User
		wantRPM int64
		wantTPM int64
	}{
		{
			name:    "no stored limits",
			user:    utils.User{Name: "alice"},
			wantRPM: int64(DefaultRPM),
			wantTPM: int64(DefaultRPM) * int64(DefaultTPMMultiplier),
		},
		{
			name:    "rpm only",
			user:    utils.User{Name: "bob", Rpm: 10},
			wantRPM: 10,
			wantTPM: 10 * int64(DefaultTPMMultiplier),
		},
		{
			name:    "tpm only",
			user:    utils.User{Name: "carol", Tpm: 500},
			wantRPM: int64(DefaultRPM),
			wantTPM: 500,
		},
		{
			name:    "explicit limits",
			user:    utils.User{Name: "dave", Rpm: 5, Tpm: 2000},
			wantRPM: 5,
			wantTPM: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyDefaultUserLimits(tt.user)
			assert.EqualValues(t, tt.wantRPM, got.Rpm)
			assert.EqualValues(t, tt.wantTPM, got.Tpm)
		})
	}
}

// TestResolveUserForRequestAppliesDefaultLimits is the end-to-end regression:
// the user record has no rpm or tpm, but the resolved user must carry the
// quotas checkLimits enforces, so the streaming validator rejects a request
// that would never report usage.
func TestResolveUserForRequestAppliesDefaultLimits(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	require.NoError(t, client.Ping(ctx).Err())

	require.NoError(t, utils.SetUser(ctx, utils.User{Name: "alice"}, client))

	server := &Server{
		redisClient: client,
		ratelimiter: ratelimiter.NewRedisAccountRateLimiter("aibrix_default_limits_test", client, time.Minute),
	}

	user, rpm, errRes := server.resolveUserForRequest(ctx, "req", "alice")
	require.Nil(t, errRes)
	assert.EqualValues(t, DefaultRPM, user.Rpm)
	assert.EqualValues(t, DefaultRPM*DefaultTPMMultiplier, user.Tpm)
	assert.EqualValues(t, 1, rpm)

	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	_, _, _, _, errRes = validateChatRequest("req", PathChatCompletions, body, user)
	require.NotNil(t, errRes)
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, errRes.GetImmediateResponse().GetStatus().GetCode())
}

// Users whose stored record omits tpm are still subject to the default TPM.
// The request-body validation must see that effective limit, otherwise a
// stream without stream_options.include_usage is accepted and never metered.
func TestDefaultTPMRequiresStreamUsage(t *testing.T) {
	user := applyDefaultUserLimits(utils.User{Name: "alice"})
	require.Positive(t, user.Tpm)

	t.Run("chat completions", func(t *testing.T) {
		body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		_, _, _, _, errRes := validateChatRequest("req", PathChatCompletions, body, user)
		require.NotNil(t, errRes)
		assert.Equal(t, envoyTypePb.StatusCode_BadRequest, errRes.GetImmediateResponse().GetStatus().GetCode())
		assert.Equal(t, HeaderErrorStreamOptionsIncludeUsage, errRes.GetImmediateResponse().GetHeaders().GetSetHeaders()[0].GetHeader().GetKey())
	})

	t.Run("completions", func(t *testing.T) {
		body := []byte(`{"model":"m","stream":true,"prompt":"hi"}`)
		_, _, _, errRes := validateCompletionRequest("req", body, user)
		require.NotNil(t, errRes)
		assert.Equal(t, envoyTypePb.StatusCode_BadRequest, errRes.GetImmediateResponse().GetStatus().GetCode())
		assert.Equal(t, HeaderErrorStreamOptionsIncludeUsage, errRes.GetImmediateResponse().GetHeaders().GetSetHeaders()[0].GetHeader().GetKey())
	})
}
