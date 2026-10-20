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

package utils

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

// The ModelClaim controller and the console both read a claim's spec with the
// helpers below, so that the console refuses up front exactly what the
// controller would refuse after the claim is stored.

// NVIDIAGPUResourceName is the device plugin resource a warm runtime Pod
// requests its GPUs with.
const NVIDIAGPUResourceName v1.ResourceName = "nvidia.com/gpu"

// ModelClaimVLLMParallelism returns the fixed GPU group size required by a
// vLLM engine. ModelClaim deliberately keeps these engine options in
// engineConfig.args; the pool Pod's GPU limit is the resource contract and
// must match TP * PP.
func ModelClaimVLLMParallelism(config *modelv1alpha1.ModelClaimEngineConfig) (int64, error) {
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

// ModelClaimPodGPUCount reports the GPU devices a warm runtime Pod spec
// assigns. The initial contract is one topology-homogeneous runtime container
// per Pod.
func ModelClaimPodGPUCount(spec *v1.PodSpec) int64 {
	if spec == nil {
		return 0
	}
	var count int64
	for i := range spec.Containers {
		resources := spec.Containers[i].Resources
		if quantity, found := resources.Limits[NVIDIAGPUResourceName]; found {
			count += quantity.Value()
			continue
		}
		if quantity, found := resources.Requests[NVIDIAGPUResourceName]; found {
			count += quantity.Value()
		}
	}
	return count
}

// modelClaimMaximumDeclaredBytes is the most a claim may declare for one
// figure. No card holds a pebibyte, and figures this small can be added up for
// every instance on a card without running past an int64.
var modelClaimMaximumDeclaredBytes = resource.MustParse("1Pi")

// modelClaimSmallestRoundedBytes is the smallest figure that is rounded up to
// a whole number of bytes. A smaller one with part of a byte in it is refused.
var modelClaimSmallestRoundedBytes = resource.MustParse("1Mi")

// ModelClaimDeclaredBytes is one figure of a ModelClaim's spec.perGPU in
// bytes, or what is wrong with it.
//
// A quantity carries no schema bounds, so a figure that cannot be used is
// caught here. One that is not positive is refused rather than read as a model
// that costs nothing, which is what a zero would otherwise say. A small one
// with part of a byte in it is most likely a slip, such as 30m for 30M, and
// would otherwise be read as a single byte. A larger one comes from a decimal
// fraction of a binary unit, such as 5.6Gi, and is rounded up.
func ModelClaimDeclaredBytes(name string, declared resource.Quantity) (int64, error) {
	wrong := ""
	switch {
	case declared.Sign() <= 0:
		wrong = "not positive"
	case declared.Cmp(modelClaimMaximumDeclaredBytes) > 0:
		wrong = "more than " + modelClaimMaximumDeclaredBytes.String()
	case declared.Cmp(modelClaimSmallestRoundedBytes) < 0 &&
		declared.Cmp(*resource.NewQuantity(declared.Value(), declared.Format)) != 0:
		wrong = "not a whole number of bytes"
	}
	if wrong != "" {
		return 0, fmt.Errorf("spec.perGPU.%s is %s, which is %s", name, declared.String(), wrong)
	}
	return declared.Value(), nil
}
