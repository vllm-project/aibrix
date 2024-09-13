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

package rayclusterreplicaset

import (
	"context"
	"fmt"
	"sync"
	"time"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	"github.com/aibrix/aibrix/pkg/controller/util/expectation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	orchestrationv1alpha1 "github.com/aibrix/aibrix/api/orchestration/v1alpha1"
)

var (
	controllerName                              = "raycluster-replicaset-controller"
	defaultRequeueDurationForWaitingExpectation = 5 * time.Second
	controllerKind                              = modelv1alpha1.GroupVersion.WithKind("RayClusterReplicaSet")
)

// Add creates a new RayClusterReplicaSet Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	// TODO: check crd exists or not. If not, we should fail here directly without moving forward.
	// This is used to validate whether kuberay is installed now.

	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	reconciler := &RayClusterReplicaSetReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Recorder:     mgr.GetEventRecorderFor(controllerName),
		Expectations: expectation.NewControllerExpectations(),
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		For(&orchestrationv1alpha1.RayClusterReplicaSet{}).
		Owns(&rayclusterv1.RayCluster{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add raycluster-replicaset-controller")
	return err
}

var _ reconcile.Reconciler = &RayClusterReplicaSetReconciler{}

// RayClusterReplicaSetReconciler reconciles a RayClusterReplicaSet object
type RayClusterReplicaSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// A cache raycluster creates/deletes each raycluster replicaset to see
	// We use replicaset namespace/name as an expectation key
	// For example, there is a RayClusterReplicaSet with namespace "aibrix", name "llama7b" and replica 3,
	// We will create the expectation:
	// - "aibrix/llama7b", expects 3 adds.
	Expectations expectation.ControllerExpectationsInterface
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/finalizers,verbs=update
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/finalizers,verbs=update

// Reconcile method moves the RayClusterReplicaSet to desired State
func (r *RayClusterReplicaSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	replicaset := &orchestrationv1alpha1.RayClusterReplicaSet{}
	if err := r.Get(ctx, req.NamespacedName, replicaset); err != nil {
		klog.ErrorS(err, "unable to fetch raycluster-replicaset")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rsKey := req.NamespacedName.String()
	if r.Expectations.SatisfiedExpectations(rsKey) {
		klog.InfoS("Expectations not met, requeuing", "replicaset", rsKey)
		return ctrl.Result{RequeueAfter: defaultRequeueDurationForWaitingExpectation}, nil
	}

	// fetch current ray cluster associated with this replicaset
	rayclusterList := &rayclusterv1.RayClusterList{}
	ListOps := []client.ListOption{
		client.InNamespace(replicaset.Namespace),
		// Note: we simplify the case a little bit and
		// there's no need to follow replicaset's implementation to list all clusters here.
		client.MatchingLabels(replicaset.Spec.Selector.MatchLabels),
	}

	if err := r.Client.List(ctx, rayclusterList, ListOps...); err != nil {
		klog.ErrorS(err, "unable to list rayclusters")
		return ctrl.Result{}, err
	}

	filteredClusters := filterActiveClusters(rayclusterList.Items)
	currentReplicas := int32(len(filteredClusters))

	// Determine the scaling operation (scale up or down)
	desiredReplicas := *replicaset.Spec.Replicas
	var scaleError error
	if currentReplicas < desiredReplicas {
		diff := desiredReplicas - currentReplicas
		_ = r.Expectations.ExpectCreations(rsKey, int(diff))
		scaleError = r.scaleUp(ctx, replicaset, int(diff))
	} else if currentReplicas > desiredReplicas {
		diff := currentReplicas - desiredReplicas
		_ = r.Expectations.ExpectDeletions(rsKey, int(diff))
		scaleError = r.scaleDown(ctx, filteredClusters, int(diff))
	}

	newStatus := calculateStatus(replicaset, filteredClusters, scaleError)
	if err := r.updateReplicaSetStatus(replicaset, newStatus, rsKey); err != nil {
		return reconcile.Result{}, err
	}

	return ctrl.Result{}, nil
}

// scaleUp handles RayCluster creation logic when scaling up
func (r *RayClusterReplicaSetReconciler) scaleUp(ctx context.Context, replicaset *orchestrationv1alpha1.RayClusterReplicaSet, diff int) error {
	for i := 0; i < diff; i++ {
		newCluster := constructRayCluster(replicaset)
		if err := r.Create(ctx, newCluster); err != nil {
			r.Expectations.CreationObserved(replicaset.Name)
			return fmt.Errorf("failed to create pod: %w", err)
		}
	}
	return nil
}

// scaleDown handles RayCluster deletion logic when scaling down
func (r *RayClusterReplicaSetReconciler) scaleDown(ctx context.Context, clusters []rayclusterv1.RayCluster, diff int) error {
	var wg sync.WaitGroup
	errCh := make(chan error, diff)

	for i := 0; i < diff; i++ {
		cluster := clusters[i]
		wg.Add(1)
		go func(cluster rayclusterv1.RayCluster) {
			defer wg.Done()
			if err := r.Delete(ctx, &cluster); err != nil {
				r.Expectations.DeletionObserved(cluster.Name)
				if !apierrors.IsNotFound(err) {
					errCh <- fmt.Errorf("failed to delete pod: %w", err)
				}
			}
		}(cluster)
	}

	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// updateReplicaSetStatus attempts to update the Status.Replicas of the given ReplicaSet, with a single GET/PUT retry.
func (r *RayClusterReplicaSetReconciler) updateReplicaSetStatus(rs *orchestrationv1alpha1.RayClusterReplicaSet, newStatus orchestrationv1alpha1.RayClusterReplicaSetStatus, rsKey string) error {
	// Check if the expectations have been fulfilled for this ReplicaSet
	if !r.Expectations.SatisfiedExpectations(rsKey) {
		klog.V(4).Info("Expectations not yet fulfilled for ReplicaSet, delaying status update", "replicaSet", rsKey)
		return nil
	}

	if same := isStatusSame(rs, newStatus); same {
		return nil
	}

	// Update the observed generation to ensure the status reflects the latest changes
	newStatus.ObservedGeneration = rs.Generation

	// Log the status update
	klog.V(4).Info(fmt.Sprintf("Updating status for %v: %s/%s, ", rs.Kind, rs.Namespace, rs.Name) +
		fmt.Sprintf("replicas %d->%d (need %d), ", rs.Status.Replicas, newStatus.Replicas, *(rs.Spec.Replicas)) +
		fmt.Sprintf("fullyLabeledReplicas %d->%d, ", rs.Status.FullyLabeledReplicas, newStatus.FullyLabeledReplicas) +
		fmt.Sprintf("readyReplicas %d->%d, ", rs.Status.ReadyReplicas, newStatus.ReadyReplicas) +
		fmt.Sprintf("availableReplicas %d->%d, ", rs.Status.AvailableReplicas, newStatus.AvailableReplicas) +
		fmt.Sprintf("observedGeneration %v->%v", rs.Status.ObservedGeneration, newStatus.ObservedGeneration))

	// Update ReplicaSet status if necessary
	newInstance := rs.DeepCopy()
	newInstance.Status = newStatus
	if err := r.Status().Update(context.Background(), newInstance); err != nil {
		klog.ErrorS(err, "unable to update ReplicaSet status")
		return err
	}

	return nil
}
