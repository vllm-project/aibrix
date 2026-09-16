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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gatewayplugin "github.com/vllm-project/aibrix/pkg/plugins/gateway"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Gateway lifecycle metrics boundary", Label("gateway", "integration"), func() {
	It("finalizes exactly once and returns to idle state", func() {
		// InFlightObserver is a test observation hook: each request must emit
		// +1/-1 once and leave the fixture idle after finalization.
		fixture := newGatewayFixture([]*corev1.Pod{readyPod("target", "10.0.0.2", nil)})
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		Expect(fixture.cache.inFlightSnapshot()).To(Equal([]int{1, -1}), fixture.diagnostics())
		Expect(fixture.cache.inFlightValue()).To(Equal(0), fixture.diagnostics())
		Expect(gatewayplugin.HasRequestBuffers(fixture.requestID)).To(BeFalse(), fixture.diagnostics())
	})
})
