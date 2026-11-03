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
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/utils"
	corev1 "k8s.io/api/core/v1"
)

const nvidiaGPUResourceName = utils.NVIDIAGPUResourceName

func isVLLMModel(pm *modelv1alpha1.ModelClaim) bool {
	return pm.Spec.Engine == "" || pm.Spec.Engine == "vllm"
}

func modelParallelism(pm *modelv1alpha1.ModelClaim) (int64, error) {
	if !isVLLMModel(pm) {
		return 1, nil
	}
	return utils.ModelClaimVLLMParallelism(pm.Spec.EngineConfig)
}

// instanceGPUCount is how many cards one instance of a claim runs on, as far
// as a pod is held to that: TP * PP for vLLM. Another engine, or a topology
// that cannot be read, holds a pod to nothing, and the count is then zero.
func instanceGPUCount(pm *modelv1alpha1.ModelClaim) int64 {
	if !isVLLMModel(pm) {
		return 0
	}
	parallelism, err := modelParallelism(pm)
	if err != nil {
		return 0
	}
	return parallelism
}

// podGPUCount reports the GPU devices assigned to the warm runtime Pod.
func podGPUCount(pod corev1.Pod) int64 {
	return utils.ModelClaimPodGPUCount(&pod.Spec)
}

// podHasGPUs reports whether a pod has cards the account has to cover. The
// device plugin's nvidia.com/gpu request is one way to tell. The runtime
// reporting accelerators is the other, and it covers a pod given its GPUs some
// other way, such as a dynamic resource claim. The account counts one card
// as reported where a reading missed it, and an engine on the pod holds a KV
// segment or an instance records a limit. A pod with none of these is taken
// for one without a GPU, like the CPU pools the tests run on.
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
//
// A reading with no card at all still describes one when an engine on it holds
// a KV segment. The runtime reports no card when NVML fails, and a segment is
// only ever built on a card. The account adds what the claims say: a pod on
// which an instance records a KV limit has a card as well.
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
	if cards > 0 {
		return cards
	}
	for _, model := range snapshot.Models {
		if model.KVCapacityBytes > 0 {
			return 1
		}
	}
	return 0
}

// podSupportsVLLMParallelism accepts legacy/mock Pods without GPU resources so
// existing CPU-only controller tests remain valid. Real warm pools declare a
// GPU limit and must exactly match the requested TP * PP topology. A Pod given
// its GPUs without such a request is held to the topology once its runtime
// reports them, in admissibleCandidates.
func podSupportsVLLMParallelism(pod corev1.Pod, parallelism int64) bool {
	gpuCount := podGPUCount(pod)
	return gpuCount == 0 || gpuCount == parallelism
}
