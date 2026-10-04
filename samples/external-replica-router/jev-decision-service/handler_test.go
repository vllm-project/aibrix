/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type selectorFunc func(context.Context, ReplicaSelectionRequest) (replicaSelection, error)

func (f selectorFunc) selectReplica(ctx context.Context, req ReplicaSelectionRequest) (replicaSelection, error) {
	return f(ctx, req)
}

func TestHealthHandler(t *testing.T) {
	handler := newHandler(selectorFunc(nil), log.New(io.Discard, "", 0))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("GET /healthz = %d %q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d", recorder.Code)
	}
}

func TestHandlerReturnsSelectedResponse(t *testing.T) {
	req := validRequest()
	selector := selectorFunc(func(_ context.Context, got ReplicaSelectionRequest) (replicaSelection, error) {
		if got.Metadata.RequestID != req.Metadata.RequestID {
			t.Fatalf("request ID = %q", got.Metadata.RequestID)
		}
		return replicaSelection{Candidate: got.Spec.Candidates[0], Confidence: 0.64}, nil
	})
	handler := newHandler(selector, log.New(io.Discard, "", 0))
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1alpha1/select", bytes.NewReader(body)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != mediaType {
		t.Fatalf("Content-Type = %q", got)
	}
	var response ReplicaSelectionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Metadata.RequestID != req.Metadata.RequestID || response.Status.Target == nil || response.Status.Target.ID != "default/pod-a" {
		t.Fatalf("response = %#v", response)
	}
}

func TestHandlerRejectsBadRequests(t *testing.T) {
	handler := newHandler(selectorFunc(func(context.Context, ReplicaSelectionRequest) (replicaSelection, error) {
		t.Fatal("selector must not be called")
		return replicaSelection{}, nil
	}), log.New(io.Discard, "", 0))
	tests := []struct {
		name   string
		method string
		path   string
		body   io.Reader
		status int
	}{
		{"wrong method", http.MethodGet, "/v1alpha1/select", nil, http.StatusMethodNotAllowed},
		{"wrong path", http.MethodPost, "/other", strings.NewReader(`{}`), http.StatusNotFound},
		{"empty body", http.MethodPost, "/v1alpha1/select", nil, http.StatusBadRequest},
		{"malformed json", http.MethodPost, "/v1alpha1/select", strings.NewReader(`{`), http.StatusBadRequest},
		{"oversized body", http.MethodPost, "/v1alpha1/select", strings.NewReader(strings.Repeat("x", maxRequestBytes+1)), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tt.method, tt.path, tt.body))
			if recorder.Code != tt.status {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if tt.status == http.StatusBadRequest && recorder.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("Content-Type = %q", recorder.Header().Get("Content-Type"))
			}
		})
	}
}

func TestHandlerMapsJevErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"unavailable", errUpstreamUnavailable, http.StatusServiceUnavailable},
		{"invalid", errUpstreamInvalid, http.StatusBadGateway},
		{"wrapped unavailable", errors.Join(errors.New("context"), errUpstreamUnavailable), http.StatusServiceUnavailable},
	}
	body, _ := json.Marshal(validRequest())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newHandler(selectorFunc(func(context.Context, ReplicaSelectionRequest) (replicaSelection, error) {
				return replicaSelection{}, tt.err
			}), log.New(io.Discard, "", 0))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1alpha1/select", bytes.NewReader(body)))
			if recorder.Code != tt.status {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandlerKeepsConcurrentRequestsIsolated(t *testing.T) {
	selector := selectorFunc(func(_ context.Context, req ReplicaSelectionRequest) (replicaSelection, error) {
		candidate := req.Spec.Candidates[0]
		return replicaSelection{Candidate: candidate, Confidence: 0.5}, nil
	})
	handler := newHandler(selector, log.New(io.Discard, "", 0))
	const requests = 20
	errorsCh := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := validRequest()
			req.Metadata.RequestID = fmt.Sprintf("req-%d", i)
			req.Spec.Candidates[0].ID = fmt.Sprintf("default/pod-%d", i)
			body, _ := json.Marshal(req)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1alpha1/select", bytes.NewReader(body)))
			var response ReplicaSelectionResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				errorsCh <- err
				return
			}
			if response.Metadata.RequestID != req.Metadata.RequestID || response.Status.Target == nil || response.Status.Target.ID != req.Spec.Candidates[0].ID {
				errorsCh <- fmt.Errorf("request %d got %#v", i, response)
			}
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
}
