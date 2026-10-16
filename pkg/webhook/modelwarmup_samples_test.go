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

package webhook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

func TestModelWarmupSamples(t *testing.T) {
	samplePaths := []string{
		"samples/modelwarmup/modelwarmup.yaml",
		"samples/modelwarmup/model-download.yaml",
		"samples/modelwarmup/node-precheck.yaml",
		"samples/modelwarmup/gpu-precheck.yaml",
		"samples/modelwarmup/vllm-qwen-warmup.yaml",
		"samples/modelwarmup/sglang-qwen-warmup.yaml",
	}
	samples := make(map[string]*modelapi.ModelWarmup, len(samplePaths))
	for _, samplePath := range samplePaths {
		raw, err := os.ReadFile(filepath.Join("..", "..", samplePath))
		require.NoError(t, err, "read %s", samplePath)

		warmup := &modelapi.ModelWarmup{}
		require.NoError(t, yaml.UnmarshalStrict(raw, warmup), "decode %s", samplePath)
		require.NoError(t, validateModelWarmup(warmup), "validate %s", samplePath)
		samples[samplePath] = warmup
	}

	modelDownload := samples["samples/modelwarmup/model-download.yaml"]
	require.Empty(t, modelDownload.Spec.ImagePreload.Images)
	require.NotNil(t, modelDownload.Spec.Custom)
	require.NotEmpty(t, modelDownload.Spec.Custom.Containers)

	vllm := samples["samples/modelwarmup/vllm-qwen-warmup.yaml"]
	require.Equal(t, "vllm/vllm-openai:v0.10.2", vllm.Spec.ImagePreload.Images[0].Image)
	require.Contains(t, vllm.Spec.Custom.Containers[0].Args, "Qwen/Qwen3-0.6B")
	require.NotEmpty(t, vllm.Spec.Custom.InitContainers)

	sglang := samples["samples/modelwarmup/sglang-qwen-warmup.yaml"]
	require.Equal(t, "lmsysorg/sglang:v0.5.5.post3", sglang.Spec.ImagePreload.Images[0].Image)
	require.Contains(t, sglang.Spec.Custom.Containers[0].Args, "Qwen/Qwen3-0.6B")
	require.NotEmpty(t, sglang.Spec.Custom.InitContainers)

	nodePrecheck := samples["samples/modelwarmup/node-precheck.yaml"]
	require.NotNil(t, nodePrecheck.Spec.Custom)
	require.NotEmpty(t, nodePrecheck.Spec.Custom.InitContainers)
	require.Equal(t, vllm.Spec.Custom.InitContainers[0].Command, sglang.Spec.Custom.InitContainers[0].Command)
	require.Equal(t, vllm.Spec.Custom.InitContainers[0].Args, sglang.Spec.Custom.InitContainers[0].Args)
	for _, engine := range []*modelapi.ModelWarmup{vllm, sglang} {
		require.EqualValues(t, 2, *engine.Spec.Policies.Parallelism)
		precheck := engine.Spec.Custom.InitContainers[0]
		script := strings.Join(precheck.Args, "\n")
		require.Contains(t, script, "10485760")
		require.Contains(t, script, "cpu_count")
		require.Contains(t, script, "MemAvailable")
		require.Contains(t, script, "nslookup huggingface.co")
	}

	gpuPrecheck := samples["samples/modelwarmup/gpu-precheck.yaml"]
	require.NotNil(t, gpuPrecheck.Spec.Custom)
	require.NotEmpty(t, gpuPrecheck.Spec.Custom.InitContainers)
	limits := gpuPrecheck.Spec.Custom.InitContainers[0].Resources.Limits
	gpuLimit := limits["nvidia.com/gpu"]
	require.Equal(t, "1", gpuLimit.String())
}
