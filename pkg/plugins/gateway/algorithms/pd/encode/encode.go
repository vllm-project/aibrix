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
	"net/http"
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
// response. It returns the failures joined, or nil when every item was encoded.
func (e *Executor) Execute(routingCtx *types.RoutingContext, encoders []*v1.Pod, items []string, timeout time.Duration) error {
	if len(encoders) == 0 || len(items) == 0 {
		return nil
	}
	start := e.next.Add(uint64(len(items))) - uint64(len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		pod := encoders[(start+uint64(i))%uint64(len(encoders))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.encode(routingCtx, pod, i, item, timeout); err != nil {
				errs[i] = fmt.Errorf("encode item %d on pod %s: %w", i, pod.Name, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (e *Executor) encode(routingCtx *types.RoutingContext, pod *v1.Pod, index int, item string, timeout time.Duration) error {
	payload, err := requestBody(routingCtx.ReqBody, item)
	if err != nil {
		return fmt.Errorf("build encode request: %w", err)
	}
	url := fmt.Sprintf("http://%s:%d/v1/chat/completions",
		pod.Status.PodIP, utils.GetModelPortForPod(routingCtx.RequestID, pod))

	ctx, cancel := context.WithTimeout(routingCtx.Context, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create encode request: %w", err)
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
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		emit(routingCtx, "read_error", "200")
		return fmt.Errorf("read encode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		status, code := metrics.HttpFailureStatusCode(ctx, nil, resp)
		emit(routingCtx, status, code)
		return fmt.Errorf("encoder returned %d: %s", resp.StatusCode, body)
	}
	emit(routingCtx, "success", "200")
	return nil
}

func emit(routingCtx *types.RoutingContext, status, code string) {
	metrics.EmitMetricToPrometheus(routingCtx, nil, metrics.GatewayPDEncodeRequestTotal, &metrics.SimpleMetricValue{Value: 1.0},
		map[string]string{"status": status, "status_code": code})
}
