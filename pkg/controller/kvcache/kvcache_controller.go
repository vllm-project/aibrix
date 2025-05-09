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
	"fmt"

	"github.com/vllm-project/aibrix/pkg/constants"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller/kvcache/backends"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
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
		Backends: map[string]backends.BackendReconciler{
			constants.KVCacheBackendVineyard:    backends.NewVineyardReconciler(mgr.GetClient()),
			constants.KVCacheBackendHPKV:        backends.NewDistributedReconciler(mgr.GetClient(), constants.KVCacheBackendHPKV),
			constants.KVCacheBackendInfinistore: backends.NewDistributedReconciler(mgr.GetClient(), constants.KVCacheBackendInfinistore),
		},
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
		For(&orchestrationv1alpha1.KVCache{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(podWithLabelFilter(constants.KVCacheLabelKeyIdentifier))).
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
	Backends      map[string]backends.BackendReconciler
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=kvcaches/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/exec,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;delete;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list

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

	backend := getKVCacheBackendFromMetadata(kvCache)
	handler, ok := r.Backends[backend]
	if !ok {
		klog.Warningf("unsupported backend %s", backend)
		return ctrl.Result{}, fmt.Errorf("unsupported backend: %s", backend)
	}

	return handler.Reconcile(ctx, kvCache)
}

// SetupWithManager sets up the controller with the Manager.
func (r *KVCacheReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.KVCache{}).
		Complete(r)
}

// getKVCacheBackendFromMetadata returns the backend based on labels and annotations with fallback logic.
func getKVCacheBackendFromMetadata(kv *orchestrationv1alpha1.KVCache) string {
	backend := kv.Annotations[constants.KVCacheLabelKeyBackend]
	if backend != "" {
		if isValidKVCacheBackend(backend) {
			return backend
		}

		// TODO: Move validation logic to webhook.
		// invalid value provided, fall back to default backend
		return constants.KVCacheBackendDefault
	}

	// provide the compatibility for distributed, centralized mode.
	mode := kv.Annotations[constants.KVCacheAnnotationMode]
	switch mode {
	case "distributed":
		return constants.KVCacheBackendInfinistore
	case "centralized":
		return constants.KVCacheBackendVineyard
	default:
		return constants.KVCacheBackendDefault
	}
}

// isValidKVCacheBackend returns true if the backend is one of the supported backends.
func isValidKVCacheBackend(b string) bool {
	switch b {
	case constants.KVCacheBackendVineyard, constants.KVCacheBackendHPKV, constants.KVCacheBackendInfinistore:
		return true
	default:
		return false
	}
}
