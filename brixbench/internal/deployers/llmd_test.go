package deployers

import (
	"context"
	"testing"
)

func TestInstallLLMdRouterDisablesEPPMetricsAuth(t *testing.T) {
	runner := &fakeCommandRunner{}
	deployer := &LLMdDeployer{
		namespace:   LLMdBenchmarkNamespace,
		projectRoot: t.TempDir(),
		runner:      runner,
	}

	if err := deployer.installLLMdRouter(context.Background()); err != nil {
		t.Fatalf("installLLMdRouter returned error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one Helm command, got %d", len(runner.calls))
	}
	if runner.calls[0].name != "env" {
		t.Fatalf("expected Helm command to run through env, got %q", runner.calls[0].name)
	}

	args := runner.calls[0].args
	for index := 0; index < len(args)-1; index++ {
		if args[index] == "--set" && args[index+1] == "router.monitoring.prometheus.auth.enabled=false" {
			return
		}
	}
	t.Fatalf("expected Helm args to disable EPP metrics auth, got %v", args)
}
