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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const llmdRouterHelmReleaseName = "llmd-brixbench"
const LLMdBenchmarkNamespace = "brixbench-llmd"
const llmdRouterChart = "oci://ghcr.io/llm-d/charts/llm-d-router-standalone"
const llmdRouterChartVersion = "v0.9.0"
const llmdGAIEVersion = "v1.5.0"
const llmdGAIEManifestURL = "https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.5.0/v1-manifests.yaml"
const llmdDefaultRepoPath = "../llm-d"
const llmdReadinessTimeout = 10 * time.Minute
const llmdReadinessPollInterval = 5 * time.Second
const llmdHelmStateDirName = "llmd-helm"
const llmdRegistrySecretName = "aibrix-registry-secret"
const llmdRegistryServer = "aibrix-container-registry-cn-beijing.cr.volces.com"
const llmdRouterDeploymentName = llmdRouterHelmReleaseName + "-epp"
const llmdRouterServiceName = llmdRouterHelmReleaseName + "-epp"
const llmdModelDeploymentPrefix = "pd-disaggregation-nvidia-gpu-vllm-"
const llmdDecodeDeploymentName = llmdModelDeploymentPrefix + "decode"
const llmdPrefillDeploymentName = llmdModelDeploymentPrefix + "prefill"

// LLMdDeployer deploys llm-d standalone router and user-provided vLLM manifests.
type LLMdDeployer struct {
	namespace         string
	logDir            string
	projectRoot       string
	version           string
	engineManifest    string
	controlPlanePaths []string
	llmdRepoPath      string
	registrySecret    bool
	runner            commandRunner
}

var _ Deployer = (*LLMdDeployer)(nil)

// NewLLMdDeployer creates a release-based LLM-d deployer.
func NewLLMdDeployer() *LLMdDeployer {
	return &LLMdDeployer{
		runner: execCommandRunner{},
	}
}

func (d *LLMdDeployer) Initialize(ctx context.Context, config Config) error {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}

	d.namespace = LLMdBenchmarkNamespace
	d.logDir = strings.TrimSpace(config.LogDir)
	d.projectRoot = strings.TrimSpace(config.ProjectRoot)
	d.engineManifest = strings.TrimSpace(config.EnginePath)
	d.controlPlanePaths = trimStringSlice(config.ControlPlanePaths)
	d.llmdRepoPath = strings.TrimSpace(os.Getenv("LLMD_REPO"))
	if d.llmdRepoPath == "" {
		d.llmdRepoPath = llmdDefaultRepoPath
	}
	if d.llmdRepoPath != "" && !filepath.IsAbs(d.llmdRepoPath) && d.projectRoot != "" {
		d.llmdRepoPath = filepath.Clean(filepath.Join(d.projectRoot, d.llmdRepoPath))
	}
	if config.TestCase != nil {
		d.version = strings.TrimSpace(config.TestCase.Version)
		if d.engineManifest == "" {
			d.engineManifest = strings.TrimSpace(config.TestCase.Engine.Manifest)
		}
		if len(d.controlPlanePaths) == 0 {
			d.controlPlanePaths = trimStringSlice(config.TestCase.ControlPlane)
		}
	}

	if d.version == "" {
		return fmt.Errorf("LLM-d deployer requires version")
	}
	if d.namespace == "" {
		return fmt.Errorf("LLM-d deployer requires namespace")
	}
	if d.projectRoot == "" {
		return fmt.Errorf("LLM-d deployer requires project root")
	}
	if d.engineManifest == "" {
		return fmt.Errorf("LLM-d deployer requires engine manifest")
	}
	if !pathExists(d.engineManifest) {
		return fmt.Errorf("LLM-d engine manifest %s was not found", d.engineManifest)
	}
	if len(d.controlPlanePaths) == 0 {
		return fmt.Errorf("LLM-d deployer requires router values files in controlplane")
	}
	for _, path := range d.controlPlanePaths {
		if !pathExists(path) {
			return fmt.Errorf("LLM-d controlplane values file %s was not found", path)
		}
	}
	_ = ctx
	return nil
}

