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
	"strings"
	"testing"

	plannerapi "github.com/vllm-project/aibrix/apps/console/api/planner/api"
	rmtypes "github.com/vllm-project/aibrix/apps/console/api/resource_manager/types"
)

func TestRuntimeForProvisionResultRunPodSuccess(t *testing.T) {
	sshPort := 22150
	publicIP := "203.0.113.10"
	rt, err := RuntimeForProvisionResult(rmtypes.ResourceProvisionTypeRunPod, &rmtypes.ProvisionResult{
		RunPod: &rmtypes.RunPodProvisionDetail{
			Pods: []rmtypes.RunPodPodDetail{
				{
					PublicIp:    &publicIP,
					SshPort:     &sshPort,
					SshUser:     "root",
					HttpBaseUrl: "https://pod-8000.proxy.runpod.net",
				},
			},
		},
	}, "Qwen/Qwen2.5-0.5B-Instruct", "vllm/vllm-openai:v0.8.5", nil)
	if err != nil {
		t.Fatalf("RuntimeForProvisionResult: %v", err)
	}
	if rt.Target != "RunPod" {
		t.Fatalf("Target = %q, want RunPod", rt.Target)
	}
	assertOption(t, rt, "host", "203.0.113.10")
	assertOption(t, rt, "ssh_port", 22150)
	assertOption(t, rt, "ssh_user", "root")
	assertOption(t, rt, "http_base_url", "https://pod-8000.proxy.runpod.net")
	assertOption(t, rt, "model", "Qwen/Qwen2.5-0.5B-Instruct")
	assertOption(t, rt, "image", "vllm/vllm-openai:v0.8.5")
}

func TestRuntimeForProvisionResultRunPodMissingFields(t *testing.T) {
	base := func() *rmtypes.ProvisionResult {
		sshPort := 22150
		publicIP := "203.0.113.10"
		return &rmtypes.ProvisionResult{
			RunPod: &rmtypes.RunPodProvisionDetail{
				Pods: []rmtypes.RunPodPodDetail{
					{
						PublicIp:    &publicIP,
						SshPort:     &sshPort,
						SshUser:     "root",
						HttpBaseUrl: "https://pod-8000.proxy.runpod.net",
					},
				},
			},
		}
	}
	cases := []struct {
		name    string
		result  func() *rmtypes.ProvisionResult
		model   string
		wantErr string
	}{
		{
			name:    "nil result",
			result:  func() *rmtypes.ProvisionResult { return nil },
			model:   "model",
			wantErr: "provision result",
		},
		{
			name:    "missing detail",
			result:  func() *rmtypes.ProvisionResult { return &rmtypes.ProvisionResult{} },
			model:   "model",
			wantErr: "runpod detail",
		},
		{
			name: "missing pods",
			result: func() *rmtypes.ProvisionResult {
				return &rmtypes.ProvisionResult{RunPod: &rmtypes.RunPodProvisionDetail{}}
			},
			model:   "model",
			wantErr: "runpod pod",
		},
		{
			name: "missing public_ip",
			result: func() *rmtypes.ProvisionResult {
				r := base()
				r.RunPod.Pods[0].PublicIp = nil
				return r
			},
			model:   "model",
			wantErr: "public_ip",
		},
		{
			name: "empty public_ip",
			result: func() *rmtypes.ProvisionResult {
				r := base()
				publicIP := ""
				r.RunPod.Pods[0].PublicIp = &publicIP
				return r
			},
			model:   "model",
			wantErr: "public_ip",
		},
		{
			name: "missing ssh port",
			result: func() *rmtypes.ProvisionResult {
				r := base()
				r.RunPod.Pods[0].SshPort = nil
				return r
			},
			model:   "model",
			wantErr: "ssh_port",
		},
		{
			name: "missing ssh user",
			result: func() *rmtypes.ProvisionResult {
				r := base()
				r.RunPod.Pods[0].SshUser = ""
				return r
			},
			model:   "model",
			wantErr: "ssh_user",
		},
		{
			name: "missing http base url",
			result: func() *rmtypes.ProvisionResult {
				r := base()
				r.RunPod.Pods[0].HttpBaseUrl = ""
				return r
			},
			model:   "model",
			wantErr: "http_base_url",
		},
		{
			name:    "missing model",
			result:  base,
			model:   "",
			wantErr: "model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RuntimeForProvisionResult(rmtypes.ResourceProvisionTypeRunPod, tc.result(), tc.model, "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRuntimeForProvisionResultLambdaCloudSuccess(t *testing.T) {
	sshPort := 22
	publicIP := "198.51.100.7"
	rt, err := RuntimeForProvisionResult(rmtypes.ResourceProvisionTypeLambdaCloud, &rmtypes.ProvisionResult{
		LambdaCloud: &rmtypes.LambdaCloudProvisionDetail{
			GroupResults: []rmtypes.LambdaCloudGroupResult{
				{
					Instances: []rmtypes.LambdaCloudInstanceDetail{
						{
							PublicIp:    &publicIP,
							SshPort:     &sshPort,
							SshUser:     "ubuntu",
							HttpBaseUrl: "http://198.51.100.7:8000",
						},
					},
				},
			},
		},
	}, "model", "vllm/vllm-openai:latest", nil)
	if err != nil {
		t.Fatalf("RuntimeForProvisionResult: %v", err)
	}
	if rt.Target != "LambdaCloud" {
		t.Fatalf("Target = %q, want LambdaCloud", rt.Target)
	}
	assertOption(t, rt, "host", "198.51.100.7")
	assertOption(t, rt, "ssh_port", 22)
	assertOption(t, rt, "ssh_user", "ubuntu")
	assertOption(t, rt, "http_base_url", "http://198.51.100.7:8000")
	assertOption(t, rt, "model", "model")
	assertOption(t, rt, "image", "vllm/vllm-openai:latest")
}

func TestRuntimeForProvisionResultDefaultFallback(t *testing.T) {
	rt, err := RuntimeForProvisionResult(rmtypes.ResourceProvisionTypeKubernetes, nil, "", "", nil)
	if err != nil {
		t.Fatalf("RuntimeForProvisionResult: %v", err)
	}
	if rt == nil || rt.Target != "Kubernetes" || rt.Options != nil {
		t.Fatalf("Runtime = %+v, want Kubernetes without options", rt)
	}
}

func TestRuntimeForProvisionTypeUnknownProviderFallback(t *testing.T) {
	rt := RuntimeForProvisionType(rmtypes.ResourceProvisionType("CustomProvider"))
	if rt == nil || rt.Target != "CustomProvider" || rt.Options != nil {
		t.Fatalf("Runtime = %+v, want CustomProvider without options", rt)
	}
	if rt := RuntimeForProvisionType(""); rt != nil {
		t.Fatalf("Runtime = %+v, want nil for empty provider", rt)
	}
}

func assertOption(t *testing.T, rt *plannerapi.RuntimeRef, key string, want any) {
	t.Helper()
	if rt.Options[key] != want {
		t.Fatalf("Options[%q] = %v, want %v", key, rt.Options[key], want)
	}
}
