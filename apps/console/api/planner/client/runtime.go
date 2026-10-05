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

package client

import (
	"fmt"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

// RuntimeForProvisionResult builds the MDS RuntimeRef (target + options) from a
// ready provision. model/image/serve_args are template-derived serving config
// (planner-owned), intentionally NOT on ProvisionResult.
func RuntimeForProvisionResult(t rmtypes.ResourceProvisionType, result *rmtypes.ProvisionResult, model, image string, vllmArgs []string) (*plannerapi.RuntimeRef, error) {
	switch t {
	case rmtypes.ResourceProvisionTypeRunPod:
		if result == nil {
			return nil, fmt.Errorf("missing provision result")
		}
		if result.RunPod == nil {
			return nil, fmt.Errorf("missing runpod detail")
		}
		if len(result.RunPod.Pods) < 1 {
			return nil, fmt.Errorf("missing runpod pod")
		}
		pod := &result.RunPod.Pods[0]
		if pod.PublicIp == nil {
			return nil, fmt.Errorf("missing runpod public_ip")
		}
		if *pod.PublicIp == "" {
			return nil, fmt.Errorf("empty runpod public_ip")
		}
		if pod.SshPort == nil {
			return nil, fmt.Errorf("missing runpod ssh_port")
		}
		if pod.SshUser == "" {
			return nil, fmt.Errorf("missing runpod ssh_user")
		}
		if pod.HttpBaseUrl == "" {
			return nil, fmt.Errorf("missing runpod http_base_url")
		}
		if model == "" {
			return nil, fmt.Errorf("missing model")
		}
		return &plannerapi.RuntimeRef{
			Target:  "RunPod",
			Options: sshRuntimeOptions(*pod.PublicIp, *pod.SshPort, pod.SshUser, pod.HttpBaseUrl, model, image, vllmArgs),
		}, nil
	case rmtypes.ResourceProvisionTypeLambdaCloud:
		if result == nil {
			return nil, fmt.Errorf("missing provision result")
		}
		if result.LambdaCloud == nil {
			return nil, fmt.Errorf("missing lambdaCloud detail")
		}
		inst := firstLambdaCloudInstance(result.LambdaCloud)
		if inst == nil {
			return nil, fmt.Errorf("missing lambdaCloud instance")
		}
		if inst.PublicIp == nil {
			return nil, fmt.Errorf("missing lambdaCloud public_ip")
		}
		if *inst.PublicIp == "" {
			return nil, fmt.Errorf("empty lambdaCloud public_ip")
		}
		if inst.SshPort == nil {
			return nil, fmt.Errorf("missing lambdaCloud ssh_port")
		}
		if inst.SshUser == "" {
			return nil, fmt.Errorf("missing lambdaCloud ssh_user")
		}
		if inst.HttpBaseUrl == "" {
			return nil, fmt.Errorf("missing lambdaCloud http_base_url")
		}
		if model == "" {
			return nil, fmt.Errorf("missing model")
		}
		return &plannerapi.RuntimeRef{
			Target:  "LambdaCloud",
			Options: sshRuntimeOptions(*inst.PublicIp, *inst.SshPort, inst.SshUser, inst.HttpBaseUrl, model, image, vllmArgs),
		}, nil
	default:
		return RuntimeForProvisionType(t), nil
	}
}

func RuntimeForProvisionType(t rmtypes.ResourceProvisionType) *plannerapi.RuntimeRef {
	target := ""
	switch t {
	case rmtypes.ResourceProvisionTypeKubernetes:
		target = "Kubernetes"
	case rmtypes.ResourceProvisionTypeLambdaCloud:
		target = "LambdaCloud"
	case rmtypes.ResourceProvisionTypeRunPod:
		target = "RunPod"
	case rmtypes.ResourceProvisionTypeAWS:
		target = "External"
	default:
		target = string(t)
	}
	if target == "" {
		return nil
	}
	return &plannerapi.RuntimeRef{Target: target}
}

// sshRuntimeOptions assembles the runtime.options the MDS SSH-launch runtime consumes.
// At this moment, it's for LambdaCloud and RunPod. Kubernetes options like namespace
// are not included yet.
func sshRuntimeOptions(host string, sshPort int, sshUser, httpBaseURL, model, image string, vllmArgs []string) map[string]any {
	opts := map[string]any{
		"host":          host,
		"ssh_port":      sshPort,
		"ssh_user":      sshUser,
		"http_base_url": httpBaseURL,
		"model":         model,
	}
	// TODO: let runtime read image and engine args from model deployment template directly.
	if image != "" {
		opts["image"] = image
	}
	if len(vllmArgs) > 0 {
		opts["vllm_args"] = vllmArgs
	}
	return opts
}

func firstLambdaCloudInstance(detail *rmtypes.LambdaCloudProvisionDetail) *rmtypes.LambdaCloudInstanceDetail {
	if detail == nil {
		return nil
	}
	for gi := range detail.GroupResults {
		group := &detail.GroupResults[gi]
		if len(group.Instances) > 0 {
			return &group.Instances[0]
		}
	}
	return nil
}
