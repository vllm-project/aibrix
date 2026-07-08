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

package monitoring

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	defaultKubectl       = "kubectl"
	managedLabelKey      = "brixbench.aibrix.ai/managed"
	managedLabelSelector = managedLabelKey + "=true"
	podMonitorCRD        = "podmonitors.monitoring.coreos.com"
)

// CommandRunner executes kubectl commands. Tests can inject a fake runner.
type CommandRunner func(ctx context.Context, stdin string, args ...string) (string, error)

// Config controls benchmark workload PodMonitor setup.
type Config struct {
	Namespace string
	Provider  string
	Engine    string
	Enabled   bool
	Strict    bool
	Kubectl   string
	Runner    CommandRunner
}

// Ensure creates or updates PodMonitors needed by the benchmark dashboard.
func Ensure(ctx context.Context, config Config) error {
	if !config.Enabled {
		fmt.Printf("[benchmark] PodMonitoring disabled\n")
		return nil
	}
	if strings.TrimSpace(config.Namespace) == "" {
		return config.handleError("namespace is required for PodMonitoring", nil)
	}

	if _, err := config.run(ctx, "", "get", "crd", podMonitorCRD); err != nil {
		return config.handleError("PodMonitor CRD is not available; skipping scrape setup", err)
	}

	if strings.EqualFold(strings.TrimSpace(config.Provider), "dynamo") {
		if err := config.labelDynamoChartPodMonitors(ctx); err != nil {
			return err
		}
	}

	manifests := podMonitorManifests(config)
	if len(manifests) == 0 {
		fmt.Printf("[benchmark] No PodMonitoring profile for provider=%q engine=%q\n", config.Provider, config.Engine)
		return nil
	}

	for _, manifest := range manifests {
		if _, err := config.run(ctx, manifest, "apply", "-f", "-"); err != nil {
			return config.handleError("failed to apply benchmark PodMonitor", err)
		}
	}
	fmt.Printf("[benchmark] PodMonitoring configured in namespace %s\n", config.Namespace)
	return nil
}

// Cleanup deletes PodMonitors created by brixbench.
func Cleanup(ctx context.Context, namespace string) error {
	config := Config{Namespace: namespace, Enabled: true}
	_, err := config.run(ctx, "", "delete", "podmonitor", "-n", namespace, "-l", managedLabelSelector, "--ignore-not-found")
	if err != nil {
		return fmt.Errorf("failed to cleanup brixbench PodMonitors in namespace %s: %w", namespace, err)
	}
	return nil
}

func (config Config) run(ctx context.Context, stdin string, args ...string) (string, error) {
	if config.Runner != nil {
		return config.Runner(ctx, stdin, args...)
	}
	kubectl := strings.TrimSpace(config.Kubectl)
	if kubectl == "" {
		kubectl = defaultKubectl
	}
	cmd := exec.CommandContext(ctx, kubectl, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("%s %s failed: %w, output: %s", kubectl, strings.Join(args, " "), err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

func (config Config) handleError(message string, err error) error {
	if err == nil {
		err = fmt.Errorf(message)
	}
	if config.Strict {
		return fmt.Errorf("%s: %w", message, err)
	}
	fmt.Printf("[benchmark] PodMonitoring warning: %s: %v\n", message, err)
	return nil
}

func (config Config) labelDynamoChartPodMonitors(ctx context.Context) error {
	output, err := config.run(ctx, "", "get", "podmonitor", "-n", config.Namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return config.handleError("failed to list Dynamo chart PodMonitors", err)
	}

	existing := map[string]bool{}
	for _, name := range strings.Fields(output) {
		existing[name] = true
	}
	for _, name := range []string{"dynamo-frontend", "dynamo-worker", "dynamo-planner", "dynamo-router"} {
		if !existing[name] {
			continue
		}
		if _, err := config.run(ctx, "", "label", "podmonitor", name, "-n", config.Namespace, "volcengine.vmp=true", "--overwrite"); err != nil {
			return config.handleError(fmt.Sprintf("failed to label Dynamo chart PodMonitor %s", name), err)
		}
	}
	return nil
}

func podMonitorManifests(config Config) []string {
	namespace := strings.TrimSpace(config.Namespace)
	provider := strings.ToLower(strings.TrimSpace(config.Provider))
	engine := strings.ToLower(strings.TrimSpace(config.Engine))

	switch {
	case provider == "" && engine == "vllm":
		return []string{plainVLLMPodMonitor(namespace)}
	case provider == "aibrix" && engine == "vllm":
		return []string{aibrixVLLMPodMonitor(namespace)}
	case provider == "dynamo":
		return []string{
			dynamoWorkerPodMonitor(namespace),
			dynamoFrontendPodMonitor(namespace),
			dynamoKVBMPodMonitor(namespace),
		}
	default:
		return nil
	}
}

func commonLabels() string {
	return `    volcengine.vmp: "true"
    app.kubernetes.io/part-of: brixbench
    brixbench.aibrix.ai/managed: "true"`
}

func podMonitorHeader(name, namespace string) string {
	return fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: %s
  namespace: %s
  labels:
%s
spec:
  namespaceSelector:
    matchNames:
    - %s
`, name, namespace, commonLabels(), namespace)
}

func plainVLLMPodMonitor(namespace string) string {
	return podMonitorHeader("brixbench-vllm-metrics", namespace) + `  podMetricsEndpoints:
  - interval: 15s
    path: /metrics
    port: http
    honorLabels: true
    relabelings:
    - action: replace
      replacement: brixbench-vllm
      targetLabel: job
  selector:
    matchLabels:
      app: vllm-basic
`
}

func aibrixVLLMPodMonitor(namespace string) string {
	return podMonitorHeader("brixbench-aibrix-vllm-metrics", namespace) + `  podMetricsEndpoints:
  - interval: 15s
    path: /metrics
    port: http
    honorLabels: true
    relabelings:
    - action: replace
      replacement: brixbench-vllm
      targetLabel: job
  selector:
    matchLabels:
      model.aibrix.ai/engine: vllm
`
}

func dynamoWorkerPodMonitor(namespace string) string {
	return podMonitorHeader("brixbench-dynamo-vllm-worker-metrics", namespace) + `  podMetricsEndpoints:
  - interval: 15s
    path: /metrics
    port: system
    honorLabels: true
    relabelings:
    - action: replace
      replacement: dynamo-vllm-worker
      targetLabel: job
  selector:
    matchExpressions:
    - key: nvidia.com/dynamo-component
      operator: In
      values:
      - VllmDecodeWorker
      - VllmPrefillWorker
`
}

func dynamoFrontendPodMonitor(namespace string) string {
	return podMonitorHeader("brixbench-dynamo-frontend-metrics", namespace) + `  podMetricsEndpoints:
  - interval: 15s
    path: /metrics
    port: http
    honorLabels: true
    relabelings:
    - action: replace
      replacement: dynamo-vllm-frontend
      targetLabel: job
  selector:
    matchLabels:
      nvidia.com/dynamo-component: Frontend
`
}

func dynamoKVBMPodMonitor(namespace string) string {
	return podMonitorHeader("brixbench-dynamo-kvbm-metrics", namespace) + `  podMetricsEndpoints:
  - interval: 15s
    path: /metrics
    port: kvbm-metrics
    honorLabels: true
    relabelings:
    - action: replace
      replacement: dynamo-kvbm
      targetLabel: job
  selector:
    matchLabels:
      nvidia.com/dynamo-component: VllmDecodeWorker
`
}
