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

// Package encode runs the encode stage of Encode/Prefill/Decode (EPD)
// disaggregated serving: the media items of a request are encoded on dedicated
// encoder instances before the request goes to prefill.
package encode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// mediaTypes are the chat content item types vLLM can encode on an encoder
// instance.
var mediaTypes = map[string]struct{}{
	"image_url":   {},
	"audio_url":   {},
	"input_audio": {},
	"video_url":   {},
}

// passthroughFields change how media is loaded or processed, so the encoder
// has to see them to produce the same output the prefill instance expects.
var passthroughFields = []string{"mm_processor_kwargs", "media_io_kwargs"}

// ECTransferParamsField holds the encoder's handles on its outputs, keyed by
// mm_hash. ECCPUConnector with NIXL needs it to pull them.
const ECTransferParamsField = "ec_transfer_params"

const maxEncodeResponseBytes = 1 << 20

// MediaItems returns the raw JSON of every media item in a chat completions
// request body, in message order.
func MediaItems(body []byte) []string {
	var items []string
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, item gjson.Result) bool {
			if _, ok := mediaTypes[item.Get("type").String()]; ok {
				items = append(items, item.Raw)
			}
			return true
		})
		return true
	})
	return items
}

// requestBody builds the encoder request for one media item, the same shape
// vLLM's reference EPD proxy sends: a single user message with only that item.
// The item is copied byte for byte, so the encoder hashes exactly what the
// prefill instance will hash when it looks the output up.
func requestBody(body []byte, item string) ([]byte, error) {
	out := []byte(`{}`)
	var err error
	if model := gjson.GetBytes(body, "model"); model.Exists() {
		if out, err = sjson.SetRawBytes(out, "model", []byte(model.Raw)); err != nil {
			return nil, err
		}
	}
	messages := `[{"role":"user","content":[` + item + `]}]`
	if out, err = sjson.SetRawBytes(out, "messages", []byte(messages)); err != nil {
		return nil, err
	}
	if out, err = sjson.SetBytes(out, "stream", false); err != nil {
		return nil, err
	}
	// An encoder started with --mm-encoder-only stops after the prompt anyway;
	// without it (or before vLLM 0.29) this keeps it from generating.
	if out, err = sjson.SetBytes(out, "max_tokens", 1); err != nil {
		return nil, err
	}
	for _, field := range passthroughFields {
		if v := gjson.GetBytes(body, field); v.Exists() {
			if out, err = sjson.SetRawBytes(out, field, []byte(v.Raw)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Executor sends media items to encoder pods.
type Executor struct {
	httpClient *http.Client
	// next spreads items over the encoders across requests, so single-item
	// requests don't all land on the first encoder.
	next atomic.Uint64
}

func NewExecutor(httpClient *http.Client) *Executor {
	return &Executor{httpClient: httpClient}
}

// Execute sends each item to one of encoders, all at once, and waits for every
// response. It returns the merged ec_transfer_params (nil if none) and the
// failures joined.
func (e *Executor) Execute(routingCtx *types.RoutingContext, encoders []*v1.Pod, items []string, timeout time.Duration) ([]byte, error) {
	if len(encoders) == 0 || len(items) == 0 {
		return nil, nil
	}
	start := e.next.Add(uint64(len(items))) - uint64(len(items))
	errs := make([]error, len(items))
	params := make([]gjson.Result, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		pod := encoders[(start+uint64(i))%uint64(len(encoders))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := e.encode(routingCtx, pod, i, item, timeout)
			if err != nil {
				errs[i] = fmt.Errorf("encode item %d on pod %s: %w", i, pod.Name, err)
			}
			params[i] = p
		}()
	}
	wg.Wait()
	return mergeTransferParams(params), errors.Join(errs...)
}

// A repeated item has the same mm_hash, so the first entry wins.
func mergeTransferParams(params []gjson.Result) []byte {
	var out []byte
	seen := map[string]struct{}{}
	for _, p := range params {
		if !p.IsObject() {
			continue
		}
		p.ForEach(func(key, value gjson.Result) bool {
			if _, ok := seen[key.String()]; ok {
				return true
			}
			seen[key.String()] = struct{}{}
			if out == nil {
				out = []byte{'{'}
			} else {
				out = append(out, ',')
			}
			out = append(out, key.Raw...)
			out = append(out, ':')
			out = append(out, value.Raw...)
			return true
		})
	}
	if out == nil {
		return nil
	}
	return append(out, '}')
}

// Drops every copy the client sent, in one pass: sjson only edits the first,
// and with a repeated key vLLM takes the last one.
func setTransferParams(body, params []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("request body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, errors.New("request body is not a JSON object")
	}
	if !root.Get(ECTransferParamsField).Exists() {
		if params == nil {
			return body, nil
		}
		return sjson.SetRawBytes(body, ECTransferParamsField, params)
	}
	out := make([]byte, 0, len(body)+len(params))
	out = append(out, '{')
	root.ForEach(func(key, value gjson.Result) bool {
		if key.String() == ECTransferParamsField {
			return true
		}
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, key.Raw...)
		out = append(out, ':')
		out = append(out, value.Raw...)
		return true
	})
	if params != nil {
		if len(out) > 1 {
			out = append(out, ',')
		}
		out = append(out, `"`+ECTransferParamsField+`":`...)
		out = append(out, params...)
	}
	return append(out, '}'), nil
}

func (e *Executor) encode(routingCtx *types.RoutingContext, pod *v1.Pod, index int, item string, timeout time.Duration) (gjson.Result, error) {
	if pod == nil {
		return gjson.Result{}, errors.New("encode pod is nil")
	}
	if pod.Status.PodIP == "" {
		return gjson.Result{}, fmt.Errorf("encode pod %s has no IP address", pod.Name)
	}
	payload, err := requestBody(routingCtx.ReqBody, item)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("build encode request: %w", err)
	}
	url := "http://" + net.JoinHostPort(
		pod.Status.PodIP,
		strconv.FormatInt(utils.GetModelPortForPod(routingCtx.RequestID, pod), 10),
	) + "/v1/chat/completions"

	ctx, cancel := context.WithTimeout(routingCtx.Context, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return gjson.Result{}, fmt.Errorf("create encode request: %w", err)
	}
	for key, value := range routingCtx.ReqHeaders {
		// Envoy pseudo-headers are not valid HTTP/1 field names.
		if key == "" || strings.HasPrefix(key, ":") {
			continue
		}
		req.Header.Set(key, value)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Request-Id", fmt.Sprintf("%s-encode-%d", routingCtx.RequestID, index))

	resp, err := e.httpClient.Do(req)
	if err != nil {
		status, code := metrics.HttpFailureStatusCode(ctx, err, nil)
		emit(routingCtx, status, code)
		return gjson.Result{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		status, code := metrics.HttpFailureStatusCode(ctx, nil, resp)
		emit(routingCtx, status, code)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return gjson.Result{}, fmt.Errorf("encoder returned %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEncodeResponseBytes+1))
	if err == nil && len(body) > maxEncodeResponseBytes {
		err = fmt.Errorf("larger than %d bytes", maxEncodeResponseBytes)
	}
	if err != nil {
		emit(routingCtx, "read_error", "200")
		return gjson.Result{}, fmt.Errorf("read encode response: %w", err)
	}
	emit(routingCtx, "success", "200")
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, nil
	}
	return gjson.GetBytes(body, ECTransferParamsField), nil
}

func emit(routingCtx *types.RoutingContext, status, code string) {
	metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPDEncodeRequestTotal, &metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"status": status, "status_code": code})
}
