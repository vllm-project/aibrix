package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// VLLMBenchDriver implements the Driver interface for vllm-bench.
type VLLMBenchDriver struct {
	resultsPath string
	runDir      string
}

type vllmBenchConfig struct {
	Kind          string `yaml:"kind"`
	Execution     string `yaml:"execution"`
	Image         string `yaml:"image"`
	Namespace     string `yaml:"namespace"`
	PodName       string `yaml:"podName"`
	ModelHostPath string `yaml:"modelHostPath"`
	RootHostPath  string `yaml:"rootHostPath"`
	Artifacts     struct {
		ResultFilename string `yaml:"resultFilename"`
		LogDir         string `yaml:"logDir"`
	} `yaml:"artifacts"`
	VLLMArgs map[string]interface{} `yaml:"vllmArgs"`
}

func NewVLLMBenchDriver() *VLLMBenchDriver {
	return &VLLMBenchDriver{
		resultsPath: "bench_results.json",
	}
}

func (d *VLLMBenchDriver) Run(ctx context.Context, benchmarkPath string, logDir string) error {
	fmt.Printf("Running vLLM benchmark: %s\n", benchmarkPath)

	config, err := loadBenchmarkConfig(benchmarkPath)
	if err != nil {
		return err
	}
	if config.Execution != "cluster" {
		return fmt.Errorf("unsupported vllm-bench execution mode: %s", config.Execution)
	}
	applyRuntimeOverrides(config)

	explicitLogDir := strings.TrimSpace(logDir) != ""
	if !explicitLogDir && config.Artifacts.LogDir != "" {
		logDir = config.Artifacts.LogDir
	}
	if !filepath.IsAbs(logDir) {
		workingDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %v", err)
		}
		logDir = filepath.Join(workingDir, logDir)
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}
	runDir := logDir
	if !explicitLogDir {
		runDir = filepath.Join(logDir, fmt.Sprintf("vllm-bench-%s", time.Now().Format("20060102-150405")))
	}
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return fmt.Errorf("failed to create run directory: %v", err)
	}
	d.runDir = runDir
	fmt.Printf("[vllm-bench] Run directory: %s\n", runDir)

	podManifest, err := buildPodManifest(config)
	if err != nil {
		return err
	}
	podManifestPath := filepath.Join(runDir, "vllm-bench-pod.yaml")
	if err := os.WriteFile(podManifestPath, podManifest, 0644); err != nil {
		return fmt.Errorf("failed to write pod manifest: %v", err)
	}
	fmt.Printf("[vllm-bench] Wrote benchmark pod manifest: %s\n", podManifestPath)

	fmt.Printf("[vllm-bench] Cleaning previous benchmark pod: %s/%s\n", config.Namespace, config.PodName)
	if err := runBash(ctx, fmt.Sprintf("kubectl delete pod %s -n %s --ignore-not-found", shellWord(config.PodName), shellWord(config.Namespace))); err != nil {
		return fmt.Errorf("failed to clean previous benchmark pod: %v", err)
	}
	if err := waitForPodDeletion(ctx, config.Namespace, config.PodName, 2*time.Minute); err != nil {
		return fmt.Errorf("failed waiting for previous benchmark pod deletion: %v", err)
	}
	fmt.Printf("[vllm-bench] Applying benchmark pod manifest in namespace %s\n", config.Namespace)
	// The runner deletes and recreates the shared namespace between cases. If the namespace is still
	// terminating, pod creation can fail with Forbidden. Ensure the namespace is usable before apply.
	if err := ensureNamespaceReadyForBenchmark(ctx, config.Namespace); err != nil {
		return err
	}
	if err := runBash(ctx, fmt.Sprintf("kubectl apply -f %q", podManifestPath)); err != nil {
		// If we raced with namespace deletion or a previous pod object, wait/cleanup and retry once.
		if strings.Contains(err.Error(), "because it is being terminated") || strings.Contains(err.Error(), "already exists") {
			_ = runBash(ctx, fmt.Sprintf("kubectl delete pod %s -n %s --ignore-not-found", shellWord(config.PodName), shellWord(config.Namespace)))
			_ = waitForPodDeletion(ctx, config.Namespace, config.PodName, 2*time.Minute)
			if err2 := ensureNamespaceReadyForBenchmark(ctx, config.Namespace); err2 != nil {
				return fmt.Errorf("failed to apply benchmark pod manifest: %v", err)
			}
			if err2 := runBash(ctx, fmt.Sprintf("kubectl apply -f %q", podManifestPath)); err2 == nil {
				goto applied
			}
		}
		return fmt.Errorf("failed to apply benchmark pod manifest: %v", err)
	}
