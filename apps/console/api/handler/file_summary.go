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
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/metrics"
)

// Summaries are a bounded preview, fetched only when a dataset is selected.
// Never download an entire large dataset into the browser to inspect it.
const fileSummaryMaxBytes = 8 * 1024 * 1024

type fileSummary struct {
	RequestCount int      `json:"request_count"`
	InvalidLines int      `json:"invalid_lines"`
	Models       []string `json:"models"`
	Endpoints    []string `json:"endpoints"`
	Truncated    bool     `json:"truncated"`
}

func (h *FileHandler) handleSummary(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	defer metrics.Duration(metrics.Emitter, metricConsoleFileDuration, time.Now(), metrics.T("method", "summary"))
	w.Header().Set("Cache-Control", "no-store")
	fileID := pathParams["file_id"]
	if fileID == "" || fileID == "." || fileID == ".." || strings.ContainsAny(fileID, "/\\") {
		http.Error(w, `{"error":"invalid file ID"}`, http.StatusBadRequest)
		return
	}
	if auth := h.authorizeFileDownload(r.Context(), fileID); auth != nil {
		http.Error(w, `{"error":"file access denied"}`, auth.status)
		return
	}

	// /content buffers the full object in the metadata service. Check its
	// storage-backed metadata first, so previews never initiate a large GET.
	metadata, status := h.readSummaryObject(r, fileID, "", 64*1024)
	if status != http.StatusOK {
		http.Error(w, `{"error":"dataset metadata unavailable"}`, status)
		return
	}
	var info struct {
		Bytes *int64 `json:"bytes"`
	}
	if err := json.Unmarshal(metadata, &info); err != nil || info.Bytes == nil || *info.Bytes < 0 {
		http.Error(w, `{"error":"invalid dataset metadata"}`, http.StatusBadGateway)
		return
	}
	if *info.Bytes > fileSummaryMaxBytes {
		http.Error(w, `{"error":"dataset exceeds the 8 MiB preview limit"}`, http.StatusRequestEntityTooLarge)
		return
	}
	data, status := h.readSummaryObject(r, fileID, "/content", fileSummaryMaxBytes)
	if status != http.StatusOK {
		http.Error(w, `{"error":"dataset content unavailable"}`, status)
		return
	}
	summary, err := summarizeFileData(data)
	if err != nil {
		http.Error(w, `{"error":"failed to scan dataset preview"}`, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(summary); err != nil {
		klog.ErrorS(err, "Failed to write dataset summary")
	}
}

func summarizeFileData(data []byte) (fileSummary, error) {
	summary := fileSummary{Models: []string{}, Endpoints: []string{}, Truncated: len(data) > fileSummaryMaxBytes}
	if summary.Truncated {
		data = data[:fileSummaryMaxBytes]
		// A cut-off record is neither a request we counted nor invalid JSON.
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	modelNames, endpoints := map[string]bool{}, map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), fileSummaryMaxBytes+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		summary.RequestCount++
		var row struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Body   struct {
				Model string `json:"model"`
			} `json:"body"`
		}
		if err := json.Unmarshal(line, &row); err != nil || row.Method != http.MethodPost ||
			row.URL == "" || row.Body.Model == "" {
			summary.InvalidLines++
			continue
		}
		modelNames[row.Body.Model] = true
		endpoints[row.URL] = true
	}
	if err := scanner.Err(); err != nil {
		return summary, err
	}
	for name := range modelNames {
		summary.Models = append(summary.Models, name)
	}
	for endpoint := range endpoints {
		summary.Endpoints = append(summary.Endpoints, endpoint)
	}
	sort.Strings(summary.Models)
	sort.Strings(summary.Endpoints)
	return summary, nil
}

func (h *FileHandler) readSummaryObject(r *http.Request, fileID, suffix string, limit int64) ([]byte, int) {
	target := h.metadataServiceURL + "/v1/files/" + url.PathEscape(fileID) + suffix
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	for _, name := range []string{"Authorization", "X-User-ID"} {
		if value := r.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	resp, err := h.clientForOperation("summary").Do(upstream)
	if err != nil {
		return nil, http.StatusBadGateway
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < http.StatusBadRequest {
			return nil, http.StatusBadGateway
		}
		return nil, resp.StatusCode
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, http.StatusBadGateway
	}
	return data, http.StatusOK
}
