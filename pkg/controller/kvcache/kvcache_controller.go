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

package kvcache

import (
	"context"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	KVCacheLabelKeyIdentifier    = "kvcache.orchestration.aibrix.ai/name"
	KVCacheLabelKeyRole          = "kvcache.orchestration.aibrix.ai/role"
	KVCacheLabelKeyMetadataIndex = "kvcache.orchestration.aibrix.ai/etcd-index"

	KVCacheAnnotationNodeAffinityKey        = "kvcache.orchestration.aibrix.ai/node-affinity-key"
	KVCacheAnnotationNodeAffinityDefaultKey = "machine.cluster.vke.volcengine.com/gpu-name"
	KVCacheAnnotationNodeAffinityGPUType    = "kvcache.orchestration.aibrix.ai/node-affinity-gpu-type"
	KVCacheAnnotationPodAffinityKey         = "kvcache.orchestration.aibrix.ai/pod-affinity-workload"

	KVCacheAnnotationPodAntiAffinity = "kvcache.orchestration.aibrix.ai/pod-anti-affinity"

	// Vineyard, HPKV, InfiniStore
	KVCacheAnnotationMode = "kvcache.orchestration.aibrix.ai/mode"

	KVCacheLabelValueRoleCache    = "cache"
	KVCacheLabelValueRoleMetadata = "metadata"
)

var (
	controllerKind = orchestrationv1alpha1.GroupVersion.WithKind("KVCache")
	controllerName = "kv-cache-controller"
)

// Add creates a new ModelAdapter Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	r, err := newReconciler(mgr, runtimeConfig)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &KVCacheReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor(controllerName),
		RuntimeConfig: runtimeConfig,
	}
	return reconciler, nil
}

func podWithLabelFilter(labelKey string) predicate.Predicate {
	hasLabelAndModelIdentifier := func(labels map[string]string, labelKey string) bool {
		_, exists := labels[labelKey]
		return exists
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasLabelAndModelIdentifier(e.ObjectNew.GetLabels(), labelKey)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey)
		},
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&orchestrationv1alpha1.KVCache{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(podWithLabelFilter(KVCacheLabelKeyIdentifier))).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add kv-cache-controller")
	return nil
}

// KVCacheReconciler reconciles a KVCache object
type KVCacheReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	RuntimeConfig config.RuntimeConfig
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch

// Reconcile reconciles a KVCache to desired state.
func (r *KVCacheReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the KVCache instance
	kvCache := &orchestrationv1alpha1.KVCache{}
	err := r.Get(ctx, req.NamespacedName, kvCache)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Extract the deployment mode for distributed KV Cache
	// By default it's still centrailized,
	mode := kvCache.ObjectMeta.Annotations[KVCacheAnnotationMode]
	switch mode {
	case "distributed":
		return r.reconcileDistributedMode(ctx, kvCache)
	case "centralized":
	default:
		return r.reconcileCentralizedMode(ctx, kvCache)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KVCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.KVCache{}).
		Complete(r)
}

func (r *KVCacheReconciler) ReconcilePodObject(ctx context.Context, desired *corev1.Pod) error {
	found := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
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

func (r *KVCacheReconciler) ReconcileDeploymentObject(ctx context.Context, desired *appsv1.Deployment) error {
	found := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
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

func (r *KVCacheReconciler) ReconcileServiceObject(ctx context.Context, service *corev1.Service) error {
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: service.Name, Namespace: service.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
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
