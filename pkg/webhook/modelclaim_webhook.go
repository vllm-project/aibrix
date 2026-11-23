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

package webhook

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type ModelClaimWebhook struct{}

func SetupModelClaimWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&modelapi.ModelClaim{}).
		WithValidator(&ModelClaimWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-model-aibrix-ai-v1alpha1-modelclaim,mutating=false,failurePolicy=ignore,sideEffects=None,groups=model.aibrix.ai,resources=modelclaims,verbs=create;update,versions=v1alpha1,name=vmodelclaim.kb.io,admissionReviewVersions=v1,timeoutSeconds=5

var _ webhook.CustomValidator = &ModelClaimWebhook{}

func (w *ModelClaimWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateModelClaim(nil, obj.(*modelapi.ModelClaim))
}

func (w *ModelClaimWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateModelClaim(oldObj.(*modelapi.ModelClaim), newObj.(*modelapi.ModelClaim))
}

func (w *ModelClaimWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateModelClaim(oldClaim, claim *modelapi.ModelClaim) error {
	var allErrs field.ErrorList
	path := field.NewPath("spec")
	spec := &claim.Spec
	// Existing claims may predate these checks. Validate changed field groups
	// so their metadata and finalizers can still be updated without repairing
	// immutable fields. The CRD continues to enforce immutability.
	if oldClaim == nil || !equality.Semantic.DeepEqual(oldClaim.Spec.PodSelector, spec.PodSelector) {
		if spec.PodSelector == nil {
			allErrs = append(allErrs, field.Required(path.Child("podSelector"), "a pod selector is required"))
		} else {
			// Empty selectors intentionally span eligible warm pools in the
			// namespace; the controller still applies its warm-pod checks.
			allErrs = append(allErrs, metav1validation.ValidateLabelSelector(spec.PodSelector,
				metav1validation.LabelSelectorValidationOptions{}, path.Child("podSelector"))...)
		}
	}
	if (oldClaim == nil || oldClaim.Spec.ArtifactURL != spec.ArtifactURL) && strings.TrimSpace(spec.ArtifactURL) == "" {
		allErrs = append(allErrs, field.Required(path.Child("artifactURL"), "a non-blank artifact path or URL is required"))
	}
	if (oldClaim == nil || !equality.Semantic.DeepEqual(oldClaim.Spec.ModelName, spec.ModelName)) &&
		spec.ModelName != nil && strings.TrimSpace(*spec.ModelName) == "" {
		allErrs = append(allErrs, field.Invalid(path.Child("modelName"), *spec.ModelName, "must not be blank when provided"))
	}
	if oldClaim == nil || oldClaim.Spec.Engine != spec.Engine || !equality.Semantic.DeepEqual(oldClaim.Spec.EngineConfig, spec.EngineConfig) {
		// An omitted engineConfig uses the default parallelism and has no
		// arguments to validate or include in a field error.
		if spec.EngineConfig != nil && (spec.Engine == "" || spec.Engine == "vllm") {
			if _, err := utils.ModelClaimVLLMParallelism(spec.EngineConfig); err != nil {
				allErrs = append(allErrs, field.Invalid(path.Child("engineConfig", "args"), "<redacted>", err.Error()))
			}
		}
	}
	allErrs = append(allErrs, validateModelClaimServingPort(oldClaim, claim)...)
	// Require a declaration on creation and prevent removal of an existing one.
	// Grandfather absence only for legacy objects; keep the schema optional.
	if spec.PerGPU == nil && (oldClaim == nil || oldClaim.Spec.PerGPU != nil) {
		allErrs = append(allErrs, field.Required(path.Child("perGPU"), "a per-GPU cost declaration is required"))
	}
	if spec.PerGPU != nil && (oldClaim == nil || !equality.Semantic.DeepEqual(oldClaim.Spec.PerGPU, spec.PerGPU)) {
		if _, err := utils.ModelClaimDeclaredBytes("maximumFootprint", spec.PerGPU.MaximumFootprint); err != nil {
			allErrs = append(allErrs, field.Invalid(path.Child("perGPU", "maximumFootprint"), spec.PerGPU.MaximumFootprint.String(), err.Error()))
		}
		if _, err := utils.ModelClaimDeclaredBytes("kvFloor", spec.PerGPU.KVFloor); err != nil {
			allErrs = append(allErrs, field.Invalid(path.Child("perGPU", "kvFloor"), spec.PerGPU.KVFloor.String(), err.Error()))
		}
	}
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(modelapi.GroupVersion.WithKind("ModelClaim").GroupKind(), claim.Name, allErrs)
}

// validateModelClaimServingPort validates new or changed port declarations,
// while preserving metadata updates for legacy claims with engine port args.
func validateModelClaimServingPort(oldClaim, claim *modelapi.ModelClaim) field.ErrorList {
	var allErrs field.ErrorList
	path := field.NewPath("spec")
	spec := &claim.Spec

	if oldClaim == nil && spec.EngineConfig != nil {
		for key := range spec.EngineConfig.Args {
			flag, _, _ := utils.ModelClaimEngineFlag(key)
			if flag == "--port" {
				allErrs = append(allErrs, field.Invalid(path.Child("engineConfig", "args"), "<redacted>",
					"must not set --port; use spec.servingPort instead"))
			}
		}
	}
	return allErrs
}
