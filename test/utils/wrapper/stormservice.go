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

package wrapper

import (
	"strconv"

	"fmt"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	webhook "github.com/vllm-project/aibrix/pkg/webhook"
)

// StormServiceWrapper wraps core StormService types to provide a fluent API for test construction.
type StormServiceWrapper struct {
	stormService orchestrationapi.StormService
}

// MakeStormService creates a new StormServiceWrapper with the given name.
func MakeStormService(name string) *StormServiceWrapper {
	return &StormServiceWrapper{
		stormService: orchestrationapi.StormService{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

// Obj returns the pointer to the underlying StormService object.
func (w *StormServiceWrapper) Obj() *orchestrationapi.StormService {
	return &w.stormService
}

// Name sets the name of the StormService.
func (w *StormServiceWrapper) Name(name string) *StormServiceWrapper {
	w.stormService.Name = name
	return w
}

// Namespace sets the namespace of the StormService.
func (w *StormServiceWrapper) Namespace(ns string) *StormServiceWrapper {
	w.stormService.Namespace = ns
	return w
}

// Annotations sets the annotations of the StormService.
func (w *StormServiceWrapper) Annotations(annotations map[string]string) *StormServiceWrapper {
	w.stormService.Annotations = annotations
	return w
}

// Replicas sets the replicas of the StormService.
func (w *StormServiceWrapper) Replicas(replicas *int32) *StormServiceWrapper {
	w.stormService.Spec.Replicas = replicas
	return w
}

// Selector sets the selector of the StormService.
func (w *StormServiceWrapper) Selector(selector *metav1.LabelSelector) *StormServiceWrapper {
	w.stormService.Spec.Selector = selector
	return w
}

// UpdateStrategy sets the update strategy of the StormService.
func (w *StormServiceWrapper) UpdateStrategy(
	strategy orchestrationapi.StormServiceUpdateStrategy,
) *StormServiceWrapper {
	w.stormService.Spec.UpdateStrategy = strategy
	return w
}

// Stateful sets the stateful flag of the StormService.
func (w *StormServiceWrapper) Stateful(stateful bool) *StormServiceWrapper {
	w.stormService.Spec.Stateful = stateful
	return w
}

// Template sets the template of the StormService.
func (w *StormServiceWrapper) Template(template orchestrationapi.RoleSetTemplateSpec) *StormServiceWrapper {
	w.stormService.Spec.Template = template
	return w
}

// WithBasicTemplate creates a basic template with common settings.
func (w *StormServiceWrapper) WithBasicTemplate() *StormServiceWrapper {
	template := orchestrationapi.RoleSetTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "my-ai-app",
			},
		},
		Spec: &orchestrationapi.RoleSetSpec{
			UpdateStrategy: orchestrationapi.InterleaveRoleSetStrategyType,
			Roles:          []orchestrationapi.RoleSpec{},
		},
	}
	w.stormService.Spec.Template = template
	return w
}

func (w *StormServiceWrapper) withRole(name string, stateful bool) *StormServiceWrapper {
	if w.stormService.Spec.Template.Spec == nil {
		w.WithBasicTemplate()
	}

	masterRole := orchestrationapi.RoleSpec{
		Name:         name,
		Replicas:     ptr.To(int32(1)),
		UpgradeOrder: ptr.To(int32(1)),
		Stateful:     stateful,
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"role": name,
					"app":  "my-ai-app",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  fmt.Sprintf("vllm-%s", name),
						Image: "vllm/vllm-openai:latest",
						Args: []string{
							"--model", "meta-llama/Llama-3-8b",
							"--tensor-parallel-size", "1",
						},
						Ports: []v1.ContainerPort{
							{ContainerPort: 8000, Protocol: "TCP"},
						},
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("4"),
								v1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					},
				},
			},
		},
	}

	w.stormService.Spec.Template.Spec.Roles = append(w.stormService.Spec.Template.Spec.Roles, masterRole)
	return w
}

// WithWorkerRole adds a worker role to the StormService.
func (w *StormServiceWrapper) WithWorkerRole() *StormServiceWrapper {
	return w.withRole("worker", false)
}

// WithMasterRole adds a master role to the StormService.
func (w *StormServiceWrapper) WithMasterRole() *StormServiceWrapper {
	return w.withRole("master", true)
}

// WithSidecarInjection adds sidecar containers to all roles.
func (w *StormServiceWrapper) WithSidecarInjection(runtimeImage string) *StormServiceWrapper {
	if w.stormService.Spec.Template.Spec == nil {
		return w
	}

	// Default runtime image if not provided
	if runtimeImage == "" {
		runtimeImage = webhook.SidecarImage
	}

	sidecarContainer := v1.Container{
		Name:  webhook.SidecarName,
		Image: runtimeImage,
		Command: []string{
			"aibrix_runtime",
			"--port", strconv.Itoa(webhook.SidecarPort),
		},
		Ports: []v1.ContainerPort{
			{
				Name:          "metrics",
				ContainerPort: webhook.SidecarPort,
				Protocol:      "TCP",
			},
		},
		Env: []v1.EnvVar{
			{
				Name:  "INFERENCE_ENGINE",
				Value: "vllm",
			},
			{
				Name:  "INFERENCE_ENGINE_ENDPOINT",
				Value: webhook.DefaultEngineEndpoint,
			},
		},
		Resources: v1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
		LivenessProbe: &v1.Probe{
			ProbeHandler: v1.ProbeHandler{
				HTTPGet: &v1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(webhook.SidecarPort),
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       2,
		},
		ReadinessProbe: &v1.Probe{
			ProbeHandler: v1.ProbeHandler{
				HTTPGet: &v1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt(webhook.SidecarPort),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	}

	// Add sidecar to all roles
	for i := range w.stormService.Spec.Template.Spec.Roles {
		role := &w.stormService.Spec.Template.Spec.Roles[i]
		// Prepend sidecar container
		containers := []v1.Container{sidecarContainer}
		containers = append(containers, role.Template.Spec.Containers...)
		role.Template.Spec.Containers = containers
	}

	return w
}

// WithDefaultConfiguration sets up the StormService with default configuration.
func (w *StormServiceWrapper) WithDefaultConfiguration() *StormServiceWrapper {
	return w.
		Replicas(ptr.To(int32(1))).
		Selector(&metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "my-ai-app",
			},
		}).
		UpdateStrategy(orchestrationapi.StormServiceUpdateStrategy{
			Type: orchestrationapi.RollingUpdateStormServiceStrategyType,
		}).
		Stateful(true).
		WithBasicTemplate().
		WithWorkerRole().
		WithMasterRole()
}
