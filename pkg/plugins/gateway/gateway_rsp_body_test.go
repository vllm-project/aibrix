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
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// mockRateLimiter implements ratelimiter.RateLimiter for testing
type mockRateLimiter struct {
	mock.Mock
}

func (m *mockRateLimiter) Get(ctx context.Context, key string) (int64, error) {
	args := m.Called(ctx, key)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockRateLimiter) GetLimit(ctx context.Context, key string) (int64, error) {
	args := m.Called(ctx, key)
	return args.Get(0).(int64), args.Error(1)
}

func (m *mockRateLimiter) Incr(ctx context.Context, key string, val int64) (int64, error) {
	args := m.Called(ctx, key, val)
	return args.Get(0).(int64), args.Error(1)
}

func TestIsLanguageRequest(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
		want        bool
	}{
		{
			name:        "chat completions is language",
			requestPath: "/v1/chat/completions",
			want:        true,
		},
		{
			name:        "completions is language",
			requestPath: "/v1/completions",
			want:        true,
		},
		{
			name:        "embeddings is language",
			requestPath: "/v1/embeddings",
			want:        true,
		},
		{
			name:        "images generations is not language",
			requestPath: "/v1/images/generations",
			want:        false,
		},
		{
			name:        "video generations is not language",
			requestPath: "/v1/video/generations",
			want:        false,
		},
		{
			name:        "audio transcriptions is not language",
			requestPath: "/v1/audio/transcriptions",
			want:        false,
		},
		{
			name:        "audio translations is not language",
			requestPath: "/v1/audio/translations",
			want:        false,
		},
		{
			name:        "empty path is language",
			requestPath: "",
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLanguageRequest(tt.requestPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTokenBucketLabel(t *testing.T) {
	tests := []struct {
		name   string
		tokens int64
		want   string
	}{
		{"zero", 0, "0-256"},
		{"small", 100, "0-256"},
		{"boundary 256", 256, "256-512"},
		{"mid range", 500, "256-512"},
		{"boundary 512", 512, "512-1024"},
		{"1024", 1024, "1024-2048"},
		{"2048", 2048, "2048-4096"},
		{"4096", 4096, "4096-8192"},
		{"8192", 8192, "8192-16384"},
		{"16384", 16384, "16384-32768"},
		{"32768", 32768, "32768+"},
		{"large", 100000, "32768+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenBucketLabel(tt.tokens)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDurationBucketLabel(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0-1ms"},
		{"sub millisecond", 500 * time.Microsecond, "0-1ms"},
		{"1ms", time.Millisecond, "1-2ms"},
		{"2ms", 2 * time.Millisecond, "2-5ms"},
		{"5ms", 5 * time.Millisecond, "5-10ms"},
		{"10ms", 10 * time.Millisecond, "10-20ms"},
		{"50ms", 50 * time.Millisecond, "50-100ms"},
		{"100ms", 100 * time.Millisecond, "100-200ms"},
		{"500ms", 500 * time.Millisecond, "500-1000ms"},
		{"1s", time.Second, "1000-2000ms"},
		{"5s", 5 * time.Second, "5000ms+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := durationBucketLabel(tt.d)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProcessLanguageResponse_PartialChunk(t *testing.T) {
	requestID := "test-partial-" + time.Now().Format("150405.000")
	body := []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`)

	req := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        body,
			EndOfStream: false,
		},
	}

	res, complete, promptTokens, completionTokens, totalTokens := processLanguageResponse(requestID, req)

	assert.False(t, complete)
	assert.Equal(t, int64(0), promptTokens)
	assert.Equal(t, int64(0), completionTokens)
	assert.Equal(t, int64(0), totalTokens)
	assert.NotNil(t, res)
	assert.NotNil(t, res.GetResponseBody())
	assert.NotNil(t, res.GetResponseBody().GetResponse())
}

func TestProcessLanguageResponse_ValidFullResponse(t *testing.T) {
	requestID := "test-valid-" + time.Now().Format("150405.000")
	body := []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`)

	req := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        body,
			EndOfStream: true,
		},
	}

	res, complete, promptTokens, completionTokens, totalTokens := processLanguageResponse(requestID, req)

	// processLanguageResponse returns complete=false for valid case (no early return)
	assert.False(t, complete)
	assert.Equal(t, int64(10), promptTokens)
	assert.Equal(t, int64(5), completionTokens)
	assert.Equal(t, int64(15), totalTokens)
	assert.Nil(t, res) // No error response for valid case
}

func TestProcessLanguageResponse_InvalidJSON(t *testing.T) {
	requestID := "test-invalid-json-" + time.Now().Format("150405.000")
	body := []byte(`{invalid json}`)

	req := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        body,
			EndOfStream: true,
		},
	}

	res, complete, _, _, _ := processLanguageResponse(requestID, req)

	assert.True(t, complete)
	assert.NotNil(t, res)
	// buildErrorResponse returns ImmediateResponse, not ResponseBody
	immResp := res.GetImmediateResponse()
	assert.NotNil(t, immResp)
	headers := immResp.GetHeaders().GetSetHeaders()
	found := false
	for _, h := range headers {
		if h.Header.Key == HeaderErrorResponseUnmarshal {
			found = true
			break
		}
	}
	assert.True(t, found, "expected HeaderErrorResponseUnmarshal in response")
}

