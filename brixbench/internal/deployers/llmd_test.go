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

package deployers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateLLMdReleaseTagUsesGitLsRemote(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "0123456789abcdef0123456789abcdef01234567\trefs/tags/v0.8.1\n"},
		},
	}
	deployer := &LLMdDeployer{
		version: "v0.8.1",
		logDir:  t.TempDir(),
		runner:  runner,
	}
	if err := deployer.validateLLMdReleaseTag(context.Background()); err != nil {
		t.Fatalf("validateLLMdReleaseTag returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one git ls-remote call, got %d", len(runner.calls))
	}
	args := runner.calls[0].args
	joined := strings.Join(args, " ")
	if runner.calls[0].name != "git" || !strings.Contains(joined, "ls-remote") || !strings.Contains(joined, llmdRepoURL) {
		t.Fatalf("unexpected command: %s %v", runner.calls[0].name, args)
	}
	if !strings.Contains(joined, "refs/tags/v0.8.1") {
		t.Fatalf("expected tag ref in args, got %v", args)
	}
}

func TestValidateLLMdReleaseTagMissingRemoteTag(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{{output: ""}},
	}
	deployer := &LLMdDeployer{
		version: "v0.0.0",
		logDir:  t.TempDir(),
		runner:  runner,
	}
	err := deployer.validateLLMdReleaseTag(context.Background())
	if err == nil {
		t.Fatal("expected missing tag error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestValidateLLMdReleaseTagPropagatesLsRemoteFailure(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{{err: errors.New("network down")}},
	}
	deployer := &LLMdDeployer{
		version: "v0.8.1",
		logDir:  t.TempDir(),
		runner:  runner,
	}
	err := deployer.validateLLMdReleaseTag(context.Background())
	if err == nil {
		t.Fatal("expected ls-remote failure")
	}
	if !strings.Contains(err.Error(), "failed to query LLM-d release tag") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInstallLLMdRouterDisablesEPPMetricsAuth(t *testing.T) {
	runner := &fakeCommandRunner{}
	deployer := &LLMdDeployer{
		namespace:   LLMdBenchmarkNamespace,
		projectRoot: t.TempDir(),
		runner:      runner,
	}

	if err := deployer.installLLMdRouter(context.Background()); err != nil {
		t.Fatalf("installLLMdRouter returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one Helm command, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "env" {
		t.Fatalf("expected Helm command to run through env, got %q", runner.calls[0].name)
	}

	args := runner.calls[0].args
	for index := 0; index < len(args)-1; index++ {
		if args[index] == "--set" && args[index+1] == "router.monitoring.prometheus.auth.enabled=false" {
			return
		}
	}
	t.Fatalf("expected Helm args to disable EPP metrics auth, got %v", args)
}
