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
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// SetupStormServiceWebhookWithManager registers the webhook for StormService in the manager.
func SetupStormServiceWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&orchestrationv1alpha1.StormService{}).
		WithValidator(&StormServiceCustomDefaulter{}).
		WithDefaulter(&StormServiceCustomDefaulter{}).
		Complete()
}

type StormServiceCustomDefaulter struct {
}

//+kubebuilder:webhook:path=/mutate-orchestration-aibrix-ai-v1alpha1-stormservice,mutating=true,failurePolicy=fail,sideEffects=None,groups=orchestration.aibrix.ai,resources=stormservices,verbs=create;update,versions=v1alpha1,name=mstormservice.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &StormServiceCustomDefaulter{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	stormService, ok := obj.(*orchestrationv1alpha1.StormService)
	if !ok {
		return fmt.Errorf("expected a StormService object but got %T", obj)
	}

	// Only proceed if the sidecar injection annotation is present
	if _, exists := stormService.GetAnnotations()[SidecarInjectionAnnotation]; !exists {
		return nil
	}

	// Skip if spec is nil
	if stormService.Spec.Template.Spec == nil {
		return nil
	}

	// Inject sidecar into each role
	r.injectAIBrixRuntime(stormService)

	return nil
}

// injectAIBrixRuntime injects the aibrix-runtime sidecar into each Role's pod template
func (r *StormServiceCustomDefaulter) injectAIBrixRuntime(stormService *orchestrationv1alpha1.StormService) {
	spec := stormService.Spec.Template.Spec

	for i := range spec.Roles {
		role := &spec.Roles[i]

		// Skip if sidecar already exists
		if containsContainer(role.Template.Spec.Containers, SidecarName) {
			continue
		}

		// Infer inference engine from primary containers
		engineType := r.inferEngineType(role.Template.Spec.Containers)

		// Build the sidecar container
		runtimeContainer := corev1.Container{
			Name:  SidecarName,
			Image: SidecarImage,
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
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      SidecarVolumeName,
					MountPath: SidecarModelMountPath,
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: SidecarHealthPath,
						Port: intstr.FromInt(SidecarPort),
					},
				},
				InitialDelaySeconds: 3,
				PeriodSeconds:       2,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: SidecarReadyPath,
						Port: intstr.FromInt(SidecarPort),
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

		// Inject sidecar at the beginning
		role.Template.Spec.Containers = append(
			[]corev1.Container{runtimeContainer},
			role.Template.Spec.Containers...,
		)

		// Ensure volume exists
		if !hasVolume(role.Template.Spec.Volumes, SidecarVolumeName) {
			role.Template.Spec.Volumes = append(role.Template.Spec.Volumes, corev1.Volume{
				Name: SidecarVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			})
		}
	}
}

// inferEngineType infers the inference engine based on container image names
func (r *StormServiceCustomDefaulter) inferEngineType(containers []corev1.Container) string {
	for _, c := range containers {
		img := strings.ToLower(c.Image)
		if strings.Contains(img, "vllm") {
			return "vllm"
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

// hasVolume checks if a volume with the given name exists
func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// Sidecar injection constants
const (
	SidecarInjectionAnnotation = "stormservice.orchestration.aibrix.ai/sidecar-injection"
	SidecarName                = "aibrix-runtime"
	SidecarImage               = "aibrix-container-registry-cn-beijing.cr.volces.com/aibrix/runtime:v0.3.0"
	SidecarCommand             = "aibrix_runtime"
	SidecarPort                = 8080
	SidecarModelMountPath      = "/models"
	SidecarVolumeName          = "model-hostpath"
	SidecarHealthPath          = "/healthz"
	SidecarReadyPath           = "/ready"
	DefaultEngineEndpoint      = "http://localhost:8000"
)

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
//+kubebuilder:webhook:path=/validate-orchestration-aibrix-ai-v1alpha1-stormservice,mutating=false,failurePolicy=fail,sideEffects=None,groups=orchestration.aibrix.ai,resources=stormservices,verbs=create;update,versions=v1alpha1,name=vstormservice.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &StormServiceCustomDefaulter{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object creation.
	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object update.
	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}
