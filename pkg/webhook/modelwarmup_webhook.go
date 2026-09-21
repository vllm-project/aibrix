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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type ModelWarmupWebhook struct{}

func SetupModelWarmupWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&modelapi.ModelWarmup{}).
		WithDefaulter(&ModelWarmupWebhook{}).
		WithValidator(&ModelWarmupWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-model-aibrix-ai-v1alpha1-modelwarmup,mutating=true,failurePolicy=fail,sideEffects=None,groups=model.aibrix.ai,resources=modelwarmups,verbs=create;update,versions=v1alpha1,name=mmodelwarmup.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &ModelWarmupWebhook{}

func (w *ModelWarmupWebhook) Default(_ context.Context, obj runtime.Object) error {
	warmup := obj.(*modelapi.ModelWarmup)
	if warmup.Spec.Policies == nil {
		warmup.Spec.Policies = &modelapi.ModelWarmupPolicies{}
	}
	if warmup.Spec.Policies.Parallelism == nil {
		warmup.Spec.Policies.Parallelism = ptr.To(modelapi.DefaultModelWarmupParallelism)
	}
	if warmup.Spec.Policies.GlobalTimeoutSeconds == nil {
		warmup.Spec.Policies.GlobalTimeoutSeconds = ptr.To(modelapi.DefaultModelWarmupGlobalTimeoutSeconds)
	}
	if warmup.Spec.Policies.RetryLimit == nil {
		warmup.Spec.Policies.RetryLimit = ptr.To(modelapi.DefaultModelWarmupRetryLimit)
	}
	if warmup.Spec.Policies.TTLSecondsAfterFinished == nil {
		warmup.Spec.Policies.TTLSecondsAfterFinished = ptr.To(modelapi.DefaultModelWarmupTTLSecondsAfterFinished)
	}
	for i := range warmup.Spec.ImagePreload.Images {
		if warmup.Spec.ImagePreload.Images[i].ImagePullPolicy == "" {
			warmup.Spec.ImagePreload.Images[i].ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	return nil
}

//+kubebuilder:webhook:path=/validate-model-aibrix-ai-v1alpha1-modelwarmup,mutating=false,failurePolicy=fail,sideEffects=None,groups=model.aibrix.ai,resources=modelwarmups,verbs=create;update,versions=v1alpha1,name=vmodelwarmup.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &ModelWarmupWebhook{}

func (w *ModelWarmupWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateModelWarmup(obj.(*modelapi.ModelWarmup))
}

func (w *ModelWarmupWebhook) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return nil, validateModelWarmup(newObj.(*modelapi.ModelWarmup))
}

func (w *ModelWarmupWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateModelWarmup(warmup *modelapi.ModelWarmup) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	explicitNodes := map[string]struct{}{}
	if len(warmup.Spec.Targets) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("targets"), "at least one target is required"))
	}
	for i, target := range warmup.Spec.Targets {
		path := specPath.Child("targets").Index(i)
		if (target.Nodes == nil) == (target.NodeSelector == nil) {
			allErrs = append(allErrs, field.Invalid(path, target, "exactly one of nodes or nodeSelector is required"))
		}
		if target.Nodes != nil && len(target.Nodes.Names) == 0 {
			allErrs = append(allErrs, field.Required(path.Child("nodes", "names"), "at least one node name is required"))
		}
		if target.Nodes != nil {
			for _, name := range target.Nodes.Names {
				explicitNodes[name] = struct{}{}
			}
		}
		if target.NodeSelector != nil && len(target.NodeSelector.MatchLabels) == 0 &&
			len(target.NodeSelector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(
				path.Child("nodeSelector"), target.NodeSelector, "an empty selector is not allowed",
			))
		}
	}
	if len(explicitNodes) > modelapi.MaxModelWarmupTargets {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("targets"),
			len(explicitNodes),
			modelapi.MaxModelWarmupTargets,
		))
	}

	images := map[string]int{}
	for i, image := range warmup.Spec.ImagePreload.Images {
		path := specPath.Child("imagePreload", "images").Index(i)
		if image.Image == "" {
			allErrs = append(allErrs, field.Required(path.Child("image"), "image is required"))
		}
		if len(image.Command) == 0 {
			allErrs = append(allErrs, field.Required(path.Child("command"), "a safe command is required"))
		}
		switch image.ImagePullPolicy {
		case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		default:
			supportedPolicies := []string{
				string(corev1.PullAlways), string(corev1.PullIfNotPresent), string(corev1.PullNever),
			}
			allErrs = append(allErrs, field.NotSupported(
				path.Child("imagePullPolicy"), image.ImagePullPolicy, supportedPolicies,
			))
		}
		if previous, exists := images[image.Image]; exists {
			allErrs = append(allErrs, field.Invalid(
				path.Child("image"), image.Image, fmt.Sprintf("duplicates image at index %d", previous),
			))
		}
		images[image.Image] = i
	}
	if len(warmup.Spec.ImagePreload.Images) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("imagePreload", "images"), "at least one image is required"))
	}
	if policies := warmup.Spec.Policies; policies != nil {
		if policies.Parallelism != nil && *policies.Parallelism <= 0 {
			allErrs = append(allErrs, positivePolicyError("parallelism", *policies.Parallelism))
		}
		if policies.GlobalTimeoutSeconds != nil && *policies.GlobalTimeoutSeconds <= 0 {
			allErrs = append(allErrs, positivePolicyError("globalTimeoutSeconds", *policies.GlobalTimeoutSeconds))
		}
		if policies.RetryLimit != nil && *policies.RetryLimit <= 0 {
			allErrs = append(allErrs, positivePolicyError("retryLimit", *policies.RetryLimit))
		}
		if policies.TTLSecondsAfterFinished != nil && *policies.TTLSecondsAfterFinished <= 0 {
			allErrs = append(allErrs, positivePolicyError("ttlSecondsAfterFinished", *policies.TTLSecondsAfterFinished))
		}
	}
	return allErrs.ToAggregate()
}

func positivePolicyError(name string, value interface{}) *field.Error {
	return field.Invalid(
		field.NewPath("spec", "policies", name), value, "must be greater than zero",
	)
}
