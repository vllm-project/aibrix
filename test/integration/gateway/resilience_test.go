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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Gateway resilience boundary", Label("gateway", "integration"), func() {
	It("cleans up on context cancellation", func() {
		// Process headers/body first, then block Recv so cancellation is observed
		// during processing rather than being an input-construction shortcut.
		ctx, cancel := context.WithCancel(context.Background())
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		fixture.stream = newFakeProcessStream(
			ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			requestBodyRequest(),
		)
		fixture.stream.blockOnExhaustion = true
		fixture.stream.recvStarted = make(chan struct{})
		result := make(chan error, 1)
		go func() { result <- fixture.run() }()
		Eventually(fixture.stream.recvStarted).Should(BeClosed())
		cancel()
		var err error
		select {
		case err = <-result:
		case <-time.After(2 * time.Second):
			Fail("Process did not return after context cancellation")
		}
		Expect(errors.Is(err, context.Canceled)).To(BeTrue(), fixture.diagnostics())
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
		Expect(fixture.cache.inFlightSnapshot()).To(Equal([]int{1, -1}), fixture.diagnostics())
	})

	It("cleans up when the request deadline expires while receiving", func() {
		// The result select has a safety timeout so a regression becomes a failure,
		// not a deadlocked test process.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		fixture.stream = newFakeProcessStream(
			ctx,
			requestHeadersRequest(fixture.requestID, "random", "", ""),
			requestBodyRequest(),
		)
		fixture.stream.blockOnExhaustion = true
		fixture.stream.recvStarted = make(chan struct{})
		result := make(chan error, 1)
		go func() { result <- fixture.run() }()
		Eventually(fixture.stream.recvStarted).Should(BeClosed())

		var err error
		select {
		case err = <-result:
		case <-time.After(2 * time.Second):
			Fail("Process did not return after request deadline")
		}
		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), fixture.diagnostics())
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
		Expect(fixture.cache.inFlightSnapshot()).To(Equal([]int{1, -1}), fixture.diagnostics())
	})
})