func (d *LLMdDeployer) DeployControlPlane(ctx context.Context) error {
	if err := d.ensureLLMdRuntimePrerequisites(ctx); err != nil {
		return err
	}
	if err := d.validateLLMdReleaseTag(ctx); err != nil {
		return err
	}
	if err := d.runLLMdCommand(ctx, "apply-llmd-gaie-crds", "kubectl", "apply", "-f", llmdGAIEManifestURL); err != nil {
		return err
	}
	return d.installLLMdRouter(ctx)
}

func (d *LLMdDeployer) DeployGateway(ctx context.Context) error {
	return nil
}

func (d *LLMdDeployer) DeployEngine(ctx context.Context) error {
	if strings.TrimSpace(d.engineManifest) == "" {
		return fmt.Errorf("LLM-d deployer requires engine manifest")
	}
	return d.runLLMdCommand(ctx, "apply-llmd-engine", "kubectl", "apply", "-n", d.namespace, "-f", d.engineManifest)
}

func (d *LLMdDeployer) WaitForReady(ctx context.Context) error {
	for _, deployment := range []string{llmdRouterDeploymentName, llmdDecodeDeploymentName, llmdPrefillDeploymentName} {
		if err := d.waitForLLMdDeploymentReady(ctx, deployment); err != nil {
			return err
		}
	}
	if _, _, err := d.waitForLLMdRouterService(ctx, llmdReadinessTimeout, llmdReadinessPollInterval); err != nil {
		return fmt.Errorf("LLM-d router service is not ready: %w", err)
	}
	return nil
}

func (d *LLMdDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	service, port, err := d.resolveLLMdRouterService(ctx)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(service.ClusterIP)
	if host == "" || strings.EqualFold(host, "None") {
		host = fmt.Sprintf("%s.%s.svc.cluster.local", service.Name, d.namespace)
	}
	return fmt.Sprintf("http://%s:%d", host, port), nil
}

