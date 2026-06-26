package deployers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

const testDynamoRepoURL = "https://example.test/ai-dynamo/dynamo.git"

type fakeCommandRunner struct {
	output    string
	err       error
	responses []fakeCommandResponse
	calls     []fakeCommandCall
}

type fakeCommandCall struct {
	name string
	args []string
}

type fakeCommandResponse struct {
	output string
	err    error
	after  func()
}

func (r *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, fakeCommandCall{
		name: name,
		args: append([]string(nil), args...),
	})
	if len(r.responses) > 0 {
		response := r.responses[0]
		r.responses = r.responses[1:]
		if response.after != nil {
			response.after()
		}
		return response.output, response.err
	}
	return r.output, r.err
}

type fakeDynamoReleaseSource struct {
	release     *DynamoRelease
	err         error
	projectRoot string
	version     string
	calls       int
}

func (s *fakeDynamoReleaseSource) ValidateReleaseTag(ctx context.Context, version string) error {
	return nil
}

func (s *fakeDynamoReleaseSource) PrepareRelease(ctx context.Context, projectRoot string, version string) (*DynamoRelease, error) {
	s.calls++
	s.projectRoot = projectRoot
	s.version = version
	return s.release, s.err
}

func TestValidateDynamoReleaseTagAcceptsExactRemoteTag(t *testing.T) {
	runner := &fakeCommandRunner{
		output: "abc123\trefs/tags/v1.2.1\n",
	}

	if err := validateDynamoReleaseTag(context.Background(), runner, testDynamoRepoURL, "v1.2.1"); err != nil {
		t.Fatalf("validateDynamoReleaseTag returned error: %v", err)
	}

	wantArgs := []string{"ls-remote", "--tags", "--refs", testDynamoRepoURL, "refs/tags/v1.2.1"}
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

func TestGitDynamoReleaseSourceUsesConfiguredRunnerAndRepoURL(t *testing.T) {
	runner := &fakeCommandRunner{
		output: "abc123\trefs/tags/v1.2.1\n",
	}
	tagSource := &GitDynamoReleaseSource{
		runner:  runner,
		repoURL: testDynamoRepoURL,
	}

	if err := tagSource.ValidateReleaseTag(context.Background(), "v1.2.1"); err != nil {
		t.Fatalf("ValidateReleaseTag returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(runner.calls))
	}
	if runner.calls[0].args[3] != testDynamoRepoURL {
		t.Fatalf("expected repo URL %s, got args %v", testDynamoRepoURL, runner.calls[0].args)
	}
}

func TestGitDynamoReleaseSourcePrepareReleaseClonesAndReturnsChartPath(t *testing.T) {
	projectRoot := t.TempDir()
	repoPath := filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1")
	chartPath := filepath.Join(repoPath, "deploy", "helm", "charts", "platform")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "abc123\trefs/tags/v1.2.1\n"},
			{
				after: func() {
					if err := os.MkdirAll(chartPath, 0o755); err != nil {
						t.Fatalf("failed to create fake chart path: %v", err)
					}
				},
			},
			{output: "true\n"},
			{},
			{},
		},
	}
	source := &GitDynamoReleaseSource{
		runner:  runner,
		repoURL: testDynamoRepoURL,
	}

	release, err := source.PrepareRelease(context.Background(), projectRoot, "v1.2.1")
	if err != nil {
		t.Fatalf("PrepareRelease returned error: %v", err)
	}
	if release.Version != "v1.2.1" || release.RepoPath != repoPath || release.ChartPath != chartPath {
		t.Fatalf("unexpected release: %+v", release)
	}

	wantCloneArgs := []string{"clone", "--depth=1", "--branch", "v1.2.1", testDynamoRepoURL, repoPath}
	if len(runner.calls) != 5 {
		t.Fatalf("expected 5 command calls, got %d", len(runner.calls))
	}
	if runner.calls[1].name != "git" {
		t.Fatalf("expected git command, got %s", runner.calls[1].name)
	}
	if !reflect.DeepEqual(runner.calls[1].args, wantCloneArgs) {
		t.Fatalf("expected clone args %v, got %v", wantCloneArgs, runner.calls[1].args)
	}
}

