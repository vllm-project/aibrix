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

package podset

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/controller/constants"
	ctrlutil "github.com/vllm-project/aibrix/pkg/controller/util"
	podutil "github.com/vllm-project/aibrix/pkg/utils"
)

const (
	ControllerName  = "podset-controller"
	PodSetFinalizer = "orchestration.aibrix.ai/podset-finalizer"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = orchestrationv1alpha1.SchemeGroupVersion.WithKind("PodSet")

// Add creates a new PodSet Controller and adds it to the Manager with default RBAC.
func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	r, err := newReconciler(mgr, runtimeConfig)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&orchestrationv1alpha1.PodSet{}).
		Owns(&v1.Pod{}).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add podset-controller")
	return nil
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, runtimeConfig config.RuntimeConfig) (reconcile.Reconciler, error) {
	reconciler := &PodSetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		EventRecorder: mgr.GetEventRecorderFor(ControllerName),
	}
	return reconciler, nil
}

// PodSetReconciler reconciles a PodSet object
type PodSetReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

//+kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=podsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=podsets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=podsets/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *PodSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(4).InfoS("Reconciling PodSet", "podset", req.NamespacedName)

	podSet := &orchestrationv1alpha1.PodSet{}
	if err := r.Get(ctx, req.NamespacedName, podSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !podSet.DeletionTimestamp.IsZero() {
		return r.finalizePodSet(ctx, podSet)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(podSet, PodSetFinalizer) {
		controllerutil.AddFinalizer(podSet, PodSetFinalizer)
		return ctrl.Result{}, r.Update(ctx, podSet)
	}

	// Reconcile pods
	if err := r.reconcilePods(ctx, podSet); err != nil {
		r.EventRecorder.Eventf(podSet, v1.EventTypeWarning, "ReconcileError", "Failed to reconcile pods: %v", err)
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, podSet); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PodSetReconciler) reconcilePods(ctx context.Context, podSet *orchestrationv1alpha1.PodSet) error {
	// Get existing pods
	podList, err := r.getPodsForPodSet(ctx, podSet)
	if err != nil {
		return fmt.Errorf("failed to get pods for PodSet: %w", err)
	}

	activePods := filterActivePods(podList.Items)
	currentPodCount := len(activePods)
	desiredPodCount := int(podSet.Spec.PodGroupSize)

	// The current logic for creating missing pods assumes that existing pods have contiguous indices starting from
	// 0 (i.e., 0, 1, ..., currentPodCount-1). If a pod with a lower index is deleted for some reason,
	// this logic could attempt to create a pod with an index that already exists, or it won't fill the gap.
	// TODO:
	// 1. Identify all indices of existing active pods.
	// 2. Determine which indices in the range [0, desiredPodCount-1] are missing.
	// 3. Create pods for the missing indices.
	if currentPodCount < desiredPodCount {
		// Create missing pods
		podsToCreate := desiredPodCount - currentPodCount
		for i := 0; i < podsToCreate; i++ {
			podIndex := currentPodCount + i
			pod, err := r.createPodFromTemplate(podSet, podIndex)
			if err != nil {
				return fmt.Errorf("failed to create pod template: %w", err)
			}

			if err := r.Create(ctx, pod); err != nil {
				return fmt.Errorf("failed to create pod %s: %w", pod.Name, err)
			}
			klog.InfoS("Created pod", "pod", pod.Name, "podset", podSet.Name)
		}
	} else if currentPodCount > desiredPodCount {
		// Delete excess pods
		podsToDelete := currentPodCount - desiredPodCount
		// sort pods by index to ensure deterministic deletion
		sort.Slice(activePods, func(i, j int) bool {
			iIndex, _ := strconv.Atoi(activePods[i].Labels[constants.PodGroupIndexLabelKey])
			jIndex, _ := strconv.Atoi(activePods[j].Labels[constants.PodGroupIndexLabelKey])
			return iIndex < jIndex
		})
		// Delete pods with highest indices first
		for i := 0; i < podsToDelete; i++ {
			podToDelete := activePods[len(activePods)-1-i]
			if err := r.Delete(ctx, &podToDelete); err != nil {
				return fmt.Errorf("failed to delete pod %s: %w", podToDelete.Name, err)
			}
			klog.InfoS("Deleted pod", "pod", podToDelete.Name, "podset", podSet.Name)
		}
	}

	return nil
}

func (r *PodSetReconciler) createPodFromTemplate(podSet *orchestrationv1alpha1.PodSet, podIndex int) (*v1.Pod, error) {
	pod, err := ctrlutil.GetPodFromTemplate(&podSet.Spec.Template, podSet, metav1.NewControllerRef(podSet, controllerKind))
	if err != nil {
		return nil, err
	}

	// Set pod name
	pod.Name = fmt.Sprintf("%s-%d", podSet.Name, podIndex)
	pod.Namespace = podSet.Namespace

	// Add labels
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[constants.PodSetNameLabelKey] = podSet.Name
	pod.Labels[constants.PodGroupIndexLabelKey] = strconv.Itoa(podIndex)

	// inherit podset labels and annotations
	for k, v := range podSet.Labels {
		if _, ok := pod.Labels[k]; !ok {
			pod.Labels[k] = v
		}
	}
	for k, v := range podSet.Annotations {
		if _, ok := pod.Annotations[k]; !ok {
			pod.Annotations[k] = v
		}
	}

	// Add environment variables for pod coordination
	if pod.Spec.Containers != nil {
		for i := range pod.Spec.Containers {
			pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env,
				v1.EnvVar{Name: constants.PodSetNameEnvKey, Value: podSet.Name},
				v1.EnvVar{Name: constants.PodSetIndexEnvKey, Value: strconv.Itoa(podIndex)},
				v1.EnvVar{Name: constants.PodSetSizeEnvKey, Value: strconv.Itoa(int(podSet.Spec.PodGroupSize))},
			)
		}
	}

	return pod, nil
}

