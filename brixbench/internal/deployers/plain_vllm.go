package deployers

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	plainVLLMDeploymentName = "vllm-basic"
	plainVLLMServiceName    = "vllm-service"
	plainVLLMServicePort    = 8000
)

// PlainVLLMDeployer manages the minimal no-control-plane vLLM deployment path.
type PlainVLLMDeployer struct {
	engineFile string
	namespace  string
}

var _ Deployer = (*PlainVLLMDeployer)(nil)

// NewPlainVLLMDeployer creates a deployer for raw Kubernetes vLLM manifests.
func NewPlainVLLMDeployer() *PlainVLLMDeployer {
	return &PlainVLLMDeployer{}
}

func (d *PlainVLLMDeployer) Initialize(ctx context.Context, config Config) error {
	d.engineFile = config.EnginePath
	d.namespace = config.Namespace
	return nil
}

func (d *PlainVLLMDeployer) DeployControlPlane(ctx context.Context) error {
	return nil
}

func (d *PlainVLLMDeployer) DeployGateway(ctx context.Context) error {
	return nil
}

func (d *PlainVLLMDeployer) DeployEngine(ctx context.Context) error {
	if err := d.ensureNamespace(ctx, d.namespace); err != nil {
		return err
	}
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl apply -f %s", shellQuote(d.engineFile))); err != nil {
		return fmt.Errorf("failed to deploy plain vLLM manifest %s: %w", d.engineFile, err)
	}
	return nil
}

func (d *PlainVLLMDeployer) WaitForReady(ctx context.Context) error {
	if err := d.runCommand(ctx, fmt.Sprintf(
		"kubectl wait --for=condition=available --timeout=20m deployment/%s -n %s",
		shellQuote(plainVLLMDeploymentName),
		shellQuote(d.namespace),
	)); err != nil {
		return fmt.Errorf("plain vLLM deployment did not become available: %w", err)
	}
	if err := d.runCommand(ctx, fmt.Sprintf(
		"kubectl wait --for=condition=ready --timeout=20m pod -l app=%s -n %s",
		shellQuote(plainVLLMDeploymentName),
		shellQuote(d.namespace),
	)); err != nil {
		return fmt.Errorf("plain vLLM pods did not become ready: %w", err)
	}
	if err := d.waitForServiceEndpoints(ctx); err != nil {
		return err
	}
	return nil
}

func (d *PlainVLLMDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	ip, err := d.captureCommand(ctx, fmt.Sprintf(
		"kubectl get svc %s -n %s -o jsonpath='{.spec.clusterIP}'",
		shellQuote(plainVLLMServiceName),
		shellQuote(d.namespace),
	))
	if err != nil || strings.TrimSpace(strings.Trim(ip, "'")) == "" {
		return "", fmt.Errorf("failed to resolve plain vLLM service IP: %v", err)
	}
	return fmt.Sprintf("http://%s:%d", strings.Trim(ip, "'\n "), plainVLLMServicePort), nil
}

func (d *PlainVLLMDeployer) CaptureArtifacts(ctx context.Context) error {
	return nil
}

func (d *PlainVLLMDeployer) Teardown(ctx context.Context) error {
	d.deleteNamespace(ctx, d.namespace)
	return nil
}

func (d *PlainVLLMDeployer) waitForServiceEndpoints(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Minute)
	command := fmt.Sprintf(
		"kubectl get endpoints %s -n %s -o jsonpath='{.subsets[*].addresses[*].ip}'",
		shellQuote(plainVLLMServiceName),
		shellQuote(d.namespace),
	)
	for time.Now().Before(deadline) {
		output, err := d.captureCommand(ctx, command)
		if err == nil && strings.TrimSpace(strings.Trim(output, "'")) != "" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("plain vLLM service %s has no ready endpoints in namespace %s", plainVLLMServiceName, d.namespace)
}

func (d *PlainVLLMDeployer) deleteNamespace(ctx context.Context, namespace string) {
	fmt.Printf("Deleting namespace: %s\n", namespace)
	nsCmdStr := fmt.Sprintf("kubectl delete namespace %s --ignore-not-found", shellQuote(namespace))
	nsCmd := exec.CommandContext(ctx, "bash", "-c", nsCmdStr)
	if output, err := nsCmd.CombinedOutput(); err != nil {
		fmt.Printf("Warning: failed to delete namespace %s: %v, output: %s\n", namespace, err, string(output))
		return
	}
	waitCmdStr := fmt.Sprintf("kubectl wait --for=delete namespace %s --timeout=10m", shellQuote(namespace))
	waitCmd := exec.CommandContext(ctx, "bash", "-c", waitCmdStr)
	if output, err := waitCmd.CombinedOutput(); err != nil {
		fmt.Printf("Warning: failed waiting for namespace %s deletion: %v, output: %s\n", namespace, err, string(output))
	}
}

func (d *PlainVLLMDeployer) ensureNamespace(ctx context.Context, namespace string) error {
	phase, err := d.captureCommand(ctx, fmt.Sprintf("kubectl get namespace %s -o jsonpath='{.status.phase}' 2>/dev/null || true", shellQuote(namespace)))
	if err != nil {
		return fmt.Errorf("failed to inspect namespace %s: %w", namespace, err)
	}
	if strings.Contains(phase, "Terminating") {
		if err := d.runCommand(ctx, fmt.Sprintf("kubectl wait --for=delete namespace %s --timeout=10m", shellQuote(namespace))); err != nil {
			return fmt.Errorf("failed waiting for namespace %s deletion: %w", namespace, err)
		}
	}
	cmdStr := fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -", shellQuote(namespace))
	cmd := exec.CommandContext(ctx, "bash", "-c", cmdStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to ensure namespace %s: %v, output: %s", namespace, err, string(output))
	}
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/%s --timeout=2m", shellQuote(namespace))); err != nil {
		return fmt.Errorf("failed waiting for namespace %s to become Active: %w", namespace, err)
	}
	return nil
}

func (d *PlainVLLMDeployer) runCommand(ctx context.Context, command string) error {
	fmt.Printf("Executing command: %s\n", command)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if len(strings.TrimSpace(string(output))) > 0 {
		fmt.Printf("Command output:\n%s\n", string(output))
	}
	if err != nil {
		return fmt.Errorf("%v, output: %s", err, string(output))
	}
	return nil
}

func (d *PlainVLLMDeployer) captureCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}
