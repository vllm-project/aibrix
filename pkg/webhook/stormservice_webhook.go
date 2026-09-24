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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const stormServiceScaleWebhookPath = "/validate-orchestration-aibrix-ai-v1alpha1-stormservice-scale"

// SetupStormServiceWebhookWithManager registers the webhook for StormService in the manager.
func SetupStormServiceWebhookWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr).For(&orchestrationv1alpha1.StormService{}).
		WithValidator(&StormServiceCustomDefaulter{}).
		WithDefaulter(&StormServiceCustomDefaulter{}).
		Complete(); err != nil {
		return err
	}
	// kubectl scale, HPA, and other external autoscalers write spec.replicas through
	// the /scale subresource, which does not invoke the main StormService validator.
	mgr.GetWebhookServer().Register(stormServiceScaleWebhookPath, &admission.Webhook{
		Handler: &StormServiceScaleValidator{Reader: mgr.GetAPIReader()},
	})
	return nil
}

//+kubebuilder:webhook:path=/validate-orchestration-aibrix-ai-v1alpha1-stormservice-scale,mutating=false,failurePolicy=ignore,sideEffects=None,groups=orchestration.aibrix.ai,resources=stormservices/scale,verbs=update,versions=v1alpha1,name=vstormservicescale.kb.io,admissionReviewVersions=v1

// StormServiceScaleValidator rejects /scale updates that would set spec.replicas > 1
// on a StormService with a declared mode of Pooled. Omitted mode is left alone so
// inferred pooled objects can still scale spec.replicas, matching validateStormServiceMode.
type StormServiceScaleValidator struct {
	Reader client.Reader
}

var _ admission.Handler = &StormServiceScaleValidator{}

// Handle implements admission.Handler for the StormService /scale subresource.
func (v *StormServiceScaleValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	scale := &autoscalingv1.Scale{}
	if err := json.Unmarshal(req.Object.Raw, scale); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode Scale: %w", err))
	}

	stormService := &orchestrationv1alpha1.StormService{}
	if err := v.Reader.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, stormService); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Errored(http.StatusNotFound, err)
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("failed to get StormService %s/%s: %w", req.Namespace, req.Name, err))
	}

	if err := validateStormServiceScale(stormService, scale.Spec.Replicas); err != nil {
		return admission.Denied(err.Error())
	}
	return admission.Allowed("")
}

