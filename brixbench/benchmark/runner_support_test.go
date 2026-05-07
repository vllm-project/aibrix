package benchmark

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var scenarioFlag = flag.String("scenario", "", "Path to the benchmark scenario YAML file")

var pacificLocation = mustLoadPacificLocation()

func sanitizePathComponent(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	replacer := strings.NewReplacer(" ", "-", "/", "-", "\\", "-", ":", "-", "\t", "-", "\n", "-")
	value = replacer.Replace(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !isAllowed {
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		builder.WriteRune(r)
	}
	sanitized := strings.Trim(builder.String(), "-")
	if sanitized == "" {
		return "unnamed"
	}
	return sanitized
}

func benchmarkPodNameForTest(testName string) string {
	sanitized := sanitizePathComponent(testName)
	sanitized = strings.ReplaceAll(sanitized, ".", "-")
	name := "vllm-bench-" + sanitized
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.Trim(name, "-")
	if name == "" {
		return "vllm-bench-client"
	}
	return name
}

func mustLoadPacificLocation() *time.Location {
	location, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(fmt.Sprintf("failed to load Pacific time location: %v", err))
	}
	return location
}

func nowInPacificTime() time.Time {
	return time.Now().In(pacificLocation)
}

func resolveScenarioPath(t *testing.T) string {
	t.Helper()

	scenarioPath := *scenarioFlag
	if scenarioPath == "" {
		scenarioPath = os.Getenv("BENCHMARK_SCENARIO")
	}
	if scenarioPath == "" {
		scenarioPath = defaultScenarioPath
		t.Logf("scenario not set, falling back to default: %s", scenarioPath)
	}
	return scenarioPath
}

func formatScenarioRunID(now time.Time, scenarioName string) string {
	return fmt.Sprintf("%s-PT-%s", now.Format("20060102-150405"), sanitizePathComponent(scenarioName))
}

func caseLogRoot(suiteLogRoot string, testCaseName string) string {
	return filepath.Join(suiteLogRoot, sanitizePathComponent(testCaseName))
}

func stringifyValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%v", value)
}

func resolveBenchmarkPath(benchmarkPath string) (string, error) {
	if filepath.IsAbs(benchmarkPath) {
		return benchmarkPath, nil
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to determine working directory: %w", err)
	}
	return filepath.Join(workingDir, benchmarkPath), nil
}

func resolveProjectRoot() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Dir(workingDir), nil
}

func configureBenchmarkEnvironment(t *testing.T, testCaseName string, providerName string, benchmarkNamespace string, gatewayURL string) {
	t.Helper()

	t.Setenv("BASE_URL", gatewayURL)
	t.Setenv("BENCHMARK_NAMESPACE", benchmarkNamespace)
	t.Setenv("BENCHMARK_POD_NAME", benchmarkPodNameForTest(testCaseName))
	t.Setenv("SANITY_PORT_FORWARD_RESOURCE", "")
	t.Setenv("SANITY_PORT_FORWARD_NAMESPACE", "")
	t.Setenv("SANITY_PORT_FORWARD_LOCAL_PORT", "")
	t.Setenv("SANITY_PORT_FORWARD_REMOTE_PORT", "")

	if providerName == "" {
		t.Setenv("SANITY_PORT_FORWARD_RESOURCE", "service/vllm-service")
		t.Setenv("SANITY_PORT_FORWARD_NAMESPACE", benchmarkNamespace)
		t.Setenv("SANITY_PORT_FORWARD_LOCAL_PORT", "10080")
		t.Setenv("SANITY_PORT_FORWARD_REMOTE_PORT", "8000")
	}
}

func resetBenchmarkNamespace(ctx context.Context, namespace string) error {
	checkCmd := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("kubectl get namespace %q -o name 2>/dev/null || true", namespace))
	checkOutput, err := checkCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to inspect namespace %s: %v, output: %s", namespace, err, string(checkOutput))
	}
	if strings.TrimSpace(string(checkOutput)) == "" {
		return nil
	}

	deleteCmd := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("kubectl delete namespace %q --ignore-not-found", namespace))
	if output, err := deleteCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete namespace %s: %v, output: %s", namespace, err, string(output))
	}

	waitCmd := exec.CommandContext(ctx, "bash", "-c", fmt.Sprintf("kubectl wait --for=delete namespace/%q --timeout=10m", namespace))
	if output, err := waitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed waiting for namespace %s deletion: %v, output: %s", namespace, err, string(output))
	}
	return nil
}
