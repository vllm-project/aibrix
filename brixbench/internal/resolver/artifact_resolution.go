package resolver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	releaseArtifactBaseURL = "https://github.com/vllm-project/aibrix/releases/download"
	releaseArtifactClient  = &http.Client{Timeout: 2 * time.Minute}
)

// FetchArtifacts prepares the necessary control-plane files for a test.
// For version-based AIBrix scenarios, release YAMLs are downloaded on demand
// into the repository-local cache under `.tmp/releases/<version>/`.
func FetchArtifacts(ctx context.Context, projectRoot string, test *Test) error {
	if test.ProviderName() != "aibrix" {
		return nil
	}

	if err := validateFiles("gateway resource override", test.Gateway.Resources); err != nil {
		return err
	}

	switch {
	case strings.TrimSpace(test.Version) != "":
		if test.WorkspacePath != "" {
			fmt.Printf("[Workspace Mode] Using checked-out AIBrix workspace at %s (commit %s)\n", test.WorkspacePath, test.ResolvedCommit)
		}
		return downloadReleaseArtifacts(ctx, projectRoot, test)
	case test.WorkspacePath != "":
		fmt.Printf("[Workspace Mode] Using checked-out AIBrix workspace at %s (commit %s)\n", test.WorkspacePath, test.ResolvedCommit)
		return nil
	case len(test.ControlPlane) > 0:
		return useLocalControlPlane(test)
	case test.Commit != "":
		return fmt.Errorf("commit-based fetching not yet implemented: %s", test.Commit)
	default:
		return fmt.Errorf("invalid test case configuration: neither version nor custom paths provided for %s", test.Name)
	}
}

func downloadReleaseArtifacts(ctx context.Context, projectRoot string, test *Test) error {
	releasesDir := filepath.Join(projectRoot, ".tmp", "releases", test.Version)
	artifacts := []string{
		fmt.Sprintf("aibrix-dependency-%s.yaml", test.Version),
		fmt.Sprintf("aibrix-core-%s.yaml", test.Version),
	}

	localPaths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		src, err := ensureReleaseArtifact(ctx, releasesDir, test.Version, artifact)
		if err != nil {
			return err
		}
		localPaths = append(localPaths, src)
	}

	fmt.Printf("[Releases Mode] Using downloaded AIBrix artifacts for version: %s from %s\n", test.Version, releasesDir)
	test.ControlPlane = localPaths
	return nil
}

func ensureReleaseArtifact(ctx context.Context, releasesDir string, version string, artifact string) (string, error) {
	if err := os.MkdirAll(releasesDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create release artifact cache %s: %w", releasesDir, err)
	}

	dst := filepath.Join(releasesDir, artifact)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}

	url := strings.TrimRight(releaseArtifactBaseURL, "/") + "/" + version + "/" + artifact
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create release artifact request for %s: %w", url, err)
	}

	resp, err := releaseArtifactClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download release artifact %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download release artifact %s: unexpected status %s", url, resp.Status)
	}

	tmpFile, err := os.CreateTemp(releasesDir, artifact+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for %s: %w", artifact, err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write downloaded artifact %s: %w", artifact, err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file for %s: %w", artifact, err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("failed to move downloaded artifact into place for %s: %w", artifact, err)
	}
	return dst, nil
}

func useLocalControlPlane(test *Test) error {
	fmt.Printf("[Local Mode] Using custom ControlPlane YAMLs: %v\n", test.ControlPlane)
	return validateFiles("custom local", test.ControlPlane)
}

func validateFiles(kind string, files []string) error {
	for _, path := range files {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("%s file not found: %s", kind, path)
		}
	}
	return nil
}
