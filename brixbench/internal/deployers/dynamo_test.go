package deployers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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

func wantDynamoHelmArgs(t *testing.T, projectRoot string, helmArgs ...string) []string {
	t.Helper()

	stateDir := filepath.Join(projectRoot, ".tmp", dynamoHelmStateDirName)
	configDir := filepath.Join(stateDir, "config")
	cacheDir := filepath.Join(stateDir, "cache")
	dataDir := filepath.Join(stateDir, "data")
	args := []string{
		"HELM_CONFIG_HOME=" + configDir,
		"HELM_CACHE_HOME=" + cacheDir,
		"HELM_DATA_HOME=" + dataDir,
		"HELM_REPOSITORY_CONFIG=" + filepath.Join(configDir, "repositories.yaml"),
		"HELM_REPOSITORY_CACHE=" + filepath.Join(cacheDir, "repository"),
		"helm",
	}
	return append(args, helmArgs...)
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

func writeDynamoGraphManifest(t *testing.T, namespace string, name string, components ...string) string {
	t.Helper()
	if len(components) == 0 {
		components = []string{"Frontend", "VllmDecodeWorker"}
	}

	var b strings.Builder
	b.WriteString("apiVersion: nvidia.com/v1alpha1\n")
	b.WriteString("kind: DynamoGraphDeployment\n")
	b.WriteString("metadata:\n")
	if name != "" {
		b.WriteString("  name: " + name + "\n")
	}
	if namespace != "" {
		b.WriteString("  namespace: " + namespace + "\n")
	}
	b.WriteString("spec:\n")
	b.WriteString("  services:\n")
	for _, component := range components {
		b.WriteString("    " + component + ":\n")
		b.WriteString("      componentType: worker\n")
	}

	path := filepath.Join(t.TempDir(), "dynamo-graph.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("failed to write Dynamo graph manifest: %v", err)
	}
	return path
}

func writeDynamoChartLock(t *testing.T, chartPath string) {
	t.Helper()
	content := `dependencies:
- name: dynamo-operator
  repository: file://components/operator
  version: 1.2.1
- name: nats
  repository: https://nats-io.github.io/k8s/helm/charts/
  version: 1.3.2
- name: etcd
  repository: https://charts.bitnami.com/bitnami
  version: 12.0.18
- name: kai-scheduler
  repository: oci://ghcr.io/kai-scheduler/kai-scheduler
  version: v0.13.4
`
	if err := os.MkdirAll(chartPath, 0o755); err != nil {
		t.Fatalf("failed to create chart path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chartPath, "Chart.lock"), []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write Chart.lock: %v", err)
	}
}

func writeDynamoChartYAML(t *testing.T, chartPath string) {
	t.Helper()
	content := `apiVersion: v2
name: dynamo-platform
dependencies:
- name: dynamo-operator
  repository: file://components/operator
  version: 1.2.1
- name: nats
  repository: https://nats-io.github.io/k8s/helm/charts/
  version: 1.3.2
- name: etcd
  repository: https://charts.bitnami.com/bitnami
  version: 12.0.18
- name: kai-scheduler
  repository: oci://ghcr.io/kai-scheduler/kai-scheduler
  version: v0.13.4
`
	if err := os.MkdirAll(chartPath, 0o755); err != nil {
		t.Fatalf("failed to create chart path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(chartPath, "Chart.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write Chart.yaml: %v", err)
	}
}

func dynamoServiceListJSON(items string) string {
	return `{"items":[` + items + `]}`
}

func dynamoServiceJSON(name string, ports ...int) string {
	var b strings.Builder
	b.WriteString(`{"metadata":{"name":"`)
	b.WriteString(name)
	b.WriteString(`"},"spec":{"clusterIP":"10.96.0.42","ports":[`)
	for i, port := range ports {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"port":`)
		b.WriteString(strconv.Itoa(port))
		b.WriteString("}")
	}
	b.WriteString(`]}}`)
	return b.String()
}

func dynamoPodListJSON(count int) string {
	items := make([]string, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, `{}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
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
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 command calls, got %d", len(runner.calls))
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
	if len(runner.calls) != 5 {
		t.Fatalf("expected tag validation and sync commands, got %d calls", len(runner.calls))
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
	manifest := writeDynamoGraphManifest(t, "", "vllm-dynamo")
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-adhoc",
		LogDir:      "/tmp/logs",
		ProjectRoot: "/repo",
		EnginePath:  manifest,
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	if deployer.namespace != "brixbench-dynamo" {
		t.Fatalf("expected namespace brixbench-dynamo, got %s", deployer.namespace)
	}
	if deployer.version != "v1.2.1" {
		t.Fatalf("expected version v1.2.1, got %s", deployer.version)
	}
	if deployer.engineManifest != manifest {
		t.Fatalf("unexpected engine manifest %s", deployer.engineManifest)
	}
	if deployer.projectRoot != "/repo" {
		t.Fatalf("expected project root /repo, got %s", deployer.projectRoot)
	}
	if deployer.graphName != "vllm-dynamo" {
		t.Fatalf("expected graph name vllm-dynamo, got %s", deployer.graphName)
	}
	if deployer.effectiveNS != "brixbench-dynamo" {
		t.Fatalf("expected effective namespace brixbench-dynamo, got %s", deployer.effectiveNS)
	}
}

func TestDynamoDeployerInitializeAcceptsMatchingManifestNamespace(t *testing.T) {
	manifest := writeDynamoGraphManifest(t, "brixbench-dynamo", "vllm-dynamo")
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-dynamo",
		ProjectRoot: "/repo",
		EnginePath:  manifest,
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if deployer.effectiveNS != "brixbench-dynamo" {
		t.Fatalf("expected effective namespace brixbench-dynamo, got %s", deployer.effectiveNS)
	}
}

func TestDynamoDeployerInitializeRejectsMismatchedManifestNamespace(t *testing.T) {
	manifest := writeDynamoGraphManifest(t, "other-namespace", "vllm-dynamo")
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-dynamo",
		ProjectRoot: "/repo",
		EnginePath:  manifest,
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err == nil {
		t.Fatalf("expected namespace mismatch error")
	}
	if !strings.Contains(err.Error(), "must match benchmark namespace") {
		t.Fatalf("expected namespace mismatch error, got %v", err)
	}
}

func TestDynamoDeployerInitializeReadsComponentReplicas(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "dynamo-graph.yaml")
	content := `apiVersion: nvidia.com/v1alpha1
kind: DynamoGraphDeployment
metadata:
  name: vllm-dynamo
spec:
  services:
    Frontend:
      replicas: 1
    VllmDecodeWorker:
      replicas: 8
    VllmPrefillWorker:
      replicas: 4
`
	if err := os.WriteFile(manifest, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-dynamo",
		ProjectRoot: "/repo",
		EnginePath:  manifest,
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if got := deployer.expectedDynamoComponentReplicas("VllmDecodeWorker"); got != 8 {
		t.Fatalf("expected decode replicas 8, got %d", got)
	}
	if got := deployer.expectedDynamoComponentReplicas("VllmPrefillWorker"); got != 4 {
		t.Fatalf("expected prefill replicas 4, got %d", got)
	}
}

func TestDynamoDeployerInitializeRejectsMissingGraphName(t *testing.T) {
	manifest := writeDynamoGraphManifest(t, "", "")
	deployer := &DynamoDeployer{
		releaseSource: &fakeDynamoReleaseSource{},
		runner:        &fakeCommandRunner{},
	}

	err := deployer.Initialize(context.Background(), Config{
		Namespace:   "brixbench-dynamo",
		ProjectRoot: "/repo",
		EnginePath:  manifest,
		TestCase: &resolver.Test{
			Version: "v1.2.1",
		},
	})
	if err == nil {
		t.Fatalf("expected missing metadata.name error")
	}
	if !strings.Contains(err.Error(), "metadata.name is required") {
		t.Fatalf("expected missing metadata.name error, got %v", err)
	}
}

func TestDynamoDeployerDeployControlPlanePreparesReleaseBuildsDependenciesAndInstallsHelmChart(t *testing.T) {
	projectRoot := t.TempDir()
	chartPath := filepath.Join(t.TempDir(), "deploy", "helm", "charts", "platform")
	platformValues := filepath.Join(t.TempDir(), "platform-values.yaml")
	if err := os.WriteFile(platformValues, []byte("nats: {}\n"), 0o644); err != nil {
		t.Fatalf("failed to write platform values: %v", err)
	}
	writeDynamoChartLock(t, chartPath)
	releaseSource := &fakeDynamoReleaseSource{
		release: &DynamoRelease{
			Version:   "v1.2.1",
			RepoPath:  filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"),
			ChartPath: chartPath,
		},
	}
	runner := &fakeCommandRunner{}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		projectRoot:    projectRoot,
		version:        "v1.2.1",
		platformValues: platformValues,
		releaseSource:  releaseSource,
		runner:         runner,
	}

	if err := deployer.DeployControlPlane(context.Background()); err != nil {
		t.Fatalf("DeployControlPlane returned error: %v", err)
	}
	if releaseSource.calls != 1 {
		t.Fatalf("expected PrepareRelease to be called once, got %d", releaseSource.calls)
	}
	if releaseSource.projectRoot != projectRoot || releaseSource.version != "v1.2.1" {
		t.Fatalf("unexpected PrepareRelease args: projectRoot=%s version=%s", releaseSource.projectRoot, releaseSource.version)
	}
	if deployer.release != releaseSource.release {
		t.Fatalf("expected deployer release to be recorded")
	}

	wantDependencyBuildArgs := []string{
		"dependency", "build", "--skip-refresh", chartPath,
	}
	wantInstallArgs := []string{
		"upgrade", "--install", "dynamo-platform", chartPath,
		"-n", "brixbench-dynamo",
		"--create-namespace",
		"--set", "dynamo-operator.namespaceRestriction.enabled=true",
		"-f", platformValues,
		"--no-hooks",
		"--wait",
		"--timeout", "10m",
	}
	wantRepoAddBitnamiArgs := []string{
		"repo", "add", "dynamo-https-charts-bitnami-com-bitnami", "https://charts.bitnami.com/bitnami", "--force-update",
	}
	wantRepoAddNATSArgs := []string{
		"repo", "add", "dynamo-https-nats-io-github-io-k8s-helm-charts", "https://nats-io.github.io/k8s/helm/charts/", "--force-update",
	}
	if len(runner.calls) != 4 {
		t.Fatalf("expected 4 command calls, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "env" {
		t.Fatalf("expected env command, got %s", runner.calls[0].name)
	}
	if !reflect.DeepEqual(runner.calls[0].args, wantDynamoHelmArgs(t, projectRoot, wantRepoAddBitnamiArgs...)) {
		t.Fatalf("expected args %v, got %v", wantRepoAddBitnamiArgs, runner.calls[0].args)
	}
	if runner.calls[1].name != "env" {
		t.Fatalf("expected env command, got %s", runner.calls[1].name)
	}
	if !reflect.DeepEqual(runner.calls[1].args, wantDynamoHelmArgs(t, projectRoot, wantRepoAddNATSArgs...)) {
		t.Fatalf("expected args %v, got %v", wantRepoAddNATSArgs, runner.calls[1].args)
	}
	if runner.calls[2].name != "env" {
		t.Fatalf("expected env command, got %s", runner.calls[2].name)
	}
	if !reflect.DeepEqual(runner.calls[2].args, wantDynamoHelmArgs(t, projectRoot, wantDependencyBuildArgs...)) {
		t.Fatalf("expected args %v, got %v", wantDependencyBuildArgs, runner.calls[2].args)
	}
	if runner.calls[3].name != "env" {
		t.Fatalf("expected env command, got %s", runner.calls[3].name)
	}
	if !reflect.DeepEqual(runner.calls[3].args, wantDynamoHelmArgs(t, projectRoot, wantInstallArgs...)) {
		t.Fatalf("expected args %v, got %v", wantInstallArgs, runner.calls[3].args)
	}
}

func TestDynamoDeployerDeployControlPlaneRetriesRetryableHelmRepoFailures(t *testing.T) {
	projectRoot := t.TempDir()
	chartPath := filepath.Join(t.TempDir(), "deploy", "helm", "charts", "platform")
	writeDynamoChartLock(t, chartPath)
	releaseSource := &fakeDynamoReleaseSource{
		release: &DynamoRelease{
			Version:   "v1.2.1",
			RepoPath:  filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"),
			ChartPath: chartPath,
		},
	}
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: "Get \"https://charts.bitnami.com/bitnami/index.yaml\": EOF", err: errors.New("exit status 1")},
			{},
			{output: "Get \"https://nats-io.github.io/k8s/helm/charts/index.yaml\": EOF", err: errors.New("exit status 1")},
			{},
			{},
			{},
		},
	}
	deployer := &DynamoDeployer{
		namespace:     "brixbench-dynamo",
		projectRoot:   projectRoot,
		version:       "v1.2.1",
		releaseSource: releaseSource,
		runner:        runner,
	}

	if err := deployer.DeployControlPlane(context.Background()); err != nil {
		t.Fatalf("DeployControlPlane returned error: %v", err)
	}

	if len(runner.calls) != 6 {
		t.Fatalf("expected 6 command calls, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[0].args, runner.calls[1].args) {
		t.Fatalf("expected retry to repeat repo add args, got %+v then %+v", runner.calls[0], runner.calls[1])
	}
	if !reflect.DeepEqual(runner.calls[2].args, runner.calls[3].args) {
		t.Fatalf("expected retry to repeat repo add args, got %+v then %+v", runner.calls[2], runner.calls[3])
	}
}