func TestGitDynamoReleaseSourcePrepareReleaseReusesExistingCheckout(t *testing.T) {
	projectRoot := t.TempDir()
	chartPath := filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1", "deploy", "helm", "charts", "platform")
	if err := os.MkdirAll(chartPath, 0o755); err != nil {
		t.Fatalf("failed to create fake chart path: %v", err)
	}
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "abc123\trefs/tags/v1.2.1\n"},
			{},
			{},
			{},
			{},
			{output: "false\n"},
			{},
			{},
		},
	}
	source := &GitDynamoReleaseSource{
		runner:  runner,
		repoURL: testDynamoRepoURL,
	}

	release, err := source.PrepareRelease(context.Background(), projectRoot, "v1.2.1")
	if err != nil {
		t.Fatalf("PrepareRelease returned error: %v", err)
	}
	if release.ChartPath != chartPath {
		t.Fatalf("expected chart path %s, got %s", chartPath, release.ChartPath)
	}
	wantCalls := []fakeCommandCall{
		{
			name: "git",
			args: []string{"ls-remote", "--tags", "--refs", testDynamoRepoURL, "refs/tags/v1.2.1"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "fetch", "--filter=blob:none", "--force", testDynamoRepoURL, "+refs/tags/v1.2.1:refs/tags/v1.2.1"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "checkout", "--force", "--detach", "v1.2.1^{commit}"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "reset", "--hard", "v1.2.1^{commit}"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "clean", "-ffdx"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "rev-parse", "--is-shallow-repository"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "fetch", "--filter=blob:none", testDynamoRepoURL, "+refs/heads/main:refs/remotes/origin/main"},
		},
		{
			name: "git",
			args: []string{"-C", filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"), "merge-base", "--is-ancestor", "v1.2.1^{commit}", "refs/remotes/origin/main"},
		},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("expected calls %+v, got %+v", wantCalls, runner.calls)
	}
}

func TestGitDynamoReleaseSourcePrepareReleaseRejectsCheckoutWithoutChart(t *testing.T) {
	projectRoot := t.TempDir()
	repoPath := filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("failed to create fake checkout: %v", err)
	}
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "abc123\trefs/tags/v1.2.1\n"},
			{},
			{},
			{},
			{},
			{output: "false\n"},
			{},
			{},
		},
	}
	source := &GitDynamoReleaseSource{
		runner:  runner,
		repoURL: testDynamoRepoURL,
	}

	_, err := source.PrepareRelease(context.Background(), projectRoot, "v1.2.1")
	if err == nil {
		t.Fatalf("expected missing chart path error")
	}
	if !strings.Contains(err.Error(), "chart path") {
		t.Fatalf("expected chart path error, got %v", err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("expected tag validation, sync, and reachability commands, got %d calls", len(runner.calls))
	}
}

func TestSyncDynamoReleaseCheckoutReturnsCommandError(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "fatal: not a git repository", err: errors.New("exit status 128")},
		},
	}

	err := syncDynamoReleaseCheckout(context.Background(), runner, "/repo/.tmp/dynamo/v1.2.1", testDynamoRepoURL, "v1.2.1")
	if err == nil {
		t.Fatalf("expected sync error")
	}
	if !strings.Contains(err.Error(), "failed to sync Dynamo release tag v1.2.1") {
		t.Fatalf("expected sync error, got %v", err)
	}
	if !strings.Contains(err.Error(), "fatal: not a git repository") {
		t.Fatalf("expected command output in error, got %v", err)
	}
}

func TestValidateDynamoReleaseReachableFromMainUnshallowsAndChecksAncestor(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "true\n"},
			{},
			{},
		},
	}
	repoPath := "/repo/.tmp/dynamo/v1.2.1"

	if err := validateDynamoReleaseReachableFromMain(context.Background(), runner, repoPath, testDynamoRepoURL, "v1.2.1"); err != nil {
		t.Fatalf("validateDynamoReleaseReachableFromMain returned error: %v", err)
	}

	wantCalls := []fakeCommandCall{
		{
			name: "git",
			args: []string{"-C", repoPath, "rev-parse", "--is-shallow-repository"},
		},
		{
			name: "git",
			args: []string{"-C", repoPath, "fetch", "--filter=blob:none", "--unshallow", testDynamoRepoURL, "+refs/heads/main:refs/remotes/origin/main"},
		},
		{
			name: "git",
			args: []string{"-C", repoPath, "merge-base", "--is-ancestor", "v1.2.1^{commit}", "refs/remotes/origin/main"},
		},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("expected calls %+v, got %+v", wantCalls, runner.calls)
	}
}

