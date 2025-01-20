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

package podautoscaler

import (
	"context"
	"fmt"
	"time"

	autoscalingv1alpha1 "github.com/aibrix/aibrix/api/autoscaling/v1alpha1"
	"github.com/aibrix/aibrix/pkg/config"
	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/metrics"

	"github.com/aibrix/aibrix/pkg/controller/podautoscaler/scaler"
	podutil "github.com/aibrix/aibrix/pkg/utils"
	podutils "github.com/aibrix/aibrix/pkg/utils"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	DefaultRequeueDuration = 10 * time.Second
)

// Add creates a new PodAutoscaler Controller and adds it to the Manager with default RBAC.
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
	// Instantiate a new PodAutoscalerReconciler with the given manager's client and scheme
	reconciler := &PodAutoscalerReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		EventRecorder:  mgr.GetEventRecorderFor("PodAutoscaler"),
		Mapper:         mgr.GetRESTMapper(),
		resyncInterval: 10 * time.Second, // TODO: this should be override by an environment variable
		eventCh:        make(chan event.GenericEvent),
		AutoscalerMap:  make(map[metrics.NamespaceNameMetric]scaler.Scaler),
		RuntimeConfig:  runtimeConfig,
	}

	return reconciler, nil
}

// for hpa related changes, let's make sure we only enqueue related PodAutoscaler objects
func filterHPAObject(ctx context.Context, object client.Object) []reconcile.Request {
	hpa, ok := object.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		klog.Warningf("unexpected object type: %T, %s:%s, HPA object is expected here.", object, object.GetNamespace(), object.GetName())
		return nil
	}

	// Iterate through ownerReferences to find the PodAutoscaler.
	// if found, enqueue the managing PodAutoscaler object
	for _, ownerRef := range hpa.OwnerReferences {
		if ownerRef.Kind == "PodAutoScaler" && ownerRef.Controller != nil && *ownerRef.Controller {
			// Enqueue the managing PodAutoscaler object
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Namespace: hpa.Namespace,
						Name:      ownerRef.Name,
					},
				},
			}
		}
	}

	// no managed pod autoscaler found, no need to enqueue original object.
	return []reconcile.Request{}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Build raw source for periodical requeue events from event channel
	reconciler := r.(*PodAutoscalerReconciler)
	src := &source.Channel{
		Source: reconciler.eventCh,
	}

	// Create a new controller managed by AIBrix manager, watching for changes to PodAutoscaler objects
	// and HorizontalPodAutoscaler objects.
	err := ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.PodAutoscaler{}).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(filterHPAObject)).
		WatchesRawSource(src, &handler.EnqueueRequestForObject{}).
		Complete(r)

	klog.InfoS("Added AIBrix pod-autoscaler-controller successfully")

	errChan := make(chan error)
	go reconciler.Run(context.Background(), errChan)
	klog.InfoS("Run pod-autoscaler-controller periodical syncs successfully")

	go func() {
		for err := range errChan {
			klog.ErrorS(err, "Run function returned an error")
		}
	}()

	return err
}

var _ reconcile.Reconciler = &PodAutoscalerReconciler{}

// PodAutoscalerReconciler reconciles a PodAutoscaler object
type PodAutoscalerReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	EventRecorder  record.EventRecorder
	Mapper         apimeta.RESTMapper
	AutoscalerMap  map[metrics.NamespaceNameMetric]scaler.Scaler // AutoscalerMap maps each NamespaceNameMetric to its corresponding scaler instance.
	resyncInterval time.Duration
	eventCh        chan event.GenericEvent
	RuntimeConfig  config.RuntimeConfig
}