func TestDynamoDeployerDeployControlPlaneWritesGenericDefaultPlatformValues(t *testing.T) {
	projectRoot := t.TempDir()
	chartPath := filepath.Join(t.TempDir(), "deploy", "helm", "charts", "platform")
	writeDynamoChartLock(t, chartPath)
	releaseSource := &fakeDynamoReleaseSource{
		release: &DynamoRelease{
			Version:   "v1.2.1",
			RepoPath:  filepath.Join(projectRoot, ".tmp", "dynamo", "v1.2.1"),
			ChartPath: chartPath,
		},
	}
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{},
			{},
			{},
			{},
			{},
			{},
			{},
		},
	}
	deployer := &DynamoDeployer{
		namespace:     "brixbench-dynamo",
		projectRoot:   projectRoot,
		version:       "v1.2.1",
		releaseSource: releaseSource,
		runner:        runner,
	}

	if err := deployer.DeployControlPlane(context.Background()); err != nil {
		t.Fatalf("DeployControlPlane returned error: %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("expected 4 command calls, got %d", len(runner.calls))
	}
	for _, call := range runner.calls {
		if call.name == "kubectl" && len(call.args) > 2 && call.args[0] == "get" && call.args[1] == "secret" {
			t.Fatalf("did not expect registry secret check by default: %+v", call)
		}
	}

	content, err := os.ReadFile(deployer.platformValues)
	if err != nil {
		t.Fatalf("failed to read generated platform values: %v", err)
	}
	values := string(content)
	for _, forbidden := range []string{"imagePullSecrets", "existingSecretName", "useKubernetesSecret"} {
		if strings.Contains(values, forbidden) {
			t.Fatalf("generated platform values should not contain %q without registry secret:\n%s", forbidden, values)
		}
	}
	if strings.Contains(values, "mpiRun") || strings.Contains(values, "secretName") {
		t.Fatalf("generated platform values should not configure deployment-specific MPI secrets:\n%s", values)
	}
	if !strings.Contains(values, `tag: "1.2.1"`) {
		t.Fatalf("expected operator image tag to come from version v1.2.1:\n%s", values)
	}
	if strings.Contains(values, "1.2.0-deepseek-v4-dev.3") {
		t.Fatalf("generated platform values should not use hard-coded dev operator tag:\n%s", values)
	}
	for _, forbidden := range []string{"aibrix-container-registry-cn-beijing", "aibrix-public-release-cn-beijing"} {
		if strings.Contains(values, forbidden) {
			t.Fatalf("generated platform values should not contain internal registry %q:\n%s", forbidden, values)
		}
	}
}

