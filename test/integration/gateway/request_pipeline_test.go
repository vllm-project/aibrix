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
	"errors"
	"fmt"
	"io"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Gateway ext_proc integration", Label("gateway", "integration"), func() {
	It("routes a valid request to a ready Pod and finalizes once", func() {
		fixture := newGatewayFixture([]*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "llama2-7b-0",
				Namespace: "default",
				Labels:    map[string]string{"aibrix.ai/model-name": "llama2-7b"},
			},
			Status: corev1.PodStatus{
				PodIP:      "10.0.0.2",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}})

		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		pod, err := fixture.cache.GetPod("llama2-7b-0", "default")
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Name).To(Equal("llama2-7b-0"))
		bodyResponse := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(bodyResponse.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "random")
		expectHeader(bodyResponse.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		events := fixture.cache.eventsSnapshot()
		Expect(events).To(HaveLen(2), fixture.diagnostics())
		Expect(findEvent(events, "add")).NotTo(BeNil(), fixture.diagnostics())
		Expect(findEvent(events, "done-trace")).NotTo(BeNil(), fixture.diagnostics())
		Expect(findEvent(events, "done")).To(BeNil(), fixture.diagnostics())
		for _, event := range events {
			Expect(event.RequestID).To(Equal(fixture.requestID))
			Expect(event.Model).To(Equal("llama2-7b"))
			Expect(event.TraceTerm).To(BeNumerically(">", 0))
		}
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
		Expect(findResponseWithImmediate(fixture.stream.responses())).To(BeNil(), fixture.diagnostics())
	})

	It("returns a gateway error when no ready Pod exists", func() {
		fixture := newGatewayFixture([]*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{Name: "llama2-7b-0", Namespace: "default"},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			},
		}})

		err := fixture.run()
		Expect(err).To(HaveOccurred(), fixture.diagnostics())
		Expect(errors.Is(err, io.EOF)).To(BeTrue(), fixture.diagnostics())
		immediate := findResponseWithImmediate(fixture.stream.responses()).GetImmediateResponse()
		Expect(immediate).NotTo(BeNil(), fixture.diagnostics())
		Expect(immediate.GetStatus().GetCode()).To(Equal(envoyTypePb.StatusCode_ServiceUnavailable), fixture.diagnostics())
		Expect(immediate.GetBody()).To(ContainSubstring("error on getting pods for model llama2-7b"), fixture.diagnostics())
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		events := fixture.cache.eventsSnapshot()
		Expect(events).To(HaveLen(1), fixture.diagnostics())
		Expect(findEvent(events, "done")).NotTo(BeNil(), fixture.diagnostics())
		Expect(findEvent(events, "done-trace")).To(BeNil(), fixture.diagnostics())
		for _, event := range events {
			Expect(event.RequestID).To(Equal(fixture.requestID))
			Expect(event.Model).To(Equal("llama2-7b"))
		}
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
		Expect(fixture.stream.responses()).NotTo(BeEmpty(), fixture.diagnostics())
	})
})

func findRequestBodyResponse(responses []*extProcPb.ProcessingResponse) *extProcPb.CommonResponse {
	for _, response := range responses {
		if body := response.GetRequestBody(); body != nil && body.GetResponse() != nil {
			return body.GetResponse()
		}
	}
	Fail("missing request-body response")
	return nil
}

func findResponseWithImmediate(responses []*extProcPb.ProcessingResponse) *extProcPb.ProcessingResponse {
	for _, response := range responses {
		if response.GetImmediateResponse() != nil {
			return response
		}
	}
	return nil
}

func findEvent(events []requestEvent, kind string) *requestEvent {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

func expectHeader(headers []*configPb.HeaderValueOption, key, value string) {
	for _, header := range headers {
		if header.GetHeader().GetKey() == key {
			Expect(string(header.GetHeader().GetRawValue())).To(Equal(value))
			return
		}
	}
	Fail(fmt.Sprintf("missing header %q", key))
}
