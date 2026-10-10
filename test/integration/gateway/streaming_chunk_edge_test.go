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
	"strings"
	"unicode/utf8"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Gateway response streaming chunk edges", Label("gateway", "integration"), func() {
	It("preserves multiple SSE events delivered in a single callback chunk", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		events := "data: {\"id\":\"multi-1\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
			"data: {\"id\":\"multi-2\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk(events, false),
			responseChunk("data: [DONE]\n\n", true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte(events)))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(Equal([]byte("data: [DONE]\n\n")))
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(2), fixture.diagnostics())
		expectPreservedStreamingResponses(fixture)
		expectSuccessfulLifecycle(fixture)
	})

	It("reassembles a UTF-8 character split across callback chunks", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		payload := "data: {\"id\":\"utf8\",\"choices\":[{\"delta\":{\"content\":\"你好，世界\"}}]," +
			"\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
			"data: [DONE]\n\n"
		// Split inside a multi-byte character so the second chunk starts mid-rune.
		split := strings.Index(payload, "好")
		Expect(split).NotTo(Equal(-1), "test payload must contain the split rune")
		split++
		first, second := payload[:split], payload[split:]
		Expect(utf8.ValidString(first)).To(BeFalse(), "the first chunk must end mid-rune")
		Expect(utf8.ValidString(first+second)).To(BeTrue(), "the reassembled payload must be valid UTF-8")
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk(first, false),
			responseChunk(second, true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte(first)))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(Equal([]byte(second)))
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(2), fixture.diagnostics())
		promptTokens, completionTokens, ok := doneTraceTokens(fixture.cache, fixture.requestID)
		Expect(ok).To(BeTrue(), fixture.diagnostics())
		Expect(promptTokens).To(Equal(int64(3)), fixture.diagnostics())
		Expect(completionTokens).To(Equal(int64(5)), fixture.diagnostics())
		Expect(requestIDHeaderValue(fixture.stream.responses())).To(Equal(fixture.requestID), fixture.diagnostics())
		expectPreservedStreamingResponses(fixture)
		expectSuccessfulLifecycle(fixture)
	})

	It("processes empty chunks, including the empty end-of-stream chunk", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		content := "data: {\"id\":\"empty\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk(content, false),
			responseChunk("", false),
			responseChunk("", true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte(content)))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(BeEmpty())
		Expect(fixture.stream.inputs[4].GetResponseBody().GetEndOfStream()).To(BeFalse())
		Expect(fixture.stream.inputs[5].GetResponseBody().GetBody()).To(BeEmpty())
		Expect(fixture.stream.inputs[5].GetResponseBody().GetEndOfStream()).To(BeTrue())
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(3), fixture.diagnostics())
		expectPreservedStreamingResponses(fixture)
		expectSuccessfulLifecycle(fixture)
	})

	It("flushes a buffered partial line when the empty end-of-stream chunk arrives", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		firstEvent := "data: {\"id\":\"tail-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
		// The final line arrives without a newline terminator, so the gateway carries it
		// over as a partial line and only the empty end-of-stream chunk can flush it.
		partialLine := "data: {\"id\":\"tail-2\",\"choices\":[]," +
			"\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}"
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk(firstEvent+partialLine, false),
			responseChunk("", true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte(firstEvent + partialLine)))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(BeEmpty())
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(2), fixture.diagnostics())
		promptTokens, completionTokens, ok := doneTraceTokens(fixture.cache, fixture.requestID)
		Expect(ok).To(BeTrue(), fixture.diagnostics())
		Expect(promptTokens).To(Equal(int64(3)), fixture.diagnostics())
		Expect(completionTokens).To(Equal(int64(5)), fixture.diagnostics())
		Expect(requestIDHeaderValue(fixture.stream.responses())).To(Equal(fixture.requestID), fixture.diagnostics())
		expectPreservedStreamingResponses(fixture)
		expectSuccessfulLifecycle(fixture)
	})

	It("extracts usage from the final streaming chunk while preserving the original body", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		delta := "data: {\"id\":\"usage-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
		finalChunk := "data: {\"id\":\"usage-1\",\"choices\":[]," +
			"\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n\n" +
			"data: [DONE]\n\n"
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk(delta, false),
			responseChunk(finalChunk, true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte(delta)))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(Equal([]byte(finalChunk)))
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(2), fixture.diagnostics())
		promptTokens, completionTokens, ok := doneTraceTokens(fixture.cache, fixture.requestID)
		Expect(ok).To(BeTrue(), fixture.diagnostics())
		Expect(promptTokens).To(Equal(int64(5)), fixture.diagnostics())
		Expect(completionTokens).To(Equal(int64(7)), fixture.diagnostics())
		Expect(requestIDHeaderValue(fixture.stream.responses())).To(Equal(fixture.requestID), fixture.diagnostics())
		expectPreservedStreamingResponses(fixture)
		expectSuccessfulLifecycle(fixture)
	})
})

// expectPreservedStreamingResponses checks each callback response is untouched.
func expectPreservedStreamingResponses(fixture *gatewayFixture) {
	for _, response := range fixture.stream.responses() {
		Expect(response.GetImmediateResponse()).To(BeNil(), fixture.diagnostics())
		if response.GetResponseBody() == nil {
			continue
		}
		Expect(response.GetResponseBody().GetResponse()).NotTo(BeNil(), fixture.diagnostics())
		Expect(response.GetResponseBody().GetResponse().GetBodyMutation()).To(BeNil(), fixture.diagnostics())
	}
}

// doneTraceTokens returns the token counts passed to DoneRequestTrace.
func doneTraceTokens(c *fakeCache, requestID string) (int64, int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tokens, ok := c.doneTraceUsage[requestID]
	return tokens[0], tokens[1], ok
}

// requestIDHeaderValue returns the request ID the gateway attached, if any.
func requestIDHeaderValue(responses []*extProcPb.ProcessingResponse) string {
	value := ""
	for _, response := range responses {
		if body := response.GetResponseBody(); body != nil && body.GetResponse() != nil {
			for _, option := range body.GetResponse().GetHeaderMutation().GetSetHeaders() {
				if option.GetHeader().GetKey() == gatewayplugin.HeaderRequestID {
					value = string(option.GetHeader().GetRawValue())
				}
			}
		}
	}
	return value
}