func TestReadDynamoHelmDependencyReposFiltersFileAndOCIRepos(t *testing.T) {
	chartPath := filepath.Join(t.TempDir(), "deploy", "helm", "charts", "platform")
	writeDynamoChartLock(t, chartPath)

	repos, err := readDynamoHelmDependencyRepos(chartPath)
	if err != nil {
		t.Fatalf("readDynamoHelmDependencyRepos returned error: %v", err)
	}

	wantRepos := []dynamoHelmRepo{
		{
			Alias: "dynamo-https-charts-bitnami-com-bitnami",
			URL:   "https://charts.bitnami.com/bitnami",
		},
		{
			Alias: "dynamo-https-nats-io-github-io-k8s-helm-charts",
			URL:   "https://nats-io.github.io/k8s/helm/charts/",
		},
	}
	if !reflect.DeepEqual(repos, wantRepos) {
		t.Fatalf("expected repos %+v, got %+v", wantRepos, repos)
	}
}

func TestReadDynamoHelmDependencyReposFallsBackToChartYAML(t *testing.T) {
	chartPath := filepath.Join(t.TempDir(), "deploy", "helm", "charts", "platform")
	writeDynamoChartYAML(t, chartPath)

	repos, err := readDynamoHelmDependencyRepos(chartPath)
	if err != nil {
		t.Fatalf("readDynamoHelmDependencyRepos returned error: %v", err)
	}

	if len(repos) != 2 {
		t.Fatalf("expected 2 HTTP repos, got %+v", repos)
	}
}

