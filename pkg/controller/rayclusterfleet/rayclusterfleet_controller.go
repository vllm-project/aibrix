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

package rayclusterfleet

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/aibrix/aibrix/pkg/controller/util/expectation"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	controllerName                              = "raycluster-fleet-controller"
	defaultRequeueDurationForWaitingExpectation = 5 * time.Second
	controllerKind                              = orchestrationv1alpha1.GroupVersion.WithKind("RayClusterFleet")
)

// Add creates a new RayClusterFleet Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	// TODO: check crd exists or not. If not, we should fail here directly without moving forward.

	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	reconciler := &RayClusterFleetReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor(controllerName),
		Expectation: expectation.NewControllerExpectations(),
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.RayClusterFleet{}).
		Owns(&orchestrationv1alpha1.RayClusterReplicaSet{}).
		Owns(&rayclusterv1.RayCluster{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add model-adapter-controller")
	return err
}

var _ reconcile.Reconciler = &RayClusterFleetReconciler{}

// RayClusterFleetReconciler reconciles a RayClusterFleet object
type RayClusterFleetReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	Expectation expectation.ControllerExpectationsInterface
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterfleets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterfleets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterfleets/finalizers,verbs=update
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/finalizers,verbs=update

// Reconcile method moves the RayClusterFleet to the desired State
func (r *RayClusterFleetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	fleet := &orchestrationv1alpha1.RayClusterFleet{}
	if err := r.Get(ctx, req.NamespacedName, fleet); err != nil {
		if errors.IsNotFound(err) {
			klog.Info("Fleet not found, might have been deleted", "namespace", req.Namespace, "name", req.Name)
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}
	startTime := time.Now()
	klog.V(4).InfoS("Started syncing fleet", "fleet", klog.KRef(fleet.Namespace, fleet.Name), "startTime", startTime)
	defer func() {
		klog.V(4).InfoS("Finished syncing fleet", "fleet", klog.KRef(fleet.Namespace, fleet.Name), "duration", time.Since(startTime))
	}()

	f := fleet.DeepCopy()

	// check whether fleet matches all the ray clusters
	everything := metav1.LabelSelector{}
	if reflect.DeepEqual(f.Spec.Selector, &everything) {
		r.Recorder.Eventf(f, v1.EventTypeWarning, "SelectingAll", "This fleet is selecting all RayClusters. A non-empty selector is required.")
		if f.Status.ObservedGeneration < f.Generation {
			f.Status.ObservedGeneration = f.Generation
			err := r.Status().Update(ctx, f)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	rsList, err := r.getReplicaSetsForFleet(ctx, f)
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterMap, err := r.getRayClusterMapForFleet(f, rsList)
	if err != nil {
		return ctrl.Result{}, err
	}

	if f.DeletionTimestamp != nil {
		return ctrl.Result{}, r.syncStatusOnly(ctx, f, rsList)
	}

	// check whether the fleet is in pause status
	if err := r.checkPausedConditions(ctx, f); err != nil {
		return ctrl.Result{}, err
	}

	if f.Spec.Paused {
		return ctrl.Result{}, r.sync(ctx, f, rsList)
	}

	if getRollbackTo(f) != nil {
		return ctrl.Result{}, r.rollback(ctx, f, rsList)
	}

	scalingEvent, err := r.isScalingEvent(ctx, f, rsList)
	if err != nil {
		return ctrl.Result{}, err
	}

	if scalingEvent {
		return ctrl.Result{}, r.sync(ctx, f, rsList)
	}

	switch f.Spec.Strategy.Type {
	case appsv1.RecreateDeploymentStrategyType:
		return ctrl.Result{}, r.rolloutRecreate(ctx, f, rsList, clusterMap)
	case appsv1.RollingUpdateDeploymentStrategyType:
		return ctrl.Result{}, r.rolloutRolling(ctx, f, rsList)
	}

	return ctrl.Result{}, nil
}

// getReplicaSetsForDeployment uses ControllerRefManager to reconcile
// ControllerRef by adopting and orphaning.
// It returns the list of ReplicaSets that this Deployment should manage.
func (r *RayClusterFleetReconciler) getReplicaSetsForFleet(ctx context.Context, fleet *orchestrationv1alpha1.RayClusterFleet) ([]*orchestrationv1alpha1.RayClusterReplicaSet, error) {
	rsList := &orchestrationv1alpha1.RayClusterReplicaSetList{}

	//  Get ReplicaSets belong to the deployment
	selector, err := metav1.LabelSelectorAsSelector(fleet.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s has invalid label selector: %v", fleet.Namespace, fleet.Name, err)
	}

	err = r.List(ctx, rsList, client.InNamespace(fleet.Namespace), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return nil, err
	}

	ownedReplicaSets := make([]*orchestrationv1alpha1.RayClusterReplicaSet, 0)
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if metav1.IsControlledBy(rs, fleet) {
			ownedReplicaSets = append(ownedReplicaSets, rs)
		}
	}

	return ownedReplicaSets, nil
}

func (r *RayClusterFleetReconciler) getRayClusterMapForFleet(f *orchestrationv1alpha1.RayClusterFleet, rsList []*orchestrationv1alpha1.RayClusterReplicaSet) (map[types.UID][]*rayclusterv1.RayCluster, error) {
	clusterList := &rayclusterv1.RayClusterList{}

	// Get all RayClusters matches the fleet
	selector, err := metav1.LabelSelectorAsSelector(f.Spec.Selector)
	if err != nil {
		return nil, err
	}

	err = r.List(context.TODO(), clusterList, client.InNamespace(f.Namespace), client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return nil, err
	}

	clusterMap := make(map[types.UID][]*rayclusterv1.RayCluster, len(rsList))
	for _, rs := range rsList {
		clusterMap[rs.UID] = []*rayclusterv1.RayCluster{}
	}

	for i := range clusterList.Items {
		cluster := &clusterList.Items[i]
		controllerRef := metav1.GetControllerOf(cluster)
		if controllerRef == nil {
			continue
		}

		if _, ok := clusterMap[controllerRef.UID]; ok {
			clusterMap[controllerRef.UID] = append(clusterMap[controllerRef.UID], cluster)
		}
	}

	return clusterMap, nil
}

func (r *RayClusterFleetReconciler) createNewReplicaSet(ctx context.Context, d *orchestrationv1alpha1.RayClusterFleet) (*orchestrationv1alpha1.RayClusterReplicaSet, error) {
	newRS := &orchestrationv1alpha1.RayClusterReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: d.Name + "-",
			Namespace:    d.Namespace,
			Labels:       d.Spec.Template.Labels,
			Annotations:  make(map[string]string),
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(d, controllerKind),
			},
		},
		Spec: orchestrationv1alpha1.RayClusterReplicaSetSpec{
			Replicas: d.Spec.Replicas,
			Template: d.Spec.Template,
			Selector: d.Spec.Selector,
		},
	}

	if err := r.Create(ctx, newRS); err != nil {
		return nil, err
	}

	return newRS, nil
}
