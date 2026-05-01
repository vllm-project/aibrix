package resolver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var aibrixRepoURL = "https://github.com/vllm-project/aibrix.git"

// Workspace captures the prepared local source checkout used by the prototype flow.
type Workspace struct {
	Path       string
	Ref        string
	CommitHash string
}

// PrepareWorkspace prepares the reusable `.tmp/aibrix` checkout and resolves the requested ref.
func PrepareWorkspace(ctx context.Context, projectRoot string, test *Test) (*Workspace, error) {
	if test.ProviderName() != "aibrix" {
		return nil, nil
	}

	if strings.TrimSpace(test.LocalPath) != "" {
		return prepareWorkspaceFromLocalPath(ctx, projectRoot, test)
	}

	requestedRef := requestedWorkspaceRef(test)
	if requestedRef == "" {
		return nil, nil
	}

	workspacePath := filepath.Join(projectRoot, ".tmp", "aibrix")
	if err := ensureWorkspace(ctx, workspacePath); err != nil {
		return nil, err
	}

	resolvedCommit, err := resolveWorkspaceCommit(ctx, workspacePath, requestedRef)
	if err != nil {
		return nil, err
	}
	if err := runGit(ctx, workspacePath, "checkout", "--detach", resolvedCommit); err != nil {
		return nil, fmt.Errorf("failed to checkout resolved commit %s for ref %s in %s: %w", resolvedCommit, requestedRef, workspacePath, err)
	}

	commitHash, err := resolveWorkspaceShortCommit(ctx, workspacePath)
	if err != nil {
		return nil, err
	}
	resolvedVersion, err := resolveWorkspaceVersion(ctx, workspacePath, test.Version)
	if err != nil {
		return nil, err
	}

	workspace := &Workspace{
		Path:       workspacePath,
		Ref:        resolvedCommit,
		CommitHash: commitHash,
	}
	applyResolvedWorkspace(test, workspace.Path, resolvedCommit, workspace.CommitHash, resolvedVersion)
	return workspace, nil
}

func prepareWorkspaceFromLocalPath(ctx context.Context, projectRoot string, test *Test) (*Workspace, error) {
	sourcePath, err := normalizeLocalPath(test.LocalPath)
	if err != nil {
		return nil, err
	}

	if !isGitRepository(sourcePath) {
		return nil, fmt.Errorf("localPath must point to an AIBrix git repository: %s", sourcePath)
	}

	workspacePath := filepath.Join(projectRoot, ".tmp", "aibrix-local")
	if err := stageLocalWorkspace(ctx, sourcePath, workspacePath); err != nil {
		return nil, err
	}

	fullCommit, err := resolveWorkspaceFullCommit(ctx, workspacePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve commit from localPath %s: %w", sourcePath, err)
	}
	commitHash, err := resolveWorkspaceShortCommit(ctx, workspacePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve short commit hash from localPath %s: %w", sourcePath, err)
	}
	resolvedVersion, err := resolveWorkspaceVersion(ctx, workspacePath, test.Version)
	if err != nil {
		return nil, err
	}

	test.LocalPath = sourcePath
	applyResolvedWorkspace(test, workspacePath, fullCommit, commitHash, resolvedVersion)

	fmt.Printf("[Local Path Mode] Using local AIBrix source at %s staged into %s (commit %s)\n", sourcePath, workspacePath, fullCommit)
	return &Workspace{
		Path:       workspacePath,
		Ref:        sourcePath,
		CommitHash: commitHash,
	}, nil
}

func normalizeLocalPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("localPath cannot be empty")
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve home directory for localPath %s: %w", path, err)
		}
		if trimmed == "~" {
			trimmed = homeDir
		} else {
			trimmed = filepath.Join(homeDir, strings.TrimPrefix(trimmed, "~/"))
		}
	}

	absolutePath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("failed to resolve absolute localPath %s: %w", path, err)
	}

	info, err := os.Stat(absolutePath)
	if err != nil {
		return "", fmt.Errorf("localPath not found: %s: %w", absolutePath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("localPath must be a directory: %s", absolutePath)
	}
	return absolutePath, nil
}

