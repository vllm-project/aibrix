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

package backends

import (
	"context"
	"reflect"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type BackendReconciler interface {
	Reconcile(ctx context.Context, kv *orchestrationv1alpha1.KVCache) (reconcile.Result, error)
}

type BaseReconciler struct {
	client.Client
}

func (r *BaseReconciler) ReconcilePodObject(ctx context.Context, desired *corev1.Pod) error {
	found := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new Pod", "Pod.Namespace", desired.Namespace, "Pod.Name", desired.Name)
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	}

	// Check if the images need to be updated.
	// most Pod fields are mutable, so we just compare image here. We can extends to tolerations or other fields later.
	updateNeeded := false
	for i, container := range found.Spec.Containers {
		if len(desired.Spec.Containers) > i {
			if desired.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Containers[i].Image = desired.Spec.Containers[i].Image
				updateNeeded = true
			}
		}
	}

	if updateNeeded {
		klog.InfoS("Updating Pod", "Pod.Namespace", found.Namespace, "Pod.Name", found.Name)
		return r.Update(ctx, found)
	}

	return nil
}

func (r *BaseReconciler) ReconcileDeploymentObject(ctx context.Context, desired *appsv1.Deployment) error {
	found := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating Deployment", "Name", desired.Name)
		return r.Create(ctx, desired)
	} else if err != nil {
		return err
	} else if needsUpdateDeployment(desired, found) {
		found.Spec = desired.Spec
		klog.InfoS("Updating Deployment", "Name", desired.Name)
		return r.Update(ctx, found)
	}
	return nil
}

func (r *BaseReconciler) ReconcileServiceObject(ctx context.Context, service *corev1.Service) error {
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new Service", "Service.Namespace", service.Namespace, "Service.Name", service.Name)
		return r.Create(ctx, service)
	} else if err != nil {
		return err
	}

	// Update the found object and write the result back if there are any changes
	if needsUpdateService(service, found) {
		found.Spec.Ports = service.Spec.Ports
		found.Spec.Selector = service.Spec.Selector
		found.Spec.Type = service.Spec.Type
		klog.InfoS("Updating Service", "Service.Namespace", found.Namespace, "Service.Name", found.Name)
		return r.Update(ctx, found)
	}

	return nil
}

func (r *BaseReconciler) ReconcileStatefulsetObject(ctx context.Context, sts *appsv1.StatefulSet) error {
	found := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new StatefulSet", "Sts.Namespace", sts.Namespace, "Sts.Name", sts.Name)
		return r.Create(ctx, sts)
	} else if err != nil {
		return err
	}

	// Update the found object and write the result back if there are any changes
	if needsUpdateStatefulset(sts, found) {
		found.Spec = sts.Spec
		klog.InfoS("Updating Statefulset", "Sts.Namespace", found.Namespace, "Sts.Name", found.Name)
		return r.Update(ctx, found)
	}

	return nil
}

func (r *BaseReconciler) reconcileWatcherPodServiceAccount(ctx context.Context, sa *corev1.ServiceAccount) error {
	found := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new ServiceAccount", "SA.Namespace", sa.Namespace, "SA.Name", sa.Name)
		return r.Create(ctx, sa)
	} else if err != nil {
		return err
	}
	return nil
}

func (r *BaseReconciler) reconcileWatcherPodRole(ctx context.Context, role *rbacv1.Role) error {
	found := &rbacv1.Role{}
	err := r.Get(ctx, types.NamespacedName{Name: role.Name, Namespace: role.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new Role", "Role.Namespace", role.Namespace, "Role.Name", role.Name)
		return r.Create(ctx, role)
	} else if err != nil {
		return err
	}
	return nil
}

func (r *BaseReconciler) reconcileWatcherPodRoleBinding(ctx context.Context, rolebinding *rbacv1.RoleBinding) error {
	found := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: rolebinding.Name, Namespace: rolebinding.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		klog.InfoS("Creating a new RoleBinding", "RoleBinding.Namespace", rolebinding.Namespace, "RoleBinding.Name", rolebinding.Name)
		return r.Create(ctx, rolebinding)
	} else if err != nil {
		return err
	}
	return nil
}

// needsUpdateService checks if the service spec of the new service differs from the existing one
func needsUpdateService(service, found *corev1.Service) bool {
	// Compare relevant spec fields
	return !reflect.DeepEqual(service.Spec.Ports, found.Spec.Ports) ||
		!reflect.DeepEqual(service.Spec.Selector, found.Spec.Selector) ||
		service.Spec.Type != found.Spec.Type
}

// needsUpdateDeployment checks if the deployment spec of the new deployment differs from the existing one
// only image and replicas are considered at this moment.
func needsUpdateDeployment(deployment *appsv1.Deployment, found *appsv1.Deployment) bool {
	imageChanged := false
	for i, container := range found.Spec.Template.Spec.Containers {
		if len(deployment.Spec.Template.Spec.Containers) > i {
			if deployment.Spec.Template.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Template.Spec.Containers[i].Image = deployment.Spec.Template.Spec.Containers[i].Image
				imageChanged = true
			}
		}
	}

	return !reflect.DeepEqual(deployment.Spec.Replicas, found.Spec.Replicas) || imageChanged
}

// needsUpdateStatefulset checks if the StatefulSet spec of the new Statefulset differs from the existing one
// only image and replicas are considered at this moment.
func needsUpdateStatefulset(sts *appsv1.StatefulSet, found *appsv1.StatefulSet) bool {
	imageChanged := false
	for i, container := range found.Spec.Template.Spec.Containers {
		if len(sts.Spec.Template.Spec.Containers) > i {
			if sts.Spec.Template.Spec.Containers[i].Image != container.Image {
				// update the image
				found.Spec.Template.Spec.Containers[i].Image = sts.Spec.Template.Spec.Containers[i].Image
				imageChanged = true
			}
		}
	}

	return !reflect.DeepEqual(sts.Spec.Replicas, found.Spec.Replicas) || imageChanged
}

type KVCacheBackend interface {
	Name() string
	ValidateObject(*orchestrationv1alpha1.KVCache) error
	BuildMetadataPod(*orchestrationv1alpha1.KVCache) *corev1.Pod
	BuildMetadataService(*orchestrationv1alpha1.KVCache) *corev1.Service
	BuildWatcherPodServiceAccount(*orchestrationv1alpha1.KVCache) *corev1.ServiceAccount
	BuildWatcherPodRole(*orchestrationv1alpha1.KVCache) *rbacv1.Role
	BuildWatcherPodRoleBinding(*orchestrationv1alpha1.KVCache) *rbacv1.RoleBinding
	BuildWatcherPod(*orchestrationv1alpha1.KVCache) *corev1.Pod
	BuildCacheStatefulSet(*orchestrationv1alpha1.KVCache) *appsv1.StatefulSet
	BuildService(*orchestrationv1alpha1.KVCache) *corev1.Service
}
