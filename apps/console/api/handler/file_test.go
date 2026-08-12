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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	"github.com/vllm-project/aibrix/apps/console/api/store"
	"github.com/vllm-project/aibrix/apps/console/api/store/models"
)

func requestWithUserEmail(r *http.Request, email string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), middleware.UserContextKey, &middleware.UserInfo{
		Email: email,
	}))
}

func TestFileDownloadRejectsKnownNonOwner(t *testing.T) {
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })
	fileID := "file-owner-only"
	if err := s.UpsertJob(context.Background(), &models.Job{
		ID:            "job-owner-only",
		OutputDataset: fileID,
		CreatedBy:     "owner@example.com",
	}); err != nil {
		t.Fatalf("UpsertJob failed: %v", err)
	}

	handler := NewFileHandler("http://metadata.invalid", nil, s)
	req := requestWithUserEmail(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileID+"/content", nil), "other@example.com")
	rec := httptest.NewRecorder()

	handler.handleDownloadContent(rec, req, map[string]string{"file_id": fileID})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFileDownloadRejectsNonOwnerWhenFileAlsoHasUnownedJob(t *testing.T) {
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })
	fileID := "file-mixed-owner"
	for _, job := range []*models.Job{
		{ID: "job-legacy-no-owner", OutputDataset: fileID},
		{ID: "job-owned", OutputDataset: fileID, CreatedBy: "owner@example.com"},
	} {
		if err := s.UpsertJob(context.Background(), job); err != nil {
			t.Fatalf("UpsertJob failed: %v", err)
		}
	}

	handler := NewFileHandler("http://metadata.invalid", nil, s)
	req := requestWithUserEmail(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileID+"/content", nil), "other@example.com")
	rec := httptest.NewRecorder()

	handler.handleDownloadContent(rec, req, map[string]string{"file_id": fileID})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestFileDownloadAllowsOwner(t *testing.T) {
	s := store.NewMemoryStore(nil)
	t.Cleanup(func() { _ = s.Close() })
	fileID := "file-owner-download"
	if err := s.UpsertJob(context.Background(), &models.Job{
		ID:            "job-owner-download",
		OutputDataset: fileID,
		CreatedBy:     "owner@example.com",
	}); err != nil {
		t.Fatalf("UpsertJob failed: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files/"+fileID+"/content" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("download ok"))
	}))
	t.Cleanup(upstream.Close)

	handler := NewFileHandler(upstream.URL, nil, s)
	req := requestWithUserEmail(httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileID+"/content", nil), "owner@example.com")
	rec := httptest.NewRecorder()

	handler.handleDownloadContent(rec, req, map[string]string{"file_id": fileID})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "download ok" {
		t.Fatalf("body = %q, want download ok", rec.Body.String())
	}
}
