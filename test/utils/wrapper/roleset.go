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

package wrapper

import (
	schedv1alpha1 "github.com/kubewharf/godel-scheduler-api/pkg/apis/scheduling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// RoleSetWrapper wraps core RoleSet types to provide a fluent API for test construction.
type RoleSetWrapper struct {
	roleset orchestrationapi.RoleSet
}

// MakeRoleSet creates a new RoleSetWrapper with the given name and namespace.
func MakeRoleSet(name string) *RoleSetWrapper {
	return &RoleSetWrapper{
		roleset: orchestrationapi.RoleSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
		},
	}
}

// Obj returns the pointer to the underlying RoleSet object.
func (w *RoleSetWrapper) Obj() *orchestrationapi.RoleSet {
	return &w.roleset
}

// Name sets the name of the RoleSet.
func (w *RoleSetWrapper) Name(name string) *RoleSetWrapper {
	w.roleset.Name = name
	return w
}

// Namespace sets the namespace of the RoleSet.
func (w *RoleSetWrapper) Namespace(ns string) *RoleSetWrapper {
	w.roleset.Namespace = ns
	return w
}

// UpdateStrategy sets the RoleSet update strategy (Parallel, Sequential, Interleave).
func (w *RoleSetWrapper) UpdateStrategy(strategy orchestrationapi.RoleSetUpdateStrategyType) *RoleSetWrapper {
	w.roleset.Spec.UpdateStrategy = strategy
	return w
}

// SchedulingStrategyPodGroup sets the PodGroup scheduling strategy.
// This allows integration with kube-batchd or volcano for gang scheduling.
func (w *RoleSetWrapper) SchedulingStrategyPodGroup(pgSpec *schedv1alpha1.PodGroupSpec) *RoleSetWrapper {
	if w.roleset.Spec.SchedulingStrategy == nil {
		w.roleset.Spec.SchedulingStrategy = &orchestrationapi.SchedulingStrategy{
			GodelSchedulingStrategy: &orchestrationapi.GodelSchedulingStrategySpec{},
		}
	}
	if pgSpec != nil {
		w.roleset.Spec.SchedulingStrategy.GodelSchedulingStrategy = ptr.To(
			orchestrationapi.GodelSchedulingStrategySpec(*pgSpec))
	}
	return w
}

// WithRole adds a RoleSpec to the RoleSet.
func (w *RoleSetWrapper) WithRole(roleName string, replicas int32, podGroupSize int32,
	template corev1.PodTemplateSpec) *RoleSetWrapper {
	role := orchestrationapi.RoleSpec{
		Name:     roleName,
		Replicas: &replicas,
		Template: template,
		Stateful: false,
		UpdateStrategy: orchestrationapi.RoleUpdateStrategy{
			MaxUnavailable: nil,
			MaxSurge:       nil,
		},
		DisruptionTolerance: orchestrationapi.DisruptionTolerance{
			MaxUnavailable: nil,
		},
	}
	w.roleset.Spec.Roles = append(w.roleset.Spec.Roles, role)
	return w
}

// WithRoleAdvanced allows full customization of a RoleSpec.
func (w *RoleSetWrapper) WithRoleAdvanced(role orchestrationapi.RoleSpec) *RoleSetWrapper {
	w.roleset.Spec.Roles = append(w.roleset.Spec.Roles, role)
	return w
}

// WithRoles replaces all roles in the RoleSet.
func (w *RoleSetWrapper) WithRoles(roles []orchestrationapi.RoleSpec) *RoleSetWrapper {
	w.roleset.Spec.Roles = roles
	return w
}

// Label adds a label to the RoleSet.
func (w *RoleSetWrapper) Label(key, value string) *RoleSetWrapper {
	if w.roleset.Labels == nil {
		w.roleset.Labels = map[string]string{}
	}
	w.roleset.Labels[key] = value
	return w
}

// Annotation adds an annotation to the RoleSet.
func (w *RoleSetWrapper) Annotation(key, value string) *RoleSetWrapper {
	if w.roleset.Annotations == nil {
		w.roleset.Annotations = map[string]string{}
	}
	w.roleset.Annotations[key] = value
	return w
}
