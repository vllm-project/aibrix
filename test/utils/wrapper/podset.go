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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationapi "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
)

// PodSetWrapper wraps PodSet types to provide a fluent API for test construction.
type PodSetWrapper struct {
	podSet orchestrationapi.PodSet
}

// MakePodSet creates a new PodSetWrapper with the given name.
func MakePodSet(name string) *PodSetWrapper {
	return &PodSetWrapper{
		podSet: orchestrationapi.PodSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: orchestrationapi.PodSetSpec{
				PodGroupSize: 2, // Default minimum size
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{},
					},
				},
				Stateful: false,
			},
			Status: orchestrationapi.PodSetStatus{
				ReadyPods:  0,
				TotalPods:  0,
				Phase:      orchestrationapi.PodSetPhasePending,
				Conditions: []orchestrationapi.Condition{},
			},
		},
	}
}

// Obj returns the pointer to the underlying PodSet object.
func (w *PodSetWrapper) Obj() *orchestrationapi.PodSet {
	return &w.podSet
}

// Namespace sets the namespace of the PodSet.
func (w *PodSetWrapper) Namespace(ns string) *PodSetWrapper {
	w.podSet.Namespace = ns
	return w
}

// PodGroupSize sets the number of pods in this set.
func (w *PodSetWrapper) PodGroupSize(size int32) *PodSetWrapper {
	w.podSet.Spec.PodGroupSize = size
	return w
}

// Stateful sets whether pods should have stable network identities.
func (w *PodSetWrapper) Stateful(stateful bool) *PodSetWrapper {
	w.podSet.Spec.Stateful = stateful
	return w
}

// PodTemplate sets the pod template for pods in this set.
func (w *PodSetWrapper) PodTemplate(template corev1.PodTemplateSpec) *PodSetWrapper {
	w.podSet.Spec.Template = template
	return w
}

// AddContainer adds a container to the pod template.
func (w *PodSetWrapper) AddContainer(container corev1.Container) *PodSetWrapper {
	w.podSet.Spec.Template.Spec.Containers = append(
		w.podSet.Spec.Template.Spec.Containers,
		container,
	)
	return w
}

// Container creates and adds a container with basic configuration.
func (w *PodSetWrapper) Container(name, image string) *PodSetWrapper {
	container := corev1.Container{
		Name:  name,
		Image: image,
	}
	return w.AddContainer(container)
}

// Labels sets labels on the PodSet and its template.
func (w *PodSetWrapper) Labels(labels map[string]string) *PodSetWrapper {
	if w.podSet.Labels == nil {
		w.podSet.Labels = make(map[string]string)
	}
	if w.podSet.Spec.Template.Labels == nil {
		w.podSet.Spec.Template.ObjectMeta.Labels = make(map[string]string)
	}

	for k, v := range labels {
		w.podSet.Labels[k] = v
		w.podSet.Spec.Template.ObjectMeta.Labels[k] = v
	}
	return w
}

// Annotations sets annotations on the PodSet.
func (w *PodSetWrapper) Annotations(annotations map[string]string) *PodSetWrapper {
	if w.podSet.Annotations == nil {
		w.podSet.Annotations = make(map[string]string)
	}
	if w.podSet.Spec.Template.ObjectMeta.Annotations == nil {
		w.podSet.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	for k, v := range annotations {
		w.podSet.Annotations[k] = v
		w.podSet.Spec.Template.ObjectMeta.Annotations[k] = v
	}
	return w
}

// Status sets the status of the PodSet.
func (w *PodSetWrapper) Status(status orchestrationapi.PodSetStatus) *PodSetWrapper {
	w.podSet.Status = status
	return w
}

// ReadyPods sets the number of ready pods in the status.
func (w *PodSetWrapper) ReadyPods(ready int32) *PodSetWrapper {
	w.podSet.Status.ReadyPods = ready
	return w
}

// TotalPods sets the total number of pods in the status.
func (w *PodSetWrapper) TotalPods(total int32) *PodSetWrapper {
	w.podSet.Status.TotalPods = total
	return w
}

// Phase sets the phase of the PodSet.
func (w *PodSetWrapper) Phase(phase orchestrationapi.PodSetPhase) *PodSetWrapper {
	w.podSet.Status.Phase = phase
	return w
}

// AddCondition adds a condition to the PodSet status.
func (w *PodSetWrapper) AddCondition(condition orchestrationapi.Condition) *PodSetWrapper {
	w.podSet.Status.Conditions = append(w.podSet.Status.Conditions, condition)
	return w
}

// ReadyCondition adds a Ready condition.
func (w *PodSetWrapper) ReadyCondition(status corev1.ConditionStatus, reason, message string) *PodSetWrapper {
	now := metav1.Now()
	condition := orchestrationapi.Condition{
		Type:               orchestrationapi.PodSetReady,
		Status:             status,
		LastTransitionTime: &now,
		Reason:             reason,
		Message:            message,
	}
	return w.AddCondition(condition)
}

// ProgressingCondition adds a Progressing condition.
func (w *PodSetWrapper) ProgressingCondition(status corev1.ConditionStatus, reason, message string) *PodSetWrapper {
	now := metav1.Now()
	condition := orchestrationapi.Condition{
		Type:               orchestrationapi.PodSetProgressing,
		Status:             status,
		LastTransitionTime: &now,
		Reason:             reason,
		Message:            message,
	}
	return w.AddCondition(condition)
}
