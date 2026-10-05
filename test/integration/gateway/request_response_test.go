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
	"encoding/json"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Gateway request and response boundary", Label("gateway", "integration"), func() {
	// These assertions inspect Gateway ext_proc mutations and its error envelope.
	// Backend HTTP transport is outside this integration package's boundary.
	It("preserves the request body and applies gateway headers", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "random")
		Expect(response.GetBodyMutation().GetBody()).To(Equal(fixture.stream.inputs[1].GetRequestBody().GetBody()))
	})

	It("does not forward the user header in request mutations", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		fixture.stream.inputs[0].GetRequestHeaders().Headers.Headers = append(
			fixture.stream.inputs[0].GetRequestHeaders().Headers.Headers,
			&configPb.HeaderValue{Key: "user", RawValue: []byte("alice")},
		)
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		for _, header := range response.GetHeaderMutation().GetSetHeaders() {
			Expect(strings.ToLower(header.GetHeader().GetKey())).NotTo(Equal("user"))
		}
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
	})

	It("passes through an OpenAI-compatible upstream error without double wrapping", func() {
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		fixture.stream = newFakeProcessStream(
			context.Background(),
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			requestBodyRequest(), responseErrorHeadersRequest(), responseErrorBodyRequest(),
		)
		err := fixture.run()
		Expect(err).To(Succeed(), fixture.diagnostics())
		lastImmediate := findLastResponseWithImmediate(fixture.stream.responses())
		Expect(lastImmediate).NotTo(BeNil(), fixture.diagnostics())
		immediate := lastImmediate.GetImmediateResponse()
		Expect(immediate.GetStatus().GetCode()).To(Equal(envoyTypePb.StatusCode_InternalServerError))
		var envelope map[string]any
		Expect(json.Unmarshal([]byte(immediate.GetBody()), &envelope)).To(Succeed())
		Expect(envelope["error"]).NotTo(BeNil())
		errorObject := envelope["error"].(map[string]any)
		Expect(errorObject["message"]).To(Equal("upstream failed"))
		Expect(errorObject["type"]).To(Equal("server_error"))
		Expect(errorObject["error"]).To(BeNil())
	})
})

func responseErrorHeadersRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{{
					Key: ":status", RawValue: []byte("200"),
				}}},
			},
		},
	}
}

func responseErrorBodyRequest() *extProcPb.ProcessingRequest {
	return &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"error":{"message":"upstream failed","type":"server_error"}}`),
				EndOfStream: true,
			},
		},
	}
}

func findLastResponseWithImmediate(responses []*extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	for i := len(responses) - 1; i >= 0; i-- {
		if responses[i].GetImmediateResponse() != nil {
			return responses[i]
		}
	}
	return nil
}
