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

// Package podautoscaler provides controllers for managing PodAutoscaler resources.
// The controller supports three scaling strategies:
// - HPA: Creates and manages Kubernetes HorizontalPodAutoscaler resources (KEDA-like wrapper)
// - KPA: Knative-style Pod Autoscaling with panic/stable windows
// - APA: Application-specific Pod Autoscaling with custom metrics
//
// Architecture:
// - Stateless autoscaler management: AutoScalers are created on-demand for each reconciliation
// - HPA wrapper: For HPA strategy, we create and manage actual K8s HPA resources
// - Custom scaling: For KPA/APA, we directly compute and apply scaling decisions
package podautoscaler

import (
	"context"
	"fmt"
	"sync"
	"time"

	autoscalingv1alpha1 "github.com/vllm-project/aibrix/api/autoscaling/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	scalingctx "github.com/vllm-project/aibrix/pkg/controller/podautoscaler/context"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/metrics"
	"github.com/vllm-project/aibrix/pkg/controller/podautoscaler/monitor"
	podutils "github.com/vllm-project/aibrix/pkg/utils"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/discovery"
	memcache "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	custommetrics "k8s.io/metrics/pkg/client/custom_metrics"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const RayClusterFleet = "RayClusterFleet"

const (
	ConditionReady         = "Ready"
	ConditionValidSpec     = "ValidSpec"
	ConditionConflict      = "MultiPodAutoscalerConflict"
	ConditionScalingActive = "ScalingActive"
	ConditionAbleToScale   = "AbleToScale"

	ReasonAsExpected                = "AsExpected"
	ReasonReconcilingScaleDiff      = "ReconcilingScaleDiff"
	ReasonStable                    = "Stable"
	ReasonInvalidScalingStrategy    = "InvalidScalingStrategy"
	ReasonInvalidBounds             = "InvalidBounds"
	ReasonMissingTargetRef          = "MissingScaleTargetRef"
	ReasonMetricsConfigError        = "MetricsConfigError"
	ReasonInvalidSpec               = "InvalidSpec"
	ReasonConfigured                = "Configured"
	maxScalingHistorySize           = 5
	minScalingHistoryRecordInterval = 5 * time.Second
)

var (
	DefaultResyncInterval           = 10 * time.Second
	DefaultRequeueDuration          = 10 * time.Second
	DefaultReconcileTimeoutDuration = 10 * time.Second
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
	// Create Kubernetes metrics clients
	restConfig := mgr.GetConfig()

	// Resource metrics client (for cpu, memory metrics from metrics.k8s.io)
	resourceClient, err := versioned.NewForConfig(restConfig)
	if err != nil {
		klog.Warningf("Failed to create resource metrics client: %v. Resource metrics will be unavailable.", err)
		resourceClient = nil
	}

	// Custom metrics client (for custom.metrics.k8s.io API)
	// Requires proper AvailableAPIsGetter implementation
	disc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cached := memcache.NewMemCacheClient(disc)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cached)
	apis := custommetrics.NewAvailableAPIsGetter(disc)
	customClient := custommetrics.NewForConfig(restConfig, mapper, apis)

	workloadScaleClient := NewWorkloadScale(mgr.GetClient(), mgr.GetRESTMapper())

	// Create factory with all metric fetcher types
	factory := metrics.NewDefaultMetricFetcherFactory(resourceClient, customClient)

	// Create a unified autoscaler that can handle all strategies
	// It will be configured per-request based on PodAutoscaler spec
	autoScaler := NewDefaultAutoScaler(factory, mgr.GetClient())

	// Instantiate a new PodAutoscalerReconciler with the given manager's client and scheme
	reconciler := &PodAutoscalerReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		EventRecorder:       mgr.GetEventRecorderFor("PodAutoscaler"),
		Mapper:              mgr.GetRESTMapper(),
		workloadScaleClient: workloadScaleClient,
		resyncInterval:      DefaultResyncInterval,
		eventCh:             make(chan event.GenericEvent),
		autoScaler:          autoScaler,
		RuntimeConfig:       runtimeConfig,
		recommendations:     make(map[string][]timestampedRecommendation),
		monitor:             monitor.New(),
		scalingTargetToPA:   make(map[string]ktypes.NamespacedName),
		paToScalingKey:      make(map[ktypes.NamespacedName]string),
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
					NamespacedName: ktypes.NamespacedName{
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
	src := source.Channel(reconciler.eventCh, &handler.EnqueueRequestForObject{})

	// Create a new controller managed by AIBrix manager, watching for changes to PodAutoscaler objects
	// and HorizontalPodAutoscaler objects.
	err := ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.PodAutoscaler{}).
		Watches(&autoscalingv2.HorizontalPodAutoscaler{}, handler.EnqueueRequestsFromMapFunc(filterHPAObject)).
		WatchesRawSource(src).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Added AIBrix pod-autoscaler-controller successfully")

	errChan := make(chan error)
	go reconciler.Run(context.Background(), errChan)
	klog.InfoS("Run pod-autoscaler-controller periodical syncs successfully")

	go func() {
		for err := range errChan {
			klog.ErrorS(err, "Run function returned an error")
		}
	}()

	return nil
}

