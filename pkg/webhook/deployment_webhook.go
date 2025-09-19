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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/vllm-project/aibrix/pkg/constants"
)

// SetupDeploymentWebhookWithManager registers the webhook for Deployment in the manager.
func SetupDeploymentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&appsv1.Deployment{}).
		WithValidator(&DeploymentCustomDefaulter{}).
		WithDefaulter(&DeploymentCustomDefaulter{}).
		Complete()
}

type DeploymentCustomDefaulter struct {
}

//+kubebuilder:webhook:path=/mutate-apps-v1-deployment,mutating=true,failurePolicy=ignore,sideEffects=None,groups=apps,resources=deployments,verbs=create;update,versions=v1,name=medeployment.aibrix.ai,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &DeploymentCustomDefaulter{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *DeploymentCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	deployment, ok := obj.(*appsv1.Deployment)
	if !ok {
		return fmt.Errorf("expected a Deployment object but got %T", obj)
	}

	// Only proceed if the sidecar injection annotation is present and set to "true"
	annotations := deployment.GetAnnotations()
	if annotations == nil {
		return nil
	}
	inject, exists := annotations[SidecarInjectionAnnotation]
	if !exists || inject != "true" {
		return nil
	}

	// Inject sidecar into the Pod
	r.injectAIBrixRuntime(deployment)

	return nil
}

// injectAIBrixRuntime injects the aibrix-runtime sidecar into the Pod template of Deployment
func (r *DeploymentCustomDefaulter) injectAIBrixRuntime(deployment *appsv1.Deployment) {
	podSpec := &deployment.Spec.Template.Spec

	// Get engine type from deployment template annotations, if specified
	var engineType string
	if annotations := deployment.GetAnnotations(); annotations != nil {
		if engine, exists := annotations[constants.ModelLabelEngine]; exists && engine != "" {
			engineType = engine
		}
	}

	// Get sidecar image from deployment annotations; fall back to default if not set
	var sidecarImage string
	if annotations := deployment.GetAnnotations(); annotations != nil {
		if image, exists := annotations[SidecarInjectionRuntimeImageAnnotation]; exists && image != "" {
			sidecarImage = image
		}
	}
	if sidecarImage == "" {
		sidecarImage = SidecarImage // default
	}

	// Skip if sidecar already exists
	for _, container := range podSpec.Containers {
		if container.Name == SidecarName {
			return
		}
	}

	// Infer engine type from primary containers if not set
	if engineType == "" {
		engineType = r.inferEngineType(podSpec.Containers)
	}

	// Build the sidecar container
	runtimeContainer := corev1.Container{
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
					Scheme: "HTTP",
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
					Scheme: "HTTP",
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
	podSpec.Containers = append([]corev1.Container{runtimeContainer}, podSpec.Containers...)
}

// inferEngineType infers the inference engine based on container image names
func (r *DeploymentCustomDefaulter) inferEngineType(containers []corev1.Container) string {
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

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// +kubebuilder:webhook:path=/validate-apps-v1-deployment,mutating=false,failurePolicy=ignore,sideEffects=None,groups=apps,resources=deployments,verbs=create;update,versions=v1,name=vedeployment.aibrix.ai,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &DeploymentCustomDefaulter{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *DeploymentCustomDefaulter) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object creation.
	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *DeploymentCustomDefaulter) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object update.
	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *DeploymentCustomDefaulter) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}
