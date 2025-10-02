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

package podautoscaler

import (
	"context"
	"fmt"
	"strings"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/vllm-project/aibrix/pkg/controller/constants"
)

const AutoscalingStormServiceModeAnnotationKey = "autoscaling.aibrix.ai/storm-service-mode"

// WorkloadScale provides scaling operations for different workload types.
// It provides the mechanism to get/set replica counts on workload resources,
// while AutoScaler provides the intelligence to compute desired replica counts.
// The interface is stateless - all methods take PodAutoscaler as a parameter.
type WorkloadScale interface {
	// Validate checks if the target is valid and scalable
	Validate(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error

	// GetCurrentReplicasFromScale extracts the current replica count from a scale object
	GetCurrentReplicasFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured) (int32, error)

	// SetDesiredReplicas updates the replica count
	SetDesiredReplicas(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, replicas int32) error

	// GetPodSelectorFromScale extracts the label selector from an existing scale object
	// For role-level scaling, it adds the role label requirement
	// This avoids re-fetching the scale object when the controller already has it
	GetPodSelectorFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured) (labels.Selector, error)
}

// workloadScale is a stateless implementation of WorkloadScale
type workloadScale struct {
	client     client.Client
	restMapper meta.RESTMapper
}

// NewWorkloadScale creates a stateless WorkloadScale implementation
func NewWorkloadScale(
	client client.Client,
	restMapper meta.RESTMapper,
) WorkloadScale {
	return &workloadScale{
		client:     client,
		restMapper: restMapper,
	}
}

func (s *workloadScale) Validate(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error {
	// Role-level scaling validation
	if pa.Spec.SubTargetSelector != nil {
		return s.validateRoleScaling(ctx, pa)
	}

	// For generic scaling, just verify the resource exists
	// We don't need to validate /scale subresource anymore
	return nil
}

func (s *workloadScale) validateRoleScaling(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error {
	ref := pa.Spec.ScaleTargetRef
	if ref.Kind != "StormService" {
		return fmt.Errorf("subTargetSelector only supported for StormService, got %q", ref.Kind)
	}

	if pa.Spec.SubTargetSelector.RoleName == "" {
		return fmt.Errorf("subTargetSelector.roleName must be set")
	}

	ns := ref.Namespace
	if ns == "" {
		ns = pa.Namespace
	}

	ss := &orchestrationv1alpha1.StormService{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, ss); err != nil {
		return fmt.Errorf("failed to get StormService: %w", err)
	}

	if ss.Spec.Template.Spec == nil {
		return fmt.Errorf("StormService template.spec is nil")
	}

	// Check if role exists
	for _, role := range ss.Spec.Template.Spec.Roles {
		if role.Name == pa.Spec.SubTargetSelector.RoleName {
			return nil
		}
	}

	return fmt.Errorf("role %q not found in StormService %s", pa.Spec.SubTargetSelector.RoleName, ref.Name)
}

func (s *workloadScale) GetCurrentReplicasFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured) (int32, error) {
	// Role-level scaling
	if s.isStormServiceWorkload(scale) && pa.Spec.SubTargetSelector != nil {
		return s.getCurrentReplicasForRole(ctx, pa)
	}

	// Generic scaling - extract from spec.replicas
	currentReplicasInt64, found, err := unstructured.NestedInt64(scale.Object, "spec", "replicas")
	if !found {
		return 0, fmt.Errorf("the 'replicas' field was not found in the scale object")
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get 'replicas' from scale: %w", err)
	}
	return int32(currentReplicasInt64), nil
}

func (s *workloadScale) getCurrentReplicasForRole(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) (int32, error) {
	ref := pa.Spec.ScaleTargetRef
	ns := ref.Namespace
	if ns == "" {
		ns = pa.Namespace
	}

	ss := &orchestrationv1alpha1.StormService{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, ss); err != nil {
		return 0, err
	}

	// replica mode, return the replicas directly
	// we can not easily use `*ss.Spec.Replicas > 1` as condition since 1 could be pool or replica both case under autoscaling scenarios
	if pa.Annotations[AutoscalingStormServiceModeAnnotationKey] == "replica" {
		return *ss.Spec.Replicas, nil
	}

	roleName := pa.Spec.SubTargetSelector.RoleName
	for _, roleStatus := range ss.Status.RoleStatuses {
		if roleStatus.Name == roleName {
			return roleStatus.Replicas, nil
		}
	}

	// Fallback to spec
	if ss.Spec.Template.Spec == nil {
		return 0, fmt.Errorf("stormservice %s/%s template.spec is nil", ns, ref.Name)
	}

	for _, role := range ss.Spec.Template.Spec.Roles {
		if role.Name == roleName {
			if role.Replicas != nil {
				return *role.Replicas, nil
			}
			return 0, nil
		}
	}

	return 0, fmt.Errorf("role %s not found", roleName)
}

func (s *workloadScale) SetDesiredReplicas(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, replicas int32) error {
	// Role-level scaling
	if pa.Spec.SubTargetSelector != nil {
		return s.setDesiredReplicasForRole(ctx, pa, replicas)
	}

	// Generic scaling - use RetryOnConflict to handle concurrent updates
	ref := pa.Spec.ScaleTargetRef
	ns := ref.Namespace
	if ns == "" {
		ns = pa.Namespace
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Parse API version
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return fmt.Errorf("invalid apiVersion %q: %w", ref.APIVersion, err)
		}

		// Create unstructured object for the resource
		scale := &unstructured.Unstructured{}
		scale.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gv.Group,
			Version: gv.Version,
			Kind:    ref.Kind,
		})

		// Get current resource
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, scale); err != nil {
			return err
		}

		// Update replicas field
		if err := unstructured.SetNestedField(scale.Object, int64(replicas), "spec", "replicas"); err != nil {
			return fmt.Errorf("failed to set replicas field: %w", err)
		}

		// Update the resource
		// Note: we have choice to use scale api, but it requires /scale RBAC, to simplify the scenario, let's use current way
		if err := s.client.Update(ctx, scale); err != nil {
			return err
		}

		klog.InfoS("Scaled resource", "kind", ref.Kind, "name", ref.Name, "ns", ns, "replicas", replicas)
		return nil
	})
}