func TestDynamoDeployerDeployEngineAppliesUserManifest(t *testing.T) {
	manifest := writeDynamoGraphManifest(t, "", "vllm-dynamo")
	runner := &fakeCommandRunner{}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		engineManifest: manifest,
		runner:         runner,
	}

	if err := deployer.DeployEngine(context.Background()); err != nil {
		t.Fatalf("DeployEngine returned error: %v", err)
	}

	wantArgs := []string{"apply", "-n", "brixbench-dynamo", "-f", manifest}
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

func TestDynamoDeployerGetGatewayEndpointDiscoversFrontendServiceByLabel(t *testing.T) {
	runner := &fakeCommandRunner{
		output: dynamoServiceListJSON(dynamoServiceJSON("vllm-dynamo-frontend", 8080, 8000)),
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	endpoint, err := deployer.GetGatewayEndpoint(context.Background())
	if err != nil {
		t.Fatalf("GetGatewayEndpoint returned error: %v", err)
	}
	wantEndpoint := "http://10.96.0.42:8000"
	if endpoint != wantEndpoint {
		t.Fatalf("expected endpoint %s, got %s", wantEndpoint, endpoint)
	}
	wantArgs := []string{"get", "svc", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=Frontend", "-o", "json"}
	if len(runner.calls) != 1 {
		t.Fatalf("expected 1 command call, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("expected args %v, got %v", wantArgs, runner.calls[0].args)
	}
}

func TestDynamoDeployerGetGatewayEndpointFallsBackToGraphFrontendServiceName(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: dynamoServiceListJSON("")},
			{output: dynamoServiceJSON("vllm-dynamo-frontend", 9000)},
		},
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	endpoint, err := deployer.GetGatewayEndpoint(context.Background())
	if err != nil {
		t.Fatalf("GetGatewayEndpoint returned error: %v", err)
	}
	wantEndpoint := "http://10.96.0.42:9000"
	if endpoint != wantEndpoint {
		t.Fatalf("expected endpoint %s, got %s", wantEndpoint, endpoint)
	}
	wantFallbackArgs := []string{"get", "svc", "vllm-dynamo-frontend", "-n", "brixbench-dynamo", "-o", "json"}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 command calls, got %d", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[1].args, wantFallbackArgs) {
		t.Fatalf("expected fallback args %v, got %v", wantFallbackArgs, runner.calls[1].args)
	}
}