var _ reconcile.Reconciler = &PodAutoscalerReconciler{}

// timestampedRecommendation stores a scaling recommendation with its timestamp
type timestampedRecommendation struct {
	recommendation int32
	timestamp      time.Time
}

// PodAutoscalerReconciler reconciles a PodAutoscaler object.
// It uses stateless autoscaler management where AutoScalers are created
// on-demand for each reconciliation cycle, avoiding memory leaks and
// stale state issues.
type PodAutoscalerReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	EventRecorder       record.EventRecorder
	Mapper              apimeta.RESTMapper
	autoScaler          AutoScaler
	workloadScaleClient WorkloadScale
	resyncInterval      time.Duration
	eventCh             chan event.GenericEvent
	RuntimeConfig       config.RuntimeConfig

	// Recommendation history for cooldown windows (key: namespace/name)
	recommendationsMu sync.RWMutex
	recommendations   map[string][]timestampedRecommendation

	monitor monitor.Monitor

	scalingTargetMu   sync.RWMutex
	scalingTargetToPA map[string]ktypes.NamespacedName // keyStr → PA
	paToScalingKey    map[ktypes.NamespacedName]string // PA → keyStr (for cleanup)
}

//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=autoscaling.aibrix.ai,resources=podautoscalers/finalizers,verbs=update
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
//+kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=stormservices,verbs=get;list;watch;patch