func (s *workloadScale) setDesiredReplicasForRole(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, replicas int32) error {
	ref := pa.Spec.ScaleTargetRef
	ns := ref.Namespace
	if ns == "" {
		ns = pa.Namespace
	}
	roleName := pa.Spec.SubTargetSelector.RoleName

	// Use RetryOnConflict to handle concurrent updates
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &orchestrationv1alpha1.StormService{}
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, cur); err != nil {
			return err
		}
		upd := cur.DeepCopy()
		// TODO: tricky part. it's hard to know replica=1 is pooling or replica mode, we can use autoscaling to limit it.
		// we can extract the method and fallback to ss object annotation to check as well.
		if pa.Annotations[AutoscalingStormServiceModeAnnotationKey] == "replica" {
			upd.Spec.Replicas = ptr.To(replicas)
			return s.client.Patch(ctx, upd, client.MergeFrom(cur))
		}
		// handle pool mode
		if upd.Spec.Template.Spec == nil {
			return fmt.Errorf("template.spec is nil")
		}
		found := false
		for i := range upd.Spec.Template.Spec.Roles {
			if upd.Spec.Template.Spec.Roles[i].Name == roleName {
				upd.Spec.Template.Spec.Roles[i].Replicas = ptr.To(replicas)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("role %q not found", roleName)
		}
		return s.client.Patch(ctx, upd, client.MergeFrom(cur))
	})
}

func (s *workloadScale) getPodSelectorForRole(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) (labels.Selector, error) {
	ref := pa.Spec.ScaleTargetRef
	ns := ref.Namespace
	if ns == "" {
		ns = pa.Namespace
	}

	ss := &orchestrationv1alpha1.StormService{}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, ss); err != nil {
		return nil, err
	}
	// Note: it's possible user just configure StormService without role name, in that case, we just aggregate all the pods.
	labelSelector := labels.SelectorFromSet(labels.Set{
		constants.StormServiceNameLabelKey: ss.Name,
	})

	if pa.Spec.SubTargetSelector != nil && pa.Spec.SubTargetSelector.RoleName != "" {
		req, err := labels.NewRequirement(constants.RoleNameLabelKey, selection.Equals, []string{pa.Spec.SubTargetSelector.RoleName})
		if err != nil {
			return nil, err
		}
		return labelSelector.Add(*req), nil
	}

	return labelSelector, nil
}

func (s *workloadScale) GetPodSelectorFromScale(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured) (labels.Selector, error) {
	// For role-level scaling, get StormService and add role requirement
	isStormService := s.isStormServiceWorkload(scale)
	if isStormService {
		return s.getPodSelectorForRole(ctx, pa)
	}

	// For generic scaling, extract from scale object
	// Try status.selector first (string format used by /scale subresource)
	statusSelector, found, err := unstructured.NestedString(scale.Object, "status", "selector")
	if err == nil && found && strings.TrimSpace(statusSelector) != "" {
		return labels.Parse(statusSelector)
	}

	// Try spec.selector (LabelSelector format)
	selectorMap, found, err := unstructured.NestedMap(scale.Object, "spec", "selector")
	if err != nil {
		return nil, fmt.Errorf("failed to get 'spec.selector' from scale: %w", err)
	}
	if !found {
		// No selector found, return error
		return nil, fmt.Errorf("scale object %q is missing spec.selector", scale.GetName())
	}

	// Convert selectorMap to a *metav1.LabelSelector object
	selector := &metav1.LabelSelector{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(selectorMap, selector)
	if err != nil {
		return nil, fmt.Errorf("failed to convert 'spec.selector' to LabelSelector: %w", err)
	}

	labelsSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to convert LabelSelector to labels.Selector: %w", err)
	}

	return labelsSelector, nil
}

func (s *workloadScale) isStormServiceWorkload(scale *unstructured.Unstructured) bool {
	isStormService := false
	if scale.GetAPIVersion() == "orchestration.aibrix.ai/v1alpha1" && scale.GetKind() == "StormService" {
		isStormService = true
	}
	return isStormService
}

// isStormServiceWorkload checks if the given scale object is a StormService