func TestDynamoDeployerGetGatewayEndpointRejectsMultipleLabelServices(t *testing.T) {
	runner := &fakeCommandRunner{
		output: dynamoServiceListJSON(strings.Join([]string{
			dynamoServiceJSON("frontend-a", 8000),
			dynamoServiceJSON("frontend-b", 8000),
		}, ",")),
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	_, err := deployer.GetGatewayEndpoint(context.Background())
	if err == nil {
		t.Fatalf("expected multiple service error")
	}
	if !strings.Contains(err.Error(), "multiple Dynamo Frontend services") {
		t.Fatalf("expected multiple service error, got %v", err)
	}
}

func TestDynamoDeployerGetGatewayEndpointRejectsAmbiguousPorts(t *testing.T) {
	runner := &fakeCommandRunner{
		output: dynamoServiceListJSON(dynamoServiceJSON("vllm-dynamo-frontend", 8080, 9000)),
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	_, err := deployer.GetGatewayEndpoint(context.Background())
	if err == nil {
		t.Fatalf("expected ambiguous port error")
	}
	if !strings.Contains(err.Error(), "multiple ports") {
		t.Fatalf("expected ambiguous port error, got %v", err)
	}
}

func TestDynamoDeployerWaitForReadyWaitsForFrontendAndWorkerPods(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: dynamoServiceListJSON(dynamoServiceJSON("vllm-dynamo-frontend", 8000))},
			{output: dynamoPodListJSON(1)},
			{},
			{output: dynamoPodListJSON(1)},
			{},
			{output: dynamoPodListJSON(1)},
			{},
		},
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		components:  []string{"Frontend", "VllmDecodeWorker", "VllmPrefillWorker"},
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	if err := deployer.WaitForReady(context.Background()); err != nil {
		t.Fatalf("WaitForReady returned error: %v", err)
	}

	wantCalls := []fakeCommandCall{
		{
			name: "kubectl",
			args: []string{"get", "svc", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=Frontend", "-o", "json"},
		},
		{
			name: "kubectl",
			args: []string{"get", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=Frontend", "-o", "json"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=condition=Ready", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=Frontend", "--timeout=10m"},
		},
		{
			name: "kubectl",
			args: []string{"get", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=VllmDecodeWorker", "-o", "json"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=condition=Ready", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=VllmDecodeWorker", "--timeout=10m"},
		},
		{
			name: "kubectl",
			args: []string{"get", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=VllmPrefillWorker", "-o", "json"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=condition=Ready", "pod", "-n", "brixbench-dynamo", "-l", "nvidia.com/dynamo-component=VllmPrefillWorker", "--timeout=10m"},
		},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("expected calls %+v, got %+v", wantCalls, runner.calls)
	}
}

func TestDynamoDeployerWaitForFrontendServicePollsUntilCreated(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: dynamoServiceListJSON("")},
			{err: errors.New("not found")},
			{output: dynamoServiceListJSON(dynamoServiceJSON("vllm-dynamo-frontend", 8000))},
		},
	}
	deployer := &DynamoDeployer{
		graphName:   "vllm-dynamo",
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	serviceName, port, err := deployer.waitForDynamoFrontendService(context.Background(), time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("waitForDynamoFrontendService returned error: %v", err)
	}
	if serviceName != "vllm-dynamo-frontend" || port != 8000 {
		t.Fatalf("unexpected service %s port %d", serviceName, port)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected 3 command calls, got %d", len(runner.calls))
	}
}

func TestDynamoDeployerWaitForComponentPodsPollsUntilCreated(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: dynamoPodListJSON(0)},
			{output: dynamoPodListJSON(1)},
		},
	}
	deployer := &DynamoDeployer{
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	if err := deployer.waitForDynamoComponentPods(context.Background(), "VllmDecodeWorker", 1, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitForDynamoComponentPods returned error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 command calls, got %d", len(runner.calls))
	}
}

