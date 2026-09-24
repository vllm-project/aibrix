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
	if _, found := args["--gpu-memory-utilization"]; found {
		return 0, fmt.Errorf("--gpu-memory-utilization is incompatible with kvcached; use the pool KV policy instead")
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

// podHasGPUs reports whether a pod has cards the account has to cover. The
// device plugin's nvidia.com/gpu request is one way to tell. The runtime
// reporting accelerators is the other, and it covers a pod given its GPUs some
// other way, such as a dynamic resource claim. A pod with neither is taken for
// one without a GPU, like the CPU pools the tests run on.
func podHasGPUs(pod corev1.Pod, reportedAccelerators int) bool {
	return podGPUCount(pod) > 0 || reportedAccelerators > 0
}

// reportedAccelerators is how many cards a runtime reading describes, and zero
// when there is no reading.
//
// A card reported with no memory at all is not counted. The runtime reports a
// real card only once NVML has read its memory, so such a card is the one the
// runtime's mock mode reports for the single-GPU pool policy on CPU pools.
// There is nothing on it to account for.
func reportedAccelerators(snapshot *RuntimeSnapshot) int {
	if snapshot == nil {
		return 0
	}
	cards := 0
	for _, accelerator := range snapshot.Accelerators {
		if accelerator.HBMTotalBytes > 0 {
			cards++
		}
	}
	return cards
}

// podSupportsVLLMParallelism accepts legacy/mock Pods without GPU resources so
// existing CPU-only controller tests remain valid. Real warm pools declare a
// GPU limit and must exactly match the requested TP * PP topology.
func podSupportsVLLMParallelism(pod corev1.Pod, parallelism int64) bool {
	gpuCount := podGPUCount(pod)
	return gpuCount == 0 || gpuCount == parallelism
}
