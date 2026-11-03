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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"

	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"
	"github.com/vllm-project/aibrix/pkg/constants"
	corev1 "k8s.io/api/core/v1"
)

// pdClientBody has nested objects whose key order is deliberately unsorted:
// any map[string]any round trip inside the gateway would re-serialise them in
// a different order and break the byte-level assertions below.
const pdClientBody = `{"model":"llama2-7b",` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi"},` +
	`{"type":"image_url","image_url":{"url":"u","detail":"low"}}]}],` +
	`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object",` +
	`"properties":{"z":{"type":"string"},"a":{"type":"integer"}},"required":["z","a"]}}}],` +
	`"temperature":0.7,"max_tokens":128,"stream":false}`

// fakePrefillServer stands in for the prefill pod: it records every request
// body it receives and answers with a fixed JSON response.
type fakePrefillServer struct {
	server   *httptest.Server
	response string
	mu       sync.Mutex
	bodies   [][]byte
}

func newFakePrefillServer(response string) *fakePrefillServer {
	p := &fakePrefillServer{response: response}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, body)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(p.response))
	}))
	DeferCleanup(p.server.Close)
	return p
}

func (p *fakePrefillServer) port() string {
	u, err := url.Parse(p.server.URL)
	Expect(err).NotTo(HaveOccurred())
	return u.Port()
}

func (p *fakePrefillServer) receivedBodies() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.bodies...)
}

// pdPods returns one prefill pod (pointing at the fake prefill server on
// 127.0.0.1) and one decode pod, both in the same roleset and engine.
func pdPods(engine, prefillPort string, extraPrefillLabels map[string]string) []*corev1.Pod {
	prefillLabels := map[string]string{
		"roleset-name":             "rs-0",
		"role-name":                "prefill",
		constants.ModelLabelEngine: engine,
		constants.ModelLabelPort:   prefillPort,
	}
	for k, v := range extraPrefillLabels {
		prefillLabels[k] = v
	}
	decodeLabels := map[string]string{
		"roleset-name":             "rs-0",
		"role-name":                "decode",
		constants.ModelLabelEngine: engine,
	}
	return []*corev1.Pod{
		readyPod("prefill-0", "127.0.0.1", prefillLabels),
		readyPod("decode-0", "10.0.0.3", decodeLabels),
	}
}

// decodeBody returns the request body the gateway hands back to Envoy for the
// decode pod.
func decodeBody(fixture *gatewayFixture) []byte {
	return findRequestBodyResponse(fixture.stream.responses()).GetBodyMutation().GetBody()
}

func raw(body []byte, path string) string { return gjson.GetBytes(body, path).Raw }

// pdDupBody builds a minimal chat request whose top level ends with the given
// (duplicated) key/value fragment.
func pdDupBody(fragment string) string {
	return `{"model":"llama2-7b","messages":[{"role":"user","content":"hi"}],` + fragment + `}`
}

// expectNestedIdentical asserts the nested client fields survived the gateway
// byte-for-byte, and that the gateway-written keys appear exactly once.
func expectNestedIdentical(original, got []byte, what string) {
	Expect(gjson.ValidBytes(got)).To(BeTrue(), "%s is not valid JSON: %s", what, got)
	for _, path := range []string{"messages", "tools", "model", "temperature"} {
		Expect(raw(got, path)).To(Equal(raw(original, path)), "%s: %s must be byte-identical", what, path)
	}
	seen := map[string]int{}
	gjson.ParseBytes(got).ForEach(func(key, _ gjson.Result) bool {
		seen[key.String()]++
		return true
	})
	for key, n := range seen {
		Expect(n).To(Equal(1), "%s: top-level key %q appears %d times", what, key, n)
	}
}

func expectPrefillControlFields(prefillBody []byte) {
	Expect(gjson.GetBytes(prefillBody, "max_tokens").Int()).To(Equal(int64(1)))
	Expect(gjson.GetBytes(prefillBody, "stream").Bool()).To(BeFalse())
	Expect(gjson.GetBytes(prefillBody, "stream_options").Exists()).To(BeFalse())
	Expect(gjson.GetBytes(prefillBody, "min_tokens").Exists()).To(BeFalse())
}

func runPD(
	engine string, prefill *fakePrefillServer, extraPrefillLabels map[string]string, body string,
) *gatewayFixture {
	pods := pdPods(engine, prefill.port(), extraPrefillLabels)
	fixture := newGatewayFixtureWithRequestBody(pods, "pd", "", "", []byte(body))
	Expect(fixture.run()).To(Succeed(), fixture.diagnostics())
	bodyResponse := findRequestBodyResponse(fixture.stream.responses())
	expectHeader(bodyResponse.GetHeaderMutation().GetSetHeaders(), "routing-strategy", "pd")
	expectHeader(bodyResponse.GetHeaderMutation().GetSetHeaders(), "target-pod", "10.0.0.3:8000")
	expectHeader(bodyResponse.GetHeaderMutation().GetSetHeaders(),
		"content-length", strconv.Itoa(len(decodeBody(fixture))))
	expectSuccessfulLifecycle(fixture)
	return fixture
}

