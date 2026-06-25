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

// ValidateDynamoReleaseTag verifies that version is a stable Dynamo release tag
// and that the exact tag exists in the upstream Dynamo repository.
func ValidateDynamoReleaseTag(ctx context.Context, version string) error {
	return validateDynamoReleaseTag(ctx, execCommandRunner{}, version)
}

func validateDynamoReleaseTag(ctx context.Context, runner commandRunner, version string) error {
	version = strings.TrimSpace(version)
	if !stableDynamoReleaseTagPattern.MatchString(version) {
		return fmt.Errorf("invalid Dynamo stable release tag %q: expected vMAJOR.MINOR.PATCH", version)
	}

	output, err := runner.Run(ctx, "git", "ls-remote", "--tags", "--refs", dynamoRepoURL, "refs/tags/"+version)
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return fmt.Errorf("failed to query Dynamo release tag %s: %w: %s", version, err, output)
		}
		return fmt.Errorf("failed to query Dynamo release tag %s: %w", version, err)
	}
	if !lsRemoteOutputHasExactTag(output, version) {
		return fmt.Errorf("Dynamo release tag %s not found in ai-dynamo/dynamo", version)
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