func TestDynamoDeployerWaitForComponentPodsWaitsForExpectedReplicaCount(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{output: dynamoPodListJSON(1)},
			{output: dynamoPodListJSON(3)},
		},
	}
	deployer := &DynamoDeployer{
		effectiveNS: "brixbench-dynamo",
		runner:      runner,
	}

	if err := deployer.waitForDynamoComponentPods(context.Background(), "VllmDecodeWorker", 3, time.Second, time.Millisecond); err != nil {
		t.Fatalf("waitForDynamoComponentPods returned error: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expected 2 command calls, got %d", len(runner.calls))
	}
}

func TestDynamoDeployerTeardownBestEffortCleansManifestPlatformAndNamespace(t *testing.T) {
	projectRoot := t.TempDir()
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{},
			{output: "dynamocomponentdeployment.nvidia.com/vllm-dynamo-frontend\n"},
		},
	}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml",
		projectRoot:    projectRoot,
		runner:         runner,
	}

	if err := deployer.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown returned error: %v", err)
	}

	wantCalls := []fakeCommandCall{
		{
			name: "kubectl",
			args: []string{"patch", "-n", "brixbench-dynamo", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml", "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`},
		},
		{
			name: "kubectl",
			args: []string{"get", "dynamocomponentdeployments.nvidia.com", "-n", "brixbench-dynamo", "-o", "name"},
		},
		{
			name: "kubectl",
			args: []string{"patch", "dynamocomponentdeployment.nvidia.com/vllm-dynamo-frontend", "-n", "brixbench-dynamo", "--type=merge", "-p", `{"metadata":{"finalizers":[]}}`},
		},
		{
			name: "kubectl",
			args: []string{"delete", "-n", "brixbench-dynamo", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml", "--ignore-not-found", "--wait=false"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=delete", "-n", "brixbench-dynamo", "-f", "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml", "--timeout=2m"},
		},
		{
			name: "kubectl",
			args: []string{"delete", "dynamocomponentdeployment.nvidia.com/vllm-dynamo-frontend", "-n", "brixbench-dynamo", "--ignore-not-found", "--wait=false"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=delete", "dynamocomponentdeployment.nvidia.com/vllm-dynamo-frontend", "-n", "brixbench-dynamo", "--timeout=2m"},
		},
		{
			name: "env",
			args: wantDynamoHelmArgs(t, projectRoot, "uninstall", "dynamo-platform", "-n", "brixbench-dynamo", "--ignore-not-found", "--wait", "--timeout", "5m"),
		},
		{
			name: "kubectl",
			args: []string{"delete", "pvc", "--all", "-n", "brixbench-dynamo", "--ignore-not-found"},
		},
		{
			name: "kubectl",
			args: []string{"delete", "namespace", "brixbench-dynamo", "--ignore-not-found"},
		},
		{
			name: "kubectl",
			args: []string{"wait", "--for=delete", "namespace/brixbench-dynamo", "--timeout=10m"},
		},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("expected calls %+v, got %+v", wantCalls, runner.calls)
	}
}

func TestDynamoDeployerTeardownReturnsCriticalCleanupErrors(t *testing.T) {
	runner := &fakeCommandRunner{
		err: errors.New("cleanup failed"),
	}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml",
		projectRoot:    t.TempDir(),
		runner:         runner,
	}

	err := deployer.Teardown(context.Background())
	if err == nil {
		t.Fatalf("expected Teardown to return critical cleanup errors")
	}
	for _, want := range []string{
		"patch-dynamo-graph-deployment-finalizers",
		"delete-dynamo-graph-deployment",
		"wait-delete-dynamo-graph-deployment",
		"uninstall-dynamo-platform",
		"delete-dynamo-namespace",
		"wait-delete-dynamo-namespace",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to include %q, got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "delete-dynamo-pvcs") {
		t.Fatalf("best-effort cleanup errors should not be returned: %v", err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("expected 8 cleanup commands, got %d", len(runner.calls))
	}
}

func TestDynamoDeployerTeardownIgnoresMissingGraphDuringPartialDeploy(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{err: errors.New(`Error from server (NotFound): dynamographdeployments.nvidia.com "vllm-dynamo" not found`)},
			{output: ""},
			{},
			{err: errors.New(`Error from server (NotFound): dynamographdeployments.nvidia.com "vllm-dynamo" not found`)},
			{},
			{},
			{},
			{},
			{},
			{},
		},
	}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml",
		projectRoot:    t.TempDir(),
		runner:         runner,
	}

	if err := deployer.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown returned error for missing graph in partial deploy: %v", err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("expected 8 cleanup commands, got %d", len(runner.calls))
	}
}

func TestDynamoDeployerTeardownIgnoresMissingComponentDeploymentCRD(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{},
			{err: errors.New(`error: the server doesn't have a resource type "dynamocomponentdeployments"`)},
			{},
			{},
			{},
			{},
			{},
			{},
			{},
		},
	}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		engineManifest: "testdata/deployments/dynamo/qwen3-32b-round-robin-4p8d-vke.yaml",
		projectRoot:    t.TempDir(),
		runner:         runner,
	}

	if err := deployer.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown returned error for missing component CRD: %v", err)
	}
	if len(runner.calls) != 8 {
		t.Fatalf("expected 8 cleanup commands, got %d", len(runner.calls))
	}
	if runner.calls[1].name != "kubectl" || !reflect.DeepEqual(runner.calls[1].args, []string{"get", "dynamocomponentdeployments.nvidia.com", "-n", "brixbench-dynamo", "-o", "name"}) {
		t.Fatalf("expected component CRD list command, got %+v", runner.calls[1])
	}
	if got := runner.calls[len(runner.calls)-1].args; !reflect.DeepEqual(got, []string{"wait", "--for=delete", "namespace/brixbench-dynamo", "--timeout=10m"}) {
		t.Fatalf("expected namespace delete wait to still run, got %v", got)
	}
}

