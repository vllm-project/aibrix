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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

type RayClusterFleetCustomValidator struct{}

var _ webhook.CustomValidator = &RayClusterFleetCustomValidator{}

func SetupRayClusterFleetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&orchestrationapi.RayClusterFleet{}).
		WithValidator(&RayClusterFleetCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-orchestration-aibrix-ai-v1alpha1-rayclusterfleet,mutating=false,failurePolicy=ignore,sideEffects=None,groups=orchestration.aibrix.ai,resources=rayclusterfleets,verbs=create;update,versions=v1alpha1,name=vrayclusterfleet.kb.io,admissionReviewVersions=v1

func (v *RayClusterFleetCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	fleet, ok := obj.(*orchestrationapi.RayClusterFleet)
	if !ok {
		return nil, fmt.Errorf("expected a RayClusterFleet object but got %T", obj)
	}

	errs := v.validate(fleet)
	if len(errs) == 0 {
		return nil, nil
	}
	return nil, apierrors.NewInvalid(orchestrationapi.GroupVersion.WithKind("RayClusterFleet").GroupKind(), fleet.Name, errs)
}

func (v *RayClusterFleetCustomValidator) ValidateUpdate(_ context.Context, oldObj runtime.Object, newObj runtime.Object) (admission.Warnings, error) {
	oldFleet, ok := oldObj.(*orchestrationapi.RayClusterFleet)
	if !ok {
		return nil, fmt.Errorf("expected a RayClusterFleet object but got %T", oldObj)
	}
	newFleet, ok := newObj.(*orchestrationapi.RayClusterFleet)
	if !ok {
		return nil, fmt.Errorf("expected a RayClusterFleet object but got %T", newObj)
	}

	errs := v.validate(newFleet)
	if !equality.Semantic.DeepEqual(oldFleet.Spec.Selector, newFleet.Spec.Selector) {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "selector"), "field is immutable"))
	}
	if len(errs) == 0 {
		return nil, nil
	}
	return nil, apierrors.NewInvalid(orchestrationapi.GroupVersion.WithKind("RayClusterFleet").GroupKind(), newFleet.Name, errs)
}

func (v *RayClusterFleetCustomValidator) validate(fleet *orchestrationapi.RayClusterFleet) field.ErrorList {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	if fleet.Spec.Selector == nil {
		errs = append(errs, field.Required(specPath.Child("selector"), "a non-empty selector is required"))
	} else {
		selector, err := metav1.LabelSelectorAsSelector(fleet.Spec.Selector)
		switch {
		case err != nil:
			errs = append(errs, field.Invalid(specPath.Child("selector"), fleet.Spec.Selector, err.Error()))
		case len(fleet.Spec.Selector.MatchExpressions) > 0:
			errs = append(errs, field.Invalid(specPath.Child("selector"), fleet.Spec.Selector, "matchExpressions are not supported"))
		case selector.Empty():
			errs = append(errs, field.Required(specPath.Child("selector"), "a non-empty selector is required"))
		case !selector.Matches(labels.Set(fleet.Spec.Template.Labels)):
			errs = append(errs, field.Invalid(specPath.Child("template", "metadata", "labels"), fleet.Spec.Template.Labels, "selector does not match template labels"))
		}
	}
	return errs
}

func (v *RayClusterFleetCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