func (r *PodAutoscalerReconciler) deleteStaleScalerInCache(request types.NamespacedName) {
	// When deleting, we only have access to the Namespace and Name, not other attributes in pa_types.
	// We should scan `AutoscalerMap` and remove the matched objects.
	// Note that due to the OwnerRef, the created HPA object will automatically be removed when AIBrix-HPA is deleted.
	// Therefore, manual deletion of the HPA is not necessary.
	for namespaceNameMetric := range r.AutoscalerMap {
		if namespaceNameMetric.PaNamespace == request.Namespace && namespaceNameMetric.PaName == request.Name {
			// remove matched entry from the map
			klog.InfoS("Delete scaler", "PaName", namespaceNameMetric.PaName,
				"PaNamespace", namespaceNameMetric.PaNamespace,
				"TargetRefNamespace", request.Namespace, "TargetRefName", request.Name)
			delete(r.AutoscalerMap, namespaceNameMetric)
		}
	}
}

//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update

// Reconcile is part of the main Kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state as specified by
// the PodAutoscaler resource. It handles the creation, update, and deletion logic for
// HorizontalPodAutoscalers based on the PodAutoscaler specifications.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *PodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Implement a timeout for the reconciliation process.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	klog.V(4).InfoS("Reconciling PodAutoscaler", "obj", req.NamespacedName)

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &pa); err != nil {
		if errors.IsNotFound(err) {
			r.deleteStaleScalerInCache(req.NamespacedName)
			// Object might have been deleted after reconcile request, clean it and return.
			klog.Infof("PodAutoscaler resource not found. Clean scaler object in memory since object %s must have been deleted", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.ErrorS(err, "Failed to get PodAutoscaler")
		return ctrl.Result{}, err
	}

	if !checkValidAutoscalingStrategy(pa.Spec.ScalingStrategy) {
		// TODO: update status or conditions
		// this is unrecoverable unless user make changes.
		return ctrl.Result{}, nil
	}

	switch pa.Spec.ScalingStrategy {
	case autoscalingv1alpha1.HPA:
		return r.reconcileHPA(ctx, pa)
	case autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA:
		return r.reconcileCustomPA(ctx, pa)
	}

	newStatus := computeStatus(ctx, pa)
	if err := r.updateStatusIfNeeded(ctx, newStatus, &pa); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PodAutoscalerReconciler) Run(ctx context.Context, errChan chan<- error) {
	ticker := time.NewTicker(r.resyncInterval)
	defer ticker.Stop()
	defer close(r.eventCh)

	for {
		select {
		case <-ticker.C:
			klog.V(4).Info("enqueue all autoscalers")
			// periodically sync all autoscaling objects
			if err := r.enqueuePodAutoscalers(ctx); err != nil {
				klog.ErrorS(err, "Failed to enqueue pod autoscalers")
				errChan <- err
			}
		case <-ctx.Done():
			klog.Info("context done, stopping running the loop")
			errChan <- ctx.Err()
			return
		}
	}
}

func (r *PodAutoscalerReconciler) enqueuePodAutoscalers(ctx context.Context) error {
	podAutoscalerLists := &autoscalingv1alpha1.PodAutoscalerList{}
	if err := r.List(ctx, podAutoscalerLists); err != nil {
		return err
	}
	for _, pa := range podAutoscalerLists.Items {
		// Let's operate the queue and just enqueue the object, that should be ok.
		e := event.GenericEvent{
			Object: &pa,
		}
		r.eventCh <- e
	}

	return nil
}

// checkValidAutoscalingStrategy checks if a string is in a list of valid strategies
func checkValidAutoscalingStrategy(strategy autoscalingv1alpha1.ScalingStrategyType) bool {
	validStrategies := []autoscalingv1alpha1.ScalingStrategyType{autoscalingv1alpha1.HPA, autoscalingv1alpha1.APA, autoscalingv1alpha1.KPA}
	for _, v := range validStrategies {
		if v == strategy {
			return true
		}
	}
	return false
}

func computeStatus(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) *autoscalingv1alpha1.PodAutoscalerStatus {
	// take condition into consideration
	// TODO: not implemented
	return nil
}

func (r *PodAutoscalerReconciler) reconcileHPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	// Generate a corresponding HorizontalPodAutoscaler
	hpa := makeHPA(&pa)
	hpaName := types.NamespacedName{
		Name:      hpa.Name,
		Namespace: hpa.Namespace,
	}

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, hpaName, existingHPA)
	if err != nil && errors.IsNotFound(err) {
		// HPA does not exist, create a new one.
		klog.InfoS("Creating a new HPA", "HPA", hpaName)
		if err = r.Create(ctx, hpa); err != nil {
			klog.ErrorS(err, "Failed to create new HPA", "HPA", hpaName)
			return ctrl.Result{}, err
		}
	} else if err != nil {
		// Error occurred while fetching the existing HPA, report the error and requeue.
		klog.ErrorS(err, "Failed to get HPA", "HPA", hpaName)
		return ctrl.Result{}, err
	} else {
		// Update the existing HPA if it already exists.
		klog.V(4).InfoS("Updating existing HPA to desired state", "HPA", hpaName)

		err = r.Update(ctx, hpa)
		if err != nil {
			klog.ErrorS(err, "Failed to update HPA")
			return ctrl.Result{}, err
		}
	}

	// TODO: add status update. Currently, actualScale and desireScale are not synced from HPA object yet.
	// Return with no error and no requeue needed.
	return ctrl.Result{}, nil
}

