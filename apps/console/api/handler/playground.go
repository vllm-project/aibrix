/*
Copyright 2025 The Aibrix Team.

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

package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"k8s.io/klog/v2"
)

// PlaygroundHandler proxies chat completion requests to the AIBrix gateway as SSE.
type PlaygroundHandler struct {
	gatewayEndpoint string
	httpClient      *http.Client
}

// NewPlaygroundHandler creates a new playground SSE proxy handler.
func NewPlaygroundHandler(gatewayEndpoint string) *PlaygroundHandler {
	return &PlaygroundHandler{
		gatewayEndpoint: strings.TrimRight(gatewayEndpoint, "/"),
		httpClient:      &http.Client{},
	}
}

// chatCompletionRequest is the request body from the frontend.
type chatCompletionRequest struct {
	Model            string    `json:"model"`
	Messages         []message `json:"messages"`
	Temperature      *float64  `json:"temperature,omitempty"`
	MaxTokens        *int      `json:"max_tokens,omitempty"`
	TopP             *float64  `json:"top_p,omitempty"`
	TopK             *int      `json:"top_k,omitempty"`
	PresencePenalty  *float64  `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64  `json:"frequency_penalty,omitempty"`
	Stop             []string  `json:"stop,omitempty"`
	Stream           bool      `json:"stream"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// HandleChatCompletion proxies the request to AIBrix gateway and streams SSE back.
func (h *PlaygroundHandler) HandleChatCompletion(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	// Parse request
	var req chatCompletionRequest
	body, err := io.ReadAll(r.Body)
	if err != nil {
		runtime.DefaultHTTPErrorHandler(r.Context(), nil, nil, w, r, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Force streaming
	req.Stream = true

	// Forward to AIBrix gateway
	reqBody, _ := json.Marshal(req)
	gatewayURL := h.gatewayEndpoint + "/v1/chat/completions"

	proxyReq, err := http.NewRequestWithContext(r.Context(), "POST", gatewayURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, `{"error":"failed to create proxy request"}`, http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")

	// Forward authorization header if present
	if auth := r.Header.Get("Authorization"); auth != "" {
		proxyReq.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(proxyReq)
	if err != nil {
		klog.Errorf("Failed to proxy to gateway: %v", err)
		http.Error(w, `{"error":"gateway unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	// Stream the response
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			if readErr != io.EOF {
				klog.Errorf("Error reading gateway response: %v", readErr)
			}
			return
		}
	}
}
