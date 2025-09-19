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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// DeploymentWrapper wraps core Deployment types to provide a fluent API for test construction.
type DeploymentWrapper struct {
	deployment appsv1.Deployment
}

// Obj returns the pointer to the underlying Deployment object.
func (w *DeploymentWrapper) Obj() *appsv1.Deployment {
	return &w.deployment
}

// MakeDeployment creates a new DeploymentWrapper.
func MakeDeployment(name, namespace string, annotations map[string]string) *DeploymentWrapper {
	if annotations == nil {
		annotations = map[string]string{}
	}

	return &DeploymentWrapper{
		deployment: appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   namespace,
				Annotations: annotations,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](1),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": name},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": name},
						Annotations: annotations,
					},
					Spec: corev1.PodSpec{
						Containers:                    []corev1.Container{},
						RestartPolicy:                 "Always",
						TerminationGracePeriodSeconds: ptr.To[int64](30),
						DNSPolicy:                     "ClusterFirst",
						SecurityContext:               &corev1.PodSecurityContext{},
						SchedulerName:                 "default-scheduler",
					},
				},
				Strategy: appsv1.DeploymentStrategy{
					Type: "RollingUpdate",
					RollingUpdate: &appsv1.RollingUpdateDeployment{
						MaxUnavailable: ptr.To[intstr.IntOrString](intstr.FromString("25%")),
						MaxSurge:       ptr.To[intstr.IntOrString](intstr.FromString("25%")),
					},
				},
				RevisionHistoryLimit:    ptr.To[int32](10),
				ProgressDeadlineSeconds: ptr.To[int32](600),
			},
		},
	}
}

// AddContainer adds a container to the pod template.
func (w *DeploymentWrapper) AddContainer(container corev1.Container) *DeploymentWrapper {
	// Set default value
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = "/dev/termination-log"
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = "File"
	}
	if container.ImagePullPolicy == "" {
		container.ImagePullPolicy = "Always"
	}

	w.deployment.Spec.Template.Spec.Containers = append(
		w.deployment.Spec.Template.Spec.Containers,
		container,
	)
	return w
}

// AddRuntimeContainer adds a runtime container to the pod template.
func (w *DeploymentWrapper) AddRuntimeContainer(name, image string, command []string) *DeploymentWrapper {
	w.AddContainer(
		corev1.Container{
			Name:            name,
			Image:           image,
			Command:         command,
			ImagePullPolicy: "IfNotPresent",
			Ports: []corev1.ContainerPort{
				{
					Name:          "metrics",
					ContainerPort: 8080,
					Protocol:      "TCP",
				},
			},
			Env: []corev1.EnvVar{
				{
					Name:  "INFERENCE_ENGINE",
					Value: "vllm",
				},
				{
					Name:  "INFERENCE_ENGINE_ENDPOINT",
					Value: "http://localhost:8000",
				},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/healthz",
						Port:   intstr.FromInt(8080),
						Scheme: "HTTP",
					},
				},
				InitialDelaySeconds: 3,
				TimeoutSeconds:      1,
				SuccessThreshold:    1,
				FailureThreshold:    3,
				PeriodSeconds:       2,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/ready",
						Port:   intstr.FromInt(8080),
						Scheme: "HTTP",
					},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       10,
				TimeoutSeconds:      1,
				SuccessThreshold:    1,
				FailureThreshold:    3,
			},
		},
	)
	return w
}

// AddModelContainer adds a model container to the pod template.
func (w *DeploymentWrapper) AddModelContainer(name, image string, args []string) *DeploymentWrapper {
	w.AddContainer(
		corev1.Container{
			Name:  name,
			Image: image,
			Args:  args,
			Ports: []corev1.ContainerPort{
				{
					ContainerPort: 8000,
					Protocol:      "TCP",
				},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
		},
	)
	return w
}
