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

package stormservice

import (
	"context"
	"time"

	apps "k8s.io/api/apps/v1"
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
	"github.com/vllm-project/aibrix/pkg/controller/util/history"
	utils "github.com/vllm-project/aibrix/pkg/controller/util/orchestration"
	"github.com/vllm-project/aibrix/pkg/controller/util/patch"
)

const (
	ControllerName              = "stormservice-controller"
	DefaultRequeueAfter         = 15 * time.Second
	DefaultRevisionHistoryLimit = 10
	StormServiceFinalizer       = "orchestration.aibrix.ai/stormservice-finalizer"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = orchestrationv1alpha1.SchemeGroupVersion.WithKind(orchestrationv1alpha1.StormServiceKind)

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
		For(&orchestrationv1alpha1.StormService{}).
		Owns(&orchestrationv1alpha1.RoleSet{}).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add stormservice-controller")
	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &StormServiceReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		EventRecorder: mgr.GetEventRecorderFor(ControllerName),
	}
	return reconciler, nil
}

// StormServiceReconciler reconciles a StormService object
type StormServiceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=stormservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=stormservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=stormservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete

func (r *StormServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
	r.logReconcileStart(req.NamespacedName.String(), startTime)
	defer r.logReconcileEnd(req.NamespacedName.String(), startTime)

	stormService := &orchestrationv1alpha1.StormService{}
	if err := r.Get(ctx, req.NamespacedName, stormService); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if stormService.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, stormService)
	}

	if err := r.ensureFinalizer(ctx, stormService); err != nil {
		return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, err
	}

	revisions, err := r.getAndSortRevisions(ctx, stormService)
	if err != nil {
		return ctrl.Result{}, err
	}

	currentRevision, updateRevision, collisionCount, err := r.syncRevisions(ctx, stormService, revisions)
	if err != nil {
		return ctrl.Result{}, err
	}

	requeueAfter, err := r.performSync(ctx, stormService, currentRevision, updateRevision, collisionCount)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.cleanupHistory(ctx, stormService, revisions, currentRevision, updateRevision); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *StormServiceReconciler) logReconcileStart(name string, startTime time.Time) {
	klog.Infof("Started syncing stormservice %s (%v)", name, startTime)
}

func (r *StormServiceReconciler) logReconcileEnd(name string, startTime time.Time) {
	klog.Infof("Finished syncing stormservice %q (%v)", name, time.Since(startTime))
}

func (r *StormServiceReconciler) handleDeletion(ctx context.Context, stormService *orchestrationv1alpha1.StormService) (ctrl.Result, error) {
	done, err := r.finalize(ctx, stormService)
	if err != nil {
		klog.Errorf("stormservice %s/%s finalize failed: %v", stormService.Namespace, stormService.Name, err)
		return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: DefaultRequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *StormServiceReconciler) ensureFinalizer(ctx context.Context, stormService *orchestrationv1alpha1.StormService) error {
	if controllerutil.ContainsFinalizer(stormService, StormServiceFinalizer) {
		return nil
	}

	if err := utils.Patch(ctx, r.Client, stormService, patch.AddFinalizerPatch(stormService, StormServiceFinalizer)); err != nil {
		klog.Errorf("add finalizer failed: %v, stormService %s/%s", err, stormService.Namespace, stormService.Name)
		return err
	}
	return nil
}

func (r *StormServiceReconciler) getAndSortRevisions(ctx context.Context, stormService *orchestrationv1alpha1.StormService) ([]*apps.ControllerRevision, error) {
	revisions, err := r.getControllerRevision(ctx, stormService)
	if err != nil {
		return nil, err
	}
	history.SortControllerRevisions(revisions)
	return revisions, nil
}

func (r *StormServiceReconciler) syncRevisions(ctx context.Context, stormService *orchestrationv1alpha1.StormService, revisions []*apps.ControllerRevision) (*apps.ControllerRevision, *apps.ControllerRevision, int64, error) {
	currentRevision, updateRevision, collisionCount, err := r.syncRevision(ctx, stormService, revisions)
	if err != nil {
		return nil, nil, 0, err
	}
	return currentRevision, updateRevision, int64(collisionCount), err
}

func (r *StormServiceReconciler) performSync(ctx context.Context, stormService *orchestrationv1alpha1.StormService, currentRevision, updateRevision *apps.ControllerRevision, collisionCount int64) (time.Duration, error) {
	return r.sync(ctx, stormService, currentRevision, updateRevision, int32(collisionCount))
}

func (r *StormServiceReconciler) cleanupHistory(ctx context.Context, stormService *orchestrationv1alpha1.StormService, revisions []*apps.ControllerRevision, currentRevision, updateRevision *apps.ControllerRevision) error {
	return r.truncateHistory(ctx, stormService, revisions, currentRevision, updateRevision)
}