func (r *PodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultReconcileTimeoutDuration)
	defer cancel()

	var pa autoscalingv1alpha1.PodAutoscaler
	if err := r.Get(ctx, req.NamespacedName, &pa); err != nil {
		if errors.IsNotFound(err) {
			// Object might have been deleted after reconcile request
			r.cleanupDeletedPA(req.NamespacedName)
			klog.Infof("PodAutoscaler resource not found. Object %s must have been deleted", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.ErrorS(err, "Failed to get PodAutoscaler", "obj", req.NamespacedName)
		return ctrl.Result{}, err
	}

	// validate spec; if invalid, write conditions and exit without requeue
	specVR := r.validateSpec(&pa)
	// conflict: Ensure no other PA controls the same target
	conflictVR := r.checkNoMultiPodAutoscalerConflict(&pa)

	newStatus := computeStatus(ctx, pa, specVR, conflictVR)
	paCopy := pa.DeepCopy()
	paCopy.Status = *newStatus
	if err := r.updateStatusIfNeeded(ctx, &pa.Status, paCopy); err != nil {
		return ctrl.Result{}, err
	}

	// if invalid, skip reconciliation (no scaling)
	if !specVR.Valid || !conflictVR.Valid {
		return ctrl.Result{}, nil
	}

	switch pa.Spec.ScalingStrategy {
	case autoscalingv1alpha1.HPA:
		return r.reconcileHPA(ctx, pa)
	case autoscalingv1alpha1.KPA, autoscalingv1alpha1.APA:
		return r.reconcileCustomPA(ctx, pa)
	default:
		// Status already updated above; nothing more to do
		return ctrl.Result{}, nil
	}
}

type ScalingTargetKey struct {
	Namespace     string
	APIVersion    string
	Kind          string
	Name          string
	SubTargetRole string // from SubTargetSelector.RoleName
}

func (r *PodAutoscalerReconciler) buildScalingTargetKey(pa *autoscalingv1alpha1.PodAutoscaler) string {
	ref := pa.Spec.ScaleTargetRef
	ns := pa.Namespace
	name := ref.Name

	// Base: <apiVersion>.<Kind>/<namespace>/<name>
	base := fmt.Sprintf("%s.%s/%s/%s", ref.APIVersion, ref.Kind, ns, name)

	// Only StormService supports sub-targeting by roleName
	if ref.APIVersion == orchestrationv1alpha1.GroupVersion.String() && ref.Kind == orchestrationv1alpha1.StormServiceKind {
		if sel := pa.Spec.SubTargetSelector; sel != nil && sel.RoleName != "" {
			return base + "/" + sel.RoleName
		}
	}

	return base
}

// cleanupDeletedPA removes the PA from conflict maps.
func (r *PodAutoscalerReconciler) cleanupDeletedPA(paKey ktypes.NamespacedName) {
	r.scalingTargetMu.Lock()
	defer r.scalingTargetMu.Unlock()

	if keyStr, exists := r.paToScalingKey[paKey]; exists {
		delete(r.scalingTargetToPA, keyStr)
	}
	delete(r.paToScalingKey, paKey)
}

// checkNoMultiPodAutoscalerConflict checks whether the PodAutoscaler's target is already controlled by another PA.
func (r *PodAutoscalerReconciler) checkNoMultiPodAutoscalerConflict(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	key := r.buildScalingTargetKey(pa)
	currentPAKey := ktypes.NamespacedName{Namespace: pa.Namespace, Name: pa.Name}

	r.scalingTargetMu.Lock()
	defer r.scalingTargetMu.Unlock()

	// Step 1: Check if target is claimed
	if existingPA, exists := r.scalingTargetToPA[key]; exists {
		if existingPA == currentPAKey {
			// Self-owned — OK
			r.paToScalingKey[currentPAKey] = key
			return validOK()
		}
		// Conflict with another PA
		errMsg := fmt.Sprintf("Scaling target %s is already controlled by PodAutoscaler %s/%s, "+
			"it will not take effect", key, existingPA.Namespace, existingPA.Name)
		return invalid(ConditionConflict, errMsg)
	}

	// Step 2: Claim the target
	if oldKey, exists := r.paToScalingKey[currentPAKey]; exists && oldKey != key {
		delete(r.scalingTargetToPA, oldKey)
	}
	r.scalingTargetToPA[key] = currentPAKey
	r.paToScalingKey[currentPAKey] = key

	return validOK()
}

func (r *PodAutoscalerReconciler) validateSpec(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	if vr := r.validateScaleTargetRef(pa); !vr.Valid {
		return vr
	}
	if vr := r.validateReplicaBounds(pa); !vr.Valid {
		return vr
	}
	if vr := r.validateScalingStrategy(pa); !vr.Valid {
		return vr
	}
	if vr := r.validateMetricsSources(pa); !vr.Valid {
		return vr
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateScaleTargetRef(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	if pa.Spec.ScaleTargetRef.Name == "" || pa.Spec.ScaleTargetRef.Kind == "" {
		return invalid(ReasonMissingTargetRef, "scaleTargetRef.kind and scaleTargetRef.name must be set.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateReplicaBounds(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	if pa.Spec.MinReplicas != nil && pa.Spec.MaxReplicas < *pa.Spec.MinReplicas {
		return invalid(ReasonInvalidBounds, "minReplicas cannot be greater than maxReplicas.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateScalingStrategy(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	if !checkValidAutoscalingStrategy(pa.Spec.ScalingStrategy) {
		return invalid(ReasonInvalidScalingStrategy, "Unsupported scalingStrategy; must be one of HPA/KPA/APA.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateMetricsSources(pa *autoscalingv1alpha1.PodAutoscaler) ValidationResult {
	if len(pa.Spec.MetricsSources) != 1 {
		return invalid(ReasonMetricsConfigError, "exactly one metricsSource is required for current implementation.")
	}

	ms := &pa.Spec.MetricsSources[0]
	if ms.TargetMetric == "" {
		return invalid(ReasonMetricsConfigError, "targetMetric must be specified in metricsSource.")
	}
	if ms.TargetValue == "" {
		return invalid(ReasonMetricsConfigError, "targetValue must be specified in metricsSource.")
	}

	switch ms.MetricSourceType {
	case autoscalingv1alpha1.POD:
		return r.validatePodMetricSource(ms)
	case autoscalingv1alpha1.EXTERNAL, autoscalingv1alpha1.DOMAIN:
		return r.validateExternalMetricSource(ms)
	case autoscalingv1alpha1.RESOURCE:
		return r.validateResourceMetricSource(ms)
	case autoscalingv1alpha1.CUSTOM:
		return r.validateCustomMetricSource(ms)
	default:
		return invalid(ReasonMetricsConfigError, fmt.Sprintf("unsupported metricSourceType: %s", ms.MetricSourceType))
	}
}

func (r *PodAutoscalerReconciler) validatePodMetricSource(ms *autoscalingv1alpha1.MetricSource) ValidationResult {
	if ms.ProtocolType == "" {
		return invalid(ReasonMetricsConfigError, "protocolType is required for metricSourceType=pod.")
	}
	if ms.Port == "" {
		return invalid(ReasonMetricsConfigError, "port is required for metricSourceType=pod.")
	}
	if ms.Path == "" {
		return invalid(ReasonMetricsConfigError, "path is required for metricSourceType=pod.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateExternalMetricSource(ms *autoscalingv1alpha1.MetricSource) ValidationResult {
	if ms.ProtocolType == "" {
		return invalid(ReasonMetricsConfigError, "protocolType is required for metricSourceType=external.")
	}
	if ms.Endpoint == "" {
		return invalid(ReasonMetricsConfigError, "endpoint is required for metricSourceType=external.")
	}
	if ms.Path == "" {
		return invalid(ReasonMetricsConfigError, "path is required for metricSourceType=external.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateResourceMetricSource(ms *autoscalingv1alpha1.MetricSource) ValidationResult {
	validMetrics := map[string]bool{"cpu": true, "memory": true}
	if !validMetrics[ms.TargetMetric] {
		return invalid(ReasonMetricsConfigError, "for metricSourceType=resource, targetMetric must be 'cpu' or 'memory'.")
	}
	if ms.Port != "" || ms.Endpoint != "" || ms.Path != "" || ms.ProtocolType != "" {
		return invalid(ReasonMetricsConfigError, "port, endpoint, path, and protocolType are not allowed for metricSourceType=resource.")
	}
	return validOK()
}

func (r *PodAutoscalerReconciler) validateCustomMetricSource(_ *autoscalingv1alpha1.MetricSource) ValidationResult {
	// Custom metrics: no additional required fields
	// You may add format validation later if needed (e.g., must contain '/')
	return validOK()
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

func (r *PodAutoscalerReconciler) setInvalidSpecStatus(
	ctx context.Context,
	pa *autoscalingv1alpha1.PodAutoscaler,
	reason, msg string,
) error {
	conds := pa.Status.Conditions
	now := metav1.Now()

	apimeta.SetStatusCondition(&conds, metav1.Condition{
		Type:               ConditionValidSpec,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
	apimeta.SetStatusCondition(&conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInvalidSpec,
		Message:            "Spec invalid; controller will not reconcile until fixed.",
		LastTransitionTime: now,
	})

	st := &autoscalingv1alpha1.PodAutoscalerStatus{
		LastScaleTime: pa.Status.LastScaleTime,
		DesiredScale:  pa.Status.DesiredScale,
		ActualScale:   pa.Status.ActualScale,
		Conditions:    conds,
	}
	return r.updateStatusIfNeeded(ctx, st, pa)
}

func (r *PodAutoscalerReconciler) Run(ctx context.Context, errChan chan<- error) {
	ticker := time.NewTicker(r.resyncInterval)
	defer ticker.Stop()
	defer close(r.eventCh)

	for {
		select {
		case <-ticker.C:
			klog.V(4).Info("enqueue all autoscaler objects")
			// periodically sync all autoscaling objects
			if err := r.enqueuePodAutoscalers(ctx); err != nil {
				klog.ErrorS(err, "Failed to enqueue pod autoscaler objects")
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

func computeStatus(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler,
	specValidationResult, conflictValidationResult ValidationResult) *autoscalingv1alpha1.PodAutoscalerStatus {
	now := metav1.Now()
	st := &autoscalingv1alpha1.PodAutoscalerStatus{
		LastScaleTime: pa.Status.LastScaleTime,
		DesiredScale:  pa.Status.DesiredScale,
		ActualScale:   pa.Status.ActualScale,
		Conditions:    pa.Status.Conditions, // upsert onto existing
	}

	// ValidSpec
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionValidSpec,
		Status:             boolToCond(specValidationResult.Valid),
		Reason:             map[bool]string{true: ReasonAsExpected, false: specValidationResult.Reason}[specValidationResult.Valid],
		Message:            specValidationResult.Message,
		LastTransitionTime: now,
	})

	if !conflictValidationResult.Valid {
		// if conflictValidationResult is invalid, then we set condition
		apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               ConditionConflict,
			Status:             boolToCond(conflictValidationResult.Valid),
			Reason:             map[bool]string{false: conflictValidationResult.Reason}[conflictValidationResult.Valid],
			Message:            conflictValidationResult.Message,
			LastTransitionTime: now,
		})
	} else {
		// Conflict is resolved: remove ConditionConflict if present
		if cond := apimeta.FindStatusCondition(st.Conditions, ConditionConflict); cond != nil {
			apimeta.RemoveStatusCondition(&st.Conditions, ConditionConflict)
		}
	}

	// ScalingActive
	scalingActive := st.DesiredScale != st.ActualScale
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionScalingActive,
		Status:             boolToCond(scalingActive),
		Reason:             map[bool]string{true: ReasonReconcilingScaleDiff, false: ReasonStable}[scalingActive],
		Message:            fmt.Sprintf("desired=%d, actual=%d", st.DesiredScale, st.ActualScale),
		LastTransitionTime: now,
	})

	// AbleToScale (minimal; extend later with cooldown/limits applied)
	specOK := specValidationResult.Valid
	noConflict := conflictValidationResult.Valid
	able := specOK && noConflict && pa.Spec.MaxReplicas > 0
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionAbleToScale,
		Status:             boolToCond(able),
		Reason:             map[bool]string{true: ReasonConfigured, false: ReasonInvalidSpec}[able],
		LastTransitionTime: now,
	})

	// Ready = ValidSpec && !ScalingActive
	ready := able && !scalingActive
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             boolToCond(ready),
		Reason:             map[bool]string{true: ReasonAsExpected, false: ReasonReconcilingScaleDiff}[ready],
		LastTransitionTime: now,
	})
	return st
}

// reconcileHPA handles HPA strategy by creating and managing Kubernetes HorizontalPodAutoscaler resources.
// This provides KEDA-like functionality where we wrap standard K8s HPA with additional features.
func (r *PodAutoscalerReconciler) reconcileHPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	// Create ScalingContext as single source of truth for configuration
	scalingContext := r.createScalingContext(pa)

	// Generate a corresponding HorizontalPodAutoscaler
	hpa, err := makeHPA(&pa, scalingContext)
	if err != nil {
		klog.ErrorS(err, "Failed to generate a HPA object", "PA", ktypes.NamespacedName{Name: pa.Name, Namespace: pa.Namespace})
		return ctrl.Result{}, err
	}
	hpaName := ktypes.NamespacedName{
		Name:      hpa.Name,
		Namespace: hpa.Namespace,
	}

	existingHPA := &autoscalingv2.HorizontalPodAutoscaler{}
	err = r.Get(ctx, hpaName, existingHPA)
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

	// Add status update. Sync actualScale, desireScale, conditions, and lastScaleTime from HPA to pa.
	pa.Status.ActualScale = existingHPA.Status.CurrentReplicas
	pa.Status.DesiredScale = existingHPA.Status.DesiredReplicas
	pa.Status.LastScaleTime = existingHPA.Status.LastScaleTime
	for _, condition := range existingHPA.Status.Conditions {
		setCondition(&pa, string(condition.Type), metav1.ConditionStatus(condition.Status), condition.Reason, condition.Message)
	}
	if err = r.Status().Update(ctx, &pa); err != nil {
		klog.ErrorS(err, "Failed to update PodAutoscaler status")
	}
	// Return with no error and no requeue needed.
	return ctrl.Result{}, nil
}

// reconcileCustomPA handles KPA and APA strategies using WorkloadScaler (generic /scale or StormService role-level).
func (r *PodAutoscalerReconciler) reconcileCustomPA(ctx context.Context, pa autoscalingv1alpha1.PodAutoscaler) (ctrl.Result, error) {
	paStatusOriginal := pa.Status.DeepCopy()
	scaleReference := fmt.Sprintf("%s/%s/%s", pa.Spec.ScaleTargetRef.Kind, pa.Namespace, pa.Spec.ScaleTargetRef.Name)
	if pa.Spec.SubTargetSelector != nil && pa.Spec.SubTargetSelector.RoleName != "" {
		scaleReference = fmt.Sprintf("%s/%s/%s/%s", pa.Spec.ScaleTargetRef.Kind, pa.Namespace, pa.Spec.ScaleTargetRef.Name, pa.Spec.SubTargetSelector.RoleName)
	}

	// Step 1: Get scaleObj resource
	scaleObj, _, err := r.getScaleResource(ctx, &pa)
	if err != nil {
		r.recordScaleError(&pa, "FailedGetScale", err)
		setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedGetScale", "unable to get target scaleObj: %v", err)
		if updateErr := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, fmt.Errorf("failed to get scaleObj for %s: %w", scaleReference, err)
	}
	setCondition(&pa, "AbleToScale", metav1.ConditionTrue, "SucceededGetScale", "successfully retrieved target scale")

	// Step 2: Get current replicas from scale object
	currentReplicas, err := r.workloadScaleClient.GetCurrentReplicasFromScale(ctx, &pa, scaleObj)
	if err != nil {
		r.EventRecorder.Eventf(&pa, corev1.EventTypeWarning, "FailedGetReplicas", "Error extracting replicas: %v", err)
		return ctrl.Result{}, fmt.Errorf("failed to get current replicas: %w", err)
	}

	// Step 3: Compute scaling decision with selector
	// Pass ScaleTargetRef to handle special cases like RayClusterFleet
	scaleDecision, err := r.computeScaleDecision(ctx, pa, scaleObj, currentReplicas)
	if err != nil {
		setStatus(&pa, currentReplicas, currentReplicas, false, "FailedComputeScale", false, err)
		if updateErr := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		r.EventRecorder.Event(&pa, corev1.EventTypeWarning, "FailedComputeScale", err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to compute scaling decision for %s: %w", scaleReference, err)
	}
	r.monitor.RecordScaleAction(pa.Namespace, pa.Name, scaleDecision.Algorithm, scaleDecision.Reason, scaleDecision.DesiredReplicas)

	// Only emit event if we should scale
	if scaleDecision.ShouldScale {
		r.EventRecorder.Eventf(&pa, corev1.EventTypeNormal, "AlgorithmRun",
			"%s algorithm completed. currentReplicas: %d, desiredReplicas: %d, shouldScale: %t",
			pa.Spec.ScalingStrategy, currentReplicas, scaleDecision.DesiredReplicas, scaleDecision.ShouldScale)
	} else {
		klog.V(4).InfoS("Skipping event: no change in replica count", "replicas", currentReplicas)
	}

	// Step 4: Apply scaling if needed
	var scaleError error
	if scaleDecision.ShouldScale {
		scaleError := r.workloadScaleClient.SetDesiredReplicas(ctx, &pa, scaleDecision.DesiredReplicas)
		if scaleError != nil {
			r.recordScaleError(&pa, "FailedRescale", scaleError)
			setCondition(&pa, "AbleToScale", metav1.ConditionFalse, "FailedUpdateScale", "unable to apply desired replicas: %v", scaleError)
			klog.ErrorS(scaleError, "Failed to apply scaling", "PodAutoscaler", klog.KObj(&pa))
			return ctrl.Result{}, fmt.Errorf("failed to apply scaling for %s: %w", scaleReference, scaleError)
		} else {
			klog.InfoS("Successfully rescaled",
				"PodAutoscaler", klog.KObj(&pa),
				"currentReplicas", currentReplicas,
				"desiredReplicas", scaleDecision.DesiredReplicas,
				"reason", scaleDecision.Reason)
			r.EventRecorder.Eventf(&pa, corev1.EventTypeNormal, "SuccessfulRescale",
				"New size: %d; reason: %s", scaleDecision.DesiredReplicas, scaleDecision.Reason)
		}
	}

	// Step 5: Update status
	setStatus(&pa, currentReplicas, scaleDecision.DesiredReplicas,
		scaleDecision.ShouldScale, scaleDecision.Reason, scaleError == nil, scaleError)

	if err := r.updateStatusIfNeeded(ctx, paStatusOriginal, &pa); err != nil {
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

		err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, scale)
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

// setStatus recreates the status of the given PA, updating the current and
// desired replicas, as well as the metric statuses and optionally records a scaling decision
func setStatus(pa *autoscalingv1alpha1.PodAutoscaler, currentReplicas, desiredReplicas int32, rescale bool, reason string, success bool, err error) {
	pa.Status = autoscalingv1alpha1.PodAutoscalerStatus{
		ActualScale:    currentReplicas,
		DesiredScale:   desiredReplicas,
		LastScaleTime:  pa.Status.LastScaleTime,
		Conditions:     pa.Status.Conditions,
		ScalingHistory: pa.Status.ScalingHistory, // preserve existing history
	}

	if rescale || !success {
		now := metav1.NewTime(time.Now())

		// Check if this is a genuine scaling event or just a quick follow-up reconciliation
		var shouldRecordHistory bool
		if len(pa.Status.ScalingHistory) == 0 {
			shouldRecordHistory = true
		} else {
			lastScale := pa.Status.ScalingHistory[len(pa.Status.ScalingHistory)-1]
			// Only record if:
			// 1. More than minScalingHistoryRecordInterval have passed since last scaling decision, or
			// 2. The desired scale is different from the last recorded scale, or
			// 3. The success status has changed (success to failure or vice versa), or
			// 4. There's a different error message
			timeSinceLastScale := now.Time.Sub(lastScale.Timestamp.Time)
			if timeSinceLastScale > minScalingHistoryRecordInterval ||
				lastScale.NewScale != desiredReplicas ||
				lastScale.Success != success ||
				(err != nil && lastScale.Error != err.Error()) {
				shouldRecordHistory = true
			}
		}
		if shouldRecordHistory {
			pa.Status.LastScaleTime = &now
			decision := autoscalingv1alpha1.ScalingDecision{
				Timestamp:     metav1.Now(),
				PreviousScale: currentReplicas,
				NewScale:      desiredReplicas,
				Reason:        reason,
				Success:       success,
			}
			if err != nil {
				decision.Error = err.Error()
			}
			// First trim if at maximum size
			if len(pa.Status.ScalingHistory) >= maxScalingHistorySize {
				pa.Status.ScalingHistory = pa.Status.ScalingHistory[1:]
			}

			// Then append the new decision
			pa.Status.ScalingHistory = append(pa.Status.ScalingHistory, decision)
		}

	}
}

func (r *PodAutoscalerReconciler) updateStatusIfNeeded(ctx context.Context, oldStatus *autoscalingv1alpha1.PodAutoscalerStatus, newPA *autoscalingv1alpha1.PodAutoscaler) error {
	// skip status update if the status is not exact same
	if apiequality.Semantic.DeepEqual(oldStatus, &newPA.Status) {
		return nil
	}
	// Use retry.RetryOnConflict to handle "object has been modified"
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &autoscalingv1alpha1.PodAutoscaler{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(newPA), latest); err != nil {
			return err
		}
		// second semantic equality check on the API server.
		// It's possible that another reconciler or controller
		// has already updated the status to the desired state while we were waiting.
		// If so, skip the update to avoid redundant writes and unnecessary resourceVersion bumps.
		if apiequality.Semantic.DeepEqual(&latest.Status, &newPA.Status) {
			return nil
		}
		latest.Status = newPA.Status
		return r.Status().Update(ctx, latest)
	})
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

// ScaleDecision represents a scaling decision made by the autoscaler
type ScaleDecision struct {
	DesiredReplicas int32
	ShouldScale     bool
	Reason          string
	Algorithm       string
}

// getScaleResource retrieves the scale resource for the PodAutoscaler target
func (r *PodAutoscalerReconciler) getScaleResource(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) (*unstructured.Unstructured, schema.GroupResource, error) {
	targetGV, err := schema.ParseGroupVersion(pa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		return nil, schema.GroupResource{}, fmt.Errorf("invalid API version in scale target reference: %w", err)
	}

	targetGK := schema.GroupKind{
		Group: targetGV.Group,
		Kind:  pa.Spec.ScaleTargetRef.Kind,
	}
	mappings, err := r.Mapper.RESTMappings(targetGK)
	if err != nil {
		return nil, schema.GroupResource{}, fmt.Errorf("unable to determine resource for scale target reference: %w", err)
	}

	scale, targetGR, err := r.scaleForResourceMappings(ctx, pa.Namespace, pa.Spec.ScaleTargetRef.Name, mappings)
	if err != nil {
		return nil, schema.GroupResource{}, fmt.Errorf("failed to query scale subresource: %w", err)
	}

	return scale, targetGR, nil
}

// computeScaleDecision determines if scaling is needed and what the target should be
func (r *PodAutoscalerReconciler) computeScaleDecision(
	ctx context.Context,
	pa autoscalingv1alpha1.PodAutoscaler,
	scaleObj *unstructured.Unstructured,
	currentReplicas int32,
) (*ScaleDecision, error) {
	var minReplicas int32 = 1
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	}

	// Check if scaling should be disabled (replica is 0 and minReplicas != 0)
	if currentReplicas == 0 && minReplicas != 0 {
		return &ScaleDecision{
			DesiredReplicas: 0,
			ShouldScale:     false,
			Reason:          "scaling disabled: current replicas is 0",
			Algorithm:       "boundary-check",
		}, nil
	}

	// Check boundary conditions first
	if currentReplicas > pa.Spec.MaxReplicas {
		return &ScaleDecision{
			DesiredReplicas: pa.Spec.MaxReplicas,
			ShouldScale:     true,
			Reason:          "current replicas exceed maximum",
			Algorithm:       "boundary-check",
		}, nil
	}

	if currentReplicas < minReplicas {
		return &ScaleDecision{
			DesiredReplicas: minReplicas,
			ShouldScale:     true,
			Reason:          "current replicas below minimum",
			Algorithm:       "boundary-check",
		}, nil
	}

	// Create scaling context as single source of truth for PA-level configuration
	scalingContext := r.createScalingContext(pa)

	// Use autoscaler for metric-based scaling with provided selector
	replicaResult, err := r.computeMetricBasedReplicas(ctx, pa, scalingContext, scaleObj, currentReplicas)
	if err != nil {
		return nil, fmt.Errorf("failed to compute metric-based replicas: %w", err)
	}

	metricDesiredReplicas := replicaResult.DesiredReplicas
	metricName := replicaResult.Algorithm

	// Apply cooldown window for KPA/APA (not for HPA which is managed by K8s)
	desiredReplicas := metricDesiredReplicas
	if pa.Spec.ScalingStrategy != autoscalingv1alpha1.HPA {
		desiredReplicas = r.stabilizeRecommendation(&pa, scalingContext, metricDesiredReplicas, currentReplicas)
	}

	reason := ""

	// Apply constraints
	if desiredReplicas > pa.Spec.MaxReplicas {
		klog.InfoS("Scaling adjustment: Algorithm recommended scaling above maximum limit",
			"recommendedReplicas", desiredReplicas, "adjustedTo", pa.Spec.MaxReplicas)
		desiredReplicas = pa.Spec.MaxReplicas
	} else if desiredReplicas < minReplicas {
		klog.InfoS("Scaling adjustment: Algorithm recommended scaling below minimum limit",
			"recommendedReplicas", desiredReplicas, "adjustedTo", minReplicas)
		desiredReplicas = minReplicas
	}

	shouldScale := desiredReplicas != currentReplicas
	if shouldScale {
		if desiredReplicas > currentReplicas {
			reason = fmt.Sprintf("%s above target", metricName)
		} else {
			reason = "All metrics below target"
		}
	} else {
		reason = "stable"
	}

	return &ScaleDecision{
		DesiredReplicas: desiredReplicas,
		ShouldScale:     shouldScale,
		Reason:          reason,
		Algorithm:       metricName,
	}, nil
}

// applyScaling applies the scaling decision to the target resource
func (r *PodAutoscalerReconciler) applyScaling(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler, targetGR schema.GroupResource, scale *unstructured.Unstructured, decision *ScaleDecision) error {
	return r.updateScale(ctx, pa.Namespace, targetGR, scale, decision.DesiredReplicas)
}

// recordScaleError records scaling-related errors as events
func (r *PodAutoscalerReconciler) recordScaleError(pa *autoscalingv1alpha1.PodAutoscaler, reason string, err error) {
	r.EventRecorder.Event(pa, corev1.EventTypeWarning, reason, err.Error())
}

// computeMetricBasedReplicas uses the autoscaler to compute desired replicas based on metrics
func (r *PodAutoscalerReconciler) computeMetricBasedReplicas(
	ctx context.Context,
	pa autoscalingv1alpha1.PodAutoscaler,
	scalingContext scalingctx.ScalingContext,
	scaleObject *unstructured.Unstructured,
	currentReplicas int32,
) (*ReplicaComputeResult, error) {
	// Get pod selector - this handles both generic scaling and role-level scaling
	// Use the scale object we already have to avoid extra API call
	labelsSelector, err := r.workloadScaleClient.GetPodSelectorFromScale(ctx, &pa, scaleObject)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod selector: %w", err)
	}

	// Append ray head worker requirement for label selector if needed
	if scaleObject.GetAPIVersion() == orchestrationv1alpha1.GroupVersion.String() && scaleObject.GetKind() == RayClusterFleet {
		newRequirement, err := labels.NewRequirement("ray.io/node-type", selection.Equals, []string{"head"})
		if err != nil {
			return nil, fmt.Errorf("failed to add ray.io/node-type requirement to label selector: %w", err)
		}
		labelsSelector = labelsSelector.Add(*newRequirement)
	}

	podList, err := podutils.GetPodListByLabelSelector(ctx, r.Client, pa.Namespace, labelsSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod list: %w", err)
	}

	// Create request for autoscaler with ScalingContext
	replicaRequest := ReplicaComputeRequest{
		PodAutoscaler:   pa,
		ScalingContext:  scalingContext,
		CurrentReplicas: currentReplicas,
		Pods:            podList.Items,
		Timestamp:       time.Now(),
	}

	// Use autoscaler to compute desired replicas
	return r.autoScaler.ComputeDesiredReplicas(ctx, replicaRequest)
}

// stabilizeRecommendation applies cooldown window logic to smooth out replica recommendations.
// It keeps a history of recommendations and returns the max (for scale-up) or min (for scale-down)
// from the cooldown window, similar to K8s HPA behavior.
func (r *PodAutoscalerReconciler) stabilizeRecommendation(
	pa *autoscalingv1alpha1.PodAutoscaler,
	scalingContext scalingctx.ScalingContext,
	recommendation,
	current int32,
) int32 {
	key := fmt.Sprintf("%s/%s", pa.Namespace, pa.Name)
	now := time.Now()

	// Get cooldown window durations from ScalingContext
	scaleUpWindow := scalingContext.GetScaleUpCooldownWindow()
	scaleDownWindow := scalingContext.GetScaleDownCooldownWindow()

	r.recommendationsMu.Lock()
	defer r.recommendationsMu.Unlock()

	// Initialize history if not exists
	if r.recommendations == nil {
		r.recommendations = make(map[string][]timestampedRecommendation)
	}

	// Add current recommendation to history
	r.recommendations[key] = append(r.recommendations[key], timestampedRecommendation{
		recommendation: recommendation,
		timestamp:      now,
	})

	// Determine which window to use based on scaling direction
	var windowDuration time.Duration
	var selectMax bool

	if recommendation > current {
		windowDuration = scaleUpWindow
		selectMax = true
	} else if recommendation < current {
		windowDuration = scaleDownWindow
		selectMax = false
	} else {
		// No change, clean old recommendations and return current
		r.cleanOldRecommendations(key, now, scaleUpWindow, scaleDownWindow)
		return current
	}

	// Clean recommendations outside the window
	r.cleanOldRecommendations(key, now, scaleUpWindow, scaleDownWindow)

	// Select from recommendations within the window
	cutoff := now.Add(-windowDuration)
	var stabilized int32
	first := true

	for _, rec := range r.recommendations[key] {
		if rec.timestamp.After(cutoff) {
			if first {
				stabilized = rec.recommendation
				first = false
			} else {
				if selectMax && rec.recommendation > stabilized {
					stabilized = rec.recommendation
				} else if !selectMax && rec.recommendation < stabilized {
					stabilized = rec.recommendation
				}
			}
		}
	}

	if first {
		// No recommendations within window, use current recommendation
		stabilized = recommendation
	}

	klog.V(4).InfoS("Stabilization applied",
		"pa", key,
		"recommendation", recommendation,
		"current", current,
		"stabilized", stabilized,
		"windowDuration", windowDuration,
		"selectMax", selectMax,
		"historyCount", len(r.recommendations[key]))

	return stabilized
}

// cleanOldRecommendations removes recommendations older than the largest window
func (r *PodAutoscalerReconciler) cleanOldRecommendations(key string, now time.Time, scaleUpWindow, scaleDownWindow time.Duration) {
	maxWindow := scaleUpWindow
	if scaleDownWindow > maxWindow {
		maxWindow = scaleDownWindow
	}

	cutoff := now.Add(-maxWindow)
	history := r.recommendations[key]

	newHistory := history[:0]
	for _, rec := range history {
		if rec.timestamp.After(cutoff) {
			newHistory = append(newHistory, rec)
		}
	}
	r.recommendations[key] = newHistory
}

// createScalingContext creates a ScalingContext for the given PodAutoscaler
// This is the single source of truth for all PA-level configuration
func (r *PodAutoscalerReconciler) createScalingContext(pa autoscalingv1alpha1.PodAutoscaler) scalingctx.ScalingContext {
	// Create base context with defaults
	ctx := scalingctx.NewBaseScalingContext()

	// Update with PA-specific values from annotations
	if err := ctx.UpdateByPaTypes(&pa); err != nil {
		// Log error but continue with defaults
		klog.ErrorS(err, "Failed to update scaling context from PodAutoscaler, using defaults",
			"namespace", pa.Namespace, "name", pa.Name)
	}

	// Set min/max replicas
	minReplicas := int32(1)
	if pa.Spec.MinReplicas != nil {
		minReplicas = *pa.Spec.MinReplicas
	}
	ctx.SetMinReplicas(minReplicas)
	ctx.SetMaxReplicas(pa.Spec.MaxReplicas)

	return ctx
}
