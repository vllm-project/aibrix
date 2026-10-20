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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaygroundForwardsGatewayErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"model unavailable"}`))
	}))
	t.Cleanup(upstream.Close)

	handler := NewPlaygroundHandler(upstream.URL)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"/models/mock","messages":[{"role":"user","content":"hi"}]}`),
	)
	handler.HandleChatCompletion(recorder, request, nil)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "model unavailable") {
		t.Fatalf("body = %q, want upstream error", got)
	}
}

func TestPlaygroundForwardsRoutingStrategyAndRetryAfter(t *testing.T) {
	var gotStrategy string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStrategy = r.Header.Get("routing-strategy")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"model qwen3-0.6b is sleeping; retry shortly"}}`))
	}))
	t.Cleanup(upstream.Close)

	handler := NewPlaygroundHandler(upstream.URL)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"qwen3-0.6b","messages":[{"role":"user","content":"hi"}]}`),
	)
	request.Header.Set("routing-strategy", " random ")
	handler.HandleChatCompletion(recorder, request, nil)

	if gotStrategy != "random" {
		t.Fatalf("gateway routing-strategy = %q, want %q", gotStrategy, "random")
	}
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "3" {
		t.Fatalf("status = %d, Retry-After = %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
}

func TestPlaygroundSendsNoRoutingStrategyByDefault(t *testing.T) {
	strategy := "unset"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		strategy = r.Header.Get("routing-strategy")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)

	handler := NewPlaygroundHandler(upstream.URL)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"/models/mock","messages":[{"role":"user","content":"hi"}]}`),
	)
	handler.HandleChatCompletion(recorder, request, nil)

	if strategy != "" {
		t.Fatalf("gateway routing-strategy = %q, want none", strategy)
	}
}