func TestProcessLanguageResponse_EmptyModel(t *testing.T) {
	requestID := "test-empty-model-" + time.Now().Format("150405.000")
	body := []byte(`{"model": "", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`)

	req := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        body,
			EndOfStream: true,
		},
	}

	res, complete, _, _, _ := processLanguageResponse(requestID, req)

	assert.True(t, complete)
	assert.NotNil(t, res)
	immResp := res.GetImmediateResponse()
	assert.NotNil(t, immResp)
	headers := immResp.GetHeaders().GetSetHeaders()
	found := false
	for _, h := range headers {
		if h.Header.Key == HeaderErrorResponseUnknown {
			found = true
			break
		}
	}
	assert.True(t, found, "expected HeaderErrorResponseUnknown in response")
}

func TestProcessLanguageResponse_ChunkedAccumulation(t *testing.T) {
	requestID := "test-chunked-" + time.Now().Format("150405.000")

	// First chunk - partial
	chunk1 := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": `),
			EndOfStream: false,
		},
	}
	res1, complete1, _, _, _ := processLanguageResponse(requestID, chunk1)
	assert.False(t, complete1)
	assert.NotNil(t, res1)

	// Second chunk - complete
	chunk2 := &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{
			Body:        []byte(`10, "completion_tokens": 5, "total_tokens": 15}}`),
			EndOfStream: true,
		},
	}
	_, complete2, promptTokens, completionTokens, totalTokens := processLanguageResponse(requestID, chunk2)
	assert.False(t, complete2) // processLanguageResponse returns complete=false for valid case
	assert.Equal(t, int64(10), promptTokens)
	assert.Equal(t, int64(5), completionTokens)
	assert.Equal(t, int64(15), totalTokens)
}

func TestHandleResponseBody_NonStreamNoTokens(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	// DoneRequestTrace(ctx, requestID, model, inputTokens, outputTokens, traceTerm)
	mockCache.On("DoneRequestTrace", mock.Anything, "test-req-id", "test-model", int64(10), int64(5), int64(0)).Maybe()

	server := &Server{
		cache: mockCache,
	}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "test-req-id", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, "test-req-id", req, utils.User{}, 0, "test-model", false, 0, false)

	assert.True(t, complete)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody())
}

func TestHandleResponseBody_WithUserAndTPM(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	// DoneRequestTrace(ctx, requestID, model, term, inputTokens, outputTokens) - use mock.Anything for dynamic requestID
	mockCache.On("DoneRequestTrace", mock.Anything, mock.Anything, "test-model", int64(10), int64(5), int64(0)).Maybe()

	mockRL := &mockRateLimiter{}
	mockRL.On("Incr", mock.Anything, "test-user_TPM_CURRENT", int64(15)).Return(int64(100), nil)

	server := &Server{
		cache:       mockCache,
		ratelimiter: mockRL,
	}

	requestID := "test-req-tpm-" + time.Now().Format("150405.000")
	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", requestID, "test-user")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, requestID, req, utils.User{Name: "test-user"}, 42, "test-model", false, 0, false)

	assert.True(t, complete)
	assert.NotNil(t, resp)
	headers := resp.GetResponseBody().GetResponse().GetHeaderMutation().GetSetHeaders()
	foundTPM := false
	foundRPM := false
	foundReqID := false
	for _, h := range headers {
		switch h.Header.Key {
		case HeaderUpdateTPM:
			foundTPM = true
			assert.Equal(t, []byte("100"), h.Header.RawValue)
		case HeaderUpdateRPM:
			foundRPM = true
			assert.Equal(t, []byte("42"), h.Header.RawValue)
		case HeaderRequestID:
			foundReqID = true
			assert.Equal(t, []byte(requestID), h.Header.RawValue)
		}
	}
	assert.True(t, foundTPM, "expected HeaderUpdateTPM in response")
	assert.True(t, foundRPM, "expected HeaderUpdateRPM in response")
	assert.True(t, foundReqID, "expected request-id in response")
	mockRL.AssertExpectations(t)
}

