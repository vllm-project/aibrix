package benchmark

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/brixbench/internal/deployers"
	"github.com/vllm-project/aibrix/brixbench/internal/drivers"
	"github.com/vllm-project/aibrix/brixbench/internal/observability"
	"github.com/vllm-project/aibrix/brixbench/internal/resolver"
)

const (
	defaultScenarioPath       = "testdata/scenarios/aibrix-hello-world.yaml"
	defaultGatewayEndpoint    = "http://aibrix-gateway.bytedance.net"
	defaultBenchmarkNamespace = "brixbench-adhoc"
)

func executeScenarioTestCase(t *testing.T, scenarioName string, scenarioLogRoot string, testCase resolver.Test, exporter observability.MetricExporter) (scenarioCaseResult, error) {
	t.Helper()

	ctx := context.Background()
	var deployer deployers.Deployer
	result := scenarioCaseResult{
		TestCase:      testCase.Name,
		Version:       testCase.Version,
		Commit:        testCase.Commit,
		Status:        "failed",
		BenchmarkKind: "",
	}

	testDone := progressStep(t, "test case %s", testCase.Name)
	defer testDone()

	projectRoot, err := resolveProjectRoot()
	if err != nil {
		result.Error = fmt.Sprintf("Preparation failed: %v", err)
		return result, fmt.Errorf("failed to determine working directory: %w", err)
	}

	result.BenchmarkKind = testCase.BenchmarkKind

	namespaceResetDone := progressStep(t, "reset benchmark namespace %s for %s", defaultBenchmarkNamespace, testCase.Name)
	if resetErr := resetBenchmarkNamespace(ctx, defaultBenchmarkNamespace); resetErr != nil {
		result.Error = fmt.Sprintf("Benchmark namespace reset failed: %v", resetErr)
		return result, fmt.Errorf("Benchmark namespace reset failed: %w", resetErr)
	}
	namespaceResetDone()

	preflightDone := progressStep(t, "check existing StormService resources for %s", testCase.Name)
	if preflightErr := ensureStormServicesCleared(ctx); preflightErr != nil {
		result.Error = fmt.Sprintf("StormService preflight failed: %v", preflightErr)
		return result, fmt.Errorf("StormService preflight failed: %w", preflightErr)
	}
	preflightDone()

	caseLogDir := caseLogRoot(scenarioLogRoot, testCase.Name)
	deployDone := progressStep(t, "deploy control plane and engine for %s", testCase.Name)
	deployer, gatewayURL, err := setupAndRunDeployment(ctx, t, projectRoot, &testCase, defaultBenchmarkNamespace, caseLogDir)
	result.Version = testCase.Version
	result.Commit = testCase.Commit
	result.ResolvedCommit = testCase.ResolvedCommit
	if err != nil {
		captureDeploymentArtifacts(t, ctx, deployer)
		result.Error = fmt.Sprintf("Deployment failed: %v", err)
		return result, fmt.Errorf("Deployment failed: %w", err)
	}
	result.GatewayURL = gatewayURL
	deployDone()
	defer teardownTestResources(t, ctx, deployer, defaultBenchmarkNamespace, testCase.Name)
	defer captureCasePodLogs(t, ctx, &testCase, defaultBenchmarkNamespace, caseLogDir)

	configureBenchmarkEnvironment(t, testCase.Name, testCase.ProviderName(), defaultBenchmarkNamespace, gatewayURL)
	progressLog(t, "Gateway endpoint for %s: %s", testCase.Name, gatewayURL)

	benchmarkDone := progressStep(t, "run benchmark and export metrics for %s", testCase.Name)
	metrics, resultPath, err := runBenchmarkAndExportMetrics(ctx, &testCase, scenarioName, scenarioLogRoot, exporter)
	result.ResultPath = resultPath
	if err != nil {
		result.Metrics = metrics
		captureDeploymentArtifacts(t, ctx, deployer)
		result.Error = fmt.Sprintf("Benchmark execution failed: %v", err)
		return result, fmt.Errorf("Benchmark execution failed: %w", err)
	}
	result.Metrics = metrics
	result.Status = "passed"
	captureDeploymentArtifacts(t, ctx, deployer)
	benchmarkDone()

	progressLog(t, "Successfully ran test for %s", testCase.Name)
	return result, nil
}