applied:
	fmt.Printf("[vllm-bench] Waiting for benchmark pod to become Ready: %s/%s\n", config.Namespace, config.PodName)
	if err := runBash(ctx, fmt.Sprintf("kubectl wait --for=condition=Ready --timeout=5m pod/%s -n %s", shellWord(config.PodName), shellWord(config.Namespace))); err != nil {
		return fmt.Errorf("benchmark pod did not become ready: %v", err)
	}
	fmt.Printf("[vllm-bench] Benchmark pod is Ready: %s/%s\n", config.Namespace, config.PodName)

	logPath := filepath.Join(runDir, "vllm-bench-client.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, "bash", "-lc", fmt.Sprintf("kubectl logs -f %s -n %s", shellWord(config.PodName), shellWord(config.Namespace)))
	cmd.Stdout = io.MultiWriter(os.Stdout, logFile)
	cmd.Stderr = io.MultiWriter(os.Stderr, logFile)

	fmt.Printf("[vllm-bench] Output will be logged to: %s\n", logPath)
	fmt.Printf("[vllm-bench] START streaming pod logs: %s/%s\n", config.Namespace, config.PodName)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vllm-bench failed: %v (check %s for details)", err, logPath)
	}
	fmt.Printf("[vllm-bench] DONE streaming pod logs: %s/%s\n", config.Namespace, config.PodName)

	fmt.Printf("[vllm-bench] Waiting for benchmark pod completion: %s/%s\n", config.Namespace, config.PodName)
	podPhase, err := waitForBenchmarkPodCompletion(ctx, config.Namespace, config.PodName, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to wait for benchmark pod completion: %v", err)
	}
	fmt.Printf("[vllm-bench] Benchmark pod completed with phase: %s\n", podPhase)
	if podPhase != "Succeeded" {
		return fmt.Errorf("benchmark pod finished with phase %s", podPhase)
	}

	resultsFilename := config.Artifacts.ResultFilename
	d.resultsPath = filepath.Join(runDir, resultsFilename)
	fmt.Printf("[vllm-bench] Persisting benchmark results to: %s\n", d.resultsPath)
	if err := persistBenchResultsFromLog(logPath, d.resultsPath); err != nil {
		return fmt.Errorf("failed to persist benchmark results: %v (check %s for details)", err, logPath)
	}

	fmt.Printf("[vllm-bench] Completed successfully. Log saved to: %s\n", logPath)
	return nil
}

func (d *VLLMBenchDriver) CollectMetrics() (map[string]interface{}, error) {
	fmt.Printf("Collecting metrics from %s\n", d.resultsPath)

	// Read from a JSON file generated by vllm-bench
	data, err := os.ReadFile(d.resultsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read results file: %v", err)
	}

	var metrics map[string]interface{}
	if err := json.Unmarshal(data, &metrics); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %v", err)
	}

	// Log key metrics (TTFT, TPOT)
	if ttft, ok := metrics["ttft"]; ok {
		fmt.Printf("TTFT (Time to First Token): %v\n", ttft)
	}
	if tpot, ok := metrics["tpot"]; ok {
		fmt.Printf("TPOT (Time Per Output Token): %v\n", tpot)
	}

	return metrics, nil
}