func TestHandleResponseBody_NonLanguageRequest(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	// Non-language request: no tokens from processLanguageResponse, EndOfStream triggers complete
	mockCache.On("DoneRequestTrace", mock.Anything, "test-req-id", "test-model", int64(0), int64(0), int64(0)).Maybe()

	server := &Server{
		cache: mockCache,
	}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "test-req-id", "")
	routerCtx.ReqPath = "/v1/images/generations"
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model"}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, "test-req-id", req, utils.User{}, 0, "test-model", false, 0, false)

	// Non-language request with EndOfStream sets complete=true
	assert.True(t, complete)
	assert.NotNil(t, resp)
}

func TestHandleResponseBody_EndOfStreamNoTokens(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	// Body {} parses but has empty model - returns error, DoneRequestTrace called with 0,0,0
	mockCache.On("DoneRequestTrace", mock.Anything, "test-req-id", "test-model", int64(0), int64(0), int64(0)).Maybe()

	server := &Server{
		cache: mockCache,
	}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", "test-req-id", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, "test-req-id", req, utils.User{}, 0, "test-model", false, 0, false)

	assert.True(t, complete)
	assert.NotNil(t, resp)
}

func TestHandleResponseBody_TPMIncrError(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	mockCache.On("DoneRequestTrace", mock.Anything, mock.Anything, "test-model", int64(10), int64(5), int64(0)).Maybe()

	mockRL := &mockRateLimiter{}
	mockRL.On("Incr", mock.Anything, "test-user_TPM_CURRENT", int64(15)).Return(int64(0), errors.New("mock error"))
	server := &Server{
		cache:       mockCache,
		ratelimiter: mockRL,
	}

	requestID := "test-req-tpm-err-" + time.Now().Format("150405.000")
	routerCtx := types.NewRoutingContext(context.Background(), "random", "test-model", "", requestID, "test-user")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model": "test-model", "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}`),
				EndOfStream: true,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, requestID, req, utils.User{Name: "test-user"}, 0, "test-model", false, 0, false)

	assert.True(t, complete)
	assert.NotNil(t, resp)
	// Error response uses ImmediateResponse
	immResp := resp.GetImmediateResponse()
	assert.NotNil(t, immResp)
	headers := immResp.GetHeaders().GetSetHeaders()
	found := false
	for _, h := range headers {
		if h.Header.Key == HeaderErrorIncrTPM {
			found = true
			break
		}
	}
	assert.True(t, found, "expected HeaderErrorIncrTPM in response")
	mockRL.AssertExpectations(t)
}

func TestHandleResponseBody_LanguagePartialResponse(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	server := &Server{cache: mockCache}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "m", "", "rid-partial", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model":"m","usage":{"prompt_tokens":1}}`),
				EndOfStream: false,
			},
		},
	}

	resp, complete := server.HandleResponseBody(routerCtx, "rid-partial", req, utils.User{}, 0, "m", false, 0, false)
	assert.False(t, complete)
	assert.NotNil(t, resp)
	assert.NotNil(t, resp.GetResponseBody().GetResponse())
}

func TestHandleResponseBody_DoesNotDuplicateTrace(t *testing.T) {
	mockCache := &MockCache{Cache: cache.NewForTest()}
	server := &Server{cache: mockCache}

	routerCtx := types.NewRoutingContext(context.Background(), "random", "m", "", "rid", "")
	routerCtx.ReqPath = PathChatCompletions
	routerCtx.RequestTime = time.Now()

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model":"m","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
				EndOfStream: true,
			},
		},
	}

	_, complete := server.HandleResponseBody(routerCtx, "rid", req, utils.User{}, 0, "m", false, 0, true)
	assert.True(t, complete)
	mockCache.AssertNotCalled(t, "DoneRequestTrace", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
