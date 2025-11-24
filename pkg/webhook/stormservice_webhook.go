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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
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

//+kubebuilder:webhook:path=/mutate-orchestration-aibrix-ai-v1alpha1-stormservice,mutating=true,failurePolicy=ignore,sideEffects=None,groups=orchestration.aibrix.ai,resources=stormservices,verbs=create;update,versions=v1alpha1,name=mstormservice.kb.io,admissionReviewVersions=v1

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

	// Get engine type from RoleSet template annotations, if specified
	var engineType string
	if annotations := stormService.Spec.Template.Annotations; annotations != nil {
		if engine, exists := annotations[constants.ModelLabelEngine]; exists && engine != "" {
			engineType = engine
		}
	}

	// Get sidecar image from stormService annotations; fall back to default if not set
	var sidecarImage string
	if annotations := stormService.GetAnnotations(); annotations != nil {
		if image, exists := annotations[SidecarInjectionRuntimeImageAnnotation]; exists && image != "" {
			sidecarImage = image
		}
	}

	if sidecarImage == "" {
		sidecarImage = SidecarImage // default
	}

	for i := range spec.Roles {
		role := &spec.Roles[i]

		// Skip if sidecar already exists
		if containsContainer(role.Template.Spec.Containers, SidecarName) {
			continue
		}

		currentEngineType := engineType
		if currentEngineType == "" {
			// fallback：get inference engine from primary containers
			currentEngineType = inferEngineType(role.Template.Spec.Containers)
		}

		// Build the sidecar container using shared logic
		runtimeContainer := buildRuntimeSidecarContainer(sidecarImage, currentEngineType)

		// Inject sidecar at the beginning
		role.Template.Spec.Containers = append(
			[]corev1.Container{runtimeContainer},
			role.Template.Spec.Containers...,
		)
	}
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
//+kubebuilder:webhook:path=/validate-orchestration-aibrix-ai-v1alpha1-stormservice,mutating=false,failurePolicy=ignore,sideEffects=None,groups=orchestration.aibrix.ai,resources=stormservices,verbs=create;update,versions=v1alpha1,name=vstormservice.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &StormServiceCustomDefaulter{}

// estimatedPodNameSuffixLength is an approximation for the total length of suffixes
// added to a StormService name and role name to form a final Pod name.
// e.g., <stormservice>-<revision>-<role>-<hash>-<index>
const estimatedPodNameSuffixLength = 36

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	stormService, ok := obj.(*orchestrationv1alpha1.StormService)
	if !ok {
		return nil, fmt.Errorf("expected a StormService object but got %T", obj)
	}

	// 1. Validate StormService.Name itself (≤63, DNS-1123 compliant)
	if len(stormService.Name) > 63 {
		return nil, fmt.Errorf("StormService name must be no more than 63 characters")
	}

	// 2. Only validate roles that actually create PodSet (i.e., PodGroupSize > 1)
	maxPodSetRoleNameLen := 0
	hasPodSetRole := false
	for _, role := range stormService.Spec.Template.Spec.Roles {
		if role.PodGroupSize != nil && *role.PodGroupSize > 1 {
			hasPodSetRole = true
			if len(role.Name) > maxPodSetRoleNameLen {
				maxPodSetRoleNameLen = len(role.Name)
			}
		}
	}

	if hasPodSetRole {
		// Estimated PodSet name length:
		// RoleSet: <stormService.Name>-roleset-xxxxx → N + 14
		// PodSet: <roleSet.Name>-<roleName>-<hash(6)>-99999 → ≈ N + M + 36
		estimatedPodSetNameLen := len(stormService.Name) + maxPodSetRoleNameLen + estimatedPodNameSuffixLength
		if estimatedPodSetNameLen > 63 {
			return nil, fmt.Errorf(
				"combined length of StormService name (%d) and longest PodSet-enabled role name (%d) may produce PodSet names exceeding 63 characters (estimated: %d). Please use shorter names",
				len(stormService.Name), maxPodSetRoleNameLen, estimatedPodSetNameLen,
			)
		}
	}

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