// reconcileCustomPA handles the reconciliation logic for custom PodAutoscaler (PA) types.
// It encompasses the main stages that are common to all custom PA implementations, such as:
// - Obtaining the scale reference
// - Recording events
// - Executing the scaling actions
//
// N.B. each custom PA type (e.g., KPA, APA) has its own unique implementation for certain stages:
// - Initializing the scaling context
// - Fetching metrics
// - Applying the scaling algorithm
//
// This function serves as a unified entry point for the reconciliation process of custom PA types,
// while allowing for customization in the specific stages mentioned above.
func (r *PodAutoscalerReconciler) reconcileCustomPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	paStatusOriginal := pa.Status.DeepCopy()
	paType := pa.Spec.ScalingStrategy
	scaleReference := fmt.Sprintf("%s/%s/%s", pa.Spec.ScaleTargetRef.Kind, pa.Namespace, pa.Spec.ScaleTargetRef.Name)
	metricKey, metricSource, err := metrics.NewNamespaceNameMetric(&pa)
	if err != nil {
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedGetMetricKey", err.Error())
		return ctrl.Result{}, err
	}

	targetGV, err := schema.ParseGroupVersion(pa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedGetScale", err.Error())
		// TODO: convert conditionType to type instead of using string
		setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedGetScale", "the %s controller was unable to get the target's current scale: %v", paType, err)
		if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("invalid API version in scale target reference: %v", err)
	}

	targetGK := schema.GroupKind{
		Group: targetGV.Group,
		Kind:  pa.Spec.ScaleTargetRef.Kind,
	}
	mappings, err := r.Mapper.RESTMappings(targetGK)
	if err != nil {
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedGetScale", "the %s controller was unable to get the target's current scale: %v", paType, err)
		if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("unable to determine resource for scale target reference: %v", err)
	}

	// TODO: retrieval targetGR for future scale update
	scale, targetGR, err := r.scaleForResourceMappings(ctx, pa.Namespace, pa.Spec.ScaleTargetRef.Name, mappings)
	if err != nil {
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedGetScale", "the %s controller was unable to get the target's current scale: %v", paType, err)
		if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("failed to query scale subresource for %s: %v", scaleReference, err)
	}

	setCondition(&pa, "AbleToScale", metav1.ConditionTrue, "SucceededGetScale", "the %s controller was able to get the target's current scale", paType)

	// current scale's replica count
	currentReplicasInt64, found, err := unstructured.NestedInt64(scale.Object, "spec", "replicas")
	if !found {
		r.EventRecorder.Eventf(&pa, corev1.EventTypeWarning, "ReplicasNotFound", "The 'replicas' field is missing from the scale object")
		return ctrl.Result{}, fmt.Errorf("the 'replicas' field was not found in the scale object")
	}
	if err != nil {
		r.EventRecorder.Eventf(&pa, corev1.EventTypeWarning, "FailedGetScale", "Error retrieving 'replicas' from scale: %v", err)
		return ctrl.Result{}, fmt.Errorf("failed to get 'replicas' from scale: %v", err)
	}
	currentReplicas := int32(currentReplicasInt64)

	// Update the scale required metrics periodically
	err = r.updateMetricsForScale(ctx, pa, scale, metricKey, metricSource, int(currentReplicas))
	if err != nil {
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedUpdateMetrics", err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to update metrics for scale target reference: %v", err)
	}

	// desired replica count
	desiredReplicas := int32(0)
	rescaleReason := ""
	var minReplicas int32
	// minReplica is optional
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	} else {
		minReplicas = 1
	}

	// check if rescale is needed by checking the replica settings
	rescale := true
	if currentReplicas == int32(0) && minReplicas != 0 {
		// if the replica is 0, then we should not enable autoscaling
		desiredReplicas = 0
		rescale = false
	} else if currentReplicas > pa.Spec.MaxReplicas {
		desiredReplicas = pa.Spec.MaxReplicas
	} else if currentReplicas < minReplicas {
		desiredReplicas = minReplicas
	} else {
		// if the currentReplicas is within the range, we should
		// computeReplicasForMetrics gives
		// TODO: check why it return the metrics name here?
		metricDesiredReplicas, metricName, metricTimestamp, err := r.computeReplicasForMetrics(ctx, pa, scale, metricKey)
		if err != nil {
			r.setCurrentReplicasAndMetricsInStatus(&pa, currentReplicas)
			if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update the resource status")
			}
			r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedComputeMetricsReplicas", err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to compute desired number of replicas based on listed metrics for %s: %v", scaleReference, err)
		}

		klog.InfoS("Proposing desired replicas",
			"desiredReplicas", metricDesiredReplicas,
			"metric", metricName,
			"timestamp", metricTimestamp,
			"scaleTarget", scaleReference)

		rescaleMetric := ""
		if metricDesiredReplicas > desiredReplicas {
			desiredReplicas = metricDesiredReplicas
			rescaleMetric = metricName
		}
		if desiredReplicas > currentReplicas {
			rescaleReason = fmt.Sprintf("%s above target", rescaleMetric)
		}
		if desiredReplicas < currentReplicas {
			rescaleReason = "All metrics below target"
		}

		// adjust desired metrics within the <min, max> range
		if desiredReplicas > pa.Spec.MaxReplicas {
			klog.InfoS("Scaling adjustment: Algorithm recommended scaling to a target that exceeded the maximum limit.",
				"recommendedReplicas", desiredReplicas, "adjustedTo", pa.Spec.MaxReplicas)
			desiredReplicas = pa.Spec.MaxReplicas
		} else if desiredReplicas < minReplicas {
			klog.InfoS("Scaling adjustment: Algorithm recommended scaling to a target that fell below the minimum limit.",
				"recommendedReplicas", desiredReplicas, "adjustedTo", minReplicas)
			desiredReplicas = minReplicas
		}

		rescale = desiredReplicas != currentReplicas
	}

	r.EventRecorder.Eventf(&pa, corev1.EventTypeNormal, "AlgorithmRun",
		"%s algorithm run. currentReplicas: %d, desiredReplicas: %d, rescale: %t",
		pa.Spec.ScalingStrategy, currentReplicas, desiredReplicas, rescale)

	if rescale {
		if err := r.updateScale(ctx, pa.Namespace, targetGR, scale, desiredReplicas); err != nil {
			r.EventRecorder.Eventf(&pa, corev1.EventTypeWarning, "FailedRescale", "New size: %d; reason: %s; error: %v", desiredReplicas, rescaleReason, err)
			setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedUpdateScale", "the %s controller was unable to update the target scale: %v", paType, err)
			r.setCurrentReplicasAndMetricsInStatus(&pa, currentReplicas)
			if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
				utilruntime.HandleError(err)
			}
			return ctrl.Result{}, fmt.Errorf("failed to rescale %s: %v", scaleReference, err)
		}

		// TODO: seems not resolved yet?
		// which way to go?. not sure the best practice in controller-runtime
		//if err := r.Client.SubResource("scale").Update(ctx, scale); err != nil {
		//	return ctrl.Result{}, fmt.Errorf("failed to rescale %s: %v", scaleReference, err)
		//}

		r.EventRecorder.Eventf(&pa, corev1.EventTypeNormal, "SuccessfulRescale", "New size: %d; reason: %s", desiredReplicas, rescaleReason)

		klog.InfoS("Successfully rescaled",
			"PodAutoscaler", klog.KObj(&pa),
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"reason", rescaleReason)
	}

	if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
		// we can overwrite retErr in this case because it's an internal error.
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// scaleForResourceMappings attempts to fetch the scale for the resource with the given name and namespace,
// trying each RESTMapping in turn until a working one is found.  If none work, the first error is returned.
// It returns both the scale, as well as the group-resource from the working mapping.
func (r *PodAutoscalerReconciler) scaleForResourceMappings(ctx context.Context, namespace, name string, mappings []*apimeta.RESTMapping) (*unstructured.Unstructured, schema.GroupResource, error) {
	var firstErr error
	for i, mapping := range mappings {
		targetGR := mapping.Resource.GroupResource()

		gvk := schema.GroupVersionKind{
			Group:   mapping.GroupVersionKind.Group,
			Version: mapping.GroupVersionKind.Version,
			Kind:    mapping.GroupVersionKind.Kind,
		}
		scale := &unstructured.Unstructured{}
		scale.SetGroupVersionKind(gvk)
		scale.SetNamespace(namespace)
		scale.SetName(name)

		err := r.Get(context.TODO(), client.ObjectKey{Namespace: namespace, Name: name}, scale)
		if err == nil {
			return scale, targetGR, nil
		}

		if firstErr == nil {
			firstErr = err
		}

		// if this is the first error, remember it,
		// then go on and try other mappings until we find a good one
		if i == 0 {
			firstErr = err
		}
	}

	// make sure we handle an empty set of mappings
	if firstErr == nil {
		firstErr = fmt.Errorf("unrecognized resource")
	}

	return nil, schema.GroupResource{}, firstErr
}

