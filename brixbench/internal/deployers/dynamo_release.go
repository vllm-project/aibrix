package deployers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const dynamoRepoURL = "https://github.com/ai-dynamo/dynamo.git"

var stableDynamoReleaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// DynamoRelease describes a prepared Dynamo release checkout.
type DynamoRelease struct {
	Version   string
	RepoPath  string
	ChartPath string
}

// DynamoReleaseSource prepares Dynamo release artifacts without exposing the
// backing source details to DynamoDeployer lifecycle code.
type DynamoReleaseSource interface {
	ValidateReleaseTag(ctx context.Context, version string) error
	PrepareRelease(ctx context.Context, projectRoot string, version string) (*DynamoRelease, error)
}

// GitDynamoReleaseSource validates and prepares Dynamo releases from the
// upstream git repository.
type GitDynamoReleaseSource struct {
	runner  commandRunner
	repoURL string
}

var _ DynamoReleaseSource = (*GitDynamoReleaseSource)(nil)

// NewGitDynamoReleaseSource creates the production Dynamo release source.
func NewGitDynamoReleaseSource() *GitDynamoReleaseSource {
	return &GitDynamoReleaseSource{
		runner:  execCommandRunner{},
		repoURL: dynamoRepoURL,
	}
}

func (s *GitDynamoReleaseSource) ValidateReleaseTag(ctx context.Context, version string) error {
	return validateDynamoReleaseTag(ctx, s.runner, s.repoURL, version)
}

func (s *GitDynamoReleaseSource) PrepareRelease(ctx context.Context, projectRoot string, version string) (*DynamoRelease, error) {
	version = strings.TrimSpace(version)
	if err := s.ValidateReleaseTag(ctx, version); err != nil {
		return nil, err
	}

	repoPath := filepath.Join(projectRoot, ".tmp", "dynamo", version)
	chartPath := filepath.Join(repoPath, "deploy", "helm", "charts", "platform")
	release := &DynamoRelease{
		Version:   version,
		RepoPath:  repoPath,
		ChartPath: chartPath,
	}

	if pathExists(repoPath) {
		if !dirExists(chartPath) {
			return nil, fmt.Errorf("Dynamo release checkout %s exists but chart path %s was not found", repoPath, chartPath)
		}
		return release, nil
	}

	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create Dynamo release cache directory: %w", err)
	}
	output, err := s.runner.Run(ctx, "git", "clone", "--depth=1", "--branch", version, s.repoURL, repoPath)
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return nil, fmt.Errorf("failed to checkout Dynamo release %s: %w: %s", version, err, output)
		}
		return nil, fmt.Errorf("failed to checkout Dynamo release %s: %w", version, err)
	}
	if !dirExists(chartPath) {
		return nil, fmt.Errorf("Dynamo release %s chart path %s was not found after checkout", version, chartPath)
	}
	return release, nil
}

// ValidateDynamoReleaseTag verifies that version is a stable Dynamo release tag
// and that the exact tag exists in the upstream Dynamo repository.
func ValidateDynamoReleaseTag(ctx context.Context, version string) error {
	return NewGitDynamoReleaseSource().ValidateReleaseTag(ctx, version)
}

func validateDynamoReleaseTag(ctx context.Context, runner commandRunner, repoURL string, version string) error {
	version = strings.TrimSpace(version)
	if !stableDynamoReleaseTagPattern.MatchString(version) {
		return fmt.Errorf("invalid Dynamo stable release tag %q: expected vMAJOR.MINOR.PATCH", version)
	}

	repoURL = strings.TrimSpace(repoURL)
	output, err := runner.Run(ctx, "git", "ls-remote", "--tags", "--refs", repoURL, "refs/tags/"+version)
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return fmt.Errorf("failed to query Dynamo release tag %s: %w: %s", version, err, output)
		}
		return fmt.Errorf("failed to query Dynamo release tag %s: %w", version, err)
	}
	if !lsRemoteOutputHasExactTag(output, version) {
		return fmt.Errorf("Dynamo release tag %s not found in %s", version, repoURL)
	}
	return nil
}

func lsRemoteOutputHasExactTag(output string, version string) bool {
	targetRef := "refs/tags/" + version
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == targetRef {
			return true
		}
	}
	return false
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
