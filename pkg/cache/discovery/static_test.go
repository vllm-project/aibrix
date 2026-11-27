/*
Copyright 2024 The Aibrix Team.

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

package discovery

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// watchCollect calls Watch and collects all delivered pods.
func watchCollect(t *testing.T, p *StaticProvider) ([]*v1.Pod, error) {
	t.Helper()
	var pods []*v1.Pod
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := p.Watch(func(ev WatchEvent) {
		if pod, ok := ev.Object.(*v1.Pod); ok {
			assert.Equal(t, EventAdd, ev.Type)
			pods = append(pods, pod)
		}
	}, stopCh)
	return pods, err
}

func TestStaticProviderType(t *testing.T) {
	p := NewStaticProvider("/tmp/test.yaml")
	assert.Equal(t, "static", p.Type())
}

func TestStaticProviderNonDisaggregated(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-1.5B-Instruct"
    endpoints:
      - "vllm-0:8000"
      - "vllm-1:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	assert.Equal(t, "Qwen/Qwen2.5-1.5B-Instruct", pods[0].Labels[constants.ModelLabelName])
	assert.Equal(t, "8000", pods[0].Labels[constants.ModelLabelPort])
	assert.Equal(t, "vllm-0", pods[0].Status.PodIP)
	// No role labels for non-disaggregated
	assert.Empty(t, pods[0].Labels["role-name"])
	assert.Empty(t, pods[0].Labels["roleset-name"])

	assert.Equal(t, "vllm-1", pods[1].Status.PodIP)
}

func TestStaticProviderDisaggregated(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-72B"
    engine: vllm
    rolesets:
      - name: default
        prefill:
          - "prefill-0:8000"
          - "prefill-1:8000"
        decode:
          - "decode-0:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 3)

	// First two are prefill
	assert.Equal(t, "prefill", pods[0].Labels["role-name"])
	assert.Equal(t, "default", pods[0].Labels["roleset-name"])
	assert.Equal(t, "vllm", pods[0].Labels[constants.ModelLabelEngine])
	assert.Equal(t, "prefill-0", pods[0].Status.PodIP)

	assert.Equal(t, "prefill", pods[1].Labels["role-name"])
	assert.Equal(t, "prefill-1", pods[1].Status.PodIP)

	// Third is decode
	assert.Equal(t, "decode", pods[2].Labels["role-name"])
	assert.Equal(t, "default", pods[2].Labels["roleset-name"])
	assert.Equal(t, "decode-0", pods[2].Status.PodIP)
}

func TestStaticProviderMultipleRoleSets(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-72B"
    rolesets:
      - name: group-a
        prefill:
          - "prefill-0:8000"
        decode:
          - "decode-0:8000"
      - name: group-b
        prefill:
          - "prefill-1:8000"
        decode:
          - "decode-1:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 4)

	assert.Equal(t, "group-a", pods[0].Labels["roleset-name"])
	assert.Equal(t, "prefill", pods[0].Labels["role-name"])

	assert.Equal(t, "group-b", pods[2].Labels["roleset-name"])
	assert.Equal(t, "prefill", pods[2].Labels["role-name"])
}

func TestStaticProviderMutuallyExclusive(t *testing.T) {
	config := `
models:
  - name: "test-model"
    endpoints:
      - "vllm:8000"
    rolesets:
      - name: default
        prefill:
          - "prefill:8000"
        decode:
          - "decode:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := p.Watch(func(_ WatchEvent) {}, stopCh)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestStaticProviderRoleSetNameRequired(t *testing.T) {
	config := `
models:
  - name: "test-model"
    rolesets:
      - name: ""
        prefill:
          - "prefill:8000"
        decode:
          - "decode:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := p.Watch(func(_ WatchEvent) {}, stopCh)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "roleset name is required")
}

func TestStaticProviderDefaultPort(t *testing.T) {
	config := `
models:
  - name: "test-model"
    endpoints:
      - "vllm-host"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 1)

	assert.Equal(t, "8000", pods[0].Labels[constants.ModelLabelPort])
	assert.Equal(t, int32(8000), pods[0].Spec.Containers[0].Ports[0].ContainerPort)
}

// Static endpoints are scraped for metrics on their serving port. Without
// the metric-port label the cache falls back to its default port (8000), so
// an engine on any other port would never be scraped.
func TestStaticProviderMetricPort(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-72B"
    engine: vllm
    prefill_workers:
      - "127.0.0.1:8100"
    decode_workers:
      - "127.0.0.1:8200"
      - "127.0.0.1:8201"
  - name: "test-model"
    endpoints:
      - "vllm-host"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	require.Len(t, pods, 4)

	for i, want := range []string{"8100", "8200", "8201", "8000"} {
		assert.Equal(t, want, pods[i].Labels[constants.ModelLabelPort], "pod %s", pods[i].Name)
		assert.Equal(t, want, pods[i].Labels[constants.ModelLabelMetricPort], "pod %s", pods[i].Name)
	}
}

func TestStaticProviderPodReadyStatus(t *testing.T) {
	config := `
models:
  - name: "test-model"
    endpoints:
      - "vllm:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)

	assert.Equal(t, v1.PodRunning, pods[0].Status.Phase)
	require.Len(t, pods[0].Status.Conditions, 1)
	assert.Equal(t, v1.PodReady, pods[0].Status.Conditions[0].Type)
	assert.Equal(t, v1.ConditionTrue, pods[0].Status.Conditions[0].Status)
}

func TestStaticProviderFileNotFound(t *testing.T) {
	p := NewStaticProvider("/nonexistent/path.yaml")
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := p.Watch(func(_ WatchEvent) {}, stopCh)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read config file")
}

func TestStaticProviderInvalidYAML(t *testing.T) {
	path := writeTestConfig(t, "{{invalid yaml")
	p := NewStaticProvider(path)
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := p.Watch(func(_ WatchEvent) {}, stopCh)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse config file")
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "endpoints.yaml")
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)
	return path
}

func TestStaticProviderWorkersAlias(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-1.5B-Instruct"
    workers:
      - "vllm-0:8000"
      - "vllm-1:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 2)

	assert.Equal(t, "Qwen/Qwen2.5-1.5B-Instruct", pods[0].Labels[constants.ModelLabelName])
	assert.Equal(t, "8000", pods[0].Labels[constants.ModelLabelPort])
	assert.Equal(t, "vllm-0", pods[0].Status.PodIP)
	// workers is a non-disaggregated spelling: no role labels are attached.
	assert.Empty(t, pods[0].Labels["role-name"])
	assert.Empty(t, pods[0].Labels["roleset-name"])

	assert.Equal(t, "vllm-1", pods[1].Status.PodIP)
}

func TestStaticProviderPrefillDecodeWorkers(t *testing.T) {
	config := `
models:
  - name: "Qwen/Qwen2.5-72B"
    engine: vllm
    prefill_workers:
      - "prefill-0:8000"
      - "prefill-1:8000"
    decode_workers:
      - "decode-0:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	assert.Len(t, pods, 3)

	// prefill_workers/decode_workers map to a single roleset named "default".
	assert.Equal(t, "prefill", pods[0].Labels["role-name"])
	assert.Equal(t, "default", pods[0].Labels["roleset-name"])
	assert.Equal(t, "vllm", pods[0].Labels[constants.ModelLabelEngine])
	assert.Equal(t, "prefill-0", pods[0].Status.PodIP)

	assert.Equal(t, "prefill", pods[1].Labels["role-name"])
	assert.Equal(t, "prefill-1", pods[1].Status.PodIP)

	assert.Equal(t, "decode", pods[2].Labels["role-name"])
	assert.Equal(t, "default", pods[2].Labels["roleset-name"])
	assert.Equal(t, "decode-0", pods[2].Status.PodIP)
}

func TestStaticProviderShorthandRequiresBothSides(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{
			name: "prefill workers only",
			config: `
models:
  - name: "test-model"
    prefill_workers:
      - "prefill-0:8000"
`,
		},
		{
			name: "decode workers only",
			config: `
models:
  - name: "test-model"
    decode_workers:
      - "decode-0:8000"
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.config)
			p := NewStaticProvider(path)
			stopCh := make(chan struct{})
			defer close(stopCh)
			err := p.Watch(func(_ WatchEvent) {}, stopCh)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "prefill_workers and decode_workers must both")
		})
	}
}

func TestStaticProviderAliasConflicts(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "endpoints with workers",
			config: `
models:
  - name: "test-model"
    endpoints:
      - "vllm-0:8000"
    workers:
      - "vllm-1:8000"
`,
			wantErr: "aliases",
		},
		{
			name: "rolesets with prefill_workers",
			config: `
models:
  - name: "test-model"
    rolesets:
      - name: default
        prefill:
          - "prefill-0:8000"
    prefill_workers:
      - "prefill-1:8000"
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "workers with prefill_workers",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    prefill_workers:
      - "prefill-0:8000"
`,
			wantErr: "mutually exclusive",
		},
		{
			name: "workers with rolesets",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    rolesets:
      - name: default
        prefill:
          - "prefill-0:8000"
`,
			wantErr: "mutually exclusive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.config)
			p := NewStaticProvider(path)
			stopCh := make(chan struct{})
			defer close(stopCh)
			err := p.Watch(func(_ WatchEvent) {}, stopCh)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "test-model")
		})
	}
}

func TestStaticProviderRequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "model name is empty",
			config: `
models:
  - name: ""
    workers:
      - "vllm-0:8000"
`,
			wantErr: "name is required",
		},
		{
			name: "model has no backends",
			config: `
models:
  - name: "test-model"
    engine: vllm
`,
			wantErr: "at least one of",
		},
		{
			name: "roleset has no addresses",
			config: `
models:
  - name: "test-model"
    rolesets:
      - name: default
`,
			wantErr: "at least one of",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.config)
			p := NewStaticProvider(path)
			stopCh := make(chan struct{})
			defer close(stopCh)
			err := p.Watch(func(_ WatchEvent) {}, stopCh)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestStaticProviderModelConfig(t *testing.T) {
	profiles := `{"defaultProfile": "default", "profiles": {"default": {"routingStrategy": "least-request"}}}`
	tests := []struct {
		name        string
		config      string
		wantConfigs []*ModelConfig
		wantPods    int
	}{
		{
			name: "config is delivered for its model",
			config: `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
      - "a-1:8000"
    config: '` + profiles + `'
  - name: "model-b"
    prefill_workers:
      - "b-prefill-0:8000"
    decode_workers:
      - "b-decode-0:8000"
`,
			wantConfigs: []*ModelConfig{{ModelName: "model-a", Config: profiles}},
			wantPods:    4,
		},
		{
			name: "no config field delivers only pods",
			config: `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
`,
			wantPods: 1,
		},
		{
			name: "blank config is treated as absent",
			config: `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
    config: "  "
`,
			wantPods: 1,
		},
		{
			name: "config on one of two entries is the model's config",
			config: `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
  - name: "model-a"
    workers:
      - "a-1:8000"
    config: '` + profiles + `'
`,
			wantConfigs: []*ModelConfig{{ModelName: "model-a", Config: profiles}},
			wantPods:    2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewStaticProvider(writeTestConfig(t, tt.config))
			var configs []*ModelConfig
			var pods []*v1.Pod
			stopCh := make(chan struct{})
			defer close(stopCh)
			err := p.Watch(func(ev WatchEvent) {
				assert.Equal(t, EventAdd, ev.Type)
				switch o := ev.Object.(type) {
				case *ModelConfig:
					// Configs come first, so the cache has a model's config
					// before any of its pods.
					assert.Empty(t, pods, "config delivered after a pod")
					configs = append(configs, o)
				case *v1.Pod:
					assert.Empty(t, o.Annotations, "the config is not copied onto synthetic pods")
					pods = append(pods, o)
				default:
					t.Errorf("unexpected object %T", ev.Object)
				}
			}, stopCh)
			require.NoError(t, err)
			assert.Equal(t, tt.wantConfigs, configs)
			assert.Len(t, pods, tt.wantPods)
		})
	}
}

// A model config is checked when the file loads, so a bad profile stops the
// gateway at startup instead of being skipped at request time.
func TestStaticProviderInvalidModelConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "invalid JSON",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    config: '{"defaultProfile": "default", "profiles": '
`,
			wantErr: "parse model config",
		},
		{
			name: "no profiles",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    config: '{"defaultProfile": "default"}'
`,
			wantErr: "has no profiles",
		},
		{
			name: "config on two entries of one model",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    config: '{"profiles": {"default": {"routingStrategy": "random"}}}'
  - name: "test-model"
    workers:
      - "vllm-1:8000"
    config: '{"profiles": {"default": {"routingStrategy": "least-request"}}}'
`,
			wantErr: "more than one entry",
		},
		{
			name: "defaultProfile that is not one of the profiles",
			config: `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    config: '{"defaultProfile": "fast", "profiles": {"default": {"routingStrategy": "random"}}}'
`,
			wantErr: `defaultProfile "fast" is not one of its profiles`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestConfig(t, tt.config)
			p := NewStaticProvider(path)
			stopCh := make(chan struct{})
			defer close(stopCh)
			err := p.Watch(func(_ WatchEvent) {}, stopCh)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "test-model")
		})
	}
}

// config can also be written as a YAML mapping. It is delivered as JSON, like
// the string form.
func TestStaticProviderModelConfigAsMapping(t *testing.T) {
	path := writeTestConfig(t, `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
    config:
      defaultProfile: default
      profiles:
        default:
          routingStrategy: least-request
`)
	var configs []*ModelConfig
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := NewStaticProvider(path).Watch(func(ev WatchEvent) {
		if cfg, ok := ev.Object.(*ModelConfig); ok {
			configs = append(configs, cfg)
		}
	}, stopCh)
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "model-a", configs[0].ModelName)
	assert.JSONEq(t, `{"defaultProfile": "default", "profiles": {"default": {"routingStrategy": "least-request"}}}`,
		configs[0].Config)
}

// An unknown key is logged rather than rejected, so an existing file keeps
// loading while a typo such as confg: for config: still shows up.
func TestStaticProviderWarnsOnUnknownKeys(t *testing.T) {
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&logs)
	t.Cleanup(func() {
		klog.SetOutput(io.Discard)
		klog.LogToStderr(true)
	})

	path := writeTestConfig(t, `
models:
  - name: "test-model"
    workers:
      - "vllm-0:8000"
    confg: '{"profiles": {"default": {"routingStrategy": "random"}}}'
`)
	var objects []any
	stopCh := make(chan struct{})
	defer close(stopCh)
	err := NewStaticProvider(path).Watch(func(ev WatchEvent) {
		objects = append(objects, ev.Object)
	}, stopCh)
	require.NoError(t, err)
	klog.Flush()

	require.Len(t, objects, 1)
	assert.IsType(t, &v1.Pod{}, objects[0])
	assert.Contains(t, logs.String(), `unknown field "confg"`)
	assert.Contains(t, logs.String(), path)
}

func TestStaticProviderMixedSpellings(t *testing.T) {
	config := `
models:
  - name: "model-a"
    workers:
      - "a-0:8000"
  - name: "model-b"
    prefill_workers:
      - "b-prefill-0:8000"
    decode_workers:
      - "b-decode-0:8000"
`
	path := writeTestConfig(t, config)
	p := NewStaticProvider(path)
	pods, err := watchCollect(t, p)
	require.NoError(t, err)
	require.Len(t, pods, 3)
	assert.Equal(t, "model-a", pods[0].Labels[constants.ModelLabelName])
	assert.Empty(t, pods[0].Labels["roleset-name"])
	assert.Equal(t, "model-b", pods[1].Labels[constants.ModelLabelName])
	assert.Equal(t, "default", pods[1].Labels["roleset-name"])
	assert.Equal(t, "model-b", pods[2].Labels[constants.ModelLabelName])
	assert.Equal(t, "default", pods[2].Labels["roleset-name"])
}