// validateStormServiceScale applies the proposed replica count onto a copy of the
// parent StormService and reuses validateStormServiceMode. That keeps /scale on
// the same declared-mode rule as create/update and does not pin inferred mode.
func validateStormServiceScale(stormService *orchestrationv1alpha1.StormService, replicas int32) error {
	proposed := stormService.DeepCopy()
	proposed.Spec.Replicas = &replicas
	return validateStormServiceMode(proposed)
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

		// Ensure the artifacts download path is shared with the sidecar container
		foundEmptyDirVolume := false
		for i := range role.Template.Spec.Volumes {
			v := &role.Template.Spec.Volumes[i]
			if v.Name == DefaultAdapterVolumeName {
				if v.EmptyDir == nil {
					// Volume with same name exists but is not EmptyDir. Overwrite to ensure correct type.
					v.VolumeSource = corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					}
				}
				foundEmptyDirVolume = true
				break
			}
		}
		if !foundEmptyDirVolume {
			role.Template.Spec.Volumes = append(role.Template.Spec.Volumes, corev1.Volume{
				Name: DefaultAdapterVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			})
		}
		for ci := range role.Template.Spec.Containers {
			container := &role.Template.Spec.Containers[ci]
			if container.Name == SidecarName {
				continue
			}
			if !utils.HasVolumeMount(container.VolumeMounts, DefaultAdapterVolumeName, DefaultAdapterMountPath) {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
					Name:      DefaultAdapterVolumeName,
					MountPath: DefaultAdapterMountPath,
				})
			}
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

	if err := validateStormServiceMode(stormService); err != nil {
		return nil, err
	}
	if err := validateStormServiceSchedulingStrategy(stormService); err != nil {
		return nil, err
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
	stormService, ok := newObj.(*orchestrationv1alpha1.StormService)
	if !ok {
		return nil, fmt.Errorf("expected a StormService object but got %T", newObj)
	}
	if err := validateStormServiceMode(stormService); err != nil {
		return nil, err
	}
	// Re-run the scheduling validation only when one of its inputs changed; its result cannot
	// differ otherwise. Objects that predate this validation must keep accepting unrelated
	// updates, in particular the controller's finalizer removal, or they could never be deleted.
	// Comparing the whole template would not do: the defaulter may inject the sidecar into an
	// object that was stored without it, which looks like a template change on every update.
	oldStormService, ok := oldObj.(*orchestrationv1alpha1.StormService)
	if !ok || schedulingConfigChanged(oldStormService.Spec.Template.Spec, stormService.Spec.Template.Spec) {
		if err := validateStormServiceSchedulingStrategy(stormService); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// schedulingConfigChanged reports whether any input of validateRoleSetSchedulingStrategy differs
// between two RoleSet specs: the RoleSet-level strategy, the role names, or a role's strategy.
// Reordering roles counts as a change, which only errs on the side of re-validating.
func schedulingConfigChanged(oldSpec, newSpec *orchestrationv1alpha1.RoleSetSpec) bool {
	if oldSpec == nil || newSpec == nil {
		return oldSpec != newSpec
	}
	if !equality.Semantic.DeepEqual(oldSpec.SchedulingStrategy, newSpec.SchedulingStrategy) {
		return true
	}
	if len(oldSpec.Roles) != len(newSpec.Roles) {
		return true
	}
	for i := range newSpec.Roles {
		if oldSpec.Roles[i].Name != newSpec.Roles[i].Name ||
			!equality.Semantic.DeepEqual(oldSpec.Roles[i].SchedulingStrategy, newSpec.Roles[i].SchedulingStrategy) {
			return true
		}
	}
	return false
}

// validateStormServiceMode rejects declared mode combinations that cannot be satisfied.
//
// Pooled mode runs a single RoleSet and scales roles through spec.template.spec.roles[],
// so a replica count above one is ambiguous. Replica mode derives its update path from
// the mode (rolling replacement of RoleSets), so combining it with a declared
// spec.updateStrategy.type of InPlaceUpdate is contradictory; InPlaceUpdate can only be
// user-written because the CRD defaults the type to RollingUpdate. The reverse
// combination (Pooled with RollingUpdate) is not rejected: a RollingUpdate value is
// indistinguishable from the CRD default, so the controller resolves it in favor of the
// declared mode instead (see EffectiveUpdateStrategyType in the stormservice controller).
//
// Only an explicitly declared spec.mode is checked, so objects that rely on the inferred
// mode keep scaling spec.replicas and choosing updateStrategy.type freely. The /scale
// subresource validator reuses this helper so kubectl scale and HPA follow the same rule.
func validateStormServiceMode(stormService *orchestrationv1alpha1.StormService) error {
	switch stormService.Spec.Mode {
	case orchestrationv1alpha1.StormServicePooledMode:
		if stormService.Spec.Replicas != nil && *stormService.Spec.Replicas > 1 {
			return fmt.Errorf("StormService in %s mode must not set spec.replicas > 1 (got %d); scale roles through spec.template.spec.roles[].replicas instead",
				orchestrationv1alpha1.StormServicePooledMode, *stormService.Spec.Replicas)
		}
	case orchestrationv1alpha1.StormServiceReplicaMode:
		if stormService.Spec.UpdateStrategy.Type == orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType {
			return fmt.Errorf("StormService in %s mode must not set spec.updateStrategy.type %s; use %s, or leave spec.mode unset to keep in-place updates for inferred replica mode",
				orchestrationv1alpha1.StormServiceReplicaMode, orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType, orchestrationv1alpha1.RollingUpdateStormServiceStrategyType)
		}
	}
	return nil
}

// validateStormServiceSchedulingStrategy rejects gang scheduling configurations in the RoleSet
// template that the RoleSet controller or Volcano cannot honour.
func validateStormServiceSchedulingStrategy(stormService *orchestrationv1alpha1.StormService) error {
	allErrs := validateRoleSetSchedulingStrategy(stormService.Spec.Template.Spec, field.NewPath("spec", "template", "spec"))
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: orchestrationv1alpha1.GroupVersion.Group, Kind: orchestrationv1alpha1.StormServiceKind},
		stormService.Name,
		allErrs,
	)
}