func setupAndRunDeployment(ctx context.Context, t *testing.T, projectRoot string, testCase *resolver.Test, benchmarkNamespace string, caseLogDir string) (deployers.Deployer, string, error) {
	// Select Deployer
	var deployer deployers.Deployer
	switch providerName := testCase.ProviderName(); providerName {
	case "aibrix":
		deployer = deployers.NewAIBrixDeployer()
		t.Log("Using AIBrix deployer")
	case "llmd":
		t.Log("Using LLM-d deployer")
		// return nil, "", fmt.Errorf("LLM-d deployer not implemented")
	case "dynamo":
		t.Log("Using Dynamo deployer")
		// return nil, "", fmt.Errorf("Dynamo deployer not implemented")
	case "":
		if testCase.Engine.Type != "vllm" {
			return nil, "", fmt.Errorf("provider: null only supports engine.type=vllm, got %q", testCase.Engine.Type)
		}
		deployer = deployers.NewPlainVLLMDeployer()
		t.Log("Using plain vLLM deployer")
	default:
		return nil, "", fmt.Errorf("unknown provider: %s", providerName)
	}

	// Initialize the deployer with the parsed file paths
	if err := deployer.Initialize(ctx, deployers.Config{
		ControlPlanePaths:      testCase.ControlPlane,
		EnginePath:             testCase.Engine.Manifest,
		Namespace:              benchmarkNamespace,
		LogDir:                 caseLogDir,
		ProjectRoot:            projectRoot,
		FullStack:              testCase.FullStack,
		VKEDev:                 testCase.VKEDev,
		ResolvedCommit:         testCase.ResolvedCommit,
		WorkspacePath:          testCase.WorkspacePath,
		GatewayImageRepository: testCase.GatewayImageRepository,
		GatewayImageTag:        testCase.GatewayImageTag,
		GatewayEnv:             testCase.Gateway.Env,
		GatewayResourceFiles:   testCase.Gateway.Resources,
		TestCase:               testCase,
	}); err != nil {
		return nil, "", fmt.Errorf("failed to initialize deployer: %w", err)
	}

	// Execute Deployment logic
	if err := deployer.DeployControlPlane(ctx); err != nil {
		return deployer, "", fmt.Errorf("failed to deploy control plane: %w", err)
	}
	if err := deployer.DeployEngine(ctx); err != nil {
		return deployer, "", fmt.Errorf("failed to deploy engine: %w", err)
	}
	if err := deployer.WaitForReady(ctx); err != nil {
		return deployer, "", fmt.Errorf("engine not ready: %w", err)
	}
	time.Sleep(20 * time.Second)

	// Get dynamically assigned Gateway IP/URL
	gatewayUrl, err := deployer.GetGatewayEndpoint(ctx)
	if err != nil {
		t.Logf("Warning: failed to get gateway endpoint: %v, using default", err)
		gatewayUrl = defaultGatewayEndpoint
	}
	t.Logf("Using Gateway Endpoint: %s", gatewayUrl)

	return deployer, gatewayUrl, nil
}

func newBenchmarkDriver(benchmarkKind string) (drivers.Driver, error) {
	switch benchmarkKind {
	case "vllm-bench":
		return drivers.NewVLLMBenchDriver(), nil
	default:
		return nil, fmt.Errorf("unsupported benchmark kind: %s", benchmarkKind)
	}
}

func runBenchmarkAndExportMetrics(ctx context.Context, testCase *resolver.Test, scenarioName string, suiteLogRoot string, exporter observability.MetricExporter) (map[string]any, string, error) {
	benchmarkPath, err := resolveBenchmarkPath(testCase.Benchmark)
	if err != nil {
		return nil, "", err
	}

	var driver drivers.Driver
	driver, err = newBenchmarkDriver(testCase.BenchmarkKind)
	if err != nil {
		return nil, "", err
	}

	// Save benchmark artifacts under the suite-level top-level run directory.
	logDir := caseLogRoot(suiteLogRoot, testCase.Name)
	err = driver.Run(ctx, benchmarkPath, logDir)
	if err != nil {
		return nil, "", fmt.Errorf("benchmark execution failed: %w", err)
	}

	// Collect and Export Metrics
	metrics, err := driver.CollectMetrics()
	if err != nil {
		return nil, driver.ResultPath(), fmt.Errorf("failed to collect metrics: %w", err)
	}

	labels := map[string]string{
		"scenario": scenarioName,
		"testcase": testCase.Name,
		"version":  testCase.Version,
	}
	if err := exporter.Export(ctx, metrics, labels); err != nil {
		return metrics, driver.ResultPath(), fmt.Errorf("failed to export metrics: %w", err)
	}

	return metrics, driver.ResultPath(), nil
}

func TestAIBrixBenchmarkSuite(t *testing.T) {
	// 0. Setup Observability Exporter (Example: Prometheus Pushgateway)
	exporter := observability.NewPrometheusPushExporter("http://prometheus-pushgateway.monitoring:9091", "benchmark-suite")

	// 1. Get Scenario Path (from flag first, then env var, then default)
	scenarioPath := resolveScenarioPath(t)

	// 2. Resolve YAML configuration
	scenario, err := resolver.Resolve(scenarioPath)
	if err != nil {
		t.Fatalf("failed to resolve scenario %s: %v", scenarioPath, err)
	}

	progressLog(t, "Running Scenario: %s", scenario.Name)
	runStartedAt := nowInPacificTime()
	runID := formatScenarioRunID(runStartedAt, scenario.Name)
	scenarioLogRoot := filepath.Join("testdata/logs", runID)
	progressLog(t, "Suite log root for %s: %s", scenario.Name, scenarioLogRoot)
	resultsByCase := runScenarioTests(t, scenario, scenarioLogRoot, exporter)
	summary := buildScenarioSummary(scenario.Name, resultsByCase)
	if writeErr := writeScenarioArtifacts(scenarioLogRoot, runID, summary); writeErr != nil {
		t.Fatalf("failed to write scenario artifacts: %v", writeErr)
	}
	progressLog(t, "Wrote scenario summary: %s", scenarioLogRoot)

	figuresDone := progressStep(t, "generate scenario figures for %s", scenario.Name)
	generatedFigures, figureErr := generateScenarioFigures(scenarioLogRoot, summary)
	figuresDone()
	if figureErr != nil {
		t.Fatalf("failed to generate scenario figures: %v", figureErr)
	}
	if generatedFigures {
		progressLog(t, "Generated scenario figures under %s/figures", scenarioLogRoot)
	} else {
		progressLog(t, "Skipped scenario figure generation because the benchmark kind was mixed/unsupported or .venv/bin/python or plot_summary_vllm_bench.py was not available")
	}
}
