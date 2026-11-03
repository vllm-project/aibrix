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
	"time"
)

const maxRequestBytes = 262144

type replicaSelector interface {
	selectReplica(context.Context, ReplicaSelectionRequest) (replicaSelection, error)
}

type serviceHandler struct {
	selector replicaSelector
	logger   *log.Logger
}

func newHandler(selector replicaSelector, logger *log.Logger) http.Handler {
	return &serviceHandler{selector: selector, logger: logger}
}

func (h *serviceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	case "/v1alpha1/select":
		h.selectReplica(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *serviceHandler) selectReplica(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	started := time.Now()
	if r.ContentLength == 0 || r.ContentLength > maxRequestBytes {
		writeProblem(w, http.StatusBadRequest, "invalid body size")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil || len(body) == 0 {
		writeProblem(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var req ReplicaSelectionRequest
	if err := decodeOneJSON(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := validateRequest(req); err != nil {
		writeProblem(w, http.StatusBadRequest, err.Error())
		return
	}
	selection, err := h.selector.selectReplica(r.Context(), req)
	if err != nil {
		status, category := http.StatusBadGateway, "invalid_upstream"
		if errors.Is(err, errUpstreamUnavailable) {
			status, category = http.StatusServiceUnavailable, "upstream_unavailable"
		}
		h.logger.Printf(
			"request_id=%q candidates=%d duration_ms=%d error=%q",
			req.Metadata.RequestID,
			len(req.Spec.Candidates),
			time.Since(started).Milliseconds(),
			category,
		)
		writeProblem(w, status, http.StatusText(status))
		return
	}
	response := selectedResponse(req, selection.Candidate)
	encoded, err := json.Marshal(response)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "encode response")
		return
	}
	h.logger.Printf(
		"request_id=%q candidates=%d selected=%q confidence=%.4f duration_ms=%d error=%q",
		req.Metadata.RequestID,
		len(req.Spec.Candidates),
		selection.Candidate.ID,
		selection.Confidence,
		time.Since(started).Milliseconds(),
		"",
	)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(encoded)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func decodeOneJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeProblem(w http.ResponseWriter, status int, title string) {
	body, _ := json.Marshal(struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
	}{Title: title, Status: status})
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
