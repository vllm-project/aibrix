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

package modelclaim

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const nvidiaGPUResourceName corev1.ResourceName = "nvidia.com/gpu"

// vllmParallelism returns the fixed GPU group size required by a vLLM engine.
// ModelClaim deliberately keeps these engine options in engineConfig.args; the
// pool Pod's GPU limit is the resource contract and must match TP * PP.
func vllmParallelism(config *modelv1alpha1.ModelClaimEngineConfig) (int64, error) {
	args := map[string]string{}
	if config != nil && config.Args != nil {
		args = config.Args
	}
	tensor, err := positiveEngineArg(args, "--tensor-parallel-size")
	if err != nil {
		return 0, err
	}
	pipeline, err := positiveEngineArg(args, "--pipeline-parallel-size")
	if err != nil {
		return 0, err
	}
	data, err := positiveEngineArg(args, "--data-parallel-size")
	if err != nil {
		return 0, err
	}
	if data != 1 {
		return 0, fmt.Errorf("--data-parallel-size=%d is unsupported by fixed topology pools", data)
	}
	if tensor > math.MaxInt64/pipeline {
		return 0, fmt.Errorf("tensor and pipeline parallelism product overflows int64")
	}
	return tensor * pipeline, nil
}

func positiveEngineArg(args map[string]string, name string) (int64, error) {
	raw, found := args[name]
	if !found || raw == "" {
		return 1, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func isVLLMModel(pm *modelv1alpha1.ModelClaim) bool {
	return pm.Spec.Engine == "" || pm.Spec.Engine == "vllm"
}

func modelParallelism(pm *modelv1alpha1.ModelClaim) (int64, error) {
	if !isVLLMModel(pm) {
		return 1, nil
	}
	return vllmParallelism(pm.Spec.EngineConfig)
}

// podGPUCount reports the GPU devices assigned to the warm runtime Pod. The
// initial contract is one topology-homogeneous runtime container per Pod.
func podGPUCount(pod corev1.Pod) int64 {
	var count int64
	for i := range pod.Spec.Containers {
		resources := pod.Spec.Containers[i].Resources
		if quantity, found := resources.Limits[nvidiaGPUResourceName]; found {
			count += quantity.Value()
			continue
		}
		if quantity, found := resources.Requests[nvidiaGPUResourceName]; found {
			count += quantity.Value()
		}
	}
	return count
}

// podSupportsVLLMParallelism accepts legacy/mock Pods without GPU resources so
// existing CPU-only controller tests remain valid. Real warm pools declare a
// GPU limit and must exactly match the requested TP * PP topology.
func podSupportsVLLMParallelism(pod corev1.Pod, parallelism int64) bool {
	gpuCount := podGPUCount(pod)
	return gpuCount == 0 || gpuCount == parallelism
}
