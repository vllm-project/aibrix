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
	"strings"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// SGLang sends input usage in message_start and only output usage in message_delta.
func TestHandleResponseBody_MessagesSplitUsageIsMetered(t *testing.T) {
	for _, name := range []string{"one_callback", "separate_callbacks", "cumulative_deltas", "byte_chunks"} {
		separateCallbacks := name != "one_callback"
		t.Run(name, func(t *testing.T) {
			requestID := "messages-split-usage-" + name
			t.Cleanup(func() {
				streamUsage.Delete(requestID)
				streamBuffers.Delete(requestID)
			})
			limiter := &mockRateLimiter{}
			limiter.Test(t)
			limiter.On("Incr", mock.Anything, "test-user_TPM_CURRENT", int64(40)).Return(int64(40), nil).Once()
			server := &Server{ratelimiter: limiter}
			routingCtx := types.NewRoutingContext(context.Background(), "random", "m", "", requestID, "test-user")
			routingCtx.ReqPath = PathMessages
			routingCtx.RequestTime = time.Now()
			body := messagesStreamBody(`{"output_tokens":15}`)
			if name == "cumulative_deltas" {
				// Duplicate running counts must replace each other, never add.
				running := `data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":5}}` + "\n\n"
				body = strings.Replace(body, "event: message_delta\n", running+running+"event: message_delta\n", 1)
			}
			chunks := []string{body}
			if name == "byte_chunks" {
				chunks = strings.Split(body, "")
			} else if separateCallbacks {
				chunks = strings.SplitAfter(strings.TrimSuffix(body, "\n\n"), "\n\n")
			}
			var complete bool
			var finalUsage TokenUsage
			for i, chunk := range chunks {
				if chunk == "" {
					continue
				}
				req := &extProcPb.ProcessingRequest{Request: &extProcPb.ProcessingRequest_ResponseBody{
					ResponseBody: &extProcPb.HttpBody{Body: []byte(chunk), EndOfStream: i == len(chunks)-1},
				}}
				response, done, usage := server.HandleResponseBody(context.Background(), routingCtx, requestID, req,
					utils.User{Name: "test-user", Tpm: 1000}, 1, "m", true, complete)
				require.Nil(t, response.GetImmediateResponse())
				if i == 0 && separateCallbacks {
					assert.False(t, done, "message_start must not finalize token accounting")
					assert.Zero(t, usage)
				}
				complete = done
				if usage.TotalTokens != 0 {
					finalUsage = usage
				}
			}
			assert.True(t, complete)
			assert.Equal(t, TokenUsage{PromptTokens: 25, CompletionTokens: 15, TotalTokens: 40}, finalUsage)
			limiter.AssertExpectations(t)
		})
	}
}

func TestProcessStreamingResponse_MessagesUsageFields(t *testing.T) {
	tests := []struct {
		name  string
		delta string
		want  TokenUsage
	}{
		{"output_only", `{"output_tokens":15}`, TokenUsage{25, 15, 40}},
		{"full_snapshot", `{"input_tokens":7,"output_tokens":15}`, TokenUsage{7, 15, 22}},
		{"explicit_zero_input", `{"input_tokens":0,"output_tokens":15}`, TokenUsage{0, 15, 15}},
		{"explicit_zero_output", `{"output_tokens":0}`, TokenUsage{25, 0, 25}},
		{"null_total", `{"output_tokens":15,"total_tokens":null}`, TokenUsage{25, 15, 40}},
		{"explicit_total", `{"output_tokens":15,"total_tokens":100}`, TokenUsage{25, 15, 100}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestID := "messages-usage-fields-" + tt.name
			t.Cleanup(func() { streamUsage.Delete(requestID); streamBuffers.Delete(requestID) })
			body := messagesStreamBody(tt.delta)
			// Exercise every possible transport split, including inside usage keys.
			for split := 0; split <= len(body); split++ {
				firstUsage, firstFinal, response := processStreamingResponse(requestID, []byte(body[:split]), false)
				require.Nil(t, response)
				got, final, response := processStreamingResponse(requestID, []byte(body[split:]), true)
				require.Nil(t, response)
				require.True(t, final)
				if firstFinal {
					require.Equal(t, tt.want, firstUsage, "final usage before split at byte %d", split)
					require.Zero(t, got, "terminal usage must not be charged again at byte %d", split)
				} else {
					require.Equal(t, tt.want, got, "split at byte %d", split)
				}
			}
		})
	}
}

func TestProcessStreamingResponse_OpenAIUsageStillReplaces(t *testing.T) {
	requestID := "openai-usage-replaces"
	t.Cleanup(func() { streamUsage.Delete(requestID); streamBuffers.Delete(requestID) })
	running := `data: {"choices":[{"delta":{"content":"hi"}}],"usage":{"input_tokens":25,"output_tokens":5}}` + "\n\n"
	_, final, response := processStreamingResponse(requestID, []byte(running), false)
	require.Nil(t, response)
	require.False(t, final)
	terminal := `data: {"choices":[],"usage":{"output_tokens":15}}` + "\n\n"
	usage, final, response := processStreamingResponse(requestID, []byte(terminal), true)
	require.Nil(t, response)
	require.True(t, final)
	assert.Equal(t, TokenUsage{PromptTokens: 0, CompletionTokens: 15, TotalTokens: 15}, usage)
}
