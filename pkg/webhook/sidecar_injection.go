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

package webhook

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Sidecar injection constants
const (
	// SidecarInjectionAnnotation Annotation used to enable or disable sidecar injection
	SidecarInjectionAnnotation = "model.aibrix.ai/sidecar-injection"
	// SidecarInjectionRuntimeImageAnnotation Annotation used to specify a custom image for the sidecar container
	SidecarInjectionRuntimeImageAnnotation = "model.aibrix.ai/sidecar-runtime-image"
	SidecarName                            = "aibrix-runtime"
	SidecarImage                           = "aibrix/runtime:v0.5.0"
	SidecarCommand                         = "aibrix_runtime"
	SidecarPort                            = 8080
	SidecarHealthPath                      = "/healthz"
	SidecarReadyPath                       = "/ready"
	DefaultEngineEndpoint                  = "http://localhost:8000"
)

// buildRuntimeSidecarContainer creates a runtime sidecar container with the given image and engine type
func buildRuntimeSidecarContainer(sidecarImage, engineType string) corev1.Container {
	return corev1.Container{
		Name:  SidecarName,
		Image: sidecarImage,
		Command: []string{
			SidecarCommand,
			"--port", fmt.Sprintf("%d", SidecarPort),
		},
		Env: []corev1.EnvVar{
			{
				Name:  "INFERENCE_ENGINE",
				Value: engineType,
			},
			{
				Name:  "INFERENCE_ENGINE_ENDPOINT",
				Value: DefaultEngineEndpoint,
			},
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: SidecarPort,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   SidecarHealthPath,
					Port:   intstr.FromInt(SidecarPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       2,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   SidecarReadyPath,
					Port:   intstr.FromInt(SidecarPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
}

// inferEngineType infers the inference engine based on container image names
func inferEngineType(containers []corev1.Container) string {
	for _, c := range containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "vllm") {
			return "vllm"
		}
		if strings.Contains(img, "sglang") {
			return "sglang"
		}
		if strings.Contains(img, "text-generation-inference") || strings.Contains(img, "tgi") {
			return "tgi"
		}
		if strings.Contains(img, "triton") {
			return "triton"
		}
		if strings.Contains(img, "llama") && strings.Contains(img, "cpp") {
			return "llamacpp"
		}
	}
	return "unknown"
}

// containsContainer checks if a container with the given name exists
func containsContainer(containers []corev1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}
