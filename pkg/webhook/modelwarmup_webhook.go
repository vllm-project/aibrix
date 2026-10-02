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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	modelapi "github.com/vllm-project/aibrix/api/model/v1alpha1"
)

type ModelWarmupWebhook struct{}

func SetupModelWarmupWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&modelapi.ModelWarmup{}).
		WithValidator(&ModelWarmupWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-model-aibrix-ai-v1alpha1-modelwarmup,mutating=false,failurePolicy=fail,sideEffects=None,groups=model.aibrix.ai,resources=modelwarmups,verbs=create;update,versions=v1alpha1,name=vmodelwarmup.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &ModelWarmupWebhook{}

func (w *ModelWarmupWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, validateModelWarmup(obj.(*modelapi.ModelWarmup))
}

func (w *ModelWarmupWebhook) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldWarmup := oldObj.(*modelapi.ModelWarmup)
	newWarmup := newObj.(*modelapi.ModelWarmup)
	if !equality.Semantic.DeepEqual(oldWarmup.Spec, newWarmup.Spec) {
		return nil, field.Forbidden(field.NewPath("spec"), "ModelWarmup spec is immutable")
	}
	// The controller hashes custom JSON, which preserves Quantity formats that
	// semantic equality ignores. An admitted update must not change that revision.
	oldCustom, err := json.Marshal(oldWarmup.Spec.Custom)
	if err != nil {
		return nil, fmt.Errorf("marshal old ModelWarmup custom action: %w", err)
	}
	newCustom, err := json.Marshal(newWarmup.Spec.Custom)
	if err != nil {
		return nil, fmt.Errorf("marshal new ModelWarmup custom action: %w", err)
	}
	if !bytes.Equal(oldCustom, newCustom) {
		return nil, field.Forbidden(field.NewPath("spec"), "ModelWarmup spec is immutable")
	}
	return nil, validateModelWarmup(newWarmup)
}

