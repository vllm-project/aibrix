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
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"
)

// fileHTTPClientTimeout bounds proxy requests to the metadata service so a slow
// or hung upstream can't pin file handler goroutines indefinitely.
const fileHTTPClientTimeout = 60 * time.Second

// FileHandler proxies file operations to the AIBrix metadata service.
type FileHandler struct {
	metadataServiceURL string
	httpClient         *http.Client
	store              store.Store
}

// NewFileHandler creates a new file proxy handler.
func NewFileHandler(metadataServiceURL string, stores ...store.Store) *FileHandler {
	var st store.Store
	if len(stores) > 0 {
		st = stores[0]
	}
	return &FileHandler{
		metadataServiceURL: strings.TrimRight(metadataServiceURL, "/"),
		httpClient:         &http.Client{Timeout: fileHTTPClientTimeout},
		store:              st,
	}
}

// RegisterRoutes registers file proxy routes on the given ServeMux.
func (h *FileHandler) RegisterRoutes(mux *runtime.ServeMux) {
	if err := mux.HandlePath("POST", "/api/v1/files/upload", h.handleUpload); err != nil {
		klog.Fatalf("Failed to register file upload route: %v", err)
	}
	if err := mux.HandlePath("GET", "/api/v1/files", h.handleList); err != nil {
		klog.Fatalf("Failed to register file list route: %v", err)
	}
	if err := mux.HandlePath("GET", "/api/v1/files/{file_id}", h.handleGetMetadata); err != nil {
		klog.Fatalf("Failed to register file metadata route: %v", err)
	}
	if err := mux.HandlePath("GET", "/api/v1/files/{file_id}/content", h.handleDownloadContent); err != nil {
		klog.Fatalf("Failed to register file download route: %v", err)
	}
}

// handleUpload proxies multipart file uploads to POST {metadataServiceURL}/v1/files.
func (h *FileHandler) handleUpload(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	targetURL := h.metadataServiceURL + "/v1/files"
	h.proxyRequest(w, r, "POST", targetURL)
}

// handleList proxies file listing to GET {metadataServiceURL}/v1/files.
func (h *FileHandler) handleList(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	targetURL := h.metadataServiceURL + "/v1/files"
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	h.proxyRequest(w, r, "GET", targetURL)
}

// handleGetMetadata proxies file metadata retrieval to GET {metadataServiceURL}/v1/files/{file_id}.
func (h *FileHandler) handleGetMetadata(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	fileID := pathParams["file_id"]
	targetURL := fmt.Sprintf("%s/v1/files/%s", h.metadataServiceURL, fileID)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	h.proxyRequest(w, r, "GET", targetURL)
}

// handleDownloadContent proxies file content download to GET {metadataServiceURL}/v1/files/{file_id}/content.
func (h *FileHandler) handleDownloadContent(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	fileID := pathParams["file_id"]
	if err := h.authorizeFileDownload(r.Context(), fileID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.message), err.status)
		return
	}
	targetURL := fmt.Sprintf("%s/v1/files/%s/content", h.metadataServiceURL, fileID)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	h.proxyRequest(w, r, "GET", targetURL)
}

type fileAuthorizationError struct {
	status  int
	message string
}

func (h *FileHandler) authorizeFileDownload(ctx context.Context, fileID string) *fileAuthorizationError {
	if h.store == nil || fileID == "" {
		return nil
	}
	jobs, err := h.store.ListJobsByDatasetID(ctx, fileID)
	if err != nil {
		klog.Errorf("authorize file download %s: %v", fileID, err)
		return &fileAuthorizationError{
			status:  http.StatusInternalServerError,
			message: "failed to authorize file download",
		}
	}
	if len(jobs) == 0 {
		return nil
	}

	viewer := ""
	if user := middleware.GetUser(ctx); user != nil {
		viewer = strings.TrimSpace(user.Email)
	}
	if viewer == "" {
		return &fileAuthorizationError{
			status:  http.StatusForbidden,
			message: "only the job owner can download this file",
		}
	}

	hasOwnedJob := false
	for _, job := range jobs {
		if job == nil {
			continue
		}
		owner := strings.TrimSpace(job.CreatedBy)
		if owner == "" {
			continue
		}
		hasOwnedJob = true
		if strings.EqualFold(owner, viewer) {
			return nil
		}
	}
	if !hasOwnedJob {
		return nil
	}
	return &fileAuthorizationError{
		status:  http.StatusForbidden,
		message: "only the job owner can download this file",
	}
}

// proxyRequest forwards an HTTP request to the target URL and copies the response back.
func (h *FileHandler) proxyRequest(w http.ResponseWriter, r *http.Request, method, targetURL string) {
	proxyReq, err := http.NewRequestWithContext(r.Context(), method, targetURL, r.Body)
	if err != nil {
		klog.Errorf("Failed to create proxy request: %v", err)
		http.Error(w, `{"error":"failed to create proxy request"}`, http.StatusInternalServerError)
		return
	}

	// Forward Content-Type (important for multipart uploads)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		proxyReq.Header.Set("Content-Type", ct)
	}

	// Forward auth headers
	if userID := r.Header.Get("X-User-ID"); userID != "" {
		proxyReq.Header.Set("X-User-ID", userID)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		proxyReq.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(proxyReq)
	if err != nil {
		klog.Errorf("Failed to proxy to metadata service: %v", err)
		http.Error(w, `{"error":"metadata service unreachable"}`, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Copy response headers
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Copy response body
	if _, err := io.Copy(w, resp.Body); err != nil {
		klog.Errorf("Error copying response body: %v", err)
	}
}