func TestValidateDynamoReleaseReachableFromMainFetchesMainForNonShallowCheckout(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "false\n"},
			{},
			{},
		},
	}
	repoPath := "/repo/.tmp/dynamo/v1.2.1"

	if err := validateDynamoReleaseReachableFromMain(context.Background(), runner, repoPath, testDynamoRepoURL, "v1.2.1"); err != nil {
		t.Fatalf("validateDynamoReleaseReachableFromMain returned error: %v", err)
	}

	wantFetchArgs := []string{"-C", repoPath, "fetch", "--filter=blob:none", testDynamoRepoURL, "+refs/heads/main:refs/remotes/origin/main"}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 command calls, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[1].args, wantFetchArgs) {
		t.Fatalf("expected fetch args %v, got %v", wantFetchArgs, runner.calls[1].args)
	}
}

func TestValidateDynamoReleaseReachableFromMainRejectsNonReachableTag(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "false\n"},
			{},
			{err: errors.New("exit status 1")},
		},
	}

	err := validateDynamoReleaseReachableFromMain(context.Background(), runner, "/repo/.tmp/dynamo/v1.2.1", testDynamoRepoURL, "v1.2.1")
	if err == nil {
		t.Fatalf("expected not reachable error")
	}
	if !strings.Contains(err.Error(), "not reachable from main") {
		t.Fatalf("expected not reachable error, got %v", err)
	}
}

func TestValidateDynamoReleaseReachableFromMainReturnsGitCommandError(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "false\n"},
			{output: "fatal: couldn't find remote ref main", err: errors.New("exit status 128")},
		},
	}

	err := validateDynamoReleaseReachableFromMain(context.Background(), runner, "/repo/.tmp/dynamo/v1.2.1", testDynamoRepoURL, "v1.2.1")
	if err == nil {
		t.Fatalf("expected fetch error")
	}
	if !strings.Contains(err.Error(), "failed to fetch Dynamo main history") {
		t.Fatalf("expected fetch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "fatal: couldn't find remote ref main") {
		t.Fatalf("expected command output in error, got %v", err)
	}
}

func TestValidateDynamoReleaseReachableFromMainReturnsMergeBaseCommandError(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "false\n"},
			{},
			{output: "fatal: not a valid object name", err: errors.New("exit status 128")},
		},
	}

	err := validateDynamoReleaseReachableFromMain(context.Background(), runner, "/repo/.tmp/dynamo/v1.2.1", testDynamoRepoURL, "v1.2.1")
	if err == nil {
		t.Fatalf("expected merge-base error")
	}
	if strings.Contains(err.Error(), "not reachable from main") {
		t.Fatalf("expected command error instead of not reachable error, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed to validate Dynamo release tag v1.2.1 reachability from main") {
		t.Fatalf("expected reachability command error, got %v", err)
	}
	if !strings.Contains(err.Error(), "fatal: not a valid object name") {
		t.Fatalf("expected command output in error, got %v", err)
	}
}

