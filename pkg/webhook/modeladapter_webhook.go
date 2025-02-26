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
	"net/url"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type ModelAdapterWebhook struct{}

// SetupBackendRuntimeWebhook will setup the manager to manage the webhooks
func SetupBackendRuntimeWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&modelapi.ModelAdapter{}).
		WithDefaulter(&ModelAdapterWebhook{}).
		WithValidator(&ModelAdapterWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-model-aibrix-ai-v1alpha1-modeladapter,mutating=true,failurePolicy=fail,sideEffects=None,groups=model.aibrix.ai,resources=modeladapters,verbs=create;update,versions=v1alpha1,name=mmodeladapter.kb.io,admissionReviewVersions=v1

var _ webhook.CustomDefaulter = &ModelAdapterWebhook{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (w *ModelAdapterWebhook) Default(ctx context.Context, obj runtime.Object) error {
	return nil
}

//+kubebuilder:webhook:path=/validate-model-aibrix-ai-v1alpha1-modeladapter,mutating=false,failurePolicy=fail,sideEffects=None,groups=model.aibrix.ai,resources=modeladapters,verbs=create;update,versions=v1alpha1,name=vmodeladapter.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &ModelAdapterWebhook{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (w *ModelAdapterWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	adapter := obj.(*modelapi.ModelAdapter)

	if _, err := url.ParseRequestURI(adapter.Spec.ArtifactURL); err != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("artifactURL"), adapter.Spec.ArtifactURL, fmt.Sprintf("artifactURL is invalid: %v", err)))
	}

	return nil, allErrs.ToAggregate()
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (w *ModelAdapterWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (w *ModelAdapterWebhook) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
