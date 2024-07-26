/*
Copyright 2024.

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
	pa "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	autoscalingv2listers "k8s.io/client-go/listers/autoscaling/v2"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Add creates a new PodAutoscaler Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	reconciler := &PodAutoscalerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	ctrl.NewControllerManagedBy(mgr).
		For(&pa.PodAutoscaler{}).
		Watches(&corev1.Service{}, &handler.EnqueueRequestForObject{}).
		Watches(&discoveryv1.EndpointSlice{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add model-adapter-controller")
	return nil
}

var _ reconcile.Reconciler = &PodAutoscalerReconciler{}

// PodAutoscalerReconciler reconciles a PodAutoscaler object
type PodAutoscalerReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	kubeClient kubernetes.Interface
	hpaLister  autoscalingv2listers.HorizontalPodAutoscalerLister
}

//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodAutoscaler object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *PodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_log := log.FromContext(ctx)
	_log.Info(" ------ PA Call Reconciling Scaler", "request name", req.Name)

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}
