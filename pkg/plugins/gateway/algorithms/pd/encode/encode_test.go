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

package encode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
)

func TestEncoderRequestCarriesOneItemVerbatim(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"https://x/cat.png"},  "uuid":"u1"}`
	audio := `{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}`
	body := []byte(`{"model":"qwen-vl","stream":true,"max_tokens":64,
		"mm_processor_kwargs":{"max_pixels":1024},
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":[{"type":"text","text":"what is this?"},` + image + `]},
			{"role":"user","content":[` + audio + `]}]}`)

	items := MediaItems(body)
	require.Equal(t, []string{image, audio}, items)

	req, err := requestBody(body, items[0])
	require.NoError(t, err)
	assert.Equal(t, `"qwen-vl"`, gjson.GetBytes(req, "model").Raw)
	assert.Equal(t, "false", gjson.GetBytes(req, "stream").Raw)
	assert.Equal(t, `{"max_pixels":1024}`, gjson.GetBytes(req, "mm_processor_kwargs").Raw)
	assert.Equal(t, "user", gjson.GetBytes(req, "messages.0.role").String())
	// Byte for byte, so the encoder and the prefill instance hash the same item.
	assert.Equal(t, `[`+image+`]`, gjson.GetBytes(req, "messages.0.content").Raw)
	assert.Equal(t, "1", gjson.GetBytes(req, "max_tokens").Raw)

	assert.Empty(t, MediaItems([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)))
}

func TestEncodeRejectsPodWithoutAddress(t *testing.T) {
	executor := NewExecutor(nil)
	ctx := types.NewRoutingContext(context.Background(), "test", "qwen-vl", "", "req-1", "user")
	defer ctx.Delete()
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":"hi"}]}`)
	item := `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`

	_, err := executor.encode(ctx, nil, 0, item, time.Second)
	require.Error(t, err)

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "encoder-0"}}
	_, err = executor.encode(ctx, pod, 0, item, time.Second)
	require.Error(t, err)
}

type recordingRoundTripper struct {
	host string
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.host = req.URL.Host
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestEncodeUsesBracketedIPv6Host(t *testing.T) {
	transport := &recordingRoundTripper{}
	executor := NewExecutor(&http.Client{Transport: transport})
	ctx := types.NewRoutingContext(context.Background(), "test", "qwen-vl", "", "req-ipv6", "user")
	defer ctx.Delete()
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = []byte(`{"model":"qwen-vl","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]}]}`)
	item := `{"type":"image_url","image_url":{"url":"https://x/a.png"}}`
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "encode-0",
			Namespace: "default",
			Labels:    map[string]string{constants.ModelLabelPort: "8000"},
		},
		Status: v1.PodStatus{PodIP: "2001:db8::1"},
	}

	_, err := executor.encode(ctx, pod, 0, item, time.Second)
	require.NoError(t, err)
	if transport.host != "[2001:db8::1]:8000" {
		t.Fatalf("encode request host = %q, want %q", transport.host, "[2001:db8::1]:8000")
	}
}

// encoderStub answers each encode request with ec_transfer_params for the
// image URL it was sent, or with none when params is false.
type encoderStub struct {
	params bool
}

func (e encoderStub) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	resp := `{"choices":[]}`
	if e.params {
		url := gjson.GetBytes(body, "messages.0.content.0.image_url.url").String()
		resp = `{"choices":[],"ec_transfer_params":{"` + url + `":{"peer_host":"10.0.0.9","peer_port":5601}}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(resp)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestEncodeMediaSetsEncoderTransferParams(t *testing.T) {
	pod := func(name, role string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"roleset-name": "rs-0", "role-name": role, constants.ModelLabelPort: "8000"},
			},
			Status: v1.PodStatus{PodIP: "10.0.0.1"},
		}
	}
	target := pod("prefill-0", "prefill")
	pods := []*v1.Pod{target, pod("encode-0", "encode"), pod("encode-1", "encode")}
	// The client repeats the field; vLLM would read the last copy.
	body := `{"model":"qwen-vl","ec_transfer_params":{"x":1},"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"a"}},{"type":"image_url","image_url":{"url":"b"}}]}],` +
		`"ec_transfer_params":{"other":{"peer_host":"203.0.113.7"}}}`

	for _, tc := range []struct {
		name   string
		params bool
		want   string
		copies int
	}{
		{"encoders return params", true, `{"a":{"peer_host":"10.0.0.9","peer_port":5601},"b":{"peer_host":"10.0.0.9","peer_port":5601}}`, 1},
		{"encoders return none", false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := types.NewRoutingContext(context.Background(), "test", "qwen-vl", "", "req-ec", "user")
			defer ctx.Delete()
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(body)

			NewExecutor(&http.Client{Transport: encoderStub{params: tc.params}}).
				EncodeMedia(ctx, pods, target, time.Second)

			assert.Equal(t, tc.copies, strings.Count(string(ctx.ReqBody), `"ec_transfer_params"`))
			assert.Equal(t, tc.want, gjson.GetBytes(ctx.ReqBody, "ec_transfer_params").Raw)
			assert.Len(t, MediaItems(ctx.ReqBody), 2)
		})
	}
}

func TestSetTransferParamsDropsEveryClientCopy(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"model":"m"`)
	for i := 0; i < 50000; i++ {
		b.WriteString(`,"ec_transfer_params":{"x":1}`)
	}
	b.WriteString(`,"messages":[{"role":"user", "content":"hi"}]}`)

	start := time.Now()
	out, err := setTransferParams([]byte(b.String()), []byte(`{"a":{}}`))
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, `{"model":"m","messages":[{"role":"user", "content":"hi"}],"ec_transfer_params":{"a":{}}}`, string(out))

	out, err = setTransferParams([]byte(`{"ec_transfer_params":{"x":1}}`), nil)
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(out))

	untouched := []byte(`{ "model": "m" }`)
	out, err = setTransferParams(untouched, nil)
	require.NoError(t, err)
	assert.Equal(t, untouched, out)
}