func (w *ModelWarmupWebhook) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateModelWarmup(warmup *modelapi.ModelWarmup) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if warmup.Spec.Mode != "" && warmup.Spec.Mode != modelapi.ModelWarmupModeOnce {
		allErrs = append(allErrs, field.NotSupported(
			specPath.Child("mode"), warmup.Spec.Mode, []string{string(modelapi.ModelWarmupModeOnce)},
		))
	}
	explicitNodes := map[string]struct{}{}
	declaredNodeNames := 0
	if len(warmup.Spec.Targets) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("targets"), "at least one target is required"))
	}
	if len(warmup.Spec.Targets) > modelapi.MaxModelWarmupTargetEntries {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("targets"), len(warmup.Spec.Targets), modelapi.MaxModelWarmupTargetEntries,
		))
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
			declaredNodeNames += len(target.Nodes.Names)
			for _, name := range target.Nodes.Names {
				explicitNodes[name] = struct{}{}
			}
		}
		if target.NodeSelector != nil && len(target.NodeSelector.MatchLabels) == 0 &&
			len(target.NodeSelector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(
				path.Child("nodeSelector"), target.NodeSelector, "an empty selector is not allowed",
			))
		} else if target.NodeSelector != nil {
			if _, err := metav1.LabelSelectorAsSelector(target.NodeSelector); err != nil {
				allErrs = append(allErrs, field.Invalid(path.Child("nodeSelector"), target.NodeSelector, err.Error()))
			}
		}
	}
	if declaredNodeNames > modelapi.MaxModelWarmupTargets {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("targets"), declaredNodeNames, modelapi.MaxModelWarmupTargets,
		))
	}
	if len(explicitNodes) > modelapi.MaxModelWarmupTargets {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("targets"),
			len(explicitNodes),
			modelapi.MaxModelWarmupTargets,
		))
	}

	images := map[string]int{}
	if len(warmup.Spec.ImagePreload.Images) > modelapi.MaxModelWarmupImages {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("imagePreload", "images"), len(warmup.Spec.ImagePreload.Images), modelapi.MaxModelWarmupImages,
		))
	}
	if len(warmup.Spec.ImagePreload.PullSecrets) > modelapi.MaxModelWarmupPullSecrets {
		allErrs = append(allErrs, field.TooMany(
			specPath.Child("imagePreload", "pullSecrets"), len(warmup.Spec.ImagePreload.PullSecrets),
			modelapi.MaxModelWarmupPullSecrets,
		))
	}
	for i, image := range warmup.Spec.ImagePreload.Images {
		path := specPath.Child("imagePreload", "images").Index(i)
		if image.Image == "" {
			allErrs = append(allErrs, field.Required(path.Child("image"), "image is required"))
		}
		if len(image.Command) == 0 {
			allErrs = append(allErrs, field.Required(path.Child("command"), "a safe command is required"))
		}
		if len(image.Command) > modelapi.MaxModelWarmupCommandElements {
			allErrs = append(allErrs, field.TooMany(
				path.Child("command"), len(image.Command), modelapi.MaxModelWarmupCommandElements,
			))
		}
		if len(image.Args) > modelapi.MaxModelWarmupArgElements {
			allErrs = append(allErrs, field.TooMany(
				path.Child("args"), len(image.Args), modelapi.MaxModelWarmupArgElements,
			))
		}
		switch image.ImagePullPolicy {
		case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
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
	containerNames := make(map[string]struct{}, len(warmup.Spec.ImagePreload.Images))
	for i := range warmup.Spec.ImagePreload.Images {
		containerNames[fmt.Sprintf("image-%d", i)] = struct{}{}
	}
	allErrs = append(allErrs, validateModelWarmupCustom(specPath, warmup.Spec.Custom, containerNames)...)
	if !modelWarmupHasRegularWork(warmup.Spec) {
		allErrs = append(allErrs, field.Required(
			specPath.Child("custom", "containers"), "at least one image preload or custom container is required",
		))
	}
	allErrs = append(allErrs, validateModelWarmupPolicies(warmup.Spec.Policies)...)
	return allErrs.ToAggregate()
}

func modelWarmupHasRegularWork(spec modelapi.ModelWarmupSpec) bool {
	return len(spec.ImagePreload.Images) > 0 || (spec.Custom != nil && len(spec.Custom.Containers) > 0)
}

func validateModelWarmupCustom(
	specPath *field.Path,
	custom *modelapi.ModelWarmupCustomAction,
	containerNames map[string]struct{},
) field.ErrorList {
	if custom == nil {
		return nil
	}

	var allErrs field.ErrorList
	customPath := specPath.Child("custom")
	if len(custom.InitContainers) > modelapi.MaxModelWarmupCustomInitContainers {
		allErrs = append(allErrs, field.TooMany(
			customPath.Child("initContainers"), len(custom.InitContainers), modelapi.MaxModelWarmupCustomInitContainers,
		))
	}
	if len(custom.Containers) > modelapi.MaxModelWarmupCustomContainers {
		allErrs = append(allErrs, field.TooMany(
			customPath.Child("containers"), len(custom.Containers), modelapi.MaxModelWarmupCustomContainers,
		))
	}
	if len(custom.Volumes) > modelapi.MaxModelWarmupCustomVolumes {
		allErrs = append(allErrs, field.TooMany(
			customPath.Child("volumes"), len(custom.Volumes), modelapi.MaxModelWarmupCustomVolumes,
		))
	}
	if len(custom.ImagePullSecrets) > modelapi.MaxModelWarmupCustomPullSecrets {
		allErrs = append(allErrs, field.TooMany(
			customPath.Child("imagePullSecrets"), len(custom.ImagePullSecrets), modelapi.MaxModelWarmupCustomPullSecrets,
		))
	}

	declaredVolumes := make(map[string]struct{}, len(custom.Volumes))
	for i, volume := range custom.Volumes {
		path := customPath.Child("volumes").Index(i)
		if volume.Name == "" {
			allErrs = append(allErrs, field.Required(path.Child("name"), "volume name is required"))
		} else if errs := validation.IsDNS1123Label(volume.Name); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(path.Child("name"), volume.Name, strings.Join(errs, ", ")))
		} else if _, exists := declaredVolumes[volume.Name]; exists {
			allErrs = append(allErrs, field.Duplicate(path.Child("name"), volume.Name))
		} else {
			declaredVolumes[volume.Name] = struct{}{}
		}
		if reflect.DeepEqual(volume.VolumeSource, corev1.VolumeSource{}) {
			allErrs = append(allErrs, field.Required(path, "a volume source is required"))
		}
	}

	for i, container := range custom.InitContainers {
		path := customPath.Child("initContainers").Index(i)
		allErrs = append(allErrs, validateModelWarmupContainer(path, container, containerNames, true)...)
		allErrs = append(allErrs, validateModelWarmupVolumeReferences(path, container, declaredVolumes)...)
	}
	for i, container := range custom.Containers {
		path := customPath.Child("containers").Index(i)
		allErrs = append(allErrs, validateModelWarmupContainer(path, container, containerNames, false)...)
		allErrs = append(allErrs, validateModelWarmupVolumeReferences(path, container, declaredVolumes)...)
	}

	for i, secret := range custom.ImagePullSecrets {
		path := customPath.Child("imagePullSecrets").Index(i).Child("name")
		if secret.Name == "" {
			allErrs = append(allErrs, field.Required(path, "image pull secret name is required"))
		} else if errs := validation.IsDNS1123Subdomain(secret.Name); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(path, secret.Name, strings.Join(errs, ", ")))
		}
	}
	return allErrs
}

