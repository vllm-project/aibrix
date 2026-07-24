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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// AIBrixDeployer implements the Deployer interface for AIBrix.
type AIBrixDeployer struct {
	engineFile           string
	namespace            string
	logDir               string
	fullStack            bool
	vkeDev               bool
	resolvedCommit       string
	workspacePath        string
	gatewayServicePort   string
	gatewayImageRepo     string
	gatewayImageTag      string
	gatewayEnv           map[string]string
	gatewayResourceFiles []string
}

var _ Deployer = (*AIBrixDeployer)(nil)

const (
	directGatewayServiceName = "aibrix-gateway-plugins"
	directGatewayNamespace   = "aibrix-system"
	envoyGatewayNamespace    = "envoy-gateway-system"
	aibrixGatewayName        = "aibrix-eg"
)

func NewAIBrixDeployer() *AIBrixDeployer {
	return &AIBrixDeployer{}
}

// Initialize sets up the file paths without deploying them.
func (d *AIBrixDeployer) Initialize(ctx context.Context, config Config) error {
	if config.TestCase != nil {
		if err := d.prepareAIBrixTestCase(ctx, config.ProjectRoot, config.TestCase); err != nil {
			return err
		}
	}
	d.engineFile = config.EnginePath
	d.namespace = config.Namespace
	d.logDir = config.LogDir
	d.fullStack = config.FullStack
	d.vkeDev = config.VKEDev
	d.resolvedCommit = config.ResolvedCommit
	d.workspacePath = config.WorkspacePath
	d.gatewayImageRepo = config.GatewayImageRepository
	d.gatewayImageTag = config.GatewayImageTag
	d.gatewayEnv = config.GatewayEnv
	d.gatewayResourceFiles = config.GatewayResourceFiles
	if config.TestCase != nil {
		d.engineFile = config.TestCase.Engine.Manifest
		d.fullStack = config.TestCase.FullStack
		d.vkeDev = config.TestCase.VKEDev
		d.resolvedCommit = config.TestCase.ResolvedCommit
		d.workspacePath = config.TestCase.WorkspacePath
		d.gatewayImageRepo = config.TestCase.GatewayImageRepository
		d.gatewayImageTag = config.TestCase.GatewayImageTag
		d.gatewayEnv = config.TestCase.Gateway.Env
		d.gatewayResourceFiles = config.TestCase.Gateway.Resources
	}
	return nil
}

// DeployControlPlane mirrors the prototype flow:
// dependency apply -> readiness wait -> helm upgrade/install.
func (d *AIBrixDeployer) DeployControlPlane(ctx context.Context) error {
	if !d.fullStack {
		return d.deployGatewayOnlyControlPlane(ctx)
	}
	return d.deployFullStackControlPlane(ctx)
}