func requestedWorkspaceRef(test *Test) string {
	requestedRef := strings.TrimSpace(test.Commit)
	if requestedRef != "" {
		return requestedRef
	}
	return strings.TrimSpace(test.Version)
}

func isGitRepository(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func resolveWorkspaceCommit(ctx context.Context, workspacePath string, requestedRef string) (string, error) {
	resolvedCommit, err := captureGit(ctx, workspacePath, "rev-parse", requestedRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("failed to resolve ref %s in %s: %w", requestedRef, workspacePath, err)
	}
	return strings.TrimSpace(resolvedCommit), nil
}

func resolveWorkspaceFullCommit(ctx context.Context, workspacePath string) (string, error) {
	commit, err := captureGit(ctx, workspacePath, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

func resolveWorkspaceShortCommit(ctx context.Context, workspacePath string) (string, error) {
	commitHash, err := captureGit(ctx, workspacePath, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to resolve commit hash in %s: %w", workspacePath, err)
	}
	return strings.TrimSpace(commitHash), nil
}

func resolveWorkspaceVersion(ctx context.Context, workspacePath string, explicitVersion string) (string, error) {
	resolvedVersion := strings.TrimSpace(explicitVersion)
	if resolvedVersion != "" {
		return resolvedVersion, nil
	}
	return inferBaseVersionTag(ctx, workspacePath)
}

func applyResolvedWorkspace(test *Test, workspacePath string, commitRef string, resolvedCommit string, resolvedVersion string) {
	test.WorkspacePath = workspacePath
	test.Commit = commitRef
	test.ResolvedCommit = resolvedCommit
	test.ResolvedVersion = resolvedVersion
}

func stageLocalWorkspace(ctx context.Context, sourcePath string, workspacePath string) error {
	if err := os.RemoveAll(workspacePath); err != nil {
		return fmt.Errorf("failed to reset local workspace staging path %s: %w", workspacePath, err)
	}
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0755); err != nil {
		return fmt.Errorf("failed to create local workspace parent directory for %s: %w", workspacePath, err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-a", sourcePath, workspacePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to stage local workspace from %s to %s: %v, output: %s", sourcePath, workspacePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func ensureWorkspace(ctx context.Context, workspacePath string) error {
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0755); err != nil {
		return fmt.Errorf("failed to create workspace parent for %s: %w", workspacePath, err)
	}

	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		cmd := exec.CommandContext(ctx, "git", "clone", aibrixRepoURL, workspacePath)
		if output, cloneErr := cmd.CombinedOutput(); cloneErr != nil {
			return fmt.Errorf("failed to clone %s into %s: %v, output: %s", aibrixRepoURL, workspacePath, cloneErr, string(output))
		}
		return nil
	}

	if _, err := os.Stat(filepath.Join(workspacePath, ".git")); err != nil {
		return fmt.Errorf("workspace path exists but is not a git repository: %s", workspacePath)
	}

	if err := runGit(ctx, workspacePath, "fetch", "--all", "--tags"); err != nil {
		return fmt.Errorf("failed to refresh workspace %s: %w", workspacePath, err)
	}
	return nil
}

func runGit(ctx context.Context, repoPath string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v, output: %s", err, string(output))
	}
	return nil
}

func captureGit(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoPath}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func inferBaseVersionTag(ctx context.Context, repoPath string) (string, error) {
	tag, err := captureGit(ctx, repoPath, "describe", "--tags", "--abbrev=0", "--match", "v*")
	if err == nil && strings.TrimSpace(tag) != "" {
		return strings.TrimSpace(tag), nil
	}

	valuesPath := filepath.Join(repoPath, "dist", "chart", "vke.yaml")
	content, readErr := os.ReadFile(valuesPath)
	if readErr != nil {
		return "", fmt.Errorf("failed to infer base version tag from %s: %w", valuesPath, readErr)
	}

	text := string(content)
	switch {
	case strings.Contains(text, "envoyAsSideCar: true"):
		return "v0.6.0", nil
	case strings.Contains(text, "gateway:\n  envoyProxy:"):
		return "v0.5.0", nil
	default:
		return "", fmt.Errorf("failed to infer base version tag for workspace %s", repoPath)
	}
}
