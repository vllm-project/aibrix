package deployers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
	"gopkg.in/yaml.v3"
)

func (d *AIBrixDeployer) prepareAIBrixTestCase(ctx context.Context, projectRoot string, testCase *resolver.Test) error {
	workspace, err := resolver.PrepareWorkspace(ctx, projectRoot, testCase)
	if err != nil {
		return fmt.Errorf("failed to prepare source workspace: %w", err)
	}
	if workspace != nil {
		fmt.Printf("Prepared source workspace: %s (%s)\n", workspace.Path, workspace.CommitHash)
	}
	if !testCase.FullStack {
		gatewayImage, err := resolver.PrepareGatewayImage(ctx, projectRoot, testCase)
		if err != nil {
			return fmt.Errorf("failed to prepare gateway image: %w", err)
		}
		if gatewayImage != nil {
			fmt.Printf("Prepared gateway image: %s\n", gatewayImage.Image)
		}
	}
	if err := resolver.FetchArtifacts(ctx, projectRoot, testCase); err != nil {
		return fmt.Errorf("failed to fetch artifacts: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) deployFullStackControlPlane(ctx context.Context) error {
	if d.workspacePath == "" {
		return fmt.Errorf("workspace path is required for Helm-based AIBrix deployment")
	}
	if err := d.waitForHelmReleaseReady(ctx, "aibrix", "aibrix-system", 2*time.Minute); err != nil {
		return fmt.Errorf("helm release aibrix is locked before control-plane preparation: %w", err)
	}
	if err := d.assertWorkspaceGatewayIntent(); err != nil {
		return err
	}

	fmt.Printf("Deploying AIBrix control plane from workspace: %s\n", d.workspacePath)
	if err := d.runFullStackCleanInstall(ctx); err != nil {
		return err
	}
	if d.vkeDev {
		if err := d.applyFullStackVKEDevOverrides(ctx); err != nil {
			return err
		}
	}
	if err := d.applyFullStackGatewayRuntimeOverrides(ctx); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) runFullStackCleanInstall(ctx context.Context) error {
	if err := d.cleanupPreviousFullStackInstall(ctx); err != nil {
		return err
	}
	if err := d.installVKEDependencies(ctx); err != nil {
		return err
	}
	if err := d.applyAIBrixCRDs(ctx); err != nil {
		return err
	}
	return d.installAIBrixCoreChart(ctx)
}

func (d *AIBrixDeployer) cleanupPreviousFullStackInstall(ctx context.Context) error {
	if err := d.runCommand(ctx, "helm uninstall aibrix -n aibrix-system || true"); err != nil {
		return fmt.Errorf("failed to cleanup previous Helm release: %w", err)
	}
	if err := d.deleteFullStackNamespaces(ctx); err != nil {
		return err
	}
	if err := d.deleteFullStackClusterScopedResources(ctx); err != nil {
		return err
	}
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl delete -k %s --ignore-not-found=true || true", shellQuote(d.vkeDependencyOverlayPath()))); err != nil {
		return fmt.Errorf("failed to delete VKE dependency overlay: %w", err)
	}
	if err := d.runCommand(ctx, "kubectl wait --for=delete namespace/aibrix-system --timeout=180s || true"); err != nil {
		return fmt.Errorf("failed waiting for namespace aibrix-system deletion: %w", err)
	}
	if err := d.runCommand(ctx, "kubectl wait --for=delete namespace/envoy-gateway-system --timeout=180s || true"); err != nil {
		return fmt.Errorf("failed waiting for namespace envoy-gateway-system deletion: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) deleteFullStackNamespaces(ctx context.Context) error {
	namespaces := []string{
		"aibrix-system",
		"envoy-gateway-system",
	}
	for _, namespace := range namespaces {
		if err := d.runDirectCommand(ctx, "delete-namespace-"+namespace, "kubectl", "delete", "namespace", namespace, "--ignore-not-found=true"); err != nil {
			return err
		}
	}
	return nil
}

func (d *AIBrixDeployer) deleteFullStackClusterScopedResources(ctx context.Context) error {
	resources := []struct {
		resourceType string
		name         string
	}{
		{resourceType: "mutatingwebhookconfiguration", name: "aibrix-mutating-webhook-configuration"},
		{resourceType: "validatingwebhookconfiguration", name: "aibrix-validating-webhook-configuration"},
		{resourceType: "gatewayclass.gateway.networking.k8s.io", name: "aibrix-eg"},
		{resourceType: "validatingadmissionpolicybinding", name: "safe-upgrades.gateway.networking.k8s.io"},
		{resourceType: "validatingadmissionpolicy", name: "safe-upgrades.gateway.networking.k8s.io"},
	}
	for _, resource := range resources {
		stage := fmt.Sprintf("delete-%s-%s", resource.resourceType, resource.name)
		if err := d.runDirectCommand(ctx, stage, "kubectl", "delete", resource.resourceType, resource.name, "--ignore-not-found=true"); err != nil {
			return fmt.Errorf("failed deleting %s/%s: %w", resource.resourceType, resource.name, err)
		}
	}
	if err := d.deleteResourcesByNamePrefix(ctx, "kubectl get clusterrole -o name 2>/dev/null || true", "clusterrole.rbac.authorization.k8s.io/aibrix-"); err != nil {
		return err
	}
	if err := d.deleteResourcesByNamePrefix(ctx, "kubectl get clusterrolebinding -o name 2>/dev/null || true", "clusterrolebinding.rbac.authorization.k8s.io/aibrix-"); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) deleteResourcesByNamePrefix(ctx context.Context, listCommand string, prefix string) error {
	output, err := d.captureCommand(ctx, listCommand)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(output, "\n") {
		resourceName := strings.TrimSpace(strings.Trim(line, "'"))
		if resourceName == "" || !strings.HasPrefix(resourceName, prefix) {
			continue
		}
		if err := d.runDirectCommand(ctx, "delete-"+sanitizeCommandLogName(resourceName), "kubectl", "delete", resourceName, "--ignore-not-found=true"); err != nil {
			return fmt.Errorf("failed deleting %s: %w", resourceName, err)
		}
	}
	return nil
}

func (d *AIBrixDeployer) installVKEDependencies(ctx context.Context) error {
	if err := d.runCommandWithTimeout(ctx, 10*time.Minute, "apply VKE dependencies", fmt.Sprintf("kubectl apply -k %s --server-side --force-conflicts", shellQuote(d.vkeDependencyOverlayPath()))); err != nil {
		return err
	}
	if err := d.waitForDeploymentRollouts(ctx, envoyGatewayNamespace, []string{"envoy-gateway"}, ""); err != nil {
		return err
	}
	if err := d.waitForDeploymentRollouts(ctx, directGatewayNamespace, []string{"aibrix-kuberay-operator", "aibrix-redis-master"}, ""); err != nil {
		return err
	}
	if err := d.waitForAllNamespaceWorkloads(ctx, directGatewayNamespace, "dependency"); err != nil {
		return err
	}
	return nil
}

func (d *AIBrixDeployer) applyAIBrixCRDs(ctx context.Context) error {
	crdPath := d.chartCRDsPath()
	if err := d.runCommandWithTimeout(ctx, 3*time.Minute, "apply AIBrix CRDs", fmt.Sprintf("kubectl apply -f %s --server-side", shellQuote(crdPath))); err != nil {
		return err
	}
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl wait --for=condition=Established --timeout=180s -f %s || true", shellQuote(crdPath))); err != nil {
		return fmt.Errorf("failed waiting for CRDs to become Established: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) installAIBrixCoreChart(ctx context.Context) error {
	helmArgs := []string{
		"upgrade", "--install", "aibrix", shellQuote(d.chartPath()),
		"-n", "aibrix-system",
		"-f", shellQuote(d.chartValuesPath()),
		"--create-namespace",
		"--wait",
		"--force",
		"--timeout", "15m",
	}
	helmCmd := "helm " + strings.Join(helmArgs, " ")
	if err := d.runCommandWithTimeout(ctx, 16*time.Minute, "helm upgrade/install aibrix", helmCmd); err != nil {
		helmList := d.bestEffortCapture(ctx, "helm list -n aibrix-system -a 2>/dev/null || true")
		helmStatus := d.bestEffortCapture(ctx, "helm status aibrix -n aibrix-system 2>/dev/null || true")
		helmHistory := d.bestEffortCapture(ctx, "helm history aibrix -n aibrix-system 2>/dev/null || true")
		return fmt.Errorf(
			"failed to deploy AIBrix core via Helm: %w\nstage: helm upgrade/install aibrix\ncmd: %s\nhelm list:\n%s\nhelm status:\n%s\nhelm history:\n%s",
			err, helmCmd, helmList, helmStatus, helmHistory,
		)
	}
	if err := d.runCommand(ctx, "kubectl wait --for=condition=Accepted --timeout=5m gateway/aibrix-eg -n aibrix-system || true"); err != nil {
		return fmt.Errorf("failed waiting for gateway aibrix-eg to be accepted: %w", err)
	}
	rollouts := []string{
		"aibrix-controller-manager",
		"aibrix-gpu-optimizer",
		"aibrix-gateway-plugins",
		"aibrix-metadata-service",
	}
	for _, deployment := range rollouts {
		stage := "rollout " + deployment
		command := fmt.Sprintf("kubectl rollout status deployment/%s -n aibrix-system --timeout=10m", deployment)
		if err := d.runCommandWithTimeout(ctx, 11*time.Minute, stage, command); err != nil {
			return err
		}
	}
	if err := d.runCommand(ctx, "kubectl wait --for=condition=ready --timeout=5m pods --all -n aibrix-system || true"); err != nil {
		return fmt.Errorf("failed waiting for Helm pods: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) applyFullStackVKEDevOverrides(ctx context.Context) error {
	components := []string{"manager", "gpu-optimizer", "gateway-plugin"}
	for _, component := range components {
		overlayPath, err := d.requireVKEDevOverlayPath(component)
		if err != nil {
			return err
		}
		stage := "apply vke-dev overlay " + component
		command := fmt.Sprintf("kubectl apply -k %s --server-side --force-conflicts", shellQuote(overlayPath))
		if err := d.runCommandWithTimeout(ctx, 5*time.Minute, stage, command); err != nil {
			return err
		}
	}
	rollouts := []string{
		"aibrix-controller-manager",
		"aibrix-gpu-optimizer",
		"aibrix-gateway-plugins",
	}
	for _, deployment := range rollouts {
		stage := "rollout " + deployment + " after vke-dev override"
		command := fmt.Sprintf("kubectl rollout status deployment/%s -n aibrix-system --timeout=10m", deployment)
		if err := d.runCommandWithTimeout(ctx, 11*time.Minute, stage, command); err != nil {
			return err
		}
	}
	return nil
}

func (d *AIBrixDeployer) applyFullStackGatewayRuntimeOverrides(ctx context.Context) error {
	imageUpdated, err := d.applyGatewayImage(ctx)
	if err != nil {
		return err
	}
	envUpdated := false
	if len(d.gatewayEnv) > 0 {
		if err := d.applyGatewayEnv(ctx); err != nil {
			return err
		}
		envUpdated = true
	}
	if !imageUpdated && !envUpdated {
		return nil
	}
	if err := d.runCommandWithTimeout(ctx, 10*time.Minute, "rollout gateway deployment", "kubectl rollout status deployment/aibrix-gateway-plugins -n aibrix-system --timeout=9m"); err != nil {
		return fmt.Errorf("failed waiting for gateway rollout: %w", err)
	}
	return nil
}

func (d *AIBrixDeployer) vkeDependencyOverlayPath() string {
	return filepath.Join(d.workspacePath, "config", "overlays", "vke", "dependency")
}

func (d *AIBrixDeployer) chartPath() string {
	return filepath.Join(d.workspacePath, "dist", "chart")
}

func (d *AIBrixDeployer) chartValuesPath() string {
	return filepath.Join(d.chartPath(), "vke.yaml")
}

func (d *AIBrixDeployer) chartCRDsPath() string {
	return filepath.Join(d.chartPath(), "crds")
}

func (d *AIBrixDeployer) requireVKEDevOverlayPath(component string) (string, error) {
	overlayPath := filepath.Join(d.workspacePath, "config", "overlays", "vke-dev", component)
	if _, err := os.Stat(filepath.Join(overlayPath, "kustomization.yaml")); err != nil {
		return "", fmt.Errorf("required vke-dev overlay not found for %s: %s", component, overlayPath)
	}
	return overlayPath, nil
}

func (d *AIBrixDeployer) waitForDeploymentRollouts(ctx context.Context, namespace string, deployments []string, stageSuffix string) error {
	for _, deployment := range deployments {
		stage := "rollout " + deployment + stageSuffix
		command := fmt.Sprintf("kubectl rollout status deployment/%s -n %s --timeout=10m", deployment, namespace)
		if err := d.runCommandWithTimeout(ctx, 11*time.Minute, stage, command); err != nil {
			return err
		}
	}
	return nil
}

func (d *AIBrixDeployer) waitForAllNamespaceWorkloads(ctx context.Context, namespace string, stagePrefix string) error {
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl wait --for=condition=available --timeout=5m deployments --all -n %s || true", namespace)); err != nil {
		return fmt.Errorf("failed waiting for %s deployments: %w", stagePrefix, err)
	}
	if err := d.runCommand(ctx, fmt.Sprintf("kubectl wait --for=condition=ready --timeout=5m pods --all -n %s || true", namespace)); err != nil {
		return fmt.Errorf("failed waiting for %s pods: %w", stagePrefix, err)
	}
	return nil
}

func (d *AIBrixDeployer) assertWorkspaceGatewayIntent() error {
	if _, ok := d.resolveGatewayDevOverlayPath(); ok {
		return nil
	}
	return d.assertWorkspaceGatewayValues()
}

func (d *AIBrixDeployer) assertWorkspaceGatewayValues() error {
	if d.workspacePath == "" {
		return fmt.Errorf("workspace path is required for direct gateway validation")
	}
	valuesPath := filepath.Join(d.workspacePath, "dist", "chart", "vke.yaml")
	content, err := os.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("failed to read workspace gateway values %s: %w", valuesPath, err)
	}
	var values struct {
		Gateway struct {
			Enable         bool `yaml:"enable"`
			EnvoyAsSideCar bool `yaml:"envoyAsSideCar"`
		} `yaml:"gateway"`
	}
	if err := yaml.Unmarshal(content, &values); err != nil {
		return fmt.Errorf("failed to parse workspace gateway values %s: %w", valuesPath, err)
	}
	if values.Gateway.Enable || !values.Gateway.EnvoyAsSideCar {
		return fmt.Errorf(
			"unsupported AIBrix workspace gateway mode in %s: expected gateway.enable=false and gateway.envoyAsSideCar=true for the direct plugin path, got gateway.enable=%t envoyAsSideCar=%t",
			valuesPath,
			values.Gateway.Enable,
			values.Gateway.EnvoyAsSideCar,
		)
	}
	return nil
}

func (d *AIBrixDeployer) resolveGatewayDevOverlayPath() (string, bool) {
	if d.workspacePath == "" {
		return "", false
	}
	overlayPath := filepath.Join(d.workspacePath, "config", "overlays", "vke-dev", "gateway-plugin")
	if _, err := os.Stat(filepath.Join(overlayPath, "kustomization.yaml")); err == nil {
		return overlayPath, true
	}
	return "", false
}

func (d *AIBrixDeployer) waitForHelmReleaseReady(ctx context.Context, releaseName string, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := d.helmReleaseStatus(ctx, releaseName, namespace)
		if err != nil {
			return err
		}
		if status == "" || !isHelmReleaseLocked(status) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("release %s in namespace %s remained in %s for %s", releaseName, namespace, status, timeout)
		}
		fmt.Printf("Waiting for Helm release %s/%s to leave %s state...\n", namespace, releaseName, status)
		time.Sleep(5 * time.Second)
	}
}

func (d *AIBrixDeployer) helmReleaseStatus(ctx context.Context, releaseName string, namespace string) (string, error) {
	output, err := d.captureDirectCommand(ctx, "helm", "list", "-n", namespace, "-a", "-f", "^"+releaseName+"$", "-o", "json")
	if err != nil {
		return "", fmt.Errorf("failed to read helm release status for %s/%s: %w", namespace, releaseName, err)
	}
	var releases []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &releases); err != nil {
		return "", fmt.Errorf("failed to parse helm release status for %s/%s: %w", namespace, releaseName, err)
	}
	if len(releases) == 0 {
		return "", nil
	}
	return strings.TrimSpace(strings.ToLower(releases[0].Status)), nil
}

func isHelmReleaseLocked(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "pending-install", "pending-upgrade", "pending-rollback", "uninstalling":
		return true
	default:
		return false
	}
}