func (d *AIBrixDeployer) deployGatewayOnlyControlPlane(ctx context.Context) error {
	fmt.Println("Using gateway-only deployment mode; skipping full control-plane reinstall.")
	if err := d.requireNamespace(ctx, "aibrix-system"); err != nil {
		return err
	}

	if _, err := d.applyGatewayDevOverlay(ctx); err != nil {
		return err
	}
	if len(d.gatewayEnv) > 0 {
		if err := d.applyGatewayEnv(ctx); err != nil {
			return err
		}
	}
	if _, err := d.applyGatewayImage(ctx); err != nil {
		return err
	}

	if err := d.runCommandWithTimeout(ctx, 10*time.Minute, "rollout gateway deployment", "kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system --timeout=9m"); err != nil {
		return fmt.Errorf("failed waiting for gateway rollout: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) applyGatewayEnv(ctx context.Context) error {
	keys := make([]string, 0, len(d.gatewayEnv))
	for key := range d.gatewayEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	args := []string{"set", "env", "deployment/aibrix-gateway-plugins", "-n", "aibrix-system"}
	for _, key := range keys {
		args = append(args, fmt.Sprintf("%s=%s", key, d.gatewayEnv[key]))
	}
	if err := d.runDirectCommand(ctx, "set-gateway-env", "kubectl", args...); err != nil {
		return fmt.Errorf("failed to update gateway environment: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) applyGatewayImage(ctx context.Context) (bool, error) {
	if d.gatewayImageRepo == "" || d.gatewayImageTag == "" {
		return false, nil
	}
	imageRef := fmt.Sprintf("%s:%s", d.gatewayImageRepo, d.gatewayImageTag)
	fmt.Printf("Rolling out source-built gateway image: %s\n", imageRef)
	if err := d.runDirectCommand(ctx, "set-gateway-image", "kubectl", "set", "image", "deployment/"+directGatewayServiceName, "gateway-plugin="+imageRef, "-n", directGatewayNamespace); err != nil {
		return false, fmt.Errorf("failed to update gateway image: %w", err)
	}
	return true, nil
}

func (d *AIBrixDeployer) applyGatewayDevOverlay(ctx context.Context) (bool, error) {
	overlayPath, ok := d.resolveGatewayDevOverlayPath()
	if !ok {
		return false, nil
	}
	if err := d.runDirectCommand(ctx, "apply-gateway-dev-overlay", "kubectl", "apply", "-k", overlayPath); err != nil {
		return false, fmt.Errorf("failed applying gateway dev overlay %s: %w", overlayPath, err)
	}
	return true, nil
}

func (d *AIBrixDeployer) SanityPortForwardTarget(ctx context.Context) (string, string, string, error) {
	namespace, serviceName, _, err := d.resolveGatewayHTTPService(ctx)
	if err != nil {
		return "", "", "", err
	}
	port, err := d.resolveConfiguredGatewayServicePort(ctx)
	if err != nil {
		return "", "", "", err
	}
	if err := d.assertGatewayHTTPServiceReady(ctx); err != nil {
		return "", "", "", err
	}
	return namespace, "service/" + serviceName, port, nil
}

// DeployGateway runs kubectl apply for the AIBrix Gateway YAML.
// Gateway is a cluster-wide resource, so we apply it without -n flag.
func (d *AIBrixDeployer) DeployGateway(ctx context.Context) error {
	fmt.Println("Skipping standalone gateway manifest deployment; using Helm-managed gateway runtime.")
	return nil
}

// DeployEngine runs kubectl apply for the inference engine YAML.
func (d *AIBrixDeployer) DeployEngine(ctx context.Context) error {
	fmt.Printf("Deploying AIBrix Inference Engine: %s\n", d.engineFile)
	if err := d.ensureNamespace(ctx, d.namespace); err != nil {
		return err
	}
	if _, err := d.runLoggedCommand(ctx, "deploy-engine-manifest", "kubectl", "apply", "-f", d.engineFile); err != nil {
		return fmt.Errorf("failed to deploy engine %s: %w", d.engineFile, err)
	}
	if err := d.applyGatewayResourceFiles(ctx); err != nil {
		return fmt.Errorf("failed to apply gateway resource overrides: %w", err)
	}
	if len(d.gatewayResourceFiles) > 0 {
		if err := d.runCommandWithTimeout(ctx, 10*time.Minute, "rollout gateway deployment after resource overrides", "kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system --timeout=9m"); err != nil {
			return fmt.Errorf("failed waiting for gateway rollout after resource overrides: %w", err)
		}
	}
	fmt.Printf("Successfully deployed engine: %s\n", d.engineFile)
	return nil
}

// WaitForReady waits until the AIBrix components are ready.
func (d *AIBrixDeployer) WaitForReady(ctx context.Context) error {
	fmt.Println("Waiting for AIBrix components to be ready...")
	if err := d.waitForGatewayServicePort(ctx); err != nil {
		return err
	}
	if err := d.waitForStormServiceResources(ctx); err != nil {
		return err
	}
	if err := d.waitForEnginePodsReady(ctx); err != nil {
		return err
	}
	if err := d.waitForGatewayChatReady(ctx); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) waitForEnginePodsReady(ctx context.Context) error {
	for i := 0; i < 30; i++ {
		output, err := d.runLoggedCommand(ctx, "wait-engine-pods-ready", "kubectl", "wait", "--for=condition=ready", "--timeout=30s", "pods", "--all", "-n", d.namespace)
		if err == nil {
			fmt.Printf("Engine namespace pods are ready in %s: %s\n", d.namespace, strings.TrimSpace(output))
			return nil
		}
		if i == 29 {
			resourceState := d.bestEffortCapture(ctx, fmt.Sprintf("kubectl get stormservice,svc,pods -n %s -o wide 2>/dev/null || true", shellQuote(d.namespace)))
			return fmt.Errorf("engine pods were not ready in namespace %s after retries: %w\ncurrent resources:\n%s", d.namespace, err, resourceState)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("engine pods were not ready in namespace %s", d.namespace)
}

func (d *AIBrixDeployer) waitForStormServiceResources(ctx context.Context) error {
	modelName, _, _, err := d.inferModelRoutingSpec()
	if err != nil {
		return fmt.Errorf("failed to infer model name for StormService wait: %w", err)
	}

	if err := d.waitForNamedNamespacedResource(ctx, "stormservice", modelName, 2*time.Minute); err != nil {
		return err
	}
	if err := d.waitForNamedNamespacedResource(ctx, "service", modelName, 2*time.Minute); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) waitForNamedNamespacedResource(ctx context.Context, resourceType string, resourceName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := d.captureDirectCommand(ctx, "kubectl", "get", resourceType, resourceName, "-n", d.namespace, "--ignore-not-found", "-o", "name")
		if err != nil {
			return fmt.Errorf("failed to inspect %s/%s in namespace %s: %w", resourceType, resourceName, d.namespace, err)
		}
		if strings.TrimSpace(strings.Trim(output, "'")) != "" {
			fmt.Printf("Found %s/%s in namespace %s\n", resourceType, resourceName, d.namespace)
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	resourceState := d.bestEffortCapture(ctx, fmt.Sprintf("kubectl get stormservice,svc,pods -n %s -o wide 2>/dev/null || true", shellQuote(d.namespace)))
	return fmt.Errorf(
		"timed out waiting for %s/%s in namespace %s within %s\ncurrent resources:\n%s",
		resourceType,
		resourceName,
		d.namespace,
		timeout,
		resourceState,
	)
}

func (d *AIBrixDeployer) waitForGatewayChatReady(ctx context.Context) error {
	modelName, _, _, err := d.inferModelRoutingSpec()
	if err != nil {
		return fmt.Errorf("failed to infer model routing sanity target: %w", err)
	}

	cmd, baseURL, err := d.startGatewayPortForward(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	const maxRetries = 30
	const retryInterval = 2 * time.Second

	var lastStatus string
	for i := 0; i < maxRetries; i++ {
		status, body, probeErr := d.performGatewayChatProbe(ctx, baseURL, modelName)
		if probeErr == nil {
			fmt.Printf("Gateway chat sanity succeeded via %s\n", baseURL)
			return nil
		}
		lastStatus = fmt.Sprintf("%s: %s", status, body)
		fmt.Printf("Gateway chat sanity waiting (attempt %d/%d): %s\n", i+1, maxRetries, lastStatus)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}

	fmt.Printf("Warning: gateway chat sanity did not pass after %d retries via %s; continuing to benchmark anyway. Last result: %s\n", maxRetries, baseURL, lastStatus)
	return nil
}

func (d *AIBrixDeployer) startGatewayPortForward(ctx context.Context) (*exec.Cmd, string, error) {
	localPort, err := reserveLocalPort()
	if err != nil {
		return nil, "", err
	}
	namespace, resource, port, err := d.SanityPortForwardTarget(ctx)
	if err != nil {
		return nil, "", err
	}
	pfCmd := fmt.Sprintf("kubectl port-forward -n %s %s %s:%s", namespace, resource, localPort, port)

	cmd := exec.CommandContext(ctx, "bash", "-lc", pfCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stdout
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("failed to start gateway sanity port-forward: %w", err)
	}
	if err := waitForLocalPortListening(ctx, localPort, cmd, 15*time.Second); err != nil {
		return nil, "", err
	}

	return cmd, "http://127.0.0.1:" + localPort, nil
}

func (d *AIBrixDeployer) performGatewayChatProbe(ctx context.Context, baseURL string, modelName string) (string, string, error) {
	payload := map[string]any{
		"model": modelName,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": "Hello!",
			},
		},
		"max_tokens": 50,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("model", strings.TrimSpace(modelName))
	req.Host = "default.local"

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "request-error", err.Error(), err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.Status, err.Error(), err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Status, string(respBody), fmt.Errorf("unexpected status %s", resp.Status)
	}

	return resp.Status, string(respBody), nil
}

func reserveLocalPort() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to reserve local tcp port: %w", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return "", fmt.Errorf("failed to determine reserved local tcp port")
	}
	return fmt.Sprintf("%d", addr.Port), nil
}

func waitForLocalPortListening(ctx context.Context, port string, cmd *exec.Cmd, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("gateway sanity port-forward exited before local port %s became ready", port)
		}
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("gateway sanity port-forward did not open local port %s within %s", port, timeout)
}

func (d *AIBrixDeployer) inferModelRoutingSpec() (string, string, string, error) {
	content, err := os.ReadFile(d.engineFile)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read engine file for routing inference: %w", err)
	}
	text := string(content)

	modelName := firstRegexGroup(text, `model\.aibrix\.ai/name:\s*"?([A-Za-z0-9._-]+)"?`)
	if modelName == "" {
		modelName = firstRegexGroup(text, `--served-model-name\s+([A-Za-z0-9._-]+)`)
	}
	if modelName == "" {
		return "", "", "", fmt.Errorf("failed to infer model name from engine file %s", d.engineFile)
	}

	modelEngine := firstRegexGroup(text, `model\.aibrix\.ai/engine:\s*"?([A-Za-z0-9._-]+)"?`)
	if modelEngine == "" {
		modelEngine = "vllm"
	}

	modelPort := firstRegexGroup(text, `model\.aibrix\.ai/port:\s*"?([0-9]+)"?`)
	if modelPort == "" {
		modelPort = "8000"
	}

	return modelName, modelEngine, modelPort, nil
}

func firstRegexGroup(text string, pattern string) string {
	matches := regexp.MustCompile(pattern).FindStringSubmatch(text)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

// GetGatewayEndpoint dynamically resolves the URL for the Gateway.
func (d *AIBrixDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	port, err := d.resolveConfiguredGatewayServicePort(ctx)
	if err != nil {
		return "", err
	}
	namespace, serviceName, ip, err := d.resolveGatewayHTTPService(ctx)
	if err != nil {
		return "", err
	}
	if readyErr := d.assertGatewayHTTPServiceReady(ctx); readyErr != nil {
		return "", readyErr
	}
	if strings.TrimSpace(ip) == "" {
		return "", fmt.Errorf("failed to resolve %s/%s service IP", namespace, serviceName)
	}
	return fmt.Sprintf("http://%s:%s", strings.Trim(ip, "'\n "), port), nil
}

func (d *AIBrixDeployer) CaptureArtifacts(ctx context.Context) error {
	if err := d.writeNamespaceSnapshot(ctx, directGatewayNamespace); err != nil {
		return err
	}
	if d.namespace != "" && d.namespace != directGatewayNamespace {
		if err := d.writeNamespaceSnapshot(ctx, d.namespace); err != nil {
			return err
		}
	}
	return nil
}

// Teardown cleans up the AIBrix resources.
func (d *AIBrixDeployer) Teardown(ctx context.Context) error {
	fmt.Printf("Cleaning up benchmark namespace: %s\n", d.namespace)
	d.deleteNamespace(ctx, d.namespace)
	return nil
}

func (d *AIBrixDeployer) deleteNamespace(ctx context.Context, namespace string) {
	fmt.Printf("Deleting namespace: %s\n", namespace)
	if _, err := d.runLoggedCommand(ctx, "delete-namespace-"+namespace, "kubectl", "delete", "namespace", namespace, "--ignore-not-found"); err != nil {
		fmt.Printf("Warning: failed to delete namespace %s: %v\n", namespace, err)
		return
	}
	if _, err := d.runLoggedCommand(ctx, "wait-delete-namespace-"+namespace, "kubectl", "wait", "--for=delete", "namespace/"+namespace, "--timeout=10m"); err != nil {
		fmt.Printf("Warning: failed waiting for namespace %s deletion: %v\n", namespace, err)
	}
}

func (d *AIBrixDeployer) ensureNamespace(ctx context.Context, namespace string) error {
	// status.phase can be stale; deletionTimestamp is a more reliable termination indicator.
	deletionTS, err := d.captureDirectCommand(ctx, "kubectl", "get", "namespace", namespace, "--ignore-not-found", "-o", "jsonpath={.metadata.deletionTimestamp}")
	if err != nil {
		return fmt.Errorf("failed to inspect namespace %s: %w", namespace, err)
	}
	if strings.TrimSpace(strings.Trim(deletionTS, "'")) != "" {
		if err := d.runCommandWithTimeout(ctx, 11*time.Minute, "wait namespace deletion "+namespace, fmt.Sprintf("kubectl wait --for=delete namespace/%s --timeout=10m", shellQuote(namespace))); err != nil {
			return err
		}
	}
	cmdStr := fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -", shellQuote(namespace))
	if _, err := d.runLoggedShellCommand(ctx, "ensure-namespace-"+namespace, cmdStr); err != nil {
		return fmt.Errorf("failed to ensure namespace %s: %w", namespace, err)
	}
	if err := d.runCommandWithTimeout(ctx, 3*time.Minute, "wait namespace active "+namespace, fmt.Sprintf("kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/%s --timeout=2m", shellQuote(namespace))); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) waitForGatewayServicePort(ctx context.Context) error {
	port, err := d.resolveConfiguredGatewayServicePort(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Waiting for gateway HTTP service in %s on port %s\n", envoyGatewayNamespace, port)
	for i := 0; i < 60; i++ {
		namespace, serviceName, ip, resolveErr := d.resolveGatewayHTTPService(ctx)
		if resolveErr == nil && strings.TrimSpace(ip) != "" {
			fmt.Printf("Gateway path ready: %s/%s at %s:%s\n", namespace, serviceName, ip, port)
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return d.assertGatewayHTTPServiceReady(ctx)
}

func (d *AIBrixDeployer) applyGatewayResourceFiles(ctx context.Context) error {
	for _, resourcePath := range d.gatewayResourceFiles {
		resourceBytes, readErr := os.ReadFile(resourcePath)
		if readErr != nil {
			return fmt.Errorf("failed reading gateway resource file %s: %w", resourcePath, readErr)
		}
		var patchWrapper struct {
			PatchTarget *struct {
				Kind      string `yaml:"kind"`
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
				Type      string `yaml:"type"`
			} `yaml:"patchTarget"`
		}
		if err := yaml.Unmarshal(resourceBytes, &patchWrapper); err != nil {
			return fmt.Errorf("failed parsing gateway resource file %s: %w", resourcePath, err)
		}
		if patchWrapper.PatchTarget != nil {
			var patchDoc map[string]any
			if err := yaml.Unmarshal(resourceBytes, &patchDoc); err != nil {
				return fmt.Errorf("failed parsing gateway patch file %s: %w", resourcePath, err)
			}
			delete(patchDoc, "patchTarget")
			patchJSON, err := json.Marshal(patchDoc)
			if err != nil {
				return fmt.Errorf("failed encoding gateway patch file %s: %w", resourcePath, err)
			}
			patchType := patchWrapper.PatchTarget.Type
			if strings.TrimSpace(patchType) == "" {
				patchType = "merge"
			}
			namespaceArg := ""
			if ns := strings.TrimSpace(patchWrapper.PatchTarget.Namespace); ns != "" {
				namespaceArg = " -n " + shellQuote(ns)
			}
			command := fmt.Sprintf(
				"kubectl patch %s %s%s --type=%s --patch %s",
				shellQuote(strings.ToLower(strings.TrimSpace(patchWrapper.PatchTarget.Kind))),
				shellQuote(strings.TrimSpace(patchWrapper.PatchTarget.Name)),
				namespaceArg,
				shellQuote(patchType),
				shellQuote(string(patchJSON)),
			)
			if err := d.runCommand(ctx, command); err != nil {
				return fmt.Errorf("failed patching gateway resource file %s: %w", resourcePath, err)
			}
			continue
		}
		if err := d.runCommand(ctx, fmt.Sprintf("kubectl apply -f %s", shellQuote(resourcePath))); err != nil {
			return fmt.Errorf("failed applying gateway resource file %s: %w", resourcePath, err)
		}
	}
	return nil
}

func (d *AIBrixDeployer) requireNamespace(ctx context.Context, namespace string) error {
	output, err := d.captureDirectCommand(ctx, "kubectl", "get", "namespace", namespace, "--ignore-not-found", "-o", "name")
	if err != nil {
		return fmt.Errorf("failed to inspect namespace %s: %w", namespace, err)
	}
	if strings.TrimSpace(strings.Trim(output, "'")) == "" {
		return fmt.Errorf("required shared control-plane namespace %s not found; use fullstack: true to reinstall", namespace)
	}
	return nil
}

func (d *AIBrixDeployer) runCommand(ctx context.Context, command string) error {
	_, err := d.runLoggedShellCommand(ctx, sanitizeCommandLogName(command), command)
	return err
}

func (d *AIBrixDeployer) runDirectCommand(ctx context.Context, stage string, name string, args ...string) error {
	_, err := d.runLoggedCommand(ctx, stage, name, args...)
	return err
}

func (d *AIBrixDeployer) runCommandWithTimeout(parent context.Context, timeout time.Duration, stage string, command string) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	start := time.Now()
	fmt.Printf("[aibrix] START %s (timeout %s)\n", stage, timeout)
	err := d.runCommand(ctx, command)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Explicit marker requested by user for hang-like failures.
			return fmt.Errorf("HANG: %s timed out after %s: %w", stage, timeout, err)
		}
		return fmt.Errorf("%s failed: %w", stage, err)
	}
	fmt.Printf("[aibrix] DONE %s (%s)\n", stage, time.Since(start).Round(time.Second))
	return nil
}

func (d *AIBrixDeployer) bestEffortCapture(ctx context.Context, command string) string {
	out, err := d.captureCommand(ctx, command)
	if err != nil {
		return fmt.Sprintf("<capture failed: %v>", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "<empty>"
	}
	return out
}

func (d *AIBrixDeployer) captureCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func (d *AIBrixDeployer) captureDirectCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

func shellQuote(value string) string {
	return fmt.Sprintf("%q", value)
}

func (d *AIBrixDeployer) writeNamespaceSnapshot(ctx context.Context, namespace string) error {
	if strings.TrimSpace(d.logDir) == "" || strings.TrimSpace(namespace) == "" {
		return nil
	}
	snapshotDir := filepath.Join(d.logDir, "cluster-snapshots")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return fmt.Errorf("failed to create snapshot directory %s: %w", snapshotDir, err)
	}
	snapshotPath := filepath.Join(snapshotDir, sanitizeSnapshotFileName(namespace)+".yaml")
	content, err := d.captureNamespaceSnapshot(ctx, namespace)
	if err != nil {
		return err
	}
	if err := os.WriteFile(snapshotPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write namespace snapshot %s: %w", snapshotPath, err)
	}
	return nil
}

func (d *AIBrixDeployer) captureNamespaceSnapshot(ctx context.Context, namespace string) (string, error) {
	resourceTypes, err := d.listSnapshotResourceTypes(ctx)
	if err != nil {
		return "", err
	}
	var sections []string
	header := fmt.Sprintf("# Namespace snapshot generated at %s\n# namespace: %s\n", time.Now().Format(time.RFC3339), namespace)
	sections = append(sections, header)
	for _, resourceType := range resourceTypes {
		command := fmt.Sprintf("kubectl get %s -n %s -o yaml 2>/dev/null || true", resourceType, shellQuote(namespace))
		output, err := d.captureCommand(ctx, command)
		if err != nil {
			return "", fmt.Errorf("failed to capture %s snapshot for namespace %s: %w", resourceType, namespace, err)
		}
		trimmed := strings.TrimSpace(output)
		if trimmed == "" || strings.Contains(trimmed, "No resources found") {
			continue
		}
		sections = append(sections, fmt.Sprintf("# Resource: %s\n%s", resourceType, trimmed))
	}
	if len(sections) == 1 {
		sections = append(sections, "# No matching resources were found.")
	}
	return strings.Join(sections, "\n---\n"), nil
}

func (d *AIBrixDeployer) listSnapshotResourceTypes(ctx context.Context) ([]string, error) {
	output, err := d.captureDirectCommand(ctx, "kubectl", "api-resources", "--verbs=list", "--namespaced", "-o", "name")
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaced api resources for snapshot: %w", err)
	}
	seen := make(map[string]struct{})
	var resourceTypes []string
	for _, resourceType := range strings.Fields(output) {
		trimmed := strings.TrimSpace(resourceType)
		if trimmed == "" || trimmed == "secrets" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		resourceTypes = append(resourceTypes, trimmed)
	}
	sort.Strings(resourceTypes)
	return resourceTypes, nil
}

func sanitizeSnapshotFileName(namespace string) string {
	name := strings.TrimSpace(strings.ToLower(namespace))
	if name == "" {
		return "namespace"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-", "\t", "-", "\n", "-")
	name = replacer.Replace(name)
	return strings.Trim(name, "-")
}

func (d *AIBrixDeployer) assertGatewayHTTPServiceReady(ctx context.Context) error {
	port, err := d.resolveConfiguredGatewayServicePort(ctx)
	if err != nil {
		return err
	}
	namespace, serviceName, ip, err := d.resolveGatewayHTTPService(ctx)
	if err == nil && strings.TrimSpace(ip) != "" {
		return nil
	}
	serviceStatus := strings.TrimSpace(d.bestEffortCapture(ctx, fmt.Sprintf("kubectl get svc -n %s -o yaml 2>/dev/null || true", shellQuote(envoyGatewayNamespace))))
	if serviceStatus == "" || serviceStatus == "<empty>" {
		return fmt.Errorf(
			"gateway HTTP path is not ready: no service in %s exposes port %s for %s",
			envoyGatewayNamespace,
			port,
			aibrixGatewayName,
		)
	}
	return fmt.Errorf(
		"gateway HTTP path is not ready: failed to resolve service for %s/%s on port %s: %v\nservices:\n%s",
		namespace,
		serviceName,
		port,
		err,
		serviceStatus,
	)
}

func (d *AIBrixDeployer) resolveConfiguredGatewayServicePort(ctx context.Context) (string, error) {
	if strings.TrimSpace(d.gatewayServicePort) != "" {
		return d.gatewayServicePort, nil
	}
	manifest, err := d.renderConfiguredGatewayManifest(ctx)
	if err != nil {
		return "", err
	}
	port, err := extractGatewayListenerPort(manifest, aibrixGatewayName)
	if err != nil && d.workspacePath != "" {
		baseGatewayPath := filepath.Join(d.workspacePath, "config", "gateway", "gateway.yaml")
		if content, readErr := os.ReadFile(baseGatewayPath); readErr == nil {
			port, err = extractGatewayListenerPort(string(content), aibrixGatewayName)
		}
	}
	if err != nil {
		return "", err
	}
	d.gatewayServicePort = port
	return port, nil
}

func (d *AIBrixDeployer) renderConfiguredGatewayManifest(ctx context.Context) (string, error) {
	if overlayPath, ok := d.resolveGatewayDevOverlayPath(); ok {
		return d.captureDirectCommand(ctx, "kubectl", "kustomize", overlayPath)
	}
	if d.workspacePath == "" {
		return "", fmt.Errorf("workspace path is required to resolve configured gateway manifest")
	}
	chartPath := filepath.Join(d.workspacePath, "dist", "chart")
	valuesPath := filepath.Join(chartPath, "vke.yaml")
	return d.captureDirectCommand(ctx, "helm", "template", "aibrix", chartPath, "-n", directGatewayNamespace, "-f", valuesPath)
}

func (d *AIBrixDeployer) resolveGatewayHTTPService(ctx context.Context) (string, string, string, error) {
	port, err := d.resolveConfiguredGatewayServicePort(ctx)
	if err != nil {
		return "", "", "", err
	}
	output, err := d.captureDirectCommand(ctx, "kubectl", "get", "svc", "-n", envoyGatewayNamespace, "-o", "json")
	if err != nil {
		return "", "", "", fmt.Errorf("failed to list services in %s: %w", envoyGatewayNamespace, err)
	}
	var serviceList struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Type      string `json:"type"`
				ClusterIP string `json:"clusterIP"`
				Ports     []struct {
					Name string `json:"name"`
					Port int    `json:"port"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(output), &serviceList); err != nil {
		return "", "", "", fmt.Errorf("failed to parse services in %s: %w", envoyGatewayNamespace, err)
	}

	var matches []struct {
		name string
		ip   string
	}
	for _, item := range serviceList.Items {
		if item.Metadata.Name == "" {
			continue
		}
		if item.Metadata.Labels["gateway.envoyproxy.io/owning-gateway-name"] != aibrixGatewayName {
			continue
		}
		if item.Metadata.Labels["gateway.envoyproxy.io/owning-gateway-namespace"] != directGatewayNamespace {
			continue
		}
		matchesPort := false
		for _, servicePort := range item.Spec.Ports {
			if fmt.Sprintf("%d", servicePort.Port) == port {
				matchesPort = true
				break
			}
		}
		if !matchesPort {
			continue
		}
		matches = append(matches, struct {
			name string
			ip   string
		}{
			name: item.Metadata.Name,
			ip:   strings.TrimSpace(item.Spec.ClusterIP),
		})
	}
	if len(matches) == 0 {
		return "", "", "", fmt.Errorf("no service in %s exposes HTTP listener port %s for gateway %s", envoyGatewayNamespace, port, aibrixGatewayName)
	}
	if len(matches) > 1 {
		var names []string
		for _, match := range matches {
			names = append(names, match.name)
		}
		return "", "", "", fmt.Errorf("multiple services in %s match gateway %s on port %s: %s", envoyGatewayNamespace, aibrixGatewayName, port, strings.Join(names, ", "))
	}
	if matches[0].ip == "" || strings.EqualFold(matches[0].ip, "None") {
		return envoyGatewayNamespace, matches[0].name, "", fmt.Errorf("gateway service %s/%s has no cluster IP yet", envoyGatewayNamespace, matches[0].name)
	}
	return envoyGatewayNamespace, matches[0].name, matches[0].ip, nil
}

func extractGatewayListenerPort(manifest string, gatewayName string) (string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(manifest))
	var firstGatewayPort string
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("failed to parse configured gateway manifest: %w", err)
		}
		if len(doc) == 0 {
			continue
		}
		kind, _ := doc["kind"].(string)
		if kind != "Gateway" {
			continue
		}
		metadata, _ := doc["metadata"].(map[string]any)
		if metadata == nil {
			continue
		}
		name, _ := metadata["name"].(string)
		spec, _ := doc["spec"].(map[string]any)
		if spec == nil {
			continue
		}
		listeners, _ := spec["listeners"].([]any)
		if len(listeners) == 0 {
			continue
		}
		for _, entry := range listeners {
			listenerMap, _ := entry.(map[string]any)
			if listenerMap == nil {
				continue
			}
			if listenerName, _ := listenerMap["name"].(string); listenerName == "http" {
				port, err := normalizePortValue(listenerMap["port"])
				if err != nil {
					return "", err
				}
				if firstGatewayPort == "" {
					firstGatewayPort = port
				}
				if name == gatewayName {
					return port, nil
				}
				break
			}
		}
		if name != gatewayName {
			continue
		}
		firstListenerMap, _ := listeners[0].(map[string]any)
		if firstListenerMap == nil {
			return "", fmt.Errorf("configured gateway %s has an invalid first listener entry", gatewayName)
		}
		port, err := normalizePortValue(firstListenerMap["port"])
		if err != nil {
			return "", err
		}
		return port, nil
	}
	if firstGatewayPort != "" {
		return firstGatewayPort, nil
	}
	return "", fmt.Errorf("configured gateway %s was not found in rendered manifest", gatewayName)
}

func normalizePortValue(value any) (string, error) {
	switch v := value.(type) {
	case int:
		return fmt.Sprintf("%d", v), nil
	case int64:
		return fmt.Sprintf("%d", v), nil
	case float64:
		return fmt.Sprintf("%.0f", v), nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return "", fmt.Errorf("configured gateway service port is empty")
		}
		return trimmed, nil
	default:
		return "", fmt.Errorf("unsupported gateway service port value %v", value)
	}
}