func (r *PodSetReconciler) getPodsForPodSet(ctx context.Context, podSet *orchestrationv1alpha1.PodSet) (*v1.PodList, error) {
	requirement, err := labels.NewRequirement(constants.PodSetNameLabelKey, selection.Equals, []string{podSet.Name})
	if err != nil {
		return nil, err
	}
	labelSelector := labels.NewSelector().Add(*requirement)

	podList := &v1.PodList{}
	err = r.List(ctx, podList,
		client.InNamespace(podSet.Namespace),
		client.MatchingLabelsSelector{Selector: labelSelector})
	return podList, err
}

func (r *PodSetReconciler) updateStatus(ctx context.Context, podSet *orchestrationv1alpha1.PodSet) error {
	podList, err := r.getPodsForPodSet(ctx, podSet)
	if err != nil {
		return err
	}

	activePods := filterActivePods(podList.Items)
	readyPods := filterReadyPods(activePods)

	newStatus := podSet.Status.DeepCopy()
	newStatus.TotalPods = int32(len(activePods))
	newStatus.ReadyPods = int32(len(readyPods))

	// Determine phase
	if len(activePods) == 0 {
		newStatus.Phase = orchestrationv1alpha1.PodSetPhasePending
	} else if len(readyPods) == int(podSet.Spec.PodGroupSize) {
		newStatus.Phase = orchestrationv1alpha1.PodSetPhaseReady
	} else if len(readyPods) > 0 {
		newStatus.Phase = orchestrationv1alpha1.PodSetPhaseRunning
	} else {
		newStatus.Phase = orchestrationv1alpha1.PodSetPhasePending
	}

	// For now, skip conditions to avoid compilation issues
	// TODO: Add proper condition management later

	if !apiequality.Semantic.DeepEqual(&podSet.Status, newStatus) {
		podSet.Status = *newStatus
		return r.Status().Update(ctx, podSet)
	}

	return nil
}

func (r *PodSetReconciler) finalizePodSet(ctx context.Context, podSet *orchestrationv1alpha1.PodSet) (ctrl.Result, error) {
	// Delete all pods
	podSetList, err := r.getPodsForPodSet(ctx, podSet)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(podSetList.Items) > 0 {
		for _, pod := range podSetList.Items {
			if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("failed to delete pod %s: %w", pod.Name, err)
			}
		}
		// Wait for pods to be deleted
		return ctrl.Result{Requeue: true}, nil
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(podSet, PodSetFinalizer)
	return ctrl.Result{}, r.Update(ctx, podSet)
}

// Helper functions
func filterActivePods(pods []v1.Pod) []v1.Pod {
	var active []v1.Pod
	for _, pod := range pods {
		if pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
			active = append(active, pod)
		}
	}
	return active
}

func filterReadyPods(pods []v1.Pod) []v1.Pod {
	var ready []v1.Pod
	for _, pod := range pods {
		if podutil.IsPodReady(&pod) {
			ready = append(ready, pod)
		}
	}
	return ready
}