func TestDynamoDeployerCaptureArtifactsWritesClusterStateAndManifest(t *testing.T) {
	logDir := t.TempDir()
	projectRoot := t.TempDir()
	manifest := writeDynamoGraphManifest(t, "brixbench-dynamo", "vllm-dynamo")
	runner := &fakeCommandRunner{output: "captured\n"}
	deployer := &DynamoDeployer{
		namespace:      "brixbench-dynamo",
		effectiveNS:    "brixbench-dynamo",
		logDir:         logDir,
		projectRoot:    projectRoot,
		engineManifest: manifest,
		runner:         runner,
	}

	if err := deployer.CaptureArtifacts(context.Background()); err != nil {
		t.Fatalf("CaptureArtifacts returned error: %v", err)
	}

	artifactDir := filepath.Join(logDir, "dynamo-artifacts")
	for _, name := range []string{
		"pods.yaml",
		"services.yaml",
		"events.yaml",
		"dynamographdeployments.yaml",
		"dynamocomponentdeployments.yaml",
		"deployments.yaml",
		"statefulsets.yaml",
		"frontend-logs.txt",
		"component-logs.txt",
		"helm-status.txt",
		"engine-manifest.yaml",
	} {
		if _, err := os.Stat(filepath.Join(artifactDir, name)); err != nil {
			t.Fatalf("expected artifact %s: %v", name, err)
		}
	}
	if len(runner.calls) != 10 {
		t.Fatalf("expected 10 capture commands, got %d", len(runner.calls))
	}
	wantHelmStatusArgs := wantDynamoHelmArgs(t, projectRoot, "status", dynamoPlatformHelmReleaseName, "-n", "brixbench-dynamo")
	if runner.calls[9].name != "env" || !reflect.DeepEqual(runner.calls[9].args, wantHelmStatusArgs) {
		t.Fatalf("expected helm status capture, got %+v", runner.calls[9])
	}
	content, err := os.ReadFile(filepath.Join(artifactDir, "engine-manifest.yaml"))
	if err != nil {
		t.Fatalf("failed to read copied engine manifest: %v", err)
	}
	if !strings.Contains(string(content), "kind: DynamoGraphDeployment") {
		t.Fatalf("expected copied engine manifest, got:\n%s", string(content))
	}
}

func TestDynamoDeployerCaptureArtifactsRecordsCaptureFailures(t *testing.T) {
	logDir := t.TempDir()
	runner := &fakeCommandRunner{err: errors.New("kubectl unavailable")}
	deployer := &DynamoDeployer{
		namespace:   "brixbench-dynamo",
		logDir:      logDir,
		projectRoot: t.TempDir(),
		runner:      runner,
	}

	if err := deployer.CaptureArtifacts(context.Background()); err != nil {
		t.Fatalf("CaptureArtifacts returned error: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(logDir, "dynamo-artifacts", "pods.yaml"))
	if err != nil {
		t.Fatalf("failed to read failure artifact: %v", err)
	}
	if !strings.Contains(string(content), "capture failed") {
		t.Fatalf("expected capture failure to be recorded, got:\n%s", string(content))
	}
}
