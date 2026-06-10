package benchmark

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/deployers"
	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

func progressLog(t *testing.T, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	t.Helper()
	t.Log(message)
	fmt.Printf("[benchmark] %s\n", message)
}

func progressStep(t *testing.T, format string, args ...any) func() {
	message := fmt.Sprintf(format, args...)
	startedAt := time.Now()
	progressLog(t, "START %s", message)
	return func() {
		progressLog(t, "DONE %s (%s)", message, time.Since(startedAt).Round(time.Second))
	}
}

func teardownTestResources(t *testing.T, ctx context.Context, deployer deployers.Deployer, benchmarkNamespace string, testCaseName string) {
	t.Helper()

	cleanupDone := progressStep(t, "cleanup namespace %s for %s", benchmarkNamespace, testCaseName)
	defer cleanupDone()
	if err := deployer.Teardown(ctx); err != nil {
		t.Errorf("failed to cleanup test resources: %v", err)
	}
}

func captureDeploymentArtifacts(t *testing.T, ctx context.Context, deployer deployers.Deployer) {
	t.Helper()
	if deployer == nil {
		return
	}
	if err := deployer.CaptureArtifacts(ctx); err != nil {
		t.Logf("Warning: failed to capture deployment artifacts: %v", err)
	}
}

func captureCasePodLogs(t *testing.T, ctx context.Context, testCase *resolver.Test, benchmarkNamespace string, caseLogDir string) {
	t.Helper()

	benchmarkPodName := benchmarkPodNameForTest(testCase.Name)
	enginePodNames, err := listPodsInNamespace(ctx, benchmarkNamespace)
	if err != nil {
		t.Logf("Warning: failed to list engine pods in %s for log capture: %v", benchmarkNamespace, err)
		return
	}

	var enginePods []string
	for _, podName := range enginePodNames {
		if podName == benchmarkPodName {
			continue
		}
		enginePods = append(enginePods, podName)
	}
	capturePodLogs(t, ctx, benchmarkNamespace, enginePods, filepath.Join(caseLogDir, "engine-logs"), "engine")

	if testCase.ProviderName() != "aibrix" {
		return
	}

	gatewayPodNames, err := listPodsWithPrefix(ctx, "aibrix-system", "aibrix-gateway-plugins-")
	if err != nil {
		t.Logf("Warning: failed to list gateway pods in aibrix-system for log capture: %v", err)
		return
	}
	capturePodLogs(t, ctx, "aibrix-system", gatewayPodNames, filepath.Join(caseLogDir, "gateway-logs"), "gateway")
}

func capturePodLogs(t *testing.T, ctx context.Context, namespace string, podNames []string, logDir string, logKind string) {
	t.Helper()

	if len(podNames) == 0 {
		return
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Logf("Warning: failed to create %s log directory %s: %v", logKind, logDir, err)
		return
	}

	for _, podName := range podNames {
		logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", sanitizePathComponent(podName)))
		command := fmt.Sprintf("kubectl logs -n %q %q --all-containers=true --timestamps=true", namespace, podName)
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		output, cmdErr := cmd.CombinedOutput()

		logBody := output
		if cmdErr != nil {
			logBody = append(logBody, []byte(fmt.Sprintf("\n[log capture error] %v\n", cmdErr))...)
		}
		if writeErr := os.WriteFile(logPath, logBody, 0644); writeErr != nil {
			t.Logf("Warning: failed to write %s logs for pod %s: %v", logKind, podName, writeErr)
			continue
		}
		progressLog(t, "Saved %s pod logs for %s: %s", logKind, podName, logPath)
	}
}

func listPodsWithPrefix(ctx context.Context, namespace string, prefix string) ([]string, error) {
	podNames, err := listPodsInNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []string
	for _, podName := range podNames {
		if strings.HasPrefix(podName, prefix) {
			filtered = append(filtered, podName)
		}
	}
	return filtered, nil
}

func listPodsInNamespace(ctx context.Context, namespace string) ([]string, error) {
	command := fmt.Sprintf("kubectl get pods -n %q -o jsonpath='{range .items[*]}{.metadata.name}{\"\\n\"}{end}'", namespace)
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %v, output: %s", namespace, err, string(output))
	}

	var podNames []string
	for _, line := range strings.Split(string(output), "\n") {
		podName := strings.TrimSpace(strings.Trim(line, "'"))
		if podName == "" {
			continue
		}
		podNames = append(podNames, podName)
	}
	sort.Strings(podNames)
	return podNames, nil
}
