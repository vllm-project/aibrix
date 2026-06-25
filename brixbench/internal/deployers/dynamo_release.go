package deployers

import (
	"context"
	"fmt"
	"os/exec"
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

// DynamoTagSource validates Dynamo release tags without exposing the backing
// source details to DynamoDeployer lifecycle code.
type DynamoTagSource interface {
	ValidateReleaseTag(ctx context.Context, version string) error
}

// GitDynamoTagSource validates Dynamo release tags against the upstream git
// repository.
type GitDynamoTagSource struct {
	runner  commandRunner
	repoURL string
}

var _ DynamoTagSource = (*GitDynamoTagSource)(nil)

// NewGitDynamoTagSource creates the production Dynamo release tag source.
func NewGitDynamoTagSource() *GitDynamoTagSource {
	return &GitDynamoTagSource{
		runner:  execCommandRunner{},
		repoURL: dynamoRepoURL,
	}
}

func (s *GitDynamoTagSource) ValidateReleaseTag(ctx context.Context, version string) error {
	return validateDynamoReleaseTag(ctx, s.runner, s.repoURL, version)
}

// ValidateDynamoReleaseTag verifies that version is a stable Dynamo release tag
// and that the exact tag exists in the upstream Dynamo repository.
func ValidateDynamoReleaseTag(ctx context.Context, version string) error {
	return NewGitDynamoTagSource().ValidateReleaseTag(ctx, version)
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
