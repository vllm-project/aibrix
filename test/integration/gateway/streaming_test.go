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
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Gateway response streaming boundary", Label("gateway", "integration"), func() {
	It("processes split JSON and DONE", func() {
		// Use real split JSON and DONE callbacks. Gateway does not rewrite SSE
		// bytes here; this proves callback preservation and lifecycle only. Full
		// parser matrices remain unit/follow-up coverage.
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		fixture.stream = newFakeProcessStream(
			fixture.stream.ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			streamingBodyRequest(), responseHeadersRequest(),
			responseChunk("data: {\"id\":", false),
			responseChunk("\"x\",\"choices\":[]}\n\n", false),
			responseChunk("data: [DONE]\n\n", true),
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		// Gateway returns CommonResponse for streaming callbacks; Envoy owns
		// forwarding the original SSE bytes. This verifies callback boundaries,
		// not Gateway SSE parsing; parser behavior remains unit/follow-up coverage.
		Expect(fixture.stream.inputs[3].GetResponseBody().GetBody()).To(Equal([]byte("data: {\"id\":")))
		Expect(fixture.stream.inputs[4].GetResponseBody().GetBody()).To(Equal([]byte("\"x\",\"choices\":[]}\n\n")))
		Expect(fixture.stream.inputs[5].GetResponseBody().GetBody()).To(Equal([]byte("data: [DONE]\n\n")))
		Expect(fixture.stream.inputs[3].GetResponseBody().GetEndOfStream()).To(BeFalse())
		Expect(fixture.stream.inputs[4].GetResponseBody().GetEndOfStream()).To(BeFalse())
		Expect(fixture.stream.inputs[5].GetResponseBody().GetEndOfStream()).To(BeTrue())
		Expect(responseBodyResponseCount(fixture.stream.responses())).To(Equal(3), fixture.diagnostics())
		for _, response := range fixture.stream.responses() {
			if response.GetResponseBody() == nil {
				continue
			}
			Expect(response.GetResponseBody()).NotTo(BeNil(), fixture.diagnostics())
			Expect(response.GetImmediateResponse()).To(BeNil(), fixture.diagnostics())
			Expect(response.GetResponseBody().GetResponse()).NotTo(BeNil(), fixture.diagnostics())
			Expect(response.GetResponseBody().GetResponse().GetBodyMutation()).To(BeNil(), fixture.diagnostics())
		}
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
	})
})

func streamingBodyRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestBody{
			RequestBody: &extProcPb.HttpBody{
				Body:        []byte(`{"model":"llama2-7b","messages":[{"role":"user","content":"hello"}],"stream":true}`),
				EndOfStream: true,
			},
		},
	}
}

func responseBodyResponseCount(responses []*extProcPb.ProcessingResponse) int {
	count := 0
	for _, response := range responses {
		if response.GetResponseBody() != nil {
			count++
		}
	}
	return count
}
func responseChunk(body string, end bool) *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{Body: []byte(body), EndOfStream: end},
		},
	}
}