func validateModelWarmupContainer(
	path *field.Path,
	container corev1.Container,
	containerNames map[string]struct{},
	isInitContainer bool,
) field.ErrorList {
	var allErrs field.ErrorList
	if container.Name == "" {
		allErrs = append(allErrs, field.Required(path.Child("name"), "container name is required"))
	} else if errs := validation.IsDNS1123Label(container.Name); len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(path.Child("name"), container.Name, strings.Join(errs, ", ")))
	} else if _, exists := containerNames[container.Name]; exists {
		allErrs = append(allErrs, field.Duplicate(path.Child("name"), container.Name))
	} else {
		containerNames[container.Name] = struct{}{}
	}
	if container.Image == "" {
		allErrs = append(allErrs, field.Required(path.Child("image"), "container image is required"))
	}
	if isInitContainer && container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
		allErrs = append(allErrs, field.Forbidden(path.Child("restartPolicy"), "Always is not supported for ModelWarmup init containers"))
	}
	return allErrs
}

func validateModelWarmupVolumeReferences(
	path *field.Path,
	container corev1.Container,
	declaredVolumes map[string]struct{},
) field.ErrorList {
	var allErrs field.ErrorList
	for i, mount := range container.VolumeMounts {
		if _, exists := declaredVolumes[mount.Name]; !exists {
			allErrs = append(allErrs, field.Invalid(
				path.Child("volumeMounts").Index(i).Child("name"), mount.Name, "references an undeclared volume",
			))
		}
	}
	for i, device := range container.VolumeDevices {
		if _, exists := declaredVolumes[device.Name]; !exists {
			allErrs = append(allErrs, field.Invalid(
				path.Child("volumeDevices").Index(i).Child("name"), device.Name, "references an undeclared volume",
			))
		}
	}
	return allErrs
}

func validateModelWarmupPolicies(policies *modelapi.ModelWarmupPolicies) field.ErrorList {
	if policies == nil {
		return nil
	}
	var allErrs field.ErrorList
	if policies.Parallelism != nil && *policies.Parallelism <= 0 {
		allErrs = append(allErrs, positivePolicyError("parallelism", *policies.Parallelism))
	}
	if policies.JobTimeoutSeconds != nil && *policies.JobTimeoutSeconds <= 0 {
		allErrs = append(allErrs, positivePolicyError("jobTimeoutSeconds", *policies.JobTimeoutSeconds))
	}
	if policies.RetryLimit != nil && *policies.RetryLimit < 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec", "policies", "retryLimit"), *policies.RetryLimit,
			"must be greater than or equal to zero",
		))
	}
	if policies.TTLSecondsAfterFinished != nil && *policies.TTLSecondsAfterFinished <= 0 {
		allErrs = append(allErrs, positivePolicyError("ttlSecondsAfterFinished", *policies.TTLSecondsAfterFinished))
	}
	return allErrs
}

func positivePolicyError(name string, value interface{}) *field.Error {
	return field.Invalid(
		field.NewPath("spec", "policies", name), value, "must be greater than zero",
	)
}
