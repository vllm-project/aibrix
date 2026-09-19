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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	miniredis "github.com/alicebob/miniredis/v2"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routingalgorithms "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
)

type redisCommandRecorder struct {
	mu       sync.Mutex
	commands []string
}

func (r *redisCommandRecorder) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (r *redisCommandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.record(cmd)
		return next(ctx, cmd)
	}
}

func (r *redisCommandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			r.record(cmd)
		}
		return next(ctx, cmds)
	}
}

func (r *redisCommandRecorder) record(cmd redis.Cmder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, fmt.Sprint(cmd.Args()))
}

func (r *redisCommandRecorder) snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.commands, "\n")
}

func (r *redisCommandRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = nil
}

func targetPodHeader(fixture *gatewayFixture) string {
	response := findRequestBodyResponse(fixture.stream.responses())
	for _, header := range response.GetHeaderMutation().GetSetHeaders() {
		if header.GetHeader().GetKey() == "target-pod" {
			return string(header.GetHeader().GetRawValue())
		}
	}
	Fail("missing target-pod response header")
	return ""
}

var _ = Describe("Gateway rate-limiting policy", Label("gateway", "integration"), func() {
	It("keeps Redis-backed routing while disabling every gateway quota path", func() {
		redisServer := miniredis.RunT(GinkgoT())
		redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
		commands := &redisCommandRecorder{}
		redisClient.AddHook(commands)
		DeferCleanup(redisClient.Close)

		profile := `{"defaultProfile":"default","profiles":{"default":{"requestsPerSecond":1}}}`
		podA := readyPod("target-a", "10.0.0.2", nil)
		podA.Annotations = map[string]string{constants.ModelAnnoConfig: profile}
		podB := readyPod("target-b", "10.0.0.3", nil)
		podB.Annotations = map[string]string{constants.ModelAnnoConfig: profile}
		body := []byte(`{"model":"llama2-7b",` +
			`"messages":[{"role":"user","content":"hello"}],` +
			`"stream":true,"user":"openai-user"}`)

		// This key's stateless rendezvous fallback selects pod B from the two-pod
		// set. The writer below sees only pod A and persists that different choice;
		// a fresh reader can therefore select A only by reading shared Redis state.
		sessionKey := "oi-44-session-0"
		control := newGatewayFixtureWithPolicy(
			[]*corev1.Pod{podA.DeepCopy(), podB.DeepCopy()},
			"session-affinity", "default", "", body, nil, true,
		)
		control.cache.metricValues["target-a/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
		control.cache.metricValues["target-b/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
		control.stream.inputs[0].GetRequestHeaders().Headers.Headers = append(
			control.stream.inputs[0].GetRequestHeaders().Headers.Headers,
			&configPb.HeaderValue{Key: constants.HeaderSessionKey, RawValue: []byte(sessionKey)},
		)
		Expect(control.run()).To(Succeed(), control.diagnostics())
		Expect(targetPodHeader(control)).To(Equal("10.0.0.3:8000"))
		control.server.Shutdown()

		fixture := newGatewayFixtureWithPolicy(
			[]*corev1.Pod{podA.DeepCopy()}, "session-affinity", "default", "", body, redisClient, true,
		)
		DeferCleanup(fixture.server.Shutdown)
		userHeader := &configPb.HeaderValue{Key: "user", RawValue: []byte("unknown-user")}
		sessionHeader := &configPb.HeaderValue{Key: constants.HeaderSessionKey, RawValue: []byte(sessionKey)}
		fixture.stream.inputs[0].GetRequestHeaders().Headers.Headers = append(
			fixture.stream.inputs[0].GetRequestHeaders().Headers.Headers,
			userHeader,
			sessionHeader,
		)

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		httpAddress := listener.Addr().String()
		Expect(listener.Close()).To(Succeed())
		Expect(fixture.server.StartHTTPServer(httpAddress)).To(Succeed())
		modelsResponse, err := http.Get("http://" + httpAddress + "/v1/models")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(modelsResponse.Body.Close)
		modelsBody, err := io.ReadAll(modelsResponse.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(modelsResponse.StatusCode).To(Equal(http.StatusOK))
		Expect(string(modelsBody)).To(ContainSubstring(`"id":"llama2-7b"`))

		Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
		Expect(findResponseWithImmediate(fixture.stream.responses())).To(BeNil(), fixture.diagnostics())
		requestBody := findRequestBodyResponse(fixture.stream.responses())
		Expect(requestBody.GetBodyMutation().GetBody()).To(Equal(body))
		Expect(targetPodHeader(fixture)).To(Equal("10.0.0.2:8000"))
		Expect(string(userHeader.RawValue)).To(Equal("unknown-user"))
		for _, response := range fixture.stream.responses() {
			if headers := response.GetRequestHeaders(); headers != nil {
				for _, removed := range headers.GetResponse().GetHeaderMutation().GetRemoveHeaders() {
					Expect(strings.ToLower(removed)).NotTo(Equal("user"))
				}
			}
		}
		Eventually(func() bool {
			for _, key := range redisServer.Keys() {
				if strings.HasPrefix(key, "aibrix:gateway:session_affinity:") {
					return true
				}
			}
			return false
		}).Should(BeTrue(), "disabled mode must preserve Redis-backed session affinity")
		affinityKey := ""
		for _, key := range redisServer.Keys() {
			if strings.HasPrefix(key, "aibrix:gateway:session_affinity:") {
				affinityKey = key
				break
			}
		}
		Expect(affinityKey).NotTo(BeEmpty())
		storedTarget, err := redisServer.Get(affinityKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(storedTarget).To(Equal("10.0.0.2:8000"))
		writerCommands := commands.snapshot()
		Expect(writerCommands).NotTo(ContainSubstring("aibrix-users/unknown-user"))
		Expect(writerCommands).NotTo(ContainSubstring("_RPM_CURRENT"))
		Expect(writerCommands).NotTo(ContainSubstring("_TPM_CURRENT"))
		Expect(writerCommands).NotTo(ContainSubstring("_MODEL_RPS_CURRENT"))

		commands.reset()
		reader := newGatewayFixtureWithPolicy(
			[]*corev1.Pod{podA.DeepCopy(), podB.DeepCopy()},
			"session-affinity", "default", "", body, redisClient, true,
		)
		// Keep both candidates through the gateway's load-imbalance gate. The
		// test needs session affinity itself—not the one-pod fast path—to decide.
		reader.cache.metricValues["target-a/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
		reader.cache.metricValues["target-b/"+metrics.RealtimeNumRequestsRunning] = &metrics.SimpleMetricValue{Value: 4}
		DeferCleanup(reader.server.Shutdown)
		writerRouter, err := fixture.routerManager.Lookup(routingalgorithms.RouterSessionAffinity)
		Expect(err).NotTo(HaveOccurred())
		readerRouter, err := reader.routerManager.Lookup(routingalgorithms.RouterSessionAffinity)
		Expect(err).NotTo(HaveOccurred())
		controlRouter, err := control.routerManager.Lookup(routingalgorithms.RouterSessionAffinity)
		Expect(err).NotTo(HaveOccurred())
		Expect(readerRouter).NotTo(BeIdenticalTo(writerRouter),
			"the read-back must use a distinct session-affinity router instance")
		Expect(readerRouter).NotTo(BeIdenticalTo(controlRouter),
			"the read-back must not reuse the stateless control router instance")
		reader.stream.inputs[0].GetRequestHeaders().Headers.Headers = append(
			reader.stream.inputs[0].GetRequestHeaders().Headers.Headers,
			&configPb.HeaderValue{Key: constants.HeaderSessionKey, RawValue: []byte(sessionKey)},
		)
		Expect(reader.run()).To(Succeed(), reader.diagnostics())
		Expect(findResponseWithImmediate(reader.stream.responses())).To(BeNil(), reader.diagnostics())
		Expect(commands.snapshot()).To(ContainSubstring(fmt.Sprintf("[get %s]", affinityKey)),
			"the fresh server must read the persisted session-affinity key from Redis")
		Expect(targetPodHeader(reader)).To(Equal("10.0.0.2:8000"),
			"a fresh server must honor pod A from Redis instead of its pod B rendezvous fallback")

		keys := redisServer.Keys()
		Expect(keys).NotTo(BeEmpty())
		for _, key := range keys {
			Expect(key).NotTo(ContainSubstring("_RPM_CURRENT"))
			Expect(key).NotTo(ContainSubstring("_TPM_CURRENT"))
			Expect(key).NotTo(ContainSubstring("_MODEL_RPS_CURRENT"))
		}
		readerCommands := commands.snapshot()
		Expect(readerCommands).NotTo(ContainSubstring("_RPM_CURRENT"))
		Expect(readerCommands).NotTo(ContainSubstring("_TPM_CURRENT"))
		Expect(readerCommands).NotTo(ContainSubstring("_MODEL_RPS_CURRENT"))
		Expect(fixture.cache.finalizationCount(fixture.requestID)).To(Equal(1), fixture.diagnostics())
		Expect(reader.cache.finalizationCount(reader.requestID)).To(Equal(1), reader.diagnostics())
	})
})
