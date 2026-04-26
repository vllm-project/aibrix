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
	"errors"
	"testing"

	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestCheckRPM(t *testing.T) {
	t.Run("get failure returns internal server error", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_RPM_CURRENT").Return(int64(0), errors.New("redis down")).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkRPM(context.Background(), "alice", 10)

		assert.Equal(t, envoyTypePb.StatusCode_InternalServerError, code)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fail to get RPM for user: alice")
		rl.AssertExpectations(t)
	})

	t.Run("exceeding limit returns too many requests", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_RPM_CURRENT").Return(int64(10), nil).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkRPM(context.Background(), "alice", 10)

		assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, code)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "has exceeded RPM")
		rl.AssertExpectations(t)
	})

	t.Run("below limit returns ok", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_RPM_CURRENT").Return(int64(9), nil).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkRPM(context.Background(), "alice", 10)

		assert.Equal(t, envoyTypePb.StatusCode_OK, code)
		assert.NoError(t, err)
		rl.AssertExpectations(t)
	})
}

func TestIncrRPM(t *testing.T) {
	t.Run("increment failure returns internal server error", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Incr", mock.Anything, "alice_RPM_CURRENT", int64(1)).Return(int64(0), errors.New("redis down")).Once()

		s := &Server{ratelimiter: rl}
		rpm, code, err := s.incrRPM(context.Background(), "alice")

		assert.Equal(t, int64(0), rpm)
		assert.Equal(t, envoyTypePb.StatusCode_InternalServerError, code)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fail to increment RPM for user: alice")
		rl.AssertExpectations(t)
	})

	t.Run("increment success returns updated rpm", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Incr", mock.Anything, "alice_RPM_CURRENT", int64(1)).Return(int64(7), nil).Once()

		s := &Server{ratelimiter: rl}
		rpm, code, err := s.incrRPM(context.Background(), "alice")

		assert.Equal(t, int64(7), rpm)
		assert.Equal(t, envoyTypePb.StatusCode_OK, code)
		assert.NoError(t, err)
		rl.AssertExpectations(t)
	})
}

func TestCheckTPM(t *testing.T) {
	t.Run("get failure returns internal server error", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_TPM_CURRENT").Return(int64(0), errors.New("redis down")).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkTPM(context.Background(), "alice", 100)

		assert.Equal(t, envoyTypePb.StatusCode_InternalServerError, code)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "fail to get TPM for user: alice")
		rl.AssertExpectations(t)
	})

	t.Run("exceeding limit returns too many requests", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_TPM_CURRENT").Return(int64(100), nil).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkTPM(context.Background(), "alice", 100)

		assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, code)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "has exceeded TPM")
		rl.AssertExpectations(t)
	})

	t.Run("below limit returns ok", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Get", mock.Anything, "alice_TPM_CURRENT").Return(int64(99), nil).Once()

		s := &Server{ratelimiter: rl}
		code, err := s.checkTPM(context.Background(), "alice", 100)

		assert.Equal(t, envoyTypePb.StatusCode_OK, code)
		assert.NoError(t, err)
		rl.AssertExpectations(t)
	})
}

func TestEnforceModelRPS(t *testing.T) {
	t.Run("nil config profile skips enforcement", func(t *testing.T) {
		s := &Server{modelRateLimiter: &mockRateLimiter{}}
		rc := &types.RoutingContext{}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		assert.Nil(t, resp)
	})

	t.Run("non-positive rps skips enforcement", func(t *testing.T) {
		s := &Server{modelRateLimiter: &mockRateLimiter{}}
		rc := &types.RoutingContext{ConfigProfile: &types.ResolvedConfigProfile{RequestsPerSecond: 0}}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		assert.Nil(t, resp)
	})

	t.Run("increment failure returns 500 with incr header", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Incr", mock.Anything, "llama_MODEL_RPS_CURRENT", int64(1)).Return(int64(0), errors.New("redis down")).Once()

		s := &Server{modelRateLimiter: rl}
		rc := &types.RoutingContext{ConfigProfile: &types.ResolvedConfigProfile{RequestsPerSecond: 2}}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		if assert.NotNil(t, resp) {
			imm := resp.GetImmediateResponse()
			assert.Equal(t, envoyTypePb.StatusCode_InternalServerError, imm.GetStatus().GetCode())
			assert.Contains(t, imm.GetBody(), "fail to increment RPS for model: llama")
			if assert.NotEmpty(t, imm.GetHeaders().GetSetHeaders()) {
				assert.Equal(t, HeaderErrorIncrModelRPS, imm.GetHeaders().GetSetHeaders()[0].GetHeader().GetKey())
				assert.Equal(t, "true", string(imm.GetHeaders().GetSetHeaders()[0].GetHeader().GetRawValue()))
			}
		}
		rl.AssertExpectations(t)
	})

	t.Run("exceeding limit returns 429 with model-rps header", func(t *testing.T) {
		rl := &mockRateLimiter{}
		// counter was already at limit; after increment it is limit+1
		rl.On("Incr", mock.Anything, "llama_MODEL_RPS_CURRENT", int64(1)).Return(int64(3), nil).Once()

		s := &Server{modelRateLimiter: rl}
		rc := &types.RoutingContext{ConfigProfile: &types.ResolvedConfigProfile{RequestsPerSecond: 2}}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		if assert.NotNil(t, resp) {
			imm := resp.GetImmediateResponse()
			assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, imm.GetStatus().GetCode())
			assert.Contains(t, imm.GetBody(), ErrorCodeRateLimitExceeded)
			assert.Contains(t, imm.GetBody(), "exceeded RPS")
			if assert.NotEmpty(t, imm.GetHeaders().GetSetHeaders()) {
				assert.Equal(t, HeaderErrorModelRPSExceeded, imm.GetHeaders().GetSetHeaders()[0].GetHeader().GetKey())
				assert.Equal(t, "true", string(imm.GetHeaders().GetSetHeaders()[0].GetHeader().GetRawValue()))
			}
		}
		rl.AssertExpectations(t)
	})

	t.Run("at-limit increment is accepted", func(t *testing.T) {
		rl := &mockRateLimiter{}
		// counter moves from 0 to exactly the limit — still within budget
		rl.On("Incr", mock.Anything, "llama_MODEL_RPS_CURRENT", int64(1)).Return(int64(2), nil).Once()

		s := &Server{modelRateLimiter: rl}
		rc := &types.RoutingContext{ConfigProfile: &types.ResolvedConfigProfile{RequestsPerSecond: 2}}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		assert.Nil(t, resp)
		rl.AssertExpectations(t)
	})

	t.Run("below limit returns nil", func(t *testing.T) {
		rl := &mockRateLimiter{}
		rl.On("Incr", mock.Anything, "llama_MODEL_RPS_CURRENT", int64(1)).Return(int64(1), nil).Once()

		s := &Server{modelRateLimiter: rl}
		rc := &types.RoutingContext{ConfigProfile: &types.ResolvedConfigProfile{RequestsPerSecond: 2}}

		resp := s.enforceModelRPS(context.Background(), "llama", rc)
		assert.Nil(t, resp)
		rl.AssertExpectations(t)
	})
}
