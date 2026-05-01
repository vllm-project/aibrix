package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestFetchArtifactsDownloadsReleaseArtifacts(t *testing.T) {
	projectRoot := t.TempDir()
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		switch r.URL.Path {
		case "/v0.6.0/aibrix-dependency-v0.6.0.yaml":
			_, _ = w.Write([]byte("kind: dependency\n"))
		case "/v0.6.0/aibrix-core-v0.6.0.yaml":
			_, _ = w.Write([]byte("kind: core\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restoreReleaseDownloadSettings(t, server.URL, server.Client())

	testCase := &Test{
		Name:     "download-release",
		Provider: stringPtr("aibrix"),
		Version:  "v0.6.0",
	}

	if err := FetchArtifacts(context.Background(), projectRoot, testCase); err != nil {
		t.Fatalf("FetchArtifacts returned error: %v", err)
	}

	expectedPaths := []string{
		filepath.Join(projectRoot, ".tmp", "releases", "v0.6.0", "aibrix-dependency-v0.6.0.yaml"),
		filepath.Join(projectRoot, ".tmp", "releases", "v0.6.0", "aibrix-core-v0.6.0.yaml"),
	}
	if len(testCase.ControlPlane) != len(expectedPaths) {
		t.Fatalf("expected %d control plane paths, got %d", len(expectedPaths), len(testCase.ControlPlane))
	}
	for i, expected := range expectedPaths {
		if testCase.ControlPlane[i] != expected {
			t.Fatalf("expected control plane path %s, got %s", expected, testCase.ControlPlane[i])
		}
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("expected downloaded artifact at %s: %v", expected, err)
		}
	}
	if requestCount != 2 {
		t.Fatalf("expected 2 release artifact requests, got %d", requestCount)
	}
}

func TestFetchArtifactsReusesCachedReleaseArtifacts(t *testing.T) {
	projectRoot := t.TempDir()
	var requestCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		_, _ = w.Write([]byte("kind: cached\n"))
	}))
	defer server.Close()

	restoreReleaseDownloadSettings(t, server.URL, server.Client())

	testCase := &Test{
		Name:     "cached-release",
		Provider: stringPtr("aibrix"),
		Version:  "v0.6.0",
	}

	if err := FetchArtifacts(context.Background(), projectRoot, testCase); err != nil {
		t.Fatalf("first FetchArtifacts returned error: %v", err)
	}
	if err := FetchArtifacts(context.Background(), projectRoot, testCase); err != nil {
		t.Fatalf("second FetchArtifacts returned error: %v", err)
	}

	if requestCount != 2 {
		t.Fatalf("expected cached artifacts to avoid re-downloading, got %d requests", requestCount)
	}
}

func restoreReleaseDownloadSettings(t *testing.T, baseURL string, client *http.Client) {
	t.Helper()

	previousBaseURL := releaseArtifactBaseURL
	previousClient := releaseArtifactClient
	releaseArtifactBaseURL = baseURL
	releaseArtifactClient = client
	t.Cleanup(func() {
		releaseArtifactBaseURL = previousBaseURL
		releaseArtifactClient = previousClient
	})
}
