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
	"context"
	"errors"
	"strings"
	"testing"
)

type recordedCommand struct {
	stdin string
	args  []string
}

func TestEnsureDisabledIsNoop(t *testing.T) {
	called := false
	err := Ensure(context.Background(), Config{
		Namespace: "brixbench-adhoc",
		Enabled:   false,
		Runner: func(ctx context.Context, stdin string, args ...string) (string, error) {
			called = true
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected disabled Ensure to succeed, got %v", err)
	}
	if called {
		t.Fatalf("expected disabled Ensure not to call runner")
	}
}

func TestEnsureMissingCRDNonStrictContinues(t *testing.T) {
	err := Ensure(context.Background(), Config{
		Namespace: "brixbench-adhoc",
		Provider:  "",
		Engine:    "vllm",
		Enabled:   true,
		Strict:    false,
		Runner: func(ctx context.Context, stdin string, args ...string) (string, error) {
			return "", errors.New("not found")
		},
	})
	if err != nil {
		t.Fatalf("expected non-strict missing CRD to continue, got %v", err)
	}
}

func TestEnsureMissingCRDStrictFails(t *testing.T) {
	err := Ensure(context.Background(), Config{
		Namespace: "brixbench-adhoc",
		Provider:  "",
		Engine:    "vllm",
		Enabled:   true,
		Strict:    true,
		Runner: func(ctx context.Context, stdin string, args ...string) (string, error) {
			return "", errors.New("not found")
		},
	})
	if err == nil {
		t.Fatalf("expected strict missing CRD to fail")
	}
}

func TestPlainVLLMPodMonitorManifest(t *testing.T) {
	manifests := podMonitorManifests(Config{
		Namespace: "brixbench-adhoc",
		Provider:  "",
		Engine:    "vllm",
	})
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}

	manifest := manifests[0]
	for _, want := range []string{
		"name: brixbench-vllm-metrics",
		`volcengine.vmp: "true"`,
		`brixbench.aibrix.ai/managed: "true"`,
		"port: http",
		"replacement: brixbench-vllm",
		"app: vllm-basic",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("expected manifest to contain %q:\n%s", want, manifest)
		}
	}
}

func TestAIBrixVLLMPodMonitorManifest(t *testing.T) {
	manifests := podMonitorManifests(Config{
		Namespace: "brixbench-adhoc",
		Provider:  "aibrix",
		Engine:    "vllm",
	})
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}

	manifest := manifests[0]
	for _, want := range []string{
		"name: brixbench-aibrix-vllm-metrics",
		"model.aibrix.ai/engine: vllm",
		"replacement: brixbench-vllm",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("expected manifest to contain %q:\n%s", want, manifest)
		}
	}
}

func TestDynamoPodMonitorManifests(t *testing.T) {
	manifests := podMonitorManifests(Config{
		Namespace: "brixbench-adhoc",
		Provider:  "dynamo",
		Engine:    "vllm",
	})
	if len(manifests) != 3 {
		t.Fatalf("expected 3 manifests, got %d", len(manifests))
	}
	joined := strings.Join(manifests, "\n---\n")
	for _, want := range []string{
		"name: brixbench-dynamo-vllm-worker-metrics",
		"replacement: dynamo-vllm-worker",
		"port: system",
		"name: brixbench-dynamo-frontend-metrics",
		"replacement: dynamo-vllm-frontend",
		"port: http",
		"name: brixbench-dynamo-kvbm-metrics",
		"replacement: dynamo-kvbm",
		"port: kvbm-metrics",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected manifests to contain %q:\n%s", want, joined)
		}
	}
}

func TestEnsureAppliesPlainVLLMManifest(t *testing.T) {
	var commands []recordedCommand
	err := Ensure(context.Background(), Config{
		Namespace: "brixbench-adhoc",
		Provider:  "",
		Engine:    "vllm",
		Enabled:   true,
		Runner: func(ctx context.Context, stdin string, args ...string) (string, error) {
			commands = append(commands, recordedCommand{stdin: stdin, args: append([]string(nil), args...)})
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected Ensure to succeed, got %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected CRD check and apply, got %d commands", len(commands))
	}
	if strings.Join(commands[0].args, " ") != "get crd podmonitors.monitoring.coreos.com" {
		t.Fatalf("unexpected CRD command: %v", commands[0].args)
	}
	if strings.Join(commands[1].args, " ") != "apply -f -" {
		t.Fatalf("unexpected apply command: %v", commands[1].args)
	}
	if !strings.Contains(commands[1].stdin, "brixbench-vllm-metrics") {
		t.Fatalf("expected apply stdin to contain PodMonitor manifest, got:\n%s", commands[1].stdin)
	}
}

func TestEnsureLabelsExistingDynamoChartPodMonitor(t *testing.T) {
	var commands []recordedCommand
	err := Ensure(context.Background(), Config{
		Namespace: "brixbench-adhoc",
		Provider:  "dynamo",
		Engine:    "vllm",
		Enabled:   true,
		Runner: func(ctx context.Context, stdin string, args ...string) (string, error) {
			commands = append(commands, recordedCommand{stdin: stdin, args: append([]string(nil), args...)})
			if strings.Join(args, " ") == `get podmonitor -n brixbench-adhoc -o jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}` {
				return "dynamo-frontend\ndynamo-worker\n", nil
			}
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected Ensure to succeed, got %v", err)
	}

	var labelCommands int
	for _, command := range commands {
		joined := strings.Join(command.args, " ")
		if strings.HasPrefix(joined, "label podmonitor dynamo-") {
			labelCommands++
			if !strings.Contains(joined, "volcengine.vmp=true --overwrite") {
				t.Fatalf("unexpected label command: %s", joined)
			}
		}
	}
	if labelCommands != 2 {
		t.Fatalf("expected 2 Dynamo chart label commands, got %d: %#v", labelCommands, commands)
	}
}
