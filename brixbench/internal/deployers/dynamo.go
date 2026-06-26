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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const dynamoPlatformHelmReleaseName = "dynamo-platform"
const dynamoClearFinalizersPatch = `{"metadata":{"finalizers":[]}}`

// DynamoDeployer deploys Dynamo platform releases and user-provided
// DynamoGraphDeployment manifests.
type DynamoDeployer struct {
	namespace      string
	logDir         string
	projectRoot    string
	version        string
	engineManifest string
	releaseSource  DynamoReleaseSource
	runner         commandRunner
	release        *DynamoRelease
}

var _ Deployer = (*DynamoDeployer)(nil)

// NewDynamoDeployer creates a release-based Dynamo deployer.
func NewDynamoDeployer() *DynamoDeployer {
	return &DynamoDeployer{
		releaseSource: NewGitDynamoReleaseSource(),
		runner:        execCommandRunner{},
	}
}

func (d *DynamoDeployer) Initialize(ctx context.Context, config Config) error {
	if d.releaseSource == nil {
		d.releaseSource = NewGitDynamoReleaseSource()
	}
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}

	d.namespace = strings.TrimSpace(config.Namespace)
	d.logDir = strings.TrimSpace(config.LogDir)
	d.projectRoot = strings.TrimSpace(config.ProjectRoot)
	d.engineManifest = strings.TrimSpace(config.EnginePath)
	if config.TestCase != nil {
		d.version = strings.TrimSpace(config.TestCase.Version)
		if d.engineManifest == "" {
			d.engineManifest = strings.TrimSpace(config.TestCase.Engine.Manifest)
		}
	}

	if d.version == "" {
		return fmt.Errorf("Dynamo deployer requires version")
	}
	if d.namespace == "" {
		return fmt.Errorf("Dynamo deployer requires namespace")
	}
	if d.projectRoot == "" {
		return fmt.Errorf("Dynamo deployer requires project root")
	}
	if d.engineManifest == "" {
		return fmt.Errorf("Dynamo deployer requires engine manifest")
	}
	return nil
}

func (d *DynamoDeployer) DeployControlPlane(ctx context.Context) error {
	release, err := d.releaseSource.PrepareRelease(ctx, d.projectRoot, d.version)
	if err != nil {
		return fmt.Errorf("failed to prepare Dynamo release %s: %w", d.version, err)
	}
	d.release = release

	return d.runDynamoCommand(
		ctx,
		"helm-install-dynamo-platform",
		"helm",
		"upgrade", "--install", dynamoPlatformHelmReleaseName, release.ChartPath,
		"-n", d.namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "10m",
	)
}

func (d *DynamoDeployer) DeployGateway(ctx context.Context) error {
	return nil
}

func (d *DynamoDeployer) DeployEngine(ctx context.Context) error {
	if strings.TrimSpace(d.engineManifest) == "" {
		return fmt.Errorf("Dynamo deployer requires engine manifest")
	}
	return d.runDynamoCommand(ctx, "apply-dynamo-graph-deployment", "kubectl", "apply", "-f", d.engineManifest)
}

func (d *DynamoDeployer) WaitForReady(ctx context.Context) error {
	return fmt.Errorf("Dynamo readiness wait is not implemented")
}

func (d *DynamoDeployer) GetGatewayEndpoint(ctx context.Context) (string, error) {
	return "", fmt.Errorf("Dynamo gateway endpoint discovery is not implemented")
}

func (d *DynamoDeployer) CaptureArtifacts(ctx context.Context) error {
	return nil
}

func (d *DynamoDeployer) Teardown(ctx context.Context) error {
	if strings.TrimSpace(d.engineManifest) != "" {
		d.runDynamoCleanupCommand(ctx, "patch-dynamo-graph-deployment-finalizers", "kubectl", "patch", "-f", d.engineManifest, "--type=merge", "-p", dynamoClearFinalizersPatch)
	}
	if strings.TrimSpace(d.namespace) != "" {
		d.runDynamoCleanupCommand(ctx, "patch-dynamo-component-deployment-finalizers", "kubectl", "patch", "dynamocomponentdeployments.nvidia.com", "--all", "-n", d.namespace, "--type=merge", "-p", dynamoClearFinalizersPatch)
	}
	if strings.TrimSpace(d.engineManifest) != "" {
		d.runDynamoCleanupCommand(ctx, "delete-dynamo-graph-deployment", "kubectl", "delete", "-f", d.engineManifest, "--ignore-not-found", "--wait=false")
	}
	if strings.TrimSpace(d.namespace) != "" {
		d.runDynamoCleanupCommand(ctx, "uninstall-dynamo-platform", "helm", "uninstall", dynamoPlatformHelmReleaseName, "-n", d.namespace, "--ignore-not-found", "--wait", "--timeout", "5m")
		d.runDynamoCleanupCommand(ctx, "delete-dynamo-namespace", "kubectl", "delete", "namespace", d.namespace, "--ignore-not-found")
		d.runDynamoCleanupCommand(ctx, "wait-delete-dynamo-namespace", "kubectl", "wait", "--for=delete", "namespace/"+d.namespace, "--timeout=10m")
	}
	return nil
}

func (d *DynamoDeployer) runDynamoCommand(ctx context.Context, stage string, name string, args ...string) error {
	startedAt := time.Now()
	output, err := d.runner.Run(ctx, name, args...)
	finishedAt := time.Now()

	if logErr := d.writeDynamoCommandLog(stage, name, args, startedAt, finishedAt, commandExitCode(err), output); logErr != nil && err == nil {
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

func (d *DynamoDeployer) runDynamoCleanupCommand(ctx context.Context, stage string, name string, args ...string) {
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}
	if strings.TrimSpace(name) == "" {
		return
	}
	if err := d.runDynamoCommand(ctx, stage, name, args...); err != nil {
		fmt.Printf("Warning: Dynamo cleanup step %s failed: %v\n", stage, err)
	}
}

func (d *DynamoDeployer) writeDynamoCommandLog(stage string, name string, args []string, startedAt time.Time, finishedAt time.Time, exitCode int, output string) error {
	if strings.TrimSpace(d.logDir) == "" {
		return nil
	}
	logDir := filepath.Join(d.logDir, "commands")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("failed to create Dynamo command log directory: %w", err)
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

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func sanitizeDynamoCommandStage(stage string) string {
	stage = strings.ToLower(strings.TrimSpace(stage))
	if stage == "" {
		return "command"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range stage {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
