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
	objs, err := p.Load()
	require.NoError(t, err)
	assert.Len(t, objs, 2)

	pod0 := objs[0].(*v1.Pod)
	assert.Equal(t, "Qwen/Qwen2.5-1.5B-Instruct", pod0.Labels[constants.ModelLabelName])
	assert.Equal(t, "8000", pod0.Labels[constants.ModelLabelPort])
	assert.Equal(t, "vllm-0", pod0.Status.PodIP)
	// No role labels for non-disaggregated
	assert.Empty(t, pod0.Labels["role-name"])
	assert.Empty(t, pod0.Labels["roleset-name"])

	pod1 := objs[1].(*v1.Pod)
	assert.Equal(t, "vllm-1", pod1.Status.PodIP)
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
	objs, err := p.Load()
	require.NoError(t, err)
	assert.Len(t, objs, 3)

	// First two are prefill
	prefill0 := objs[0].(*v1.Pod)
	assert.Equal(t, "prefill", prefill0.Labels["role-name"])
	assert.Equal(t, "default", prefill0.Labels["roleset-name"])
	assert.Equal(t, "vllm", prefill0.Labels[constants.ModelLabelEngine])
	assert.Equal(t, "prefill-0", prefill0.Status.PodIP)

	prefill1 := objs[1].(*v1.Pod)
	assert.Equal(t, "prefill", prefill1.Labels["role-name"])
	assert.Equal(t, "prefill-1", prefill1.Status.PodIP)

	// Third is decode
	decode0 := objs[2].(*v1.Pod)
	assert.Equal(t, "decode", decode0.Labels["role-name"])
	assert.Equal(t, "default", decode0.Labels["roleset-name"])
	assert.Equal(t, "decode-0", decode0.Status.PodIP)
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
	objs, err := p.Load()
	require.NoError(t, err)
	assert.Len(t, objs, 4)

	pod0 := objs[0].(*v1.Pod)
	assert.Equal(t, "group-a", pod0.Labels["roleset-name"])
	assert.Equal(t, "prefill", pod0.Labels["role-name"])

	pod2 := objs[2].(*v1.Pod)
	assert.Equal(t, "group-b", pod2.Labels["roleset-name"])
	assert.Equal(t, "prefill", pod2.Labels["role-name"])
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
	_, err := p.Load()
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
	_, err := p.Load()
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
	objs, err := p.Load()
	require.NoError(t, err)
	assert.Len(t, objs, 1)

	pod := objs[0].(*v1.Pod)
	assert.Equal(t, "8000", pod.Labels[constants.ModelLabelPort])
	assert.Equal(t, int32(8000), pod.Spec.Containers[0].Ports[0].ContainerPort)
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
	objs, err := p.Load()
	require.NoError(t, err)

	pod := objs[0].(*v1.Pod)
	assert.Equal(t, v1.PodRunning, pod.Status.Phase)
	require.Len(t, pod.Status.Conditions, 1)
	assert.Equal(t, v1.PodReady, pod.Status.Conditions[0].Type)
	assert.Equal(t, v1.ConditionTrue, pod.Status.Conditions[0].Status)
}

func TestStaticProviderFileNotFound(t *testing.T) {
	p := NewStaticProvider("/nonexistent/path.yaml")
	_, err := p.Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read config file")
}

func TestStaticProviderInvalidYAML(t *testing.T) {
	path := writeTestConfig(t, "{{invalid yaml")
	p := NewStaticProvider(path)
	_, err := p.Load()
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
