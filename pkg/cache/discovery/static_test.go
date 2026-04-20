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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
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
