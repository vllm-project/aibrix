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

	rayclusterv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller/util/expectation"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	controllerName                              = "raycluster-replicaset-controller"
	defaultRequeueDurationForWaitingExpectation = 5 * time.Second
	controllerKind                              = orchestrationv1alpha1.GroupVersion.WithKind("RayClusterReplicaSet")
)

// Add first validates that the required Ray CRD (e.g., "rayclusters.ray.io") exists in the cluster.
// If the CRD is not found, the function fails early with an error.
// If the CRD exists, this function creates a new RayClusterReplicaSet Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	// Check if the CRD exists. If not, fail directly.
	crdName := "rayclusters.ray.io"
	if err := checkCRDExists(mgr.GetClient(), crdName); err != nil {
		return fmt.Errorf("failed to validate CRD: %v", err)
	}

	r, err := newReconciler(mgr, runtimeConfig)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// checkCRDExists checks if the specified CRD exists in the cluster.
func checkCRDExists(c client.Client, crdName string) error {
	gvk := schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	}

	// Create an unstructured object to represent the CRD.
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(gvk)
	crd.SetName(crdName)

	err := c.Get(context.TODO(), client.ObjectKey{Name: crdName}, crd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("CRD %q not found. Please ensure %q is installed", crdName, crdName)
		}
		return fmt.Errorf("error checking CRD %q: %v", crdName, err)
	}
	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &RayClusterReplicaSetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorderFor(controllerName),
		Expectations:  expectation.NewControllerExpectations(),
		RuntimeConfig: runtimeConfig,
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
	Expectations  expectation.ControllerExpectationsInterface
	RuntimeConfig config.RuntimeConfig
}

// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterreplicasets/finalizers,verbs=update
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ray.io,resources=rayclusters/finalizers,verbs=update

// Reconcile method moves the RayClusterReplicaSet to desired State
func (r *RayClusterReplicaSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	replicaset := &orchestrationv1alpha1.RayClusterReplicaSet{}
	rsKey := req.NamespacedName.String()
	if err := r.Get(ctx, req.NamespacedName, replicaset); err != nil {
		klog.ErrorS(err, "unable to fetch object", "RayClusterReplicaSet", req.NamespacedName)
		// deletion & recreation will find the exception exist, even it always return true (result is same) but the logic is different.
		// let's remove the expectation in this case.
		r.Expectations.DeleteExpectations(rsKey)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rsNeedsSync := r.Expectations.SatisfiedExpectations(rsKey)
	// fetch current ray cluster associated with this replicaset
	rayClusterList := &rayclusterv1.RayClusterList{}
	ListOps := []client.ListOption{
		client.InNamespace(replicaset.Namespace),
		client.MatchingLabels(replicaset.Spec.Selector.MatchLabels),
	}

	if err := r.Client.List(ctx, rayClusterList, ListOps...); err != nil {
		klog.ErrorS(err, "unable to list ray clusters")
		return ctrl.Result{}, err
	}

	// ignore inactive clusters.
	filteredClusters := filterActiveClusters(rayClusterList.Items)

	// manage replica differences
	var scaleError error
	if rsNeedsSync && replicaset.DeletionTimestamp == nil {
		currentReplicas := int32(len(filteredClusters))

		// Determine the scaling operation (scale up or down)
		desiredReplicas := *replicaset.Spec.Replicas
		if currentReplicas < desiredReplicas {
			diff := desiredReplicas - currentReplicas
			_ = r.Expectations.ExpectCreations(rsKey, int(diff))
			scaleError = r.scaleUp(ctx, replicaset, int(diff))
		} else if currentReplicas > desiredReplicas {
			diff := currentReplicas - desiredReplicas
			_ = r.Expectations.ExpectDeletions(rsKey, int(diff))
			scaleError = r.scaleDown(ctx, replicaset, filteredClusters, int(diff))
		}
	}

	// status update if necessary
	newStatus := calculateStatus(replicaset, filteredClusters, scaleError)
	if err := r.updateReplicaSetStatus(ctx, replicaset, newStatus, rsKey); err != nil {
		return reconcile.Result{}, err
	}

	return ctrl.Result{}, nil
}

// scaleUp handles RayCluster creation logic when scaling up
func (r *RayClusterReplicaSetReconciler) scaleUp(ctx context.Context, replicaset *orchestrationv1alpha1.RayClusterReplicaSet, diff int) error {
	for i := 0; i < diff; i++ {
		newCluster := constructRayCluster(replicaset)
		if err := r.Create(ctx, newCluster); err != nil {
			return fmt.Errorf("failed to create pod: %w", err)
		}
		r.Expectations.CreationObserved(types.NamespacedName{Namespace: replicaset.Namespace, Name: replicaset.Name}.String())
	}
	return nil
}

// scaleDown handles RayCluster deletion logic when scaling down
func (r *RayClusterReplicaSetReconciler) scaleDown(ctx context.Context, replicaset *orchestrationv1alpha1.RayClusterReplicaSet, clusters []rayclusterv1.RayCluster, diff int) error {
	var wg sync.WaitGroup
	errCh := make(chan error, diff)
	// limit the number of concurrent deletions goroutine.
	// todo: make this configurable?
	const maxConcurrency = 10
	semaphore := make(chan struct{}, maxConcurrency)

	for i := 0; i < diff; i++ {
		cluster := clusters[i]
		semaphore <- struct{}{}
		wg.Add(1)
		go func(cluster rayclusterv1.RayCluster) {
			defer wg.Done()
			defer func() { <-semaphore }() // release the semaphore
			if err := r.Delete(ctx, &cluster); err != nil {
				if !apierrors.IsNotFound(err) {
					errCh <- fmt.Errorf("failed to delete pod: %w", err)
				}
			}
			r.Expectations.DeletionObserved(types.NamespacedName{Namespace: replicaset.Namespace, Name: replicaset.Name}.String())
		}(cluster)
	}

	wg.Wait()
	close(errCh)

	// aggregate errors if any.
	var aggregatedErrors []error
	for err := range errCh {
		aggregatedErrors = append(aggregatedErrors, err)
	}

	if len(aggregatedErrors) > 0 {
		return fmt.Errorf("encountered %d errors during deletion: %v", len(aggregatedErrors), aggregatedErrors)
	}
	return nil
}

// updateReplicaSetStatus attempts to update the Status.Replicas of the given ReplicaSet, with a single GET/PUT retry.
func (r *RayClusterReplicaSetReconciler) updateReplicaSetStatus(ctx context.Context, rs *orchestrationv1alpha1.RayClusterReplicaSet, newStatus orchestrationv1alpha1.RayClusterReplicaSetStatus, rsKey string) error {
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
	if err := r.Status().Update(ctx, newInstance); err != nil {
		klog.ErrorS(err, "unable to update ReplicaSet status")
		return err
	}

	return nil
}
