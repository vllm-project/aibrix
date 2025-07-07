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

package roleset

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	orchestrationctrl "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	"github.com/vllm-project/aibrix/pkg/controller/util/patch"
)

const (
	ControllerName            = "roleset-controller"
	RoleSetFinalizer          = "orchestration.aibrix.ai/roleset-finalizer"
	DefaultRequeueAfter       = 15 * time.Second
	PodBurst                  = 500
	PodOperationInitBatchSize = 16
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.RoleSetKind)

// Add creates a new ModelAdapter Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	r, err := newReconciler(mgr, runtimeConfig)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&orchestrationv1alpha1.RoleSet{}).
		Owns(&v1.Pod{}).
		// TODO: Bring PodGroup back later
		//Owns(&schedv1alpha1.PodGroup{}).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add roleset-controller")
	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &RoleSetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		EventRecorder: mgr.GetEventRecorderFor(ControllerName),
	}
	return reconciler, nil
}

// RoleSetReconciler reconciles a RoleSet object
type RoleSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rolesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rolesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rolesets/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch

func (r *RoleSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("Reconciling RoleSet %s", req.NamespacedName.String())
	ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()
	roleSet := &orchestrationv1alpha1.RoleSet{}
	if err := r.Client.Get(ctx, req.NamespacedName, roleSet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if roleSet.DeletionTimestamp != nil {
		if done, err := r.finalize(ctx, roleSet); err != nil {
			klog.Errorf("Reconciling RoleSet %s finalize error %v", req.NamespacedName.String(), err)
			return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, err
		} else if !done {
			klog.Infof("Reconciling RoleSet %s finalize not done yet, reconcile after %v seconds", req.NamespacedName.String(), DefaultRequeueAfter)
			return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, nil
		}
		return ctrl.Result{}, nil
	} else if !controllerutil.ContainsFinalizer(roleSet, RoleSetFinalizer) {
		// add finalizer if not exist
		if err := orchestrationctrl.Patch(ctx, r.Client, roleSet, patch.AddFinalizerPatch(roleSet, RoleSetFinalizer)); err != nil {
			klog.Errorf("Adding RoleSet %s finalizer error %v", req.NamespacedName.String(), err)
			return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, err
		}
		return ctrl.Result{}, nil
	}

	var managedErrors []error

	// TODO: disable pod group at this moment, consider to switch to open source pod group api later.
	// 1. sync pod group
	//if err := r.syncPodGroup(ctx, roleSet, &roleSet.Spec); err != nil {
	//	managedErrors = append(managedErrors, fmt.Errorf("sync pod group error %v", err))
	//}

	// 2. sync pods
	err := r.syncPods(ctx, roleSet)
	if err != nil {
		managedErrors = append(managedErrors, fmt.Errorf("sync pod error %v", err))
	}

	// 3. update roleset status
	status, err := r.calculateStatus(ctx, roleSet, managedErrors)
	if err != nil {
		klog.Infof("roleset %s/%s calculate status error %v", roleSet.Namespace, roleSet.Name, err)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}
	if apiequality.Semantic.DeepEqual(&roleSet.Status, status) {
		return ctrl.Result{}, nil
	}
	roleSet.Status = *status
	if err := orchestrationctrl.UpdateStatus(ctx, r.Scheme, r.Client, roleSet); err != nil {
		klog.Infof("roleset %s/%s update status error %v", roleSet.Namespace, roleSet.Name, err)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}
	return ctrl.Result{}, nil
}
