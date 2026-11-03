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

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/vllm-project/aibrix/apps/console/api/store"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
)

func TestFileSummary(t *testing.T) {
	request := `{"method":"POST","url":"/v1/chat/completions","body":{"model":"model-a",` +
		`"messages":[{"role":"user","content":"private prompt"}]}}`
	tests := []struct {
		name      string
		content   string
		requests  int
		invalid   int
		truncated bool
		models    []string
		endpoints []string
	}{
		{name: "empty", models: []string{}, endpoints: []string{}},
		{
			name: "blank lines and final line without newline", content: "\r\n" + request + "\r\n\n" + request,
			requests: 2, models: []string{"model-a"}, endpoints: []string{"/v1/chat/completions"},
		},
		{
			name:     "multiple models and endpoints",
			content:  request + "\n" + `{"method":"POST","url":"/v1/embeddings","body":{"model":"model-b"}}`,
			requests: 2, models: []string{"model-a", "model-b"},
			endpoints: []string{"/v1/chat/completions", "/v1/embeddings"},
		},
		{
			name: "invalid records counted without leaking content", content: request + "\nnot json\nnull\n[]\n{}\n",
			requests: 5, invalid: 4, models: []string{"model-a"}, endpoints: []string{"/v1/chat/completions"},
		},
		{
			name: "large single request",
			content: fmt.Sprintf(`{"method":"POST","url":"/v1/chat/completions",`+
				`"body":{"model":"model-a","prompt":"%s"}}`, strings.Repeat("x", 100000)),
			requests: 1, models: []string{"model-a"}, endpoints: []string{"/v1/chat/completions"},
		},
		{
			name: "exact preview limit", content: request + strings.Repeat(" ", 8*1024*1024-len(request)),
			requests: 1, models: []string{"model-a"}, endpoints: []string{"/v1/chat/completions"},
		},
		{
			name: "bounded preview excludes partial last record", content: request + "\n" + strings.Repeat("x", 8*1024*1024),
			requests: 1, truncated: true, models: []string{"model-a"}, endpoints: []string{"/v1/chat/completions"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files/file-1" {
					// The oversized-content fixture deliberately models stale metadata;
					// the content-read cap must remain a second line of defense.
					size := min(len(tt.content), 8*1024*1024)
					_, _ = fmt.Fprintf(w, `{"bytes":%d}`, size)
					return
				}
				if r.URL.Path != "/v1/files/file-1/content" || r.Header.Get("X-User-ID") != "alice" {
					t.Errorf("incorrect upstream request: %s user=%s", r.URL.Path, r.Header.Get("X-User-ID"))
				}
				_, _ = w.Write([]byte(tt.content))
			}))
			defer upstream.Close()
			h := NewFileHandler(upstream.URL, testFileUploadTimeout, nil)
			mux := runtime.NewServeMux()
			h.RegisterRoutes(mux)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil)
			req.Header.Set("X-User-ID", "alice")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				RequestCount int      `json:"request_count"`
				InvalidLines int      `json:"invalid_lines"`
				Truncated    bool     `json:"truncated"`
				Models       []string `json:"models"`
				Endpoints    []string `json:"endpoints"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.RequestCount != tt.requests || got.InvalidLines != tt.invalid || got.Truncated != tt.truncated ||
				!reflect.DeepEqual(got.Models, tt.models) || !reflect.DeepEqual(got.Endpoints, tt.endpoints) {
				t.Fatalf("unexpected summary: %+v", got)
			}
			if strings.Contains(rec.Body.String(), "private prompt") {
				t.Fatal("summary contains request content")
			}
		})
	}
}

func TestFileSummaryRejectsPathTraversal(t *testing.T) {
	h := NewFileHandler("http://metadata.invalid", testFileUploadTimeout, nil)
	mux := runtime.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/files/../summary", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFileSummaryChecksSizeBeforeDownload(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata string
		status   int
	}{
		{"oversized", `{"bytes":8388609}`, http.StatusRequestEntityTooLarge},
		{"missing size", `{}`, http.StatusBadGateway},
		{"null size", `{"bytes":null}`, http.StatusBadGateway},
		{"negative size", `{"bytes":-1}`, http.StatusBadGateway},
		{"non-numeric size", `{"bytes":"1"}`, http.StatusBadGateway},
	} {
		t.Run(tt.name, func(t *testing.T) {
			contentRequested := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/files/file-1" {
					_, _ = w.Write([]byte(tt.metadata))
					return
				}
				contentRequested <- struct{}{}
				_, _ = w.Write([]byte(`{"method":"POST","url":"/v1/embeddings","body":{"model":"model-a"}}`))
			}))
			defer upstream.Close()
			h := NewFileHandler(upstream.URL, testFileUploadTimeout, nil)
			mux := runtime.NewServeMux()
			h.RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil))
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tt.status, rec.Body.String())
			}
			select {
			case <-contentRequested:
				t.Fatal("preview downloaded content without a safe known size")
			default:
			}
		})
	}
}

func TestFileSummaryRejectsNonOwner(t *testing.T) {
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })
	job := &models.Job{ID: "owned-job", InputDataset: "file-1", CreatedBy: "alice@example.com"}
	if err := s.UpsertJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h := NewFileHandler("http://metadata.invalid", testFileUploadTimeout, nil, s)
	mux := runtime.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil)
	req = requestWithUserEmail(req, "bob@example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFileSummaryUpstreamErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private upstream details"))
			}))
			defer upstream.Close()
			h := NewFileHandler(upstream.URL, testFileUploadTimeout, nil)
			mux := runtime.NewServeMux()
			h.RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil))
			if rec.Code != status || strings.Contains(rec.Body.String(), "private upstream details") {
				t.Fatalf("unexpected upstream error response: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFileSummaryRejectsIncompleteUpstreamBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/files/file-1" {
			_, _ = w.Write([]byte(`{"bytes":1000}`))
			return
		}
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"method":"POST"}`))
	}))
	defer upstream.Close()
	h := NewFileHandler(upstream.URL, testFileUploadTimeout, nil)
	mux := runtime.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFileSummaryPropagatesCancellation(t *testing.T) {
	h := NewFileHandler("http://metadata.invalid", testFileUploadTimeout, nil)
	started := make(chan struct{})
	h.httpClient.Transport = &contextBlockingTransport{requestStarted: started}
	mux := runtime.NewServeMux()
	h.RegisterRoutes(mux)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/summary", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(rec, req)
	}()
	select {
	case <-started:
	case <-time.After(testCancellationWait):
		t.Fatal("summary request did not reach metadata service")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testCancellationWait):
		t.Fatal("summary request was not cancelled")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}