func (r *PodAutoscalerReconciler) updateScale(ctx context.Context, namespace string, targetGR schema.GroupResource, scale *unstructured.Unstructured, replicas int32) error {
	err := unstructured.SetNestedField(scale.Object, int64(replicas), "spec", "replicas")
	if err != nil {
		return err
	}

	// Update scale object
	err = r.Update(ctx, scale)
	if err != nil {
		return err
	}

	return nil
}

// setCondition sets the specific condition type on the given PA to the specified value with the given reason
// and message.  The message and args are treated like a format string.  The condition will be added if it is
// not present.
func setCondition(pa *autoscalingv1alpha1.PodAutoscaler, conditionType string, status metav1.ConditionStatus, reason, message string, args ...interface{}) {
	pa.Status.Conditions = podutils.SetConditionInList(pa.Status.Conditions, conditionType, status, reason, message, args...)
}

// setCurrentReplicasAndMetricsInStatus sets the current replica count and metrics in the status of the PA.
func (r *PodAutoscalerReconciler) setCurrentReplicasAndMetricsInStatus(pa *autoscalingv1alpha1.PodAutoscaler, currentReplicas int32) {
	r.setStatus(pa, currentReplicas, pa.Status.DesiredScale, false)
}

// setStatus recreates the status of the given PA, updating the current and
// desired replicas, as well as the metric statuses
func (r *PodAutoscalerReconciler) setStatus(pa *autoscalingv1alpha1.PodAutoscaler, currentReplicas, desiredReplicas int32, rescale bool) {
	pa.Status = autoscalingv1alpha1.PodAutoscalerStatus{
		ActualScale:   currentReplicas,
		DesiredScale:  desiredReplicas,
		LastScaleTime: pa.Status.LastScaleTime,
		Conditions:    pa.Status.Conditions,
	}

	if rescale {
		now := metav1.NewTime(time.Now())
		pa.Status.LastScaleTime = &now
	}
}