var _ = Describe("Gateway PD request body integrity", Label("gateway", "integration"), func() {
	It("preserves nested fields on the vLLM SHFS prefill and decode bodies", func() {
		prefill := newFakePrefillServer(`{"id":"p","choices":[],` +
			`"kv_transfer_params":{"do_remote_decode":false,"do_remote_prefill":true,"remote_engine_id":"engine-a",` +
			`"remote_block_ids":[9007199254740993,2,3],"remote_host":null,"remote_port":5555}}`)

		fixture := runPD("vllm", prefill, nil, pdClientBody)

		prefillBodies := prefill.receivedBodies()
		Expect(prefillBodies).To(HaveLen(1), fixture.diagnostics())
		expectNestedIdentical([]byte(pdClientBody), prefillBodies[0], "prefill body")
		expectPrefillControlFields(prefillBodies[0])
		Expect(gjson.GetBytes(prefillBodies[0], "max_completion_tokens").Int()).To(Equal(int64(1)))
		Expect(raw(prefillBodies[0], "kv_transfer_params")).To(Equal(
			`{"do_remote_decode":true,"do_remote_prefill":false,"remote_engine_id":null,` +
				`"remote_block_ids":null,"remote_host":null,"remote_port":null}`))

		decode := decodeBody(fixture)
		expectNestedIdentical([]byte(pdClientBody), decode, "decode body")
		Expect(gjson.GetBytes(decode, "max_tokens").Int()).To(Equal(int64(128)),
			"decode keeps the client's generation settings")
		Expect(gjson.GetBytes(decode, "kv_transfer_params.remote_host").String()).To(Equal("127.0.0.1"))
		Expect(raw(decode, "kv_transfer_params.remote_block_ids")).To(Equal(`[9007199254740993,2,3]`),
			"large ints must not pass through float64")
		Expect(gjson.GetBytes(decode, "kv_transfer_params.remote_engine_id").String()).To(Equal("engine-a"))
		Expect(gjson.GetBytes(decode, "kv_transfer_params.remote_port").Int()).To(Equal(int64(5555)))
		Expect(gjson.GetBytes(decode, "kv_transfer_params.do_remote_prefill").Bool()).To(BeTrue())
	})

	It("wraps the whole prefill response for vLLM NIXL without touching nested fields", func() {
		const prefillResponse = `{"id":"p","choices":[],"nixl":{"z":1,"a":[9007199254740993]}}`
		prefill := newFakePrefillServer(prefillResponse)

		fixture := runPD("vllm", prefill, map[string]string{"model.aibrix.ai/kv-connector-type": "nixl"}, pdClientBody)

		prefillBodies := prefill.receivedBodies()
		Expect(prefillBodies).To(HaveLen(1), fixture.diagnostics())
		expectNestedIdentical([]byte(pdClientBody), prefillBodies[0], "prefill body")
		expectPrefillControlFields(prefillBodies[0])
		Expect(gjson.GetBytes(prefillBodies[0], "kv_transfer_params").Exists()).To(BeFalse())

		decode := decodeBody(fixture)
		expectNestedIdentical([]byte(pdClientBody), decode, "decode body")
		Expect(raw(decode, "disagg_prefill_resp")).To(Equal(prefillResponse), "prefill response must be embedded verbatim")
	})

	It("preserves nested fields and large ints on the TensorRT-LLM bodies", func() {
		prefill := newFakePrefillServer(`{"id":"p","choices":[{"index":0,"disaggregated_params":` +
			`{"request_type":"context_only","ctx_request_id":9007199254740993,` +
			`"encoded_opaque_state":"AQID","draft_tokens":null}}],` +
			`"prompt_token_ids":[1,2,3]}`)

		fixture := runPD("trtllm", prefill, nil, pdClientBody)

		prefillBodies := prefill.receivedBodies()
		Expect(prefillBodies).To(HaveLen(1), fixture.diagnostics())
		expectNestedIdentical([]byte(pdClientBody), prefillBodies[0], "prefill body")
		expectPrefillControlFields(prefillBodies[0])
		Expect(gjson.GetBytes(prefillBodies[0], "max_completion_tokens").Exists()).To(BeFalse(),
			"TRT-LLM only accepts max_tokens")
		Expect(gjson.GetBytes(prefillBodies[0], "disaggregated_params.request_type").String()).To(Equal("context_only"))
		disaggID := gjson.GetBytes(prefillBodies[0], "disaggregated_params.disagg_request_id")
		Expect(disaggID.Int()).To(BeNumerically(">=", int64(1)<<42))
		Expect(disaggID.Raw).To(Equal(strconv.FormatInt(disaggID.Int(), 10)), "disagg_request_id must be an integer literal")

		decode := decodeBody(fixture)
		expectNestedIdentical([]byte(pdClientBody), decode, "decode body")
		Expect(gjson.GetBytes(decode, "disaggregated_params.request_type").String()).To(Equal("generation_only"))
		Expect(raw(decode, "disaggregated_params.ctx_request_id")).To(Equal("9007199254740993"))
		Expect(gjson.GetBytes(decode, "disaggregated_params.encoded_opaque_state").String()).To(Equal("AQID"))
		Expect(raw(decode, "prompt_token_ids")).To(Equal("[1,2,3]"), "chat completions carry prompt_token_ids into decode")
	})

	It("shares SGLang bootstrap fields between the decode body and the async prefill body", func() {
		prefill := newFakePrefillServer(`{"id":"p","choices":[]}`)

		fixture := runPD("sglang", prefill, nil, pdClientBody)

		decode := decodeBody(fixture)
		expectNestedIdentical([]byte(pdClientBody), decode, "decode body")
		Expect(gjson.GetBytes(decode, "bootstrap_host").String()).To(Equal("127.0.0.1"))
		Expect(gjson.GetBytes(decode, "bootstrap_port").Int()).To(Equal(int64(8998)))
		room := gjson.GetBytes(decode, "bootstrap_room")
		Expect(room.Int()).To(BeNumerically(">", 0))
		Expect(gjson.GetBytes(decode, "max_tokens").Int()).To(Equal(int64(128)))

		// SGLang prefill is fired asynchronously; wait for it to land.
		Eventually(prefill.receivedBodies).Should(HaveLen(1), fixture.diagnostics())
		prefillBody := prefill.receivedBodies()[0]
		expectNestedIdentical([]byte(pdClientBody), prefillBody, "prefill body")
		expectPrefillControlFields(prefillBody)
		Expect(raw(prefillBody, "bootstrap_room")).To(Equal(room.Raw), "prefill and decode must share one bootstrap_room")
		Expect(gjson.GetBytes(prefillBody, "bootstrap_host").String()).To(Equal("127.0.0.1"))
	})

	DescribeTable("rejects a duplicated gateway-controlled key with 400 before any prefill call",
		func(engine, body, key string) {
			prefill := newFakePrefillServer(`{}`)
			fixture := newGatewayFixtureWithRequestBody(pdPods(engine, prefill.port(), nil), "pd", "", "", []byte(body))
			// The request is rejected at the request-body phase, so Envoy never
			// sends the response phases.
			fixture.stream = newFakeProcessStream(
				context.Background(),
				requestHeadersRequest(fixture.requestID, "pd", "", ""),
				requestBodyRequestWithBody([]byte(body)),
			)

			err := fixture.run()
			Expect(err).To(HaveOccurred(), fixture.diagnostics())
			Expect(errors.Is(err, io.EOF)).To(BeTrue(), fixture.diagnostics())
			immediateResponse := findResponseWithImmediate(fixture.stream.responses())
			Expect(immediateResponse).NotTo(BeNil(), fixture.diagnostics())
			immediate := immediateResponse.GetImmediateResponse()
			Expect(immediate.GetStatus().GetCode()).To(Equal(envoyTypePb.StatusCode_BadRequest), fixture.diagnostics())
			Expect(gjson.Get(immediate.GetBody(), "error.message").String()).To(
				Equal(`duplicate top-level key "`+key+`" in request body`), fixture.diagnostics())
			Expect(gjson.Get(immediate.GetBody(), "error.type").String()).To(
				Equal("invalid_request_error"), fixture.diagnostics())
			Expect(prefill.receivedBodies()).To(BeEmpty(), "prefill pod must not be called")
			expectErrorLifecycle(fixture)
		},
		Entry("vLLM: max_tokens", "vllm",
			pdDupBody(`"max_tokens":1,"max_tokens":2`), "max_tokens"),
		Entry("vLLM: kv_transfer_params", "vllm",
			pdDupBody(`"kv_transfer_params":{},"kv_transfer_params":{"remote_host":"x"}`), "kv_transfer_params"),
		Entry("TensorRT-LLM: disaggregated_params", "trtllm",
			pdDupBody(`"disaggregated_params":{},"disaggregated_params":{}`), "disaggregated_params"),
		Entry("SGLang: bootstrap_room", "sglang",
			pdDupBody(`"bootstrap_room":1,"bootstrap_room":2`), "bootstrap_room"),
	)
})
