package deployers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const dynamoArtifactLogTailLines = "2000"

type dynamoArtifactCapture struct {
	file  string
	stage string
	name  string
	args  []string
}

func (d *DynamoDeployer) CaptureArtifacts(ctx context.Context) error {
	if strings.TrimSpace(d.logDir) == "" {
		return nil
	}
	if d.runner == nil {
		d.runner = execCommandRunner{}
	}

	namespace := d.dynamoArtifactNamespace()
	if namespace == "" {
		return nil
	}

	artifactDir := filepath.Join(d.logDir, "dynamo-artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return fmt.Errorf("failed to create Dynamo artifact directory %s: %w", artifactDir, err)
	}

	var writeErrs []error
	for _, capture := range d.dynamoArtifactCaptures(namespace) {
		output, err := d.captureDynamoCommand(ctx, capture.stage, capture.name, capture.args...)
		if err != nil {
			output = fmt.Sprintf("capture failed: %v\n", err)
		}
		if err := writeDynamoArtifactFile(artifactDir, capture.file, output); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}

	helmStatus, err := d.captureDynamoHelmCommand(ctx, "capture-dynamo-helm-status", "status", dynamoPlatformHelmReleaseName, "-n", namespace)
	if err != nil {
		helmStatus = fmt.Sprintf("capture failed: %v\n", err)
	}
	if err := writeDynamoArtifactFile(artifactDir, "helm-status.txt", helmStatus); err != nil {
		writeErrs = append(writeErrs, err)
	}

	if strings.TrimSpace(d.engineManifest) != "" {
		content, err := os.ReadFile(d.engineManifest)
		if err != nil {
			content = []byte(fmt.Sprintf("capture failed: failed to read Dynamo engine manifest %s: %v\n", d.engineManifest, err))
		}
		if err := writeDynamoArtifactFile(artifactDir, "engine-manifest.yaml", string(content)); err != nil {
			writeErrs = append(writeErrs, err)
		}
	}
	return errors.Join(writeErrs...)
}

func (d *DynamoDeployer) dynamoArtifactNamespace() string {
	namespace := strings.TrimSpace(d.effectiveNS)
	if namespace != "" {
		return namespace
	}
	namespace = strings.TrimSpace(d.namespace)
	if namespace != "" {
		return namespace
	}
	return DynamoBenchmarkNamespace
}

func (d *DynamoDeployer) dynamoArtifactCaptures(namespace string) []dynamoArtifactCapture {
	return []dynamoArtifactCapture{
		{
			file:  "pods.yaml",
			stage: "capture-dynamo-pods",
			name:  "kubectl",
			args:  []string{"get", "pods", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "services.yaml",
			stage: "capture-dynamo-services",
			name:  "kubectl",
			args:  []string{"get", "services", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "events.yaml",
			stage: "capture-dynamo-events",
			name:  "kubectl",
			args:  []string{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp", "-o", "yaml"},
		},
		{
			file:  "dynamographdeployments.yaml",
			stage: "capture-dynamo-graph-deployments",
			name:  "kubectl",
			args:  []string{"get", "dynamographdeployments.nvidia.com", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "dynamocomponentdeployments.yaml",
			stage: "capture-dynamo-component-deployments",
			name:  "kubectl",
			args:  []string{"get", "dynamocomponentdeployments.nvidia.com", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "deployments.yaml",
			stage: "capture-dynamo-deployments",
			name:  "kubectl",
			args:  []string{"get", "deployments", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "statefulsets.yaml",
			stage: "capture-dynamo-statefulsets",
			name:  "kubectl",
			args:  []string{"get", "statefulsets", "-n", namespace, "-o", "yaml"},
		},
		{
			file:  "frontend-logs.txt",
			stage: "capture-dynamo-frontend-logs",
			name:  "kubectl",
			args:  []string{"logs", "-n", namespace, "-l", dynamoFrontendComponentLabel + "=" + dynamoFrontendComponentName, "--all-containers=true", "--prefix=true", "--tail=" + dynamoArtifactLogTailLines},
		},
		{
			file:  "component-logs.txt",
			stage: "capture-dynamo-component-logs",
			name:  "kubectl",
			args:  []string{"logs", "-n", namespace, "-l", dynamoFrontendComponentLabel + "," + dynamoFrontendComponentLabel + "!=" + dynamoFrontendComponentName, "--all-containers=true", "--prefix=true", "--tail=" + dynamoArtifactLogTailLines},
		},
	}
}

func (d *DynamoDeployer) captureDynamoHelmCommand(ctx context.Context, stage string, args ...string) (string, error) {
	name, commandArgs, err := d.dynamoHelmCommand(args...)
	if err != nil {
		return "", err
	}
	return d.captureDynamoCommand(ctx, stage, name, commandArgs...)
}

func writeDynamoArtifactFile(dir string, name string, content string) error {
	path := filepath.Join(dir, name)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write Dynamo artifact %s: %w", path, err)
	}
	return nil
}
