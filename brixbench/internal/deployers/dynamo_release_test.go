package deployers

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

type fakeCommandRunner struct {
	output string
	err    error
	calls  []fakeCommandCall
}

type fakeCommandCall struct {
	name string
	args []string
}

func (r *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, fakeCommandCall{
		name: name,
		args: append([]string(nil), args...),
	})
	return r.output, r.err
}

func TestValidateDynamoReleaseTagAcceptsExactRemoteTag(t *testing.T) {
	runner := &fakeCommandRunner{
		output: "abc123\trefs/tags/v1.2.1\n",
	}

	if err := validateDynamoReleaseTag(context.Background(), runner, "v1.2.1"); err != nil {
		t.Fatalf("validateDynamoReleaseTag returned error: %v", err)
	}

	wantArgs := []string{"ls-remote", "--tags", "--refs", dynamoRepoURL, "refs/tags/v1.2.1"}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "git" {
		t.Fatalf("expected git command, got %s", runner.calls[0].name)
	}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, runner.calls[0].args)
	}
}

func TestValidateDynamoReleaseTagFromScenarioYAML(t *testing.T) {
	t.Chdir("../../benchmark")

	scenario, err := resolver.Resolve("testdata/scenarios/dynamo-hello-world.yaml")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if len(scenario.Tests) != 1 {
		t.Fatalf("expected 1 test, got %d", len(scenario.Tests))
	}
	version := scenario.Tests[0].Version
	if version != "v1.2.1" {
		t.Fatalf("expected normalized version v1.2.1, got %s", version)
	}

	runner := &fakeCommandRunner{
		output: "abc123\trefs/tags/v1.2.1\n",
	}
	if err := validateDynamoReleaseTag(context.Background(), runner, version); err != nil {
		t.Fatalf("validateDynamoReleaseTag returned error: %v", err)
	}
}

func TestValidateDynamoReleaseTagRejectsMissingRemoteTag(t *testing.T) {
	runner := &fakeCommandRunner{}

	err := validateDynamoReleaseTag(context.Background(), runner, "v1.2.1")
	if err == nil {
		t.Fatalf("expected missing tag error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestValidateDynamoReleaseTagRejectsSimilarRemoteTagRefs(t *testing.T) {
	runner := &fakeCommandRunner{
		output: strings.Join([]string{
			"abc123\trefs/tags/v1.2.10",
			"def456\trefs/tags/v1.2.1-rc0",
		}, "\n"),
	}

	err := validateDynamoReleaseTag(context.Background(), runner, "v1.2.1")
	if err == nil {
		t.Fatalf("expected missing exact tag error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestValidateDynamoReleaseTagRejectsNonStableVersionBeforeGit(t *testing.T) {
	for _, version := range []string{
		"1.2.1",
		"v1.2.1-rc0",
		"v1.2.1.post1",
		"refs/tags/v1.2.1",
		" v1.2.1-rc0 ",
	} {
		t.Run(version, func(t *testing.T) {
			runner := &fakeCommandRunner{}

			err := validateDynamoReleaseTag(context.Background(), runner, version)
			if err == nil {
				t.Fatalf("expected invalid stable release tag error")
			}
			if !strings.Contains(err.Error(), "expected vMAJOR.MINOR.PATCH") {
				t.Fatalf("expected stable release tag error, got %v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("expected no git command calls, got %d", len(runner.calls))
			}
		})
	}
}

func TestValidateDynamoReleaseTagReturnsGitError(t *testing.T) {
	runner := &fakeCommandRunner{
		output: "fatal: unable to access 'https://github.com/ai-dynamo/dynamo.git/'",
		err:    errors.New("exit status 128"),
	}

	err := validateDynamoReleaseTag(context.Background(), runner, "v1.2.1")
	if err == nil {
		t.Fatalf("expected git error")
	}
	if !strings.Contains(err.Error(), "failed to query Dynamo release tag v1.2.1") {
		t.Fatalf("expected query error, got %v", err)
	}
	if !strings.Contains(err.Error(), "fatal: unable to access") {
		t.Fatalf("expected command output in error, got %v", err)
	}
}

func TestLsRemoteOutputHasExactTag(t *testing.T) {
	output := strings.Join([]string{
		"abc123\trefs/tags/v1.2.10",
		"def456 refs/tags/v1.2.1",
		"ghi789\trefs/tags/v1.2.1-rc0",
	}, "\n")

	if !lsRemoteOutputHasExactTag(output, "v1.2.1") {
		t.Fatalf("expected exact tag to be detected")
	}
	if lsRemoteOutputHasExactTag(output, "v1.2.10-rc0") {
		t.Fatalf("did not expect non-existent tag to be detected")
	}
}
