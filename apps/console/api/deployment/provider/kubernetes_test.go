/*
Copyright 2025 The Aibrix Team.

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

package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/vllm-project/aibrix/apps/console/api/config"
	deploymentstatus "github.com/vllm-project/aibrix/apps/console/api/deployment/status"
	pb "github.com/vllm-project/aibrix/apps/console/api/gen/console/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

type staticKubernetesClientProvider struct {
	clientset kubernetes.Interface
	namespace string
}

func (p staticKubernetesClientProvider) Client() (kubernetes.Interface, string, error) {
	return p.clientset, p.namespace, nil
}

func TestRegistryResolvesDefaultAndLegacyKubernetesKinds(t *testing.T) {
	implementation := NewKubernetesDeploymentProvider(
		staticKubernetesClientProvider{clientset: fake.NewSimpleClientset(), namespace: defaultKubernetesNamespace},
		config.KubernetesWorkloadConfig{},
	)
	registry := NewRegistry(implementation)

	for _, kind := range []string{"", KubernetesProviderKind, LegacyKubernetesProviderKind} {
		got, err := registry.Get(kind)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", kind, err)
		}
		if got != implementation {
			t.Fatalf("Get(%q) returned a different provider", kind)
		}
	}
}

func TestBuildContainerArgsUsesTemplateContract(t *testing.T) {
	spec := &pb.ModelDeploymentTemplateSpec{
		Engine: &pb.EngineSpec{
			Type:      "vllm",
			ServeArgs: []string{"--dtype", "half"},
		},
		ModelSource: &pb.ModelSourceSpec{
			Uri:              "org/model",
			Revision:         "main",
			TokenizerPath:    "org/tokenizer",
			ChatTemplatePath: "/templates/chat.jinja",
		},
		Parallelism: &pb.ParallelismSpec{Tp: 2},
		EngineArgs: map[string]string{
			"block_size":            "1",
			"enable_prefix_caching": "true",
			"max_num_seqs":          "64",
			"use_v2_block_manager":  "false",
		},
		Quantization: &pb.QuantizationSpec{
			Weight:  "fp8",
			KvCache: "fp8_e4m3",
		},
	}

	got := buildContainerArgs(spec)
	want := []string{
		"--model", "org/model",
		"--revision", "main",
		"--tokenizer", "org/tokenizer",
		"--chat-template", "/templates/chat.jinja",
		"--tensor-parallel-size", "2",
		"--block-size", "1",
		"--enable-prefix-caching",
		"--max-num-seqs", "64",
		"--quantization", "fp8",
		"--kv-cache-dtype", "fp8_e4m3",
		"--dtype", "half",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected args:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildContainerArgsUsesSGLangMappings(t *testing.T) {
	spec := &pb.ModelDeploymentTemplateSpec{
		Engine:      &pb.EngineSpec{Type: "sglang"},
		ModelSource: &pb.ModelSourceSpec{Uri: "org/model"},
		EngineArgs: map[string]string{
			"gpu_memory_utilization": "0.8",
			"max_model_len":          "8192",
			"max_num_seqs":           "32",
		},
	}
	got := buildContainerArgs(spec)
	want := []string{
		"--model-path", "org/model",
		"--mem-fraction-static", "0.8",
		"--context-length", "8192",
		"--max-running-requests", "32",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected SGLang args:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildDeploymentIncludesResourcesRequiredByHPA(t *testing.T) {
	cfg := config.KubernetesWorkloadConfig{
		CPURequest:    "500m",
		ContainerPort: 8000,
	}
	spec := &pb.ModelDeploymentTemplateSpec{
		Engine: &pb.EngineSpec{
			Type:                "vllm",
			Image:               "example/vllm:latest",
			HealthEndpoint:      "/health",
			ReadyTimeoutSeconds: 900,
		},
		ModelSource: &pb.ModelSourceSpec{Uri: "org/model"},
		Accelerator: &pb.AcceleratorSpec{Type: "NVIDIA-H100", Count: 2},
	}

	deployment := buildDeployment("test", "default", map[string]string{}, cfg, spec, 1)
	resources := deployment.Spec.Template.Spec.Containers[0].Resources
	if got := resources.Requests.Cpu().String(); got != "500m" {
		t.Fatalf("expected CPU request 500m, got %s", got)
	}
	gpuRequest := resources.Requests[corev1.ResourceName("nvidia.com/gpu")]
	if got := gpuRequest.String(); got != "2" {
		t.Fatalf("expected GPU request 2, got %s", got)
	}
	gpuLimit := resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if got := gpuLimit.String(); got != "2" {
		t.Fatalf("expected GPU limit 2, got %s", got)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].LivenessProbe.InitialDelaySeconds; got != 900 {
		t.Fatalf("expected liveness delay 900, got %d", got)
	}
}

func TestCreateBuildsKubernetesResources(t *testing.T) {
	const namespace = "aibrix-console"
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	})
	workload := config.KubernetesWorkloadConfig{
		ServiceType:             string(corev1.ServiceTypeClusterIP),
		ContainerPort:           8000,
		ServicePort:             8000,
		CPURequest:              "1",
		HPATargetCPUUtilization: 80,
	}
	implementation := NewKubernetesDeploymentProvider(
		staticKubernetesClientProvider{clientset: client, namespace: namespace},
		workload,
	)
	template := &pb.ModelDeploymentTemplate{
		Id:      "template-1",
		Name:    "vllm-template",
		Version: "v1.0.0",
		ModelId: "model-1",
		Spec: &pb.ModelDeploymentTemplateSpec{
			Engine:      &pb.EngineSpec{Type: "vllm", Image: "example/vllm:latest"},
			ModelSource: &pb.ModelSourceSpec{Uri: "org/model"},
			Accelerator: &pb.AcceleratorSpec{Type: "CPU", Count: 1},
		},
	}
	req := &pb.CreateDeploymentRequest{
		Name: "test-deployment",
		Overrides: &pb.DeploymentOverrides{
			MinReplicas:       1,
			MaxReplicas:       3,
			EnableAutoScaling: true,
		},
	}

	if err := implementation.Validate(context.Background(), template, req); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	created, err := implementation.Create(context.Background(), template, req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetImplementationKind() != KubernetesProviderKind {
		t.Fatalf("implementation kind = %q", created.GetImplementationKind())
	}
	if created.GetReplicas() != "1[3]" {
		t.Fatalf("replicas = %q", created.GetReplicas())
	}

	deployments, _ := client.AppsV1().Deployments(namespace).List(context.Background(), metav1.ListOptions{})
	services, _ := client.CoreV1().Services(namespace).List(context.Background(), metav1.ListOptions{})
	hpas, _ := client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(context.Background(), metav1.ListOptions{})
	if len(deployments.Items) != 1 || len(services.Items) != 1 || len(hpas.Items) != 1 {
		t.Fatalf("created resources: deployments=%d services=%d hpas=%d", len(deployments.Items), len(services.Items), len(hpas.Items))
	}
}

func TestValidateRejectsAutoscalingWithoutReplicaRange(t *testing.T) {
	implementation := NewKubernetesDeploymentProvider(
		staticKubernetesClientProvider{clientset: fake.NewSimpleClientset(), namespace: defaultKubernetesNamespace},
		config.KubernetesWorkloadConfig{},
	)
	template := &pb.ModelDeploymentTemplate{
		Spec: &pb.ModelDeploymentTemplateSpec{
			Engine:      &pb.EngineSpec{Type: "vllm", Image: "example/vllm:latest"},
			ModelSource: &pb.ModelSourceSpec{Uri: "org/model"},
			Accelerator: &pb.AcceleratorSpec{Type: "CPU", Count: 1},
		},
	}
	req := &pb.CreateDeploymentRequest{
		Name: "test-deployment",
		Overrides: &pb.DeploymentOverrides{
			MinReplicas:       1,
			MaxReplicas:       1,
			EnableAutoScaling: true,
		},
	}
	if err := implementation.Validate(context.Background(), template, req); err == nil {
		t.Fatal("Validate() expected autoscaling range error")
	}
}

func TestGenerateResourceNameRetainsUniqueSuffixAfterTruncation(t *testing.T) {
	longName := strings.Repeat("long-name-", 10)
	first := generateResourceName(longName)
	second := generateResourceName(longName)
	if len(first) > 63 || len(second) > 63 {
		t.Fatalf("resource names exceed DNS label limit: %q, %q", first, second)
	}
	if first == second {
		t.Fatalf("resource names should retain unique suffixes: %q", first)
	}
}

func TestResolveKubernetesDeploymentStatus(t *testing.T) {
	replicas := int32(2)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			ReadyReplicas:      2,
			AvailableReplicas:  2,
		},
	}
	got, _, _ := resolveKubernetesDeploymentStatus(deployment, true)
	if got != deploymentstatus.StatusReady {
		t.Fatalf("status = %q, want %q", got, deploymentstatus.StatusReady)
	}
	got, _, _ = resolveKubernetesDeploymentStatus(deployment, false)
	if got != deploymentstatus.StatusDegraded {
		t.Fatalf("status = %q, want %q", got, deploymentstatus.StatusDegraded)
	}
}
