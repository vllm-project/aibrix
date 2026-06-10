package resolver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrepareWorkspaceFromLocalPathStagesCopy(t *testing.T) {
	projectRoot := t.TempDir()
	sourceRoot := filepath.Join(t.TempDir(), "aibrix")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "dist", "chart"), 0755); err != nil {
		t.Fatalf("failed to create source root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "dist", "chart", "vke.yaml"), []byte("version: v0.6.0\n"), 0644); err != nil {
		t.Fatalf("failed to write vke.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "README.md"), []byte("local workspace"), 0644); err != nil {
		t.Fatalf("failed to write README.md: %v", err)
	}

	runGitCommand(t, sourceRoot, "init")
	runGitCommand(t, sourceRoot, "config", "user.email", "test@example.com")
	runGitCommand(t, sourceRoot, "config", "user.name", "Test User")
	runGitCommand(t, sourceRoot, "add", ".")
	runGitCommand(t, sourceRoot, "commit", "-m", "init")
	runGitCommand(t, sourceRoot, "tag", "v0.6.0")

	testCase := &Test{
		Name:      "localpath",
		Provider:  stringPtr("aibrix"),
		LocalPath: sourceRoot,
	}

	workspace, err := PrepareWorkspace(context.Background(), projectRoot, testCase)
	if err != nil {
		t.Fatalf("PrepareWorkspace returned error: %v", err)
	}
	if workspace == nil {
		t.Fatalf("expected workspace to be prepared")
	}
	if workspace.Path == sourceRoot {
		t.Fatalf("expected staged workspace path to differ from source root")
	}
	if testCase.WorkspacePath != workspace.Path {
		t.Fatalf("expected test workspace path %s, got %s", workspace.Path, testCase.WorkspacePath)
	}
	if testCase.LocalPath != sourceRoot {
		t.Fatalf("expected normalized localPath %s, got %s", sourceRoot, testCase.LocalPath)
	}
	if testCase.ResolvedCommit == "" {
		t.Fatalf("expected resolved commit to be populated")
	}
	if testCase.ResolvedVersion != "v0.6.0" {
		t.Fatalf("expected resolved version v0.6.0, got %s", testCase.ResolvedVersion)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "README.md")); err != nil {
		t.Fatalf("expected staged workspace to contain copied files: %v", err)
	}
}

func runGitCommand(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v, output: %s", args, err, string(output))
	}
}

func stringPtr(value string) *string {
	return &value
}