func TestValidateDynamoReleaseTagRejectsMissingRemoteTag(t *testing.T) {
	runner := &fakeCommandRunner{}

	err := validateDynamoReleaseTag(context.Background(), runner, testDynamoRepoURL, "v1.2.1")
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

	err := validateDynamoReleaseTag(context.Background(), runner, testDynamoRepoURL, "v1.2.1")
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

			err := validateDynamoReleaseTag(context.Background(), runner, testDynamoRepoURL, version)
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

	err := validateDynamoReleaseTag(context.Background(), runner, dynamoRepoURL, "v1.2.1")
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

func TestDynamoDeployerInitializeStoresConfig(t *testing.T) {
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-adhoc",
		LogDir:      "/tmp/logs",
		ProjectRoot: "/repo",
		EnginePath:  "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml",
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	if deployer.namespace != "brixbench-adhoc" {
		t.Fatalf("expected namespace brixbench-adhoc, got %s", deployer.namespace)
	}
	if deployer.version != "v1.2.1" {
		t.Fatalf("expected version v1.2.1, got %s", deployer.version)
	}
	if deployer.engineManifest != "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml" {
		t.Fatalf("unexpected engine manifest %s", deployer.engineManifest)
	}
	if deployer.projectRoot != "/repo" {
		t.Fatalf("expected project root /repo, got %s", deployer.projectRoot)
	}
}

func TestDynamoDeployerDeployControlPlanePreparesReleaseAndInstallsHelmChart(t *testing.T) {
	releaseSource := &fakeDynamoReleaseSource{
		release: &DynamoRelease{
			Version:   "v1.2.1",
			RepoPath:  "/repo/.tmp/dynamo/v1.2.1",
			ChartPath: "/repo/.tmp/dynamo/v1.2.1/deploy/helm/charts/platform",
		},
	}
	runner := &fakeCommandRunner{}
	deployer := &DynamoDeployer{
		namespace:     "brixbench-adhoc",
		projectRoot:   "/repo",
		version:       "v1.2.1",
		releaseSource: releaseSource,
		runner:        runner,
	}

	if err := deployer.DeployControlPlane(context.Background()); err != nil {
		t.Fatalf("DeployControlPlane returned error: %v", err)
	}
	if releaseSource.calls != 1 {
		t.Fatalf("expected PrepareRelease to be called once, got %d", releaseSource.calls)
	}
	if releaseSource.projectRoot != "/repo" || releaseSource.version != "v1.2.1" {
		t.Fatalf("unexpected PrepareRelease args: projectRoot=%s version=%s", releaseSource.projectRoot, releaseSource.version)
	}
	if deployer.release != releaseSource.release {
		t.Fatalf("expected deployer release to be recorded")
	}

	wantArgs := []string{
		"upgrade", "--install", "dynamo-platform", "/repo/.tmp/dynamo/v1.2.1/deploy/helm/charts/platform",
		"-n", "brixbench-adhoc",
		"--create-namespace",
		"--wait",
		"--timeout", "10m",
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "helm" {
		t.Fatalf("expected helm command, got %s", runner.calls[0].name)
	}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, runner.calls[0].args)
	}
}

func TestDynamoDeployerDeployEngineAppliesUserManifest(t *testing.T) {
	runner := &fakeCommandRunner{}
	deployer := &DynamoDeployer{
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml",
		runner:         runner,
	}

	if err := deployer.DeployEngine(context.Background()); err != nil {
		t.Fatalf("DeployEngine returned error: %v", err)
	}

	wantArgs := []string{"apply", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml"}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "kubectl" {
		t.Fatalf("expected kubectl command, got %s", runner.calls[0].name)
	}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, runner.calls[0].args)
	}
}

func TestDynamoDeployerTeardownBestEffortCleansManifestPlatformAndNamespace(t *testing.T) {
	runner := &fakeCommandRunner{}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-adhoc",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml",
		runner:         runner,
	}

	if err := deployer.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown returned error: %v", err)
	}

	wantCalls := []fakeCommandCall{
		{
			name: "kubectl",
			args: []string{"patch", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml", "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`},
		},
		{
			name: "kubectl",
			args: []string{"patch", "dynamocomponentdeployments.nvidia.com", "--all", "-n", "brixbench-adhoc", "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`},
		},
		{
			name: "kubectl",
			args: []string{"delete", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml", "--ignore-not-found", "--wait=false"},
		},
		{
			name: "helm",
			args: []string{"uninstall", "dynamo-platform", "-n", "brixbench-adhoc", "--ignore-not-found", "--wait", "--timeout", "5m"},
		},
		{
			name: "kubectl",
			args: []string{"delete", "namespace", "brixbench-adhoc", "--ignore-not-found"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=delete", "namespace/brixbench-adhoc", "--timeout=10m"},
		},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("expected calls %+v, got %+v", wantCalls, runner.calls)
	}
}

func TestDynamoDeployerTeardownIgnoresCleanupErrors(t *testing.T) {
	runner := &fakeCommandRunner{
		err: errors.New("cleanup failed"),
	}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-adhoc",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d.yaml",
		runner:         runner,
	}

	if err := deployer.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown returned error: %v", err)
	}
	if len(runner.calls) != 6 {
		t.Fatalf("expected 6 cleanup commands, got %d", len(runner.calls))
	}
}