func (r *PodAutoscalerReconciler) updateStatusIfNeeded(ctx context.Context, oldStatus *autoscalingv1alpha1.PodAutoscalerStatus, newPA *autoscalingv1alpha1.PodAutoscaler) error {
	// skip status update if the status is not exact same
	if apiequality.Semantic.DeepEqual(oldStatus, newPA.Status) {
		return nil
	}
	return r.updateStatus(ctx, newPA)
}

// updateStatus actually does the update request for the status of the given PA
func (r *PodAutoscalerReconciler) updateStatus(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) error {
	if err := r.Status().Update(ctx, pa); err != nil {
		r.EventRecorder.Event(pa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return fmt.Errorf("failed to update status for %s: %v", pa.Name, err)
	}
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Successfully updated status", "PodAutoscaler", klog.KObj(pa))
	return nil
}

// computeReplicasForMetrics computes the desired number of replicas for the metric specifications listed in the pod autoscaler,
// returning the maximum of the computed replica counts, a description of the associated metric, and the statuses of
// all metrics computed.
// It may return both valid metricDesiredReplicas and an error,
// when some metrics still work and PA should perform scaling based on them.
// If PodAutoscaler cannot do anything due to error, it returns -1 in metricDesiredReplicas as a failure signal.
func (r *PodAutoscalerReconciler) computeReplicasForMetrics(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured, metricKey metrics.NamespaceNameMetric) (replicas int32, relatedMetrics string, timestamp time.Time, err error) {
	logger := klog.FromContext(ctx)
	currentTimestamp := time.Now()

	// Retrieve the selector string from the Scale object's Status,
	// and convert *metav1.LabelSelector object to labels.Selector structure
	labelsSelector, err := extractLabelSelector(scale)
	if err != nil {
		return 0, "", currentTimestamp, err
	}

	originalReadyPodsCount, err := scaler.GetReadyPodsCount(ctx, r.Client, pa.Namespace, labelsSelector)

	if err != nil {
		return 0, "", currentTimestamp, fmt.Errorf("error getting ready pods count: %w", err)
	}

	// TODO UpdateScalingContext (in updateScalerSpec) is duplicate invoked in computeReplicasForMetrics and updateMetricsForScale
	err = r.updateScalerSpec(ctx, pa, metricKey)
	if err != nil {
		klog.ErrorS(err, "Failed to update scaler spec from pa_types")
		return 0, "", currentTimestamp, fmt.Errorf("error update scaler spec: %w", err)
	}

	logger.V(4).Info("Obtained selector and get ReadyPodsCount", "selector", labelsSelector, "originalReadyPodsCount", originalReadyPodsCount)

	// Calculate the desired number of pods using the autoscaler logic.
	autoScaler, ok := r.AutoscalerMap[metricKey]
	if !ok {
		return 0, "", currentTimestamp, fmt.Errorf("unsupported scaling strategy: %s", pa.Spec.ScalingStrategy)
	}
	scaleResult := autoScaler.Scale(int(originalReadyPodsCount), metricKey, currentTimestamp)
	if scaleResult.ScaleValid {
		logger.V(4).Info("Successfully called Scale Algorithm", "scaleResult", scaleResult)
		return scaleResult.DesiredPodCount, metricKey.MetricName, currentTimestamp, nil
	}

	return 0, "", currentTimestamp, fmt.Errorf("can not calculate metrics for scale %s", pa.Spec.ScaleTargetRef.Name)
}

// refer to knative-serving.
// In pkg/reconciler/autoscaling/kpa/kpa.go:198, kpa maintains a list of deciders into multi-scaler, each of them corresponds to a pa (PodAutoscaler).
// We create or update the scaler instance according to the pa passed in
func (r *PodAutoscalerReconciler) updateScalerSpec(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler, metricKey metrics.NamespaceNameMetric) error {
	autoScaler, ok := r.AutoscalerMap[metricKey]
	if !ok {
		return fmt.Errorf("unsupported scaling strategy: %s", pa.Spec.ScalingStrategy)
	}
	return autoScaler.UpdateScalingContext(pa)
}

// updateMetricsForScale: we pass into the currentReplicas to construct autoScaler, as KNative implementation
func (r *PodAutoscalerReconciler) updateMetricsForScale(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler, scale *unstructured.Unstructured, metricKey metrics.NamespaceNameMetric, metricSource autoscalingv1alpha1.MetricSource, currentReplicas int) (err error) {
	currentTimestamp := time.Now()
	var autoScaler scaler.Scaler
	// it's similar to knative: pkg/autoscaler/scaling/multiscaler.go: func (m *MultiScaler) Create
	autoScaler, exists := r.AutoscalerMap[metricKey]
	if !exists {
		klog.InfoS("Scaler not found, creating new scaler", "metricKey", metricKey, "type", pa.Spec.ScalingStrategy)

		switch pa.Spec.ScalingStrategy {
		case autoscalingv1alpha1.KPA:
			// initialize all kinds of autoscalers, such as KPA and APA.
			// TODO Currently, we initialize kpa with default config and allocate window with default length.
			//  We then reallocate window according to pa until UpdateScalingContext.
			//  it's not wrong, but we allocate window twice, to be optimized.
			autoScaler, err = scaler.NewKpaAutoscaler(currentReplicas, &pa, time.Now())
		case autoscalingv1alpha1.APA:
			autoScaler, err = scaler.NewApaAutoscaler(currentReplicas, &pa)
		default:
			return fmt.Errorf("unsupported scaling strategy: %s", pa.Spec.ScalingStrategy)
		}
		if err != nil {
			return err
		}
		r.AutoscalerMap[metricKey] = autoScaler
		klog.InfoS("New scaler added to AutoscalerMap", "metricKey", metricKey, "type", pa.Spec.ScalingStrategy, "spec", pa.Spec)
	} else {
		err := autoScaler.UpdateScalingContext(pa)
		if err != nil {
			klog.ErrorS(err, "update existed pa failed", "metricKey", metricKey, "type", pa.Spec.ScalingStrategy, "spec", pa.Spec)
			return err
		}
	}

	// Retrieve the selector string from the Scale object's Status,
	// and convert *metav1.LabelSelector object to labels.Selector structure
	labelsSelector, err := extractLabelSelector(scale)
	if err != nil {
		return err
	}

	// Get pod list managed by scaleTargetRef
	podList, err := podutil.GetPodListByLabelSelector(ctx, r.Client, pa.Namespace, labelsSelector)
	if err != nil {
		klog.ErrorS(err, "failed to get pod list by label selector")
		return err
	}

	// TODO: do we need to indicate the metrics source.
	// Technically, the metrics could come from Kubernetes metrics API (resource or custom), pod prometheus endpoint or ai runtime

	switch metricSource.MetricSourceType {
	case autoscalingv1alpha1.POD:
		return autoScaler.UpdateScaleTargetMetrics(ctx, metricKey, metricSource, podList.Items, currentTimestamp)
	case autoscalingv1alpha1.DOMAIN:
		return autoScaler.UpdateSourceMetrics(ctx, metricKey, metricSource, currentTimestamp)
	default:
		return fmt.Errorf("unsupported protocol type: %v", metricSource.ProtocolType)
	}
}