// validateRoleSetSchedulingStrategy validates the scheduling strategies declared on a RoleSet spec.
// It takes the RoleSetSpec and its field path rather than the StormService so that a RoleSet
// webhook can reuse it.
//
// The RoleSet-level strategy and the Role-level strategies are mutually exclusive: the RoleSet
// controller points every pod at the RoleSet-level PodGroup and then re-points PodSet pods at the
// Role-level PodGroup, so setting both splits one gang across two PodGroups that can never both
// be satisfied.
func validateRoleSetSchedulingStrategy(spec *orchestrationv1alpha1.RoleSetSpec, specPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if spec == nil {
		return allErrs
	}

	roleNames := sets.New[string]()
	for _, role := range spec.Roles {
		roleNames.Insert(role.Name)
	}

	roleSetStrategyPath := specPath.Child("schedulingStrategy")
	if spec.SchedulingStrategy != nil {
		allErrs = append(allErrs, validateVolcanoSchedulingStrategy(
			spec.SchedulingStrategy.VolcanoSchedulingStrategy, roleNames, roleSetStrategyPath.Child("volcanoSchedulingStrategy"))...)
	}

	for i, role := range spec.Roles {
		if role.SchedulingStrategy == nil {
			continue
		}
		roleStrategyPath := specPath.Child("roles").Index(i).Child("schedulingStrategy")
		if spec.SchedulingStrategy != nil {
			allErrs = append(allErrs, field.Forbidden(roleStrategyPath,
				fmt.Sprintf("must not be set together with %s; RoleSet-level and Role-level scheduling strategies are mutually exclusive", roleSetStrategyPath)))
		}
		// A Role-level PodGroup only ever contains pods of that role.
		allErrs = append(allErrs, validateVolcanoSchedulingStrategy(
			role.SchedulingStrategy.VolcanoSchedulingStrategy, sets.New(role.Name), roleStrategyPath.Child("volcanoSchedulingStrategy"))...)
	}
	return allErrs
}

// validateVolcanoSchedulingStrategy checks the Volcano PodGroup fields that Volcano itself does not
// validate. taskNames holds the role names that minTaskMember keys may refer to.
func validateVolcanoSchedulingStrategy(strategy *orchestrationv1alpha1.VolcanoSchedulingStrategySpec, taskNames sets.Set[string], path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if strategy == nil {
		return allErrs
	}

	minMemberPath := path.Child("minMember")
	if strategy.MinMember <= 0 {
		allErrs = append(allErrs, field.Invalid(minMemberPath, strategy.MinMember, "must be greater than 0 when Volcano gang scheduling is configured"))
	}

	taskKeys := make([]string, 0, len(strategy.MinTaskMember))
	for key := range strategy.MinTaskMember {
		taskKeys = append(taskKeys, key)
	}
	sort.Strings(taskKeys) // deterministic error ordering

	var minTaskMemberSum int64
	taskMembersValid := true
	for _, key := range taskKeys {
		value := strategy.MinTaskMember[key]
		keyPath := path.Child("minTaskMember").Key(key)
		if value <= 0 {
			taskMembersValid = false
			allErrs = append(allErrs, field.Invalid(keyPath, value, "must be greater than 0"))
		}
		if !taskNames.Has(key) {
			allErrs = append(allErrs, field.Invalid(keyPath, key,
				fmt.Sprintf("must match a role name; valid names are: %s", strings.Join(sets.List(taskNames), ", "))))
		}
		minTaskMemberSum += int64(value)
	}

	// Volcano skips every per-task check when minMember is smaller than the sum of minTaskMember,
	// which silently disables the per-role guarantees the user asked for.
	if strategy.MinMember > 0 && taskMembersValid && int64(strategy.MinMember) < minTaskMemberSum {
		allErrs = append(allErrs, field.Invalid(minMemberPath, strategy.MinMember,
			fmt.Sprintf("must not be smaller than the sum of minTaskMember values (%d); Volcano ignores minTaskMember otherwise", minTaskMemberSum)))
	}
	return allErrs
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *StormServiceCustomDefaulter) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	// TODO(user): fill in your validation logic upon object deletion.
	return nil, nil
}
