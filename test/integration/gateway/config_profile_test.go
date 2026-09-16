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
)

var _ = Describe("Gateway config profile integration", Label("gateway", "integration"), func() {
	It("uses the profile routing strategy when the request omits one", func() {
		pods := twoReadyPods()
		pod := pods[0]
		profileConfig := `{"defaultProfile":"default","profiles":` +
			`{"default":{"routingStrategy":"least-request:1,load-balance:0"}}}`
		pod.Annotations = map[string]string{
			"model.aibrix.ai/config": profileConfig,
		}
		fixture := newGatewayFixtureWithRequest(pods, "", "default", "")
		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		response := findRequestBodyResponse(fixture.stream.responses())
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "least-request:1,load-balance:0")
		expectHeader(response.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.2:8000")
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		expectSuccessfulLifecycle(fixture)
	})
})