func (d *LLMdDeployer) CaptureArtifacts(ctx context.Context) error {
	if strings.TrimSpace(d.logDir) == "" {
		return nil
	}
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}

	namespace := strings.TrimSpace(d.namespace)
	if namespace == "" {
		return nil
	}
	artifactDir := filepath.Join(d.logDir, "llmd-artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return fmt.Errorf("failed to create LLM-d artifact directory %s: %w", artifactDir, err)
	}

	for _, capture := range d.llmdArtifactCaptures(namespace) {
		output, err := d.captureLLMdCommand(ctx, capture.stage, capture.name, capture.args...)
		if err != nil {
			output = fmt.Sprintf("capture failed: %v\n", err)
		}
		if err := os.WriteFile(filepath.Join(artifactDir, capture.file), []byte(output), 0o644); err != nil {
			return err
		}
	}

	helmStatus, err := d.captureLLMdHelmCommand(ctx, "capture-llmd-helm-status", "status", llmdRouterHelmReleaseName, "-n", namespace)
	if err != nil {
		helmStatus = fmt.Sprintf("capture failed: %v\n", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "helm-status.txt"), []byte(helmStatus), 0o644); err != nil {
		return err
	}

	if strings.TrimSpace(d.engineManifest) != "" {
		content, err := os.ReadFile(d.engineManifest)
		if err != nil {
			content = []byte(fmt.Sprintf("capture failed: failed to read LLM-d engine manifest %s: %v\n", d.engineManifest, err))
		}
		if err := os.WriteFile(filepath.Join(artifactDir, "engine-manifest.yaml"), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (d *LLMdDeployer) Teardown(ctx context.Context) error {
	namespace := strings.TrimSpace(d.namespace)
	var criticalErrs []error
	addCriticalErr := func(err error) {
		if err != nil {
			criticalErrs = append(criticalErrs, err)
		}
	}
	addCriticalErrUnlessNotFound := func(err error) {
		if err != nil && !isDynamoCleanupNotFoundError(err) {
			criticalErrs = append(criticalErrs, err)
		}
	}
	if strings.TrimSpace(d.engineManifest) != "" && namespace != "" {
		addCriticalErr(d.runLLMdCleanupCommand(ctx, "delete-llmd-engine", "kubectl", "delete", "-n", namespace, "-f", d.engineManifest, "--ignore-not-found", "--wait=false"))
		addCriticalErrUnlessNotFound(d.runLLMdCleanupCommand(ctx, "wait-delete-llmd-engine", "kubectl", "wait", "--for=delete", "-n", namespace, "-f", d.engineManifest, "--timeout=2m"))
	}
	if namespace != "" {
		addCriticalErr(d.runLLMdHelmCleanupCommand(ctx, "uninstall-llmd-router", "uninstall", llmdRouterHelmReleaseName, "-n", namespace, "--ignore-not-found", "--wait", "--timeout", "5m"))
		addCriticalErr(d.runLLMdCleanupCommand(ctx, "delete-llmd-namespace", "kubectl", "delete", "namespace", namespace, "--ignore-not-found"))
		addCriticalErr(d.runLLMdCleanupCommand(ctx, "wait-delete-llmd-namespace", "kubectl", "wait", "--for=delete", "namespace/"+namespace, "--timeout=10m"))
	}
	return errors.Join(criticalErrs...)
}

func (d *LLMdDeployer) ensureLLMdRuntimePrerequisites(ctx context.Context) error {
	if err := d.runLLMdCommand(ctx, "ensure-llmd-namespace", "bash", "-lc", fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -", shellQuote(d.namespace))); err != nil {
		return err
	}
	registrySecret, err := d.ensureLLMdImagePullSecret(ctx)
	if err != nil {
		return err
	}
	d.registrySecret = registrySecret
	return nil
}

func (d *LLMdDeployer) ensureLLMdImagePullSecret(ctx context.Context) (bool, error) {
	err := d.runLLMdCommand(ctx, "check-llmd-registry-secret", "kubectl", "get", "secret", llmdRegistrySecretName, "-n", d.namespace)
	if err == nil {
		return true, nil
	}

	username := strings.TrimSpace(os.Getenv("LLMD_REGISTRY_USERNAME"))
	password := os.Getenv("LLMD_REGISTRY_PASSWORD")
	if username == "" && password == "" {
		fmt.Printf("Warning: LLM-d image pull secret %s is missing in namespace %s; continuing without imagePullSecrets\n", llmdRegistrySecretName, d.namespace)
		return false, nil
	}
	if username == "" || password == "" {
		return false, fmt.Errorf("LLM-d image pull secret %s is missing in namespace %s; set both LLMD_REGISTRY_USERNAME and LLMD_REGISTRY_PASSWORD or unset both to run without imagePullSecrets", llmdRegistrySecretName, d.namespace)
	}

	command := fmt.Sprintf(
		"kubectl create secret docker-registry %s --docker-server=%s --docker-username=\"$LLMD_REGISTRY_USERNAME\" --docker-password=\"$LLMD_REGISTRY_PASSWORD\" -n %s --dry-run=client -o yaml | kubectl apply -f -",
		shellQuote(llmdRegistrySecretName),
		shellQuote(llmdRegistryServer),
		shellQuote(d.namespace),
	)
	if err := d.runLLMdCommand(ctx, "create-llmd-registry-secret", "bash", "-lc", command); err != nil {
		return false, err
	}
	return true, nil
}

func (d *LLMdDeployer) validateLLMdReleaseTag(ctx context.Context) error {
	repoPath := strings.TrimSpace(d.llmdRepoPath)
	if repoPath == "" || !dirExists(repoPath) {
		return fmt.Errorf("LLM-d repo path %s was not found; set LLMD_REPO to a local llm-d checkout", repoPath)
	}
	output, err := d.captureLLMdCommand(ctx, "validate-llmd-release-tag", "git", "-C", repoPath, "rev-parse", d.version+"^{tag}")
	if err != nil {
		return fmt.Errorf("failed to validate LLM-d release tag %s in %s: %w", d.version, repoPath, err)
	}
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("LLM-d release tag %s was not found in %s", d.version, repoPath)
	}
	return nil
}

func (d *LLMdDeployer) installLLMdRouter(ctx context.Context) error {
	args := []string{
		"upgrade", "--install", llmdRouterHelmReleaseName, llmdRouterChart,
		"-n", d.namespace,
		"--create-namespace",
		"--version", llmdRouterChartVersion,
		"--set", "router.monitoring.prometheus.auth.enabled=false",
	}
	for _, valuesFile := range d.controlPlanePaths {
		args = append(args, "-f", valuesFile)
	}
	if err := d.runLLMdHelmCommand(ctx, "helm-install-llmd-router", args...); err != nil {
		return err
	}
	if d.registrySecret {
		if err := d.patchLLMdRouterImagePullSecret(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (d *LLMdDeployer) patchLLMdRouterImagePullSecret(ctx context.Context) error {
	patch := fmt.Sprintf(`{"imagePullSecrets":[{"name":%q}]}`, llmdRegistrySecretName)
	if err := d.runLLMdCommand(ctx, "patch-llmd-router-serviceaccount-image-pull-secret", "kubectl", "patch", "serviceaccount", llmdRouterDeploymentName, "-n", d.namespace, "--type=merge", "-p", patch); err != nil {
		return err
	}
	return d.runLLMdCommand(ctx, "restart-llmd-router-after-image-pull-secret", "kubectl", "rollout", "restart", "deployment/"+llmdRouterDeploymentName, "-n", d.namespace)
}

func (d *LLMdDeployer) waitForLLMdDeploymentReady(ctx context.Context, deployment string) error {
	if err := d.runLLMdCommand(ctx, "rollout-llmd-"+deployment, "kubectl", "rollout", "status", "deployment/"+deployment, "-n", d.namespace, "--timeout=10m"); err != nil {
		return fmt.Errorf("LLM-d deployment %s is not ready: %w", deployment, err)
	}
	return nil
}

func (d *LLMdDeployer) waitForLLMdRouterService(ctx context.Context, timeout time.Duration, interval time.Duration) (llmdService, int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		service, port, err := d.resolveLLMdRouterService(ctx)
		if err == nil {
			return service, port, nil
		}
		lastErr = err
		if err := sleepOrContextDone(ctx, interval); err != nil {
			return llmdService{}, 0, err
		}
	}
	if lastErr != nil {
		return llmdService{}, 0, lastErr
	}
	return llmdService{}, 0, fmt.Errorf("timed out waiting for LLM-d router service")
}

func (d *LLMdDeployer) resolveLLMdRouterService(ctx context.Context) (llmdService, int, error) {
	output, err := d.captureLLMdCommand(ctx, "get-llmd-router-service", "kubectl", "get", "svc", llmdRouterServiceName, "-n", d.namespace, "-o", "json")
	if err != nil {
		return llmdService{}, 0, err
	}
	service, err := parseLLMdService(output)
	if err != nil {
		return llmdService{}, 0, err
	}
	port, err := selectLLMdRouterServicePort(service)
	if err != nil {
		return llmdService{}, 0, err
	}
	return service, port, nil
}

type llmdService struct {
	Name      string
	ClusterIP string
	Ports     []int
}

func parseLLMdService(output string) (llmdService, error) {
	var service struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			ClusterIP string `json:"clusterIP"`
			Ports     []struct {
				Port int `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(output), &service); err != nil {
		return llmdService{}, fmt.Errorf("failed to parse LLM-d router service: %w", err)
	}
	if strings.TrimSpace(service.Metadata.Name) == "" {
		return llmdService{}, fmt.Errorf("LLM-d router service has no metadata.name")
	}
	ports := make([]int, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		ports = append(ports, port.Port)
	}
	return llmdService{Name: service.Metadata.Name, ClusterIP: service.Spec.ClusterIP, Ports: ports}, nil
}

func selectLLMdRouterServicePort(service llmdService) (int, error) {
	for _, port := range service.Ports {
		if port == 80 {
			return port, nil
		}
	}
	for _, port := range service.Ports {
		if port == 8081 {
			return port, nil
		}
	}
	if len(service.Ports) == 1 {
		return service.Ports[0], nil
	}
	return 0, fmt.Errorf("LLM-d router service %s has no HTTP port 80 or 8081", service.Name)
}

type llmdArtifactCapture struct {
	file  string
	stage string
	name  string
	args  []string
}

func (d *LLMdDeployer) llmdArtifactCaptures(namespace string) []llmdArtifactCapture {
	return []llmdArtifactCapture{
		{file: "pods.yaml", stage: "capture-llmd-pods", name: "kubectl", args: []string{"get", "pods", "-n", namespace, "-o", "yaml"}},
		{file: "services.yaml", stage: "capture-llmd-services", name: "kubectl", args: []string{"get", "services", "-n", namespace, "-o", "yaml"}},
		{file: "endpoints.yaml", stage: "capture-llmd-endpoints", name: "kubectl", args: []string{"get", "endpoints", "-n", namespace, "-o", "yaml"}},
		{file: "events.yaml", stage: "capture-llmd-events", name: "kubectl", args: []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp", "-o", "yaml"}},
		{file: "deployments.yaml", stage: "capture-llmd-deployments", name: "kubectl", args: []string{"get", "deployments", "-n", namespace, "-o", "yaml"}},
		{file: "inferencepools.yaml", stage: "capture-llmd-inferencepools", name: "kubectl", args: []string{"get", "inferencepools.inference.networking.x-k8s.io", "-n", namespace, "-o", "yaml"}},
		{file: "router-logs.txt", stage: "capture-llmd-router-logs", name: "kubectl", args: []string{"logs", "-n", namespace, "deployment/" + llmdRouterDeploymentName, "--all-containers=true", "--prefix=true", "--tail=-1"}},
		{file: "modelserver-logs.txt", stage: "capture-llmd-modelserver-logs", name: "kubectl", args: []string{"logs", "-n", namespace, "-l", "llm-d.ai/guide=pd-disaggregation", "--all-containers=true", "--prefix=true", "--tail=-1"}},
	}
}

func (d *LLMdDeployer) runLLMdCommand(ctx context.Context, stage string, name string, args ...string) error {
	startedAt := time.Now()
	output, err := d.runner.Run(ctx, name, args...)
	finishedAt := time.Now()

	if logErr := d.writeLLMdCommandLog(stage, name, args, startedAt, finishedAt, commandExitCode(err), output); logErr != nil && err == nil {
		return logErr
	}
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return fmt.Errorf("%s failed: %w: %s", stage, err, output)
		}
		return fmt.Errorf("%s failed: %w", stage, err)
	}
	return nil
}

func (d *LLMdDeployer) captureLLMdCommand(ctx context.Context, stage string, name string, args ...string) (string, error) {
	startedAt := time.Now()
	output, err := d.runner.Run(ctx, name, args...)
	finishedAt := time.Now()

	if logErr := d.writeLLMdCommandLog(stage, name, args, startedAt, finishedAt, commandExitCode(err), output); logErr != nil && err == nil {
		return "", logErr
	}
	if err != nil {
		output = strings.TrimSpace(output)
		if output != "" {
			return output, fmt.Errorf("%s failed: %w: %s", stage, err, output)
		}
		return output, fmt.Errorf("%s failed: %w", stage, err)
	}
	return output, nil
}

func (d *LLMdDeployer) runLLMdCleanupCommand(ctx context.Context, stage string, name string, args ...string) error {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}
	if strings.TrimSpace(name) == "" {
		return nil
	}
	if err := d.runLLMdCommand(ctx, stage, name, args...); err != nil {
		fmt.Printf("Warning: LLM-d cleanup step %s failed: %v\n", stage, err)
		return err
	}
	return nil
}

func (d *LLMdDeployer) runLLMdHelmCommand(ctx context.Context, stage string, args ...string) error {
	name, commandArgs, err := d.llmdHelmCommand(args...)
	if err != nil {
		return err
	}
	return d.runLLMdCommand(ctx, stage, name, commandArgs...)
}

func (d *LLMdDeployer) captureLLMdHelmCommand(ctx context.Context, stage string, args ...string) (string, error) {
	name, commandArgs, err := d.llmdHelmCommand(args...)
	if err != nil {
		return "", err
	}
	return d.captureLLMdCommand(ctx, stage, name, commandArgs...)
}

func (d *LLMdDeployer) runLLMdHelmCleanupCommand(ctx context.Context, stage string, args ...string) error {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}
	if err := d.runLLMdHelmCommand(ctx, stage, args...); err != nil {
		fmt.Printf("Warning: LLM-d cleanup step %s failed: %v\n", stage, err)
		return err
	}
	return nil
}

func (d *LLMdDeployer) llmdHelmCommand(args ...string) (string, []string, error) {
	envArgs, err := d.llmdHelmEnvArgs()
	if err != nil {
		return "", nil, err
	}
	commandArgs := append(envArgs, "helm")
	commandArgs = append(commandArgs, args...)
	return "env", commandArgs, nil
}

func (d *LLMdDeployer) llmdHelmEnvArgs() ([]string, error) {
	stateDir := filepath.Join(strings.TrimSpace(d.projectRoot), ".tmp", llmdHelmStateDirName)
	if strings.TrimSpace(d.projectRoot) == "" {
		return nil, fmt.Errorf("LLM-d deployer requires project root")
	}
	configDir := filepath.Join(stateDir, "config")
	cacheDir := filepath.Join(stateDir, "cache")
	dataDir := filepath.Join(stateDir, "data")
	repositoryCacheDir := filepath.Join(cacheDir, "repository")
	for _, dir := range []string{configDir, cacheDir, dataDir, repositoryCacheDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create LLM-d Helm state directory %s: %w", dir, err)
		}
	}
	return []string{
		"HELM_CONFIG_HOME=" + configDir,
		"HELM_CACHE_HOME=" + cacheDir,
		"HELM_DATA_HOME=" + dataDir,
		"HELM_REPOSITORY_CONFIG=" + filepath.Join(configDir, "repositories.yaml"),
		"HELM_REPOSITORY_CACHE=" + repositoryCacheDir,
	}, nil
}

func (d *LLMdDeployer) writeLLMdCommandLog(stage string, name string, args []string, startedAt time.Time, finishedAt time.Time, exitCode int, output string) error {
	if strings.TrimSpace(d.logDir) == "" {
		return nil
	}
	logDir := filepath.Join(d.logDir, "commands")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("failed to create LLM-d command log directory: %w", err)
	}

	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", sanitizeDynamoCommandStage(stage)))
	content := strings.Builder{}
	content.WriteString("command: ")
	content.WriteString(formatExecCommand(name, args...))
	content.WriteString("\n")
	content.WriteString("startedAt: ")
	content.WriteString(startedAt.Format(time.RFC3339Nano))
	content.WriteString("\n")
	content.WriteString("finishedAt: ")
	content.WriteString(finishedAt.Format(time.RFC3339Nano))
	content.WriteString("\n")
	content.WriteString(fmt.Sprintf("exitCode: %d\n", exitCode))
	content.WriteString("\noutput:\n")
	content.WriteString(output)
	if output != "" && !strings.HasSuffix(output, "\n") {
		content.WriteString("\n")
	}
	return os.WriteFile(logPath, []byte(content.String()), 0o644)
}

func trimStringSlice(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}