func (d *VLLMBenchDriver) ResultPath() string {
	return d.resultsPath
}

func waitForBenchmarkPodCompletion(ctx context.Context, namespace string, podName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		phase, err := captureBash(ctx, fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.status.phase}'", shellWord(podName), shellWord(namespace)))
		if err != nil {
			return "", err
		}
		normalized := strings.TrimSpace(phase)
		switch normalized {
		case "Succeeded", "Failed":
			return normalized, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("benchmark pod %s in namespace %s did not reach a terminal phase within %s", podName, namespace, timeout)
}

func waitForPodDeletion(ctx context.Context, namespace string, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := captureBash(ctx, fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true", shellWord(podName), shellWord(namespace)))
		if err == nil && strings.TrimSpace(strings.Trim(output, "'")) == "" {
			exists, existsErr := captureBash(ctx, fmt.Sprintf("kubectl get pod %s -n %s --ignore-not-found -o name", shellWord(podName), shellWord(namespace)))
			if existsErr == nil && strings.TrimSpace(exists) == "" {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("benchmark pod %s in namespace %s was not fully deleted within %s", podName, namespace, timeout)
}

func loadBenchmarkConfig(path string) (*vllmBenchConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read benchmark config %s: %v", path, err)
	}

	var config vllmBenchConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse benchmark config %s: %v", path, err)
	}
	config.applyDefaults()
	if err := config.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

func applyRuntimeOverrides(config *vllmBenchConfig) {
	if overrideBaseURL := os.Getenv("BASE_URL"); overrideBaseURL != "" {
		if config.VLLMArgs == nil {
			config.VLLMArgs = make(map[string]interface{})
		}
		config.VLLMArgs["base-url"] = overrideBaseURL
	}
	if overrideNamespace := strings.TrimSpace(os.Getenv("BENCHMARK_NAMESPACE")); overrideNamespace != "" {
		config.Namespace = overrideNamespace
	}
	if overridePodName := strings.TrimSpace(os.Getenv("BENCHMARK_POD_NAME")); overridePodName != "" {
		config.PodName = overridePodName
	}
}

func (c *vllmBenchConfig) applyDefaults() {
	if c.Kind == "" {
		c.Kind = "vllm-bench"
	}
	if c.Execution == "" {
		c.Execution = "cluster"
	}
	if c.Namespace == "" {
		c.Namespace = "brixbench-adhoc"
	}
	if c.PodName == "" {
		c.PodName = "vllm-bench-client"
	}
	if c.ModelHostPath == "" {
		c.ModelHostPath = "/data01/models"
	}
	if c.RootHostPath == "" {
		c.RootHostPath = "/root"
	}
	if c.Artifacts.ResultFilename == "" {
		c.Artifacts.ResultFilename = "bench_results.json"
	}
	if c.VLLMArgs == nil {
		c.VLLMArgs = make(map[string]interface{})
	}

	// Apply default vLLM args if not specified
	defaults := map[string]interface{}{
		"endpoint":                "/v1/chat/completions",
		"backend":                 "openai-chat",
		"endpoint-type":           "openai-chat",
		"num-prompts":             1000,
		"request-rate":            8,
		"concurrency":             16,
		"random-prefix-len":       512,
		"random-input-len":        8192,
		"random-output-len":       256,
		"ready-check-timeout-sec": 30,
		"metric-percentiles":      "50,90,95,99",
		"percentile-metrics":      "ttft,tpot,itl,e2el",
	}

	for k, v := range defaults {
		if _, ok := c.VLLMArgs[k]; !ok {
			c.VLLMArgs[k] = v
		}
	}
}

func (c *vllmBenchConfig) validate() error {
	if c.Kind != "vllm-bench" {
		return fmt.Errorf("unsupported benchmark kind: %s", c.Kind)
	}

	requiredArgs := []string{"base-url", "model", "tokenizer"}
	for _, arg := range requiredArgs {
		if val, ok := c.VLLMArgs[arg]; !ok || val == "" {
			return fmt.Errorf("benchmark config requires vllmArgs.%s", arg)
		}
	}
	return nil
}

func buildPodManifest(config *vllmBenchConfig) ([]byte, error) {
	argsLines := []string{
		"cd /tmp",
		"python3 -m vllm.entrypoints.cli.main bench serve \\",
	}

	for k, v := range config.VLLMArgs {
		flagKey := normalizeVLLMBenchFlagKey(k)
		if v == nil {
			argsLines = append(argsLines, fmt.Sprintf("  --%s \\", flagKey))
			continue
		}

		if b, ok := v.(bool); ok {
			if b {
				argsLines = append(argsLines, fmt.Sprintf("  --%s \\", flagKey))
			}
			// if false, we just skip it or pass --k false depending on CLI.
			// Usually boolean flags in CLI don't take values, presence means true.
			continue
		}

		strVal := fmt.Sprintf("%v", v)
		if strVal != "" {
			argsLines = append(argsLines, fmt.Sprintf("  --%s %s \\", flagKey, shellWord(strVal)))
		} else {
			argsLines = append(argsLines, fmt.Sprintf("  --%s \\", flagKey))
		}
	}

	// Append fixed arguments that the framework depends on
	argsLines = append(argsLines,
		"  --dataset-name random \\",
		"  --seed 1 \\",
		"  --save-result \\",
		fmt.Sprintf("  --result-filename %s \\", shellWord(config.Artifacts.ResultFilename)),
		"  2>&1 | tee /tmp/bench.log",
		"echo __AIBRIX_BENCH_RESULTS_BEGIN__",
		fmt.Sprintf("cat /tmp/%s || true", shellWord(config.Artifacts.ResultFilename)),
		"echo __AIBRIX_BENCH_RESULTS_END__",
	)

	command := strings.Join(argsLines, "\n")

	pod := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      config.PodName,
			"namespace": config.Namespace,
		},
		"spec": map[string]interface{}{
			"restartPolicy": "Never",
			"hostNetwork":   true,
			"dnsPolicy":     "ClusterFirstWithHostNet",
			"containers": []map[string]interface{}{
				{
					"name":  config.PodName,
					"image": config.Image,
					"securityContext": map[string]interface{}{
						"capabilities": map[string]interface{}{
							"add": []string{"IPC_LOCK"},
						},
					},
					"command": []string{"bash", "-lc"},
					"args":    []string{command},
					"volumeMounts": []map[string]string{
						{
							"name":      "model-vol",
							"mountPath": config.ModelHostPath,
						},
						{
							"name":      "shared-mem",
							"mountPath": "/dev/shm",
						},
						{
							"name":      "root",
							"mountPath": "/root",
						},
					},
				},
			},
			"volumes": []map[string]interface{}{
				{
					"name": "model-vol",
					"hostPath": map[string]string{
						"path": config.ModelHostPath,
						"type": "Directory",
					},
				},
				{
					"name": "shared-mem",
					"emptyDir": map[string]string{
						"medium": "Memory",
					},
				},
				{
					"name": "root",
					"hostPath": map[string]string{
						"path": config.RootHostPath,
						"type": "Directory",
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal benchmark pod manifest: %v", err)
	}
	return data, nil
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func shellWord(value string) string {
	return strconv.Quote(value)
}

func runBash(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v, output: %s", err, string(output))
	}
	return nil
}

func captureBash(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v, output: %s", err, string(output))
	}
	return string(output), nil
}

func normalizeVLLMBenchFlagKey(key string) string {
	switch key {
	case "concurrency":
		return "max-concurrency"
	case "metric-percentiles":
		return "metric_percentiles"
	default:
		return key
	}
}

func ensureLocalPortAvailableForPortForward(ctx context.Context, port string) error {
	addr := "127.0.0.1:" + port
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		_ = listener.Close()
		return nil
	}

	// Prefer killing listeners by PID from lsof; fall back to best-effort pkill.
	_ = killLocalPortListeners(ctx, port)
	_ = runBash(ctx, fmt.Sprintf("pkill -f %s || true", shellWord(fmt.Sprintf("kubectl port-forward.* %s:", port))))
	time.Sleep(2 * time.Second)

	listener, err = net.Listen("tcp", addr)
	if err == nil {
		_ = listener.Close()
		return nil
	}
	return fmt.Errorf("local port %s is already in use and could not be freed", port)
}

func waitForLocalPortListening(ctx context.Context, port string, cmd *exec.Cmd, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("port-forward process exited before local port %s became ready", port)
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
	return fmt.Errorf("local port %s did not become ready within %s", port, timeout)
}

func killLocalPortListeners(ctx context.Context, port string) error {
	// Try to identify LISTENing processes on the port and SIGKILL them.
	// Note: lsof might not exist in all environments; callers should treat errors as non-fatal.
	out, err := captureBash(ctx, fmt.Sprintf("lsof -nP -iTCP:%s -sTCP:LISTEN -t 2>/dev/null || true", shellWord(port)))
	if err != nil {
		return err
	}
	lines := strings.Split(out, "\n")
	killed := false
	for _, line := range lines {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		_ = runBash(ctx, fmt.Sprintf("kill -9 %s 2>/dev/null || true", shellWord(pid)))
		killed = true
	}
	if killed {
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func ensureNamespaceReadyForBenchmark(ctx context.Context, namespace string) error {
	// Namespace termination is most reliably detected via deletionTimestamp.
	// status.phase can be stale or unset while admission already blocks new objects.
	deletionTS, err := captureBash(ctx, fmt.Sprintf("kubectl get namespace %s -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true", shellWord(namespace)))
	if err != nil {
		return fmt.Errorf("failed to inspect namespace %s: %v", namespace, err)
	}
	if strings.TrimSpace(strings.Trim(deletionTS, "'")) != "" {
		if err := runBash(ctx, fmt.Sprintf("kubectl wait --for=delete namespace %s --timeout=10m", shellWord(namespace))); err != nil {
			return fmt.Errorf("failed waiting for namespace %s deletion: %v", namespace, err)
		}
	}
	if err := runBash(ctx, fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -", shellWord(namespace))); err != nil {
		return fmt.Errorf("failed to ensure namespace %s: %v", namespace, err)
	}
	return nil
}

func persistBenchResultsFromLog(logPath string, resultsPath string) error {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Errorf("failed to read log file: %v", err)
	}

	beginMarker := []byte("__AIBRIX_BENCH_RESULTS_BEGIN__")
	endMarker := []byte("__AIBRIX_BENCH_RESULTS_END__")

	beginIdx := bytes.Index(data, beginMarker)
	if beginIdx < 0 {
		return fmt.Errorf("bench results begin marker not found in logs")
	}
	payloadStart := beginIdx + len(beginMarker)
	for payloadStart < len(data) && (data[payloadStart] == '\n' || data[payloadStart] == '\r') {
		payloadStart++
	}
	endRelIdx := bytes.Index(data[payloadStart:], endMarker)
	if endRelIdx < 0 {
		return fmt.Errorf("bench results end marker not found in logs")
	}
	endIdx := payloadStart + endRelIdx
	if endIdx < 0 {
		return fmt.Errorf("bench results end marker not found in logs")
	}

	payload := bytes.TrimSpace(data[payloadStart:endIdx])
	if len(payload) == 0 {
		return fmt.Errorf("bench results payload is empty")
	}
	var raw json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("bench results payload is not valid json: %v", err)
	}

	if err := os.WriteFile(resultsPath, payload, 0644); err != nil {
		return fmt.Errorf("failed to write results file: %v", err)
	}
	return nil
}
