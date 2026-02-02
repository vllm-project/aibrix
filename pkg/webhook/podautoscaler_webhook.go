/*
Copyright 2024 The Aibrix Team.

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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var podautoscalerlog = logf.Log.WithName("podautoscaler-resource")

// SetupPodAutoscalerWebhookWithManager registers the webhook for PodAutoscaler in the manager.
func SetupPodAutoscalerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&autoscalingv1alpha1.PodAutoscaler{}).
		WithValidator(&PodAutoscalerCustomValidator{}).
		WithDefaulter(&PodAutoscalerCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-autoscaling-aibrix-ai-v1alpha1-podautoscaler,mutating=true,failurePolicy=ignore,sideEffects=None,groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=create;update,versions=v1alpha1,name=mpodautoscaler-v1alpha1.kb.io,admissionReviewVersions=v1

// PodAutoscalerCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind PodAutoscaler when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PodAutoscalerCustomDefaulter struct {
}

var _ webhook.CustomDefaulter = &PodAutoscalerCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind PodAutoscaler.
func (d *PodAutoscalerCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)

	if !ok {
		return fmt.Errorf("expected an PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Defaulting for PodAutoscaler", "name", podautoscaler.GetName())
	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: The 'path' attribute must follow a specific pattern and should not be modified directly here.
// Modifying the path for an invalid path can cause API server errors; failing to locate the webhook.
// +kubebuilder:webhook:path=/validate-autoscaling-aibrix-ai-v1alpha1-podautoscaler,mutating=false,failurePolicy=ignore,sideEffects=None,groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=create;update,versions=v1alpha1,name=vpodautoscaler-v1alpha1.kb.io,admissionReviewVersions=v1

// PodAutoscalerCustomValidator struct is responsible for validating the PodAutoscaler resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PodAutoscalerCustomValidator struct {
}

var _ webhook.CustomValidator = &PodAutoscalerCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon creation", "name", podautoscaler.GetName())
	return nil, v.validatePodAutoscaler(podautoscaler)
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := newObj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object for the newObj but got %T", newObj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon update", "name", podautoscaler.GetName())

	return nil, v.validatePodAutoscaler(podautoscaler)
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PodAutoscaler.
func (v *PodAutoscalerCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	podautoscaler, ok := obj.(*autoscalingv1alpha1.PodAutoscaler)
	if !ok {
		return nil, fmt.Errorf("expected a PodAutoscaler object but got %T", obj)
	}
	podautoscalerlog.Info("Validation for PodAutoscaler upon deletion", "name", podautoscaler.GetName())
	return nil, nil
}

// validatePodAutoscaler performs all spec validations.
func (v *PodAutoscalerCustomValidator) validatePodAutoscaler(pa *autoscalingv1alpha1.PodAutoscaler) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// 1. Validate ScaleTargetRef
	targetRef := pa.Spec.ScaleTargetRef
	targetRefPath := specPath.Child("scaleTargetRef")
	if targetRef.Name == "" {
		allErrs = append(allErrs, field.Required(targetRefPath.Child("name"), "must be set"))
	}
	if targetRef.Kind == "" {
		allErrs = append(allErrs, field.Required(targetRefPath.Child("kind"), "must be set"))
	}

	// 2. Validate Replica Bounds
	if pa.Spec.MinReplicas != nil && pa.Spec.MaxReplicas < *pa.Spec.MinReplicas {
		minPath := specPath.Child("minReplicas")
		maxPath := specPath.Child("maxReplicas")
		allErrs = append(allErrs,
			field.Invalid(minPath, pa.Spec.MinReplicas, "cannot be greater than maxReplicas"),
			field.Invalid(maxPath, pa.Spec.MaxReplicas, "cannot be less than minReplicas"),
		)
	}

	// 3. Validate ScalingStrategy
	validStrategies := map[autoscalingv1alpha1.ScalingStrategyType]bool{
		autoscalingv1alpha1.HPA: true,
		autoscalingv1alpha1.KPA: true,
		autoscalingv1alpha1.APA: true,
	}
	if !validStrategies[pa.Spec.ScalingStrategy] {
		strategyPath := specPath.Child("scalingStrategy")
		allErrs = append(allErrs, field.NotSupported(strategyPath, pa.Spec.ScalingStrategy, []string{
			string(autoscalingv1alpha1.HPA),
			string(autoscalingv1alpha1.KPA),
			string(autoscalingv1alpha1.APA),
		}))
	}

	// 4. Validate MetricsSources
	metricsPath := specPath.Child("metricsSources")
	if len(pa.Spec.MetricsSources) != 1 {
		allErrs = append(allErrs, field.Invalid(metricsPath, pa.Spec.MetricsSources, "exactly one metricsSource is required"))
	} else {
		ms := &pa.Spec.MetricsSources[0]
		msPath := metricsPath.Index(0)

		if ms.TargetMetric == "" {
			allErrs = append(allErrs, field.Required(msPath.Child("targetMetric"), "must be set"))
		}
		if ms.TargetValue == "" {
			allErrs = append(allErrs, field.Required(msPath.Child("targetValue"), "must be set"))
		} else {
			qty, err := resource.ParseQuantity(ms.TargetValue)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(msPath.Child("targetValue"), ms.TargetValue, "must be a valid number"))
			} else {
				if qty.Sign() <= 0 {
					allErrs = append(allErrs, field.Invalid(msPath.Child("targetValue"), ms.TargetValue, "must be greater than 0"))
				}
			}
		}

		switch ms.MetricSourceType {
		case autoscalingv1alpha1.POD:
			if ms.ProtocolType == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("protocolType"), "required for metricSourceType=pod"))
			}
			if ms.Port == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("port"), "required for metricSourceType=pod"))
			}
			if ms.Path == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("path"), "required for metricSourceType=pod"))
			}

		case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
			if ms.ProtocolType == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("protocolType"), "required for metricSourceType=external/domain"))
			}
			if ms.Endpoint == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("endpoint"), "required for metricSourceType=external/domain"))
			}
			if ms.Path == "" {
				allErrs = append(allErrs, field.Required(msPath.Child("path"), "required for metricSourceType=external/domain"))
			}

		case autoscalingv1alpha1.RESOURCE:
			validMetrics := map[string]bool{"cpu": true, "memory": true}
			if !validMetrics[ms.TargetMetric] {
				allErrs = append(allErrs, field.NotSupported(msPath.Child("targetMetric"), ms.TargetMetric, []string{"cpu", "memory"}))
			}
			// Ensure no extra fields are set
			if ms.Port != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("port"), "not allowed for metricSourceType=resource"))
			}
			if ms.Endpoint != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("endpoint"), "not allowed for metricSourceType=resource"))
			}
			if ms.Path != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("path"), "not allowed for metricSourceType=resource"))
			}
			if ms.ProtocolType != "" {
				allErrs = append(allErrs, field.Forbidden(msPath.Child("protocolType"), "not allowed for metricSourceType=resource"))
			}

		case autoscalingv1alpha1.CUSTOM:
			// No required fields for custom metrics
			break

		default:
			allErrs = append(allErrs, field.NotSupported(msPath.Child("metricSourceType"), ms.MetricSourceType, []string{
				string(autoscalingv1alpha1.POD),
				string(autoscalingv1alpha1.EXTERNAL),
				string(autoscalingv1alpha1.DOMAIN),
				string(autoscalingv1alpha1.RESOURCE),
				string(autoscalingv1alpha1.CUSTOM),
			}))
		}
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: autoscalingv1alpha1.GroupVersion.Group, Kind: "PodAutoscaler"},
		pa.Name,
		allErrs,
	)
}
