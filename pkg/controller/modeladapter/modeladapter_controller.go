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

package modeladapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/controller/modeladapter/scheduling"
	"github.com/vllm-project/aibrix/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ModelIdentifierKey                = constants.ModelLabelName
	ModelAdapterKey                   = "adapter.model.aibrix.ai/name"
	ModelAdapterFinalizer             = "adapter.model.aibrix.ai/finalizer"
	ModelAdapterPodTemplateLabelKey   = "adapter.model.aibrix.ai/enabled"
	ModelAdapterPodTemplateLabelValue = "true"

	DefaultResyncInterval = 10 * time.Second

	// Retry configuration constants
	MaxLoadingRetries       = 5
	RetryBackoffSeconds     = 5
	PodReadinessTimeoutSecs = 60
	HTTPTimeoutSeconds      = 30

	// Annotation keys for tracking retry state
	RetryCountAnnotationKey        = "adapter.model.aibrix.ai/retry-count"
	LastRetryTimeAnnotationKey     = "adapter.model.aibrix.ai/last-retry-time"
	PodReadinessCheckAnnotationKey = "adapter.model.aibrix.ai/pod-readiness-check-time"
	ScheduledPodsAnnotationKey     = "adapter.model.aibrix.ai/scheduled-pods"

	// Reasons for model adapter conditions
	// Processing:

	// ModelAdapterInitializedReason is added in model adapter when it comes into the reconciliation loop.
	ModelAdapterInitializedReason = "ModelAdapterPending"
	// FailedServiceCreateReason is added in a model adapter when it cannot create a new service.
	FailedServiceCreateReason = "ServiceCreateError"
	// FailedEndpointSliceCreateReason is added in a model adapter when it cannot create a new replica set.
	FailedEndpointSliceCreateReason = "EndpointSliceCreateError"
	// ModelAdapterLoadingErrorReason is added in a model adapter when it cannot be loaded in an engine pod.
	ModelAdapterLoadingErrorReason = "ModelAdapterLoadingError"
	// ValidationFailedReason is added when model adapter object fails the validation
	ValidationFailedReason = "ValidationFailed"
	// StableInstanceFoundReason is added if there's stale pod and instance has been deleted successfully.
	StableInstanceFoundReason = "StableInstanceFound"
	// ConditionNotReason is added when there's no condition found in the cluster.
	ConditionNotReason = "ConditionNotFound"
	// PodNotReadyReason is added when a pod is not ready for adapter loading
	PodNotReadyReason = "PodNotReady"
	// MaxRetriesExceededReason is added when max retries are exceeded for a pod
	MaxRetriesExceededReason = "MaxRetriesExceeded"

	// Available:

	// ModelAdapterAvailable is added in a ModelAdapter when it has replicas available.
	ModelAdapterAvailable = "ModelAdapterAvailable"
	// ModelAdapterUnavailable is added in a ModelAdapter when it doesn't have any pod hosting it.
	ModelAdapterUnavailable = "ModelAdapterUnavailable"

	// Inference Service path and ports
	DefaultInferenceEnginePort      = "8000"
	DefaultDebugInferenceEnginePort = "30081"
	DefaultRuntimeAPIPort           = "8080"

	// DefaultModelAdapterSchedulerPolicy is the default scheduler policy for ModelAdapter Controller.
	DefaultModelAdapterSchedulerPolicy = "leastAdapters"
)

var (
	controllerKind         = modelv1alpha1.GroupVersion.WithKind("ModelAdapter")
	controllerName         = "model-adapter-controller"
	defaultRequeueDuration = 3 * time.Second
)

type URLConfig struct {
	BaseURL          string
	ListModelsURL    string
	LoadAdapterURL   string
	UnloadAdapterURL string
}

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
	cacher := mgr.GetCache()

	podInformer, err := cacher.GetInformer(context.TODO(), &corev1.Pod{})
	if err != nil {
		return nil, err
	}

	serviceInformer, err := cacher.GetInformer(context.TODO(), &corev1.Service{})
	if err != nil {
		return nil, err
	}

	endpointSliceInformer, err := cacher.GetInformer(context.TODO(), &discoveryv1.EndpointSlice{})
	if err != nil {
		return nil, err
	}

	// Let's generate the clientset and use ModelAdapterLister here as well.
	podLister := corelisters.NewPodLister(podInformer.(toolscache.SharedIndexInformer).GetIndexer())
	serviceLister := corelisters.NewServiceLister(serviceInformer.(toolscache.SharedIndexInformer).GetIndexer())
	endpointSliceLister := discoverylisters.NewEndpointSliceLister(endpointSliceInformer.(toolscache.SharedIndexInformer).GetIndexer())

	// init scheduler
	c, err := cache.Get()
	if err != nil {
		klog.Fatal(err)
	}

	scheduler, err := scheduling.NewScheduler(runtimeConfig.ModelAdapterOpt.SchedulerPolicyName, c)
	if err != nil {
		return nil, err
	}

	reconciler := &ModelAdapterReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		PodLister:           podLister,
		ServiceLister:       serviceLister,
		EndpointSliceLister: endpointSliceLister,
		Recorder:            mgr.GetEventRecorderFor(controllerName),
		scheduler:           scheduler,
		RuntimeConfig:       runtimeConfig,
		resyncInterval:      DefaultResyncInterval,
		eventCh:             make(chan event.GenericEvent),
		loraClient: &loraClient{
			RuntimeConfig: runtimeConfig,
		},
	}
	return reconciler, nil
}

func podWithLabelFilter(labelKey, labelValue, modelIdKey string) predicate.Predicate {
	hasLabelAndModelIdentifier := func(labels map[string]string, labelKey, labelValue, modelIdentifierKey string) bool {
		if _, exists := labels[modelIdentifierKey]; !exists {
			return false
		}
		return labels[labelKey] == labelValue
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey, labelValue, modelIdKey)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasLabelAndModelIdentifier(e.ObjectNew.GetLabels(), labelKey, labelValue, modelIdKey)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey, labelValue, modelIdKey)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return hasLabelAndModelIdentifier(e.Object.GetLabels(), labelKey, labelValue, modelIdKey)
		},
	}
}

func lookupLinkedModelAdapterInNamespace(c client.Client) handler.MapFunc {
	return func(ctx context.Context, a client.Object) []reconcile.Request {
		modelAdapterList := &modelv1alpha1.ModelAdapterList{}
		if err := c.List(ctx, modelAdapterList, client.InNamespace(a.GetNamespace())); err != nil {
			klog.ErrorS(err, "unable to list model adapters in namespace", "namespace", a.GetNamespace())
			return []reconcile.Request{}
		}

		requests := make([]reconcile.Request, 0, len(modelAdapterList.Items))
		for _, modelAdapter := range modelAdapterList.Items {
			// Originally, we think it's better to check model adapter.Status.Instances, if it includes the pod name, then we put the model adapter into the queue
			// However, there's other cases like pending model adapter needs to wait for new pods to be scheduled immediately, so we should reconcile all adapters if there're new pods added
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: a.GetNamespace(), Name: modelAdapter.GetName()}})
		}

		return requests
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Build raw source for periodical requeue events from event channel
	reconciler := r.(*ModelAdapterReconciler)
	src := source.Channel(reconciler.eventCh, &handler.EnqueueRequestForObject{})

	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&modelv1alpha1.ModelAdapter{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.LabelChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(lookupLinkedModelAdapterInNamespace(mgr.GetClient())),
			builder.WithPredicates(podWithLabelFilter(ModelAdapterPodTemplateLabelKey, ModelAdapterPodTemplateLabelValue, ModelIdentifierKey))).
		WatchesRawSource(src).
		Complete(r)
	if err != nil {
		return err
	}

	klog.InfoS("Finished to add model-adapter-controller")

	errChan := make(chan error)
	go reconciler.Run(context.Background(), errChan)
	klog.InfoS("Run model-adapter-reconciler periodical sync started successfully")

	go func() {
		for err := range errChan {
			klog.ErrorS(err, "Run model-adapter-reconciler periodical sync returned an error")
		}
	}()

	return nil
}

var _ reconcile.Reconciler = &ModelAdapterReconciler{}

// ModelAdapterReconciler reconciles a ModelAdapter object
type ModelAdapterReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	scheduler scheduling.Scheduler
	// PodLister is able to list/get pods from a shared informer's cache store
	PodLister corelisters.PodLister
	// ServiceLister is able to list/get services from a shared informer's cache store
	ServiceLister corelisters.ServiceLister
	// EndpointSliceLister is able to list/get services from a shared informer's cache store
	EndpointSliceLister discoverylisters.EndpointSliceLister
	RuntimeConfig       config.RuntimeConfig

	resyncInterval time.Duration
	eventCh        chan event.GenericEvent

	loraClient *loraClient
}

//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modeladapters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modeladapters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=model.aibrix.ai,resources=modeladapters/finalizers,verbs=update

// Reconcile reads that state of ModelAdapter object and makes changes based on the state read
// and what is in the ModelAdapter.Spec
func (r *ModelAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(4).InfoS("Starting to process ModelAdapter", "modelAdapter", req.NamespacedName)

	// Fetch the ModelAdapter instance
	modelAdapter := &modelv1alpha1.ModelAdapter{}
	err := r.Get(ctx, req.NamespacedName, modelAdapter)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.
			// For service, endpoint objects, clean up the resources using finalizers
			klog.InfoS("ModelAdapter resource not found. Ignoring since object mush be deleted", "modelAdapter", req.NamespacedName)
			return reconcile.Result{}, nil
		}

		// Error reading the object and let's requeue the request
		klog.ErrorS(err, "Failed to get ModelAdapter", "ModelAdapter", klog.KObj(modelAdapter))
		return reconcile.Result{}, err
	}

	if modelAdapter.ObjectMeta.DeletionTimestamp.IsZero() {
		// the object is not being deleted, so if it does not have the finalizer,
		// then lets add the finalizer and update the object.
		if !controllerutil.ContainsFinalizer(modelAdapter, ModelAdapterFinalizer) {
			klog.InfoS("Adding finalizer for ModelAdapter", "ModelAdapter", klog.KObj(modelAdapter))
			if ok := controllerutil.AddFinalizer(modelAdapter, ModelAdapterFinalizer); !ok {
				klog.Error("Failed to add finalizer for ModelAdapter")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, modelAdapter); err != nil {
				klog.Error("Failed to update custom resource to add finalizer")
				return ctrl.Result{}, err
			}
		}
	} else {
		// the object is being deleted
		if controllerutil.ContainsFinalizer(modelAdapter, ModelAdapterFinalizer) {
			// the finalizer is present, so let's unload lora from those inference engines
			// note: the base model pod could be deleted as well, so here we do best effort offloading
			// we do not need to reconcile the object if it encounters the unloading error.
			if err := r.unloadModelAdapter(ctx, modelAdapter); err != nil {
				return ctrl.Result{}, err
			}
			if ok := controllerutil.RemoveFinalizer(modelAdapter, ModelAdapterFinalizer); !ok {
				klog.Error("Failed to remove finalizer for ModelAdapter")
				return ctrl.Result{Requeue: true}, nil
			}
			if err := r.Update(ctx, modelAdapter); err != nil {
				klog.Error("Failed to update custom resource to remove finalizer")
				return ctrl.Result{}, err
			}
		}
		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	return r.DoReconcile(ctx, req, modelAdapter)
}

func (r *ModelAdapterReconciler) Run(ctx context.Context, errChan chan<- error) {
	ticker := time.NewTicker(r.resyncInterval)
	defer ticker.Stop()
	defer close(r.eventCh)

	for {
		select {
		case <-ticker.C:
			// periodically sync all modeladapter objects
			klog.V(4).Info("enqueue all modeladapter objects")
			if err := r.enqueueModelAdapters(ctx); err != nil {
				klog.ErrorS(err, "Failed to enqueue modeladapter objects")
				errChan <- err
			}
		case <-ctx.Done():
			klog.Info("context done, stopping running periodically sync modeladapter")
			errChan <- ctx.Err()
			return
		}
	}
}

func (r *ModelAdapterReconciler) enqueueModelAdapters(ctx context.Context) error {
	modelAdapterList := &modelv1alpha1.ModelAdapterList{}
	if err := r.List(ctx, modelAdapterList); err != nil {
		return err
	}
	for _, ma := range modelAdapterList.Items {
		// Let's operate the queue and just enqueue the object, that should be ok.
		e := event.GenericEvent{
			Object: &ma,
		}
		r.eventCh <- e
	}
	return nil
}

func (r *ModelAdapterReconciler) DoReconcile(ctx context.Context, req ctrl.Request, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	// Let's set the initial status when no status is available
	if instance.Status.Conditions == nil || len(instance.Status.Conditions) == 0 {
		instance.Status.Phase = modelv1alpha1.ModelAdapterPending
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeInitialized), metav1.ConditionUnknown,
			ModelAdapterInitializedReason, "Starting reconciliation")
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			return reconcile.Result{}, err
		} else {
			return reconcile.Result{Requeue: true}, nil
		}
	}

	oldInstance := instance.DeepCopy()

	// Save old instances list before reconcileReplicas potentially modifies it
	// This is needed to detect pod removal in reconcileLoading
	oldInstances := make([]string, len(instance.Status.Instances))
	copy(oldInstances, instance.Status.Instances)

	// Step 1: Reconcile Pod instances for ModelAdapter based on desired replicas
	if ctrlResult, err := r.reconcileReplicas(ctx, instance); err != nil || ctrlResult.Requeue || ctrlResult.RequeueAfter > 0 {
		return ctrlResult, err
	}

	// Step 2: Reconcile Loading (pass oldInstances to detect pod removal)
	if err := r.reconcileLoading(ctx, instance, oldInstances); err != nil {
		// retry any of the failure.
		instance.Status.Phase = modelv1alpha1.ModelAdapterBound
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeBound), metav1.ConditionFalse,
			ModelAdapterLoadingErrorReason, fmt.Sprintf("ModelAdapter %s is loaded", klog.KObj(instance)))
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
	}

	// Step 3: Reconcile Service
	if ctrlResult, err := r.reconcileService(ctx, instance); err != nil {
		instance.Status.Phase = modelv1alpha1.ModelAdapterResourceCreated
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated), metav1.ConditionFalse,
			FailedServiceCreateReason, "service creation failure")
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			klog.InfoS("Got error when updating status", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{}, err
		}
		return ctrlResult, err
	}

	// Step 4: Reconcile EndpointSlice
	if ctrlResult, err := r.reconcileEndpointSlice(ctx, instance); err != nil {
		instance.Status.Phase = modelv1alpha1.ModelAdapterResourceCreated
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated), metav1.ConditionFalse,
			FailedEndpointSliceCreateReason, "endpointslice creation failure")
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			klog.InfoS("Got error when updating status", "error", err, "ModelAdapter", instance)
			return ctrl.Result{}, err
		}
		return ctrlResult, err
	}

	// Check if we need to update the status.
	if r.inconsistentModelAdapterStatus(oldInstance.Status, instance.Status) {
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionTrue,
			ModelAdapterAvailable, fmt.Sprintf("ModelAdapter %s is ready", klog.KObj(instance)))
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			return reconcile.Result{}, fmt.Errorf("update modelAdapter status error: %v", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) updateStatus(ctx context.Context, instance *modelv1alpha1.ModelAdapter, conditions ...metav1.Condition) error {
	changed := false
	for _, condition := range conditions {
		if meta.SetStatusCondition(&instance.Status.Conditions, condition) {
			changed = true
		}
	}
	// TODO: sort the conditions based on LastTransitionTime if needed.
	klog.InfoS("model adapter reconcile", "Update CR status", instance.Name, "changed", changed, "status", instance.Status, "conditions", conditions)
	return r.Status().Update(ctx, instance)
}

func (r *ModelAdapterReconciler) clearModelAdapterInstanceList(ctx context.Context, instance *modelv1alpha1.ModelAdapter, stalePodName string) error {
	instance.Status.Instances = RemoveInstanceFromList(instance.Status.Instances, stalePodName)
	// remove instance means the lora has not targets at this moment.
	instance.Status.Phase = modelv1alpha1.ModelAdapterPending
	condition := NewCondition(string(modelv1alpha1.ModelAdapterFailed), metav1.ConditionTrue,
		StableInstanceFoundReason,
		fmt.Sprintf("Pod (%s/%s) is stale or invalid for model adapter (%s/%s), clean up the list", instance.GetNamespace(), stalePodName, instance.GetNamespace(), instance.Name))

	// We also need to update the scheduling and ready status to false
	// When the pod get migrated, we need to update the status with latest LastTransitionTime.
	// However, meta.SetStatusCondition won't update the status unless there's other change like Status as well.
	// Here it also help trigger time update in later updates.
	scheduleCondition := meta.FindStatusCondition(instance.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionTypeScheduled))
	if scheduleCondition == nil {
		// if not found, we add one
		scheduleCondition = &metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionTypeScheduled),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             ConditionNotReason,
			Message:            "Condition was missing and initialized by controller",
		}
	} else {
		scheduleCondition.Status = metav1.ConditionFalse
		scheduleCondition.LastTransitionTime = metav1.Now()
	}

	readyCondition := meta.FindStatusCondition(instance.Status.Conditions, string(modelv1alpha1.ModelAdapterConditionReady))
	if readyCondition == nil {
		// if not found, we add one
		readyCondition = &metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionReady),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             ConditionNotReason,
			Message:            "Condition was missing and initialized by controller",
		}
	} else {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.LastTransitionTime = metav1.Now()
	}

	if err := r.updateStatus(ctx, instance, condition, *scheduleCondition, *readyCondition); err != nil {
		return err
	}

	return nil
}

// getActivePodsForModelAdapter retrieves all pods matching the selector and filters them to only include active ones
func (r *ModelAdapterReconciler) getActivePodsForModelAdapter(ctx context.Context, instance *modelv1alpha1.ModelAdapter) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.GetNamespace()),
		client.MatchingLabels(instance.Spec.PodSelector.MatchLabels),
	}

	// List all pods matching the label selector
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, err
	}

	// Filter out terminating or not ready pods
	var activePods []corev1.Pod
	for _, pod := range podList.Items {
		if !utils.IsPodTerminating(&pod) && utils.IsPodReady(&pod) {
			activePods = append(activePods, pod)
		}
	}

	return activePods, nil
}

// reconcileReplicas ensures the desired number of replicas are scheduled
func (r *ModelAdapterReconciler) reconcileReplicas(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	// Get all active pods matching the selector
	activePods, err := r.getActivePodsForModelAdapter(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update candidates count
	instance.Status.Candidates = int32(len(activePods))

	// Create a map of active pods for quick lookup
	activeMap := make(map[string]corev1.Pod, len(activePods))
	for _, p := range activePods {
		activeMap[p.Name] = p
	}

	// Remove instances that are no longer active
	var validInstances []string
	for _, name := range instance.Status.Instances {
		if _, ok := activeMap[name]; ok {
			validInstances = append(validInstances, name)
		}
	}
	instance.Status.Instances = validInstances

	// Determine mode: nil = load on all, 1 = single pod
	loadOnAll := instance.Spec.Replicas == nil

	if loadOnAll {
		// Mode: Load on ALL matching pods
		instance.Status.DesiredReplicas = instance.Status.Candidates
		return r.reconcileLoadOnAllPods(ctx, instance, activePods, activeMap)
	} else {
		// Mode: Load on single pod (existing logic with scheduler)
		instance.Status.DesiredReplicas = 1
		return r.reconcileLoadOnSinglePod(ctx, instance, activePods, activeMap)
	}
}

// reconcileLoadOnAllPods ensures the adapter is loaded on all matching pods
func (r *ModelAdapterReconciler) reconcileLoadOnAllPods(ctx context.Context, instance *modelv1alpha1.ModelAdapter, activePods []corev1.Pod, activeMap map[string]corev1.Pod) (ctrl.Result, error) {
	// No scheduling needed - just ensure all active pods are candidates for loading
	// The actual loading happens in reconcileLoading
	klog.V(4).InfoS("Load-on-all mode: adapter will be loaded on all matching pods", "ModelAdapter", klog.KObj(instance), "targetPods", len(activePods))
	return ctrl.Result{}, nil
}

// reconcileLoadOnSinglePod ensures the adapter is loaded on a single selected pod
func (r *ModelAdapterReconciler) reconcileLoadOnSinglePod(ctx context.Context, instance *modelv1alpha1.ModelAdapter, activePods []corev1.Pod, activeMap map[string]corev1.Pod) (ctrl.Result, error) {
	currentReplicas := int32(len(instance.Status.Instances))
	desiredReplicas := int32(1)

	// Scale up if needed
	if currentReplicas < desiredReplicas {
		// Get pods that are not yet scheduled and are truly ready for scheduling
		candidatePods := []corev1.Pod{}
		for _, pod := range activePods {
			if !StringInSlice(instance.Status.Instances, pod.Name) {
				// Only consider pods that are ready and have been stable for a reasonable time
				if r.isPodReadyForScheduling(ctx, instance, &pod) {
					candidatePods = append(candidatePods, pod)
				}
			}
		}

		// Schedule additional pods
		neededReplicas := int(desiredReplicas - currentReplicas)
		if len(candidatePods) >= neededReplicas {
			selectedPods, err := r.schedulePods(ctx, instance, candidatePods, neededReplicas)
			if err != nil {
				return ctrl.Result{}, err
			}

			// Persist the scheduling decision in annotations
			r.setScheduledPods(instance, getPodNames(selectedPods))
			klog.InfoS("Selected pods for adapter scheduling", "ModelAdapter", klog.KObj(instance), "selectedPods", getPodNames(selectedPods))

			instance.Status.Phase = modelv1alpha1.ModelAdapterScheduled
			condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionTrue,
				"Scheduled", fmt.Sprintf("ModelAdapter %s has selected %d pods for scheduling: %v", klog.KObj(instance), len(selectedPods), getPodNames(selectedPods)))
			if err := r.updateStatus(ctx, instance, condition); err != nil {
				return ctrl.Result{}, err
			}
			// Continue to loading phase instead of returning early
		} else if len(candidatePods) > 0 {
			// Some pods available but not enough, try with what we have
			klog.Infof("Only %d ready pods available for model adapter %s, need %d more, will wait", len(candidatePods), klog.KObj(instance), neededReplicas)
			return ctrl.Result{RequeueAfter: time.Duration(RetryBackoffSeconds) * time.Second}, nil
		} else {
			// No ready pods available, wait for pods to become ready
			klog.Infof("No ready pods available for model adapter %s, waiting for pods to become ready", klog.KObj(instance))
			return ctrl.Result{RequeueAfter: time.Duration(RetryBackoffSeconds) * time.Second}, nil
		}
	} else if currentReplicas > desiredReplicas {
		// Scale down - remove excess instances (shouldn't happen with replicas=1, but handle it)
		excessCount := int(currentReplicas - desiredReplicas)
		removedInstances := instance.Status.Instances[len(instance.Status.Instances)-excessCount:]
		instance.Status.Instances = instance.Status.Instances[:len(instance.Status.Instances)-excessCount]

		// Unload adapters from removed instances
		for _, podName := range removedInstances {
			if err := r.unloadModelAdapterFromPod(ctx, instance, podName); err != nil {
				klog.Warningf("Failed to unload adapter from pod %s: %v", podName, err)
			}
		}

		instance.Status.Phase = modelv1alpha1.ModelAdapterScaled
		condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionTrue,
			"Scaled", fmt.Sprintf("ModelAdapter %s scaled to %d replicas", klog.KObj(instance), desiredReplicas))
		if err := r.updateStatus(ctx, instance, condition); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{}, nil
}

// schedulePods selects multiple pods to schedule the model adapter based on the configured scheduler policy
func (r *ModelAdapterReconciler) schedulePods(ctx context.Context, instance *modelv1alpha1.ModelAdapter, availablePods []corev1.Pod, count int) ([]corev1.Pod, error) {
	if count <= 0 || len(availablePods) == 0 {
		return nil, nil
	}

	selectedPods := []corev1.Pod{}
	remainingPods := append([]corev1.Pod{}, availablePods...)

	for i := 0; i < count && len(remainingPods) > 0; i++ {
		pod, err := r.scheduler.SelectPod(ctx, instance.Name, remainingPods)
		if err != nil {
			return nil, err
		}

		selectedPods = append(selectedPods, *pod)

		// Remove selected pod from remaining pods to avoid selecting it again
		for j, p := range remainingPods {
			if p.Name == pod.Name {
				remainingPods = append(remainingPods[:j], remainingPods[j+1:]...)
				break
			}
		}
	}

	return selectedPods, nil
}

func (r *ModelAdapterReconciler) reconcileLoading(ctx context.Context, instance *modelv1alpha1.ModelAdapter, oldInstances []string) error {
	// Get all active pods matching the selector to determine loading targets
	activePods, err := r.getActivePodsForModelAdapter(ctx, instance)
	if err != nil {
		return err
	}

	if len(activePods) == 0 {
		klog.V(4).InfoS("No active pods found for ModelAdapter", "ModelAdapter", klog.KObj(instance))
		return nil
	}

	// Create a map of active pods for quick lookup
	activeMap := make(map[string]corev1.Pod, len(activePods))
	for _, p := range activePods {
		activeMap[p.Name] = p
	}

	// Use desired replicas from status (set by reconcileReplicas)
	desiredReplicas := instance.Status.DesiredReplicas

	// Track successful loadings and if we need to update conditions
	successfulLoadings := 0
	var loadingErrors []string

	// Detect pod removal by comparing oldInstances with current instances
	// reconcileReplicas may have already cleaned up the instances list, so we need
	// to compare with the saved oldInstances to detect removals
	podRemoved := len(oldInstances) > len(instance.Status.Instances)

	// Count successful loadings from current instances.
	// The instance.Status.Instances list has already been filtered by reconcileReplicas
	// to only contain healthy, active pods, so we can directly use its length.
	successfulLoadings = len(instance.Status.Instances)

	// If pod was removed and instances is now empty, update conditions to reflect transition state
	if podRemoved && len(instance.Status.Instances) == 0 {
		// Mark Ready as False since no adapter instances are currently running
		readyCondition := NewCondition(string(modelv1alpha1.ModelAdapterConditionReady), metav1.ConditionFalse,
			"AdapterMigrating", "Adapter pod removed, migrating to new pod")

		// Mark Scheduled as False since we need to reschedule
		scheduledCondition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionFalse,
			"Rescheduling", "Need to reschedule adapter on available pods")

		if err := r.updateStatus(ctx, instance, readyCondition, scheduledCondition); err != nil {
			return err
		}
	}

	// Try to load on additional pods if we need more replicas
	if successfulLoadings < int(desiredReplicas) {
		neededReplicas := int(desiredReplicas) - successfulLoadings

		// First, try to load on scheduled pods from annotations (respecting scheduler decision for single-pod mode)
		scheduledPods := r.getScheduledPods(instance)
		candidatePods := []corev1.Pod{}

		// Prioritize scheduled pods (relevant for single-pod mode)
		for _, pod := range activePods {
			if !StringInSlice(instance.Status.Instances, pod.Name) && r.isPodHealthy(&pod) {
				if StringInSlice(scheduledPods, pod.Name) {
					// Insert scheduled pods at the beginning (higher priority)
					candidatePods = append([]corev1.Pod{pod}, candidatePods...)
				} else {
					// Add other pods at the end
					candidatePods = append(candidatePods, pod)
				}
			}
		}

		// Try to load on candidate pods
		loadedCount := 0
		for _, pod := range candidatePods {
			if loadedCount >= neededReplicas {
				break
			}

			success, shouldRetry, err := r.tryLoadModelAdapterOnPod(ctx, instance, &pod)
			if success {
				// Only add to instances list after successful loading
				instance.Status.Instances = append(instance.Status.Instances, pod.Name)
				loadedCount++
				klog.InfoS("Successfully loaded adapter on pod", "pod", pod.Name, "ModelAdapter", klog.KObj(instance))

				// Clear scheduled pods annotation once we have successful loadings
				if loadedCount == 1 {
					r.clearScheduledPods(instance)

					// If this is a recovery from pod removal, update Scheduled condition back to True
					if podRemoved {
						scheduledCondition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionTrue,
							"Rescheduled", fmt.Sprintf("Adapter successfully loaded on new pod %s", pod.Name))

						if err := r.updateStatus(ctx, instance, scheduledCondition); err != nil {
							klog.ErrorS(err, "Failed to update Scheduled condition after successful loading", "ModelAdapter", klog.KObj(instance))
						}
					}
				}
			} else if shouldRetry {
				// Log the error but continue trying other pods
				loadingErrors = append(loadingErrors, fmt.Sprintf("pod %s: %v", pod.Name, err))
				klog.V(4).InfoS("Loading failed on pod, will retry later", "pod", pod.Name, "error", err)
			} else {
				// Max retries exceeded for this pod, don't try again
				loadingErrors = append(loadingErrors, fmt.Sprintf("pod %s: max retries exceeded", pod.Name))
				klog.InfoS("Max retries exceeded for pod", "pod", pod.Name, "ModelAdapter", klog.KObj(instance))
			}
		}
	} else {
		// Make sure existing instances are in correct state
		// in case lora model removed by engine then load it back.
		for _, podName := range instance.Status.Instances {
			if pod, ok := activeMap[podName]; ok {
				success, shouldRetry, err := r.tryLoadModelAdapterOnPod(ctx, instance, &pod)
				if !success {
					if shouldRetry {
						// Log the error but continue trying other pods
						loadingErrors = append(loadingErrors, fmt.Sprintf("pod %s: %v", podName, err))
						klog.V(4).InfoS("Loading failed on pod, will retry later", "pod", podName, "error", err)
					} else {
						// Max retries exceeded for this pod, don't try again
						loadingErrors = append(loadingErrors, fmt.Sprintf("pod %s: max retries exceeded", podName))
						klog.InfoS("Max retries exceeded for pod", "pod", podName, "ModelAdapter", klog.KObj(instance))
					}
				}
			} else {
				// This case should ideally not be reached as reconcileReplicas should have cleaned up inactive instances.
				// However, as a safeguard, we log a warning.
				klog.Warningf("Pod %s, which is in ModelAdapter instance status, was not found in the active pods list.", podName)
			}
		}
	}

	// Update ready replicas count
	instance.Status.ReadyReplicas = int32(len(instance.Status.Instances))

	// Check if we have any successful instances
	if len(instance.Status.Instances) == 0 {
		// No successful loadings, return error to trigger retry
		if len(loadingErrors) > 0 {
			return fmt.Errorf("failed to load adapter on any pods: %v", strings.Join(loadingErrors, "; "))
		}
		return fmt.Errorf("no suitable pods available for adapter loading")
	}

	klog.V(4).InfoS("ModelAdapter loading completed", "ModelAdapter", klog.KObj(instance), "instances", instance.Status.Instances, "ready", instance.Status.ReadyReplicas, "desired", instance.Status.DesiredReplicas)
	return nil
}

func (r *ModelAdapterReconciler) unloadModelAdapter(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	if len(instance.Status.Instances) == 0 {
		klog.Warningf("model adapter %s/%s has not been deployed to any pods yet, skip unloading", instance.GetNamespace(), instance.GetName())
		return nil
	}

	for _, podName := range instance.Status.Instances {
		targetPod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: podName}, targetPod); err != nil {
			if apierrors.IsNotFound(err) {
				klog.Warningf("Failed to find lora Pod instance %s/%s from apiserver, skip unloading", instance.GetNamespace(), podName)
				continue
			}
			klog.Warning("Error getting Pod from lora instance list", err)
			return err
		}

		resp, err := r.loraClient.UnloadAdapter(instance, targetPod)
		if err != nil {
			continue
		}
		func() {
			defer func() {
				if err := resp.Body.Close(); err != nil {
					klog.InfoS("Error closing response body:", err)
				}
			}()

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				klog.Warningf("failed to unload LoRA adapter: %s", body)
			}
		}()
	}

	return nil
}

// unloadModelAdapterFromPod unloads the adapter from a specific pod
func (r *ModelAdapterReconciler) unloadModelAdapterFromPod(ctx context.Context, instance *modelv1alpha1.ModelAdapter, podName string) error {
	targetPod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: podName}, targetPod); err != nil {
		if apierrors.IsNotFound(err) {
			klog.Warningf("Failed to find lora Pod instance %s/%s from apiserver, skip unloading", instance.GetNamespace(), podName)
			return nil
		}
		return err
	}

	resp, err := r.loraClient.UnloadAdapter(instance, targetPod)
	if err != nil {
		return nil // Don't fail on HTTP errors during unload
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		klog.Warningf("failed to unload LoRA adapter from pod %s: %s", podName, body)
	}

	return nil
}

func (r *ModelAdapterReconciler) reconcileService(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	// Retrieve the Service from the Kubernetes cluster with the name and namespace.
	found := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Service does not exist, create a new one
		svc := buildModelAdapterService(instance)
		// Set the owner reference
		if err := ctrl.SetControllerReference(instance, svc, r.Scheme); err != nil {
			klog.Error(err, "Failed to set controller reference to modelAdapter")
			return ctrl.Result{}, err
		}

		// create service
		klog.InfoS("Creating a new service", "service", klog.KObj(svc))
		if err = r.Create(ctx, svc); err != nil {
			klog.ErrorS(err, "Failed to create new service resource for ModelAdapter", "service", klog.KObj(svc))
			condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated), metav1.ConditionFalse,
				FailedServiceCreateReason, fmt.Sprintf("Failed to create Service for the modeladapter (%s): (%s)", klog.KObj(instance), err))
			if err := r.updateStatus(ctx, instance, condition); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, err
		}
	} else if err != nil {
		klog.ErrorS(err, "Failed to get Service")
		return ctrl.Result{}, err
	}
	// TODO: add `else` logic let's compare the service major fields and update to the target state.

	// TODO: Now, we are using the name comparison which is not enough,
	// compare the object difference in future.
	return ctrl.Result{}, nil
}
func (r *ModelAdapterReconciler) reconcileEndpointSlice(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	found := &discoveryv1.EndpointSlice{}

	podList := []corev1.Pod{}
	for _, podName := range instance.Status.Instances {
		p := corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: podName}, &p); err == nil {
			if p.DeletionTimestamp == nil {
				podList = append(podList, p)
			}
		}
	}

	if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, found); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		if len(podList) == 0 {
			return ctrl.Result{}, nil
		}

		eps := buildModelAdapterEndpointSlice(instance, podList)
		if err := ctrl.SetControllerReference(instance, eps, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, eps); err != nil {
			return ctrl.Result{}, err
		}
		instance.Status.Phase = modelv1alpha1.ModelAdapterRunning
		return ctrl.Result{}, nil
	}

	endpoints := make([]discoveryv1.Endpoint, 0, len(podList))
	for _, p := range podList {
		endpoints = append(endpoints, discoveryv1.Endpoint{Addresses: []string{p.Status.PodIP}})
	}
	found.Endpoints = endpoints
	if err := r.Update(ctx, found); err != nil {
		return ctrl.Result{}, err
	}
	instance.Status.Phase = modelv1alpha1.ModelAdapterRunning
	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) inconsistentModelAdapterStatus(oldStatus, newStatus modelv1alpha1.ModelAdapterStatus) bool {
	// Implement your logic to check if the status is inconsistent
	if oldStatus.Phase != newStatus.Phase || !equalStringSlices(oldStatus.Instances, newStatus.Instances) {
		return true
	}

	// Check new counter fields
	if oldStatus.Candidates != newStatus.Candidates ||
		oldStatus.DesiredReplicas != newStatus.DesiredReplicas ||
		oldStatus.ReadyReplicas != newStatus.ReadyReplicas {
		return true
	}

	return false
}

// isPodReadyForScheduling checks if a pod is ready and stable for scheduling
func (r *ModelAdapterReconciler) isPodReadyForScheduling(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) bool {
	if !utils.IsPodReady(pod) {
		return false
	}

	// Check if pod has been ready for reasonable time to avoid flapping
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			timeSinceReady := time.Since(condition.LastTransitionTime.Time)
			if timeSinceReady < time.Duration(RetryBackoffSeconds)*time.Second {
				klog.V(4).InfoS("Pod recently became ready, waiting for stability", "pod", pod.Name, "timeSinceReady", timeSinceReady)
				return false
			}
			break
		}
	}

	return true
}

// isPodHealthy checks if a pod is healthy and ready for adapter operations
func (r *ModelAdapterReconciler) isPodHealthy(pod *corev1.Pod) bool {
	return !utils.IsPodTerminating(pod) && utils.IsPodReady(pod)
}

// tryLoadModelAdapterOnPod attempts to load an adapter on a pod with retry logic
// Returns (success, shouldRetry, error)
func (r *ModelAdapterReconciler) tryLoadModelAdapterOnPod(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) (bool, bool, error) {
	// Get retry count from annotations
	retryCount, lastRetryTime := r.getRetryInfo(instance, pod.Name)

	// Check if we should retry based on exponential backoff
	backoffDuration := r.calculateExponentialBackoff(retryCount)
	if time.Since(lastRetryTime) < backoffDuration {
		return false, true, fmt.Errorf("waiting for exponential backoff: %v", backoffDuration)
	}

	// Check max retries
	if retryCount >= MaxLoadingRetries {
		r.recordRetryFailure(instance, pod.Name, "max retries exceeded")
		return false, false, fmt.Errorf("max retries (%d) exceeded", MaxLoadingRetries)
	}

	// Update retry info
	r.updateRetryInfo(instance, pod.Name, retryCount+1)

	_, exists, err := r.loraClient.LoadAdapter(instance, pod)

	if err != nil {
		if r.isRetriableError(err) {
			klog.V(4).InfoS("Retriable error loading adapter", "pod", pod.Name, "error", err)
			return false, true, err
		}
		// Non-retriable error
		r.recordRetryFailure(instance, pod.Name, fmt.Sprintf("Non-retriable error loading adapter: %v", err))
		return false, false, err
	}

	if exists {
		klog.V(4).InfoS("LoRA adapter already exists on pod", "pod", pod.Name)
		// Reset retry count on success
		r.clearRetryInfo(instance, pod.Name)
		return true, false, nil
	}
	// Success - reset retry count
	r.clearRetryInfo(instance, pod.Name)
	klog.InfoS("Successfully loaded adapter on pod", "pod", pod.Name, "ModelAdapter", klog.KObj(instance))
	return true, false, nil
}

// isRetriableError determines if an error should trigger a retry
func (r *ModelAdapterReconciler) isRetriableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())
	// Common retriable errors during pod startup
	retriableErrors := []string{
		"connection refused",
		"connection reset",
		"timeout",
		"no route to host",
		"network is unreachable",
		"temporary failure",
		"service unavailable",
		"bad gateway",
	}

	for _, retriable := range retriableErrors {
		if strings.Contains(errStr, retriable) {
			return true
		}
	}

	return false
}

// getRetryInfo gets retry count and last retry time from annotations
func (r *ModelAdapterReconciler) getRetryInfo(instance *modelv1alpha1.ModelAdapter, podName string) (int32, time.Time) {
	retryCountKey := fmt.Sprintf("%s.%s", RetryCountAnnotationKey, podName)
	lastRetryTimeKey := fmt.Sprintf("%s.%s", LastRetryTimeAnnotationKey, podName)

	var retryCount int32 = 0
	var lastRetryTime time.Time

	if instance.Annotations != nil {
		if countStr, exists := instance.Annotations[retryCountKey]; exists {
			if count, err := strconv.ParseInt(countStr, 10, 32); err == nil {
				retryCount = int32(count)
			}
		}
		if timeStr, exists := instance.Annotations[lastRetryTimeKey]; exists {
			if t, err := time.Parse(time.RFC3339, timeStr); err == nil {
				lastRetryTime = t
			}
		}
	}

	return retryCount, lastRetryTime
}

// updateRetryInfo updates retry count and time in annotations
func (r *ModelAdapterReconciler) updateRetryInfo(instance *modelv1alpha1.ModelAdapter, podName string, retryCount int32) {
	if instance.Annotations == nil {
		instance.Annotations = make(map[string]string)
	}

	retryCountKey := fmt.Sprintf("%s.%s", RetryCountAnnotationKey, podName)
	lastRetryTimeKey := fmt.Sprintf("%s.%s", LastRetryTimeAnnotationKey, podName)

	instance.Annotations[retryCountKey] = strconv.FormatInt(int64(retryCount), 10)
	instance.Annotations[lastRetryTimeKey] = time.Now().Format(time.RFC3339)
}

// clearRetryInfo removes retry annotations for a pod
func (r *ModelAdapterReconciler) clearRetryInfo(instance *modelv1alpha1.ModelAdapter, podName string) {
	if instance.Annotations == nil {
		return
	}

	retryCountKey := fmt.Sprintf("%s.%s", RetryCountAnnotationKey, podName)
	lastRetryTimeKey := fmt.Sprintf("%s.%s", LastRetryTimeAnnotationKey, podName)

	delete(instance.Annotations, retryCountKey)
	delete(instance.Annotations, lastRetryTimeKey)
}

// recordRetryFailure records a retry failure event
func (r *ModelAdapterReconciler) recordRetryFailure(instance *modelv1alpha1.ModelAdapter, podName string, reason string) {
	r.Recorder.Eventf(instance, corev1.EventTypeWarning, MaxRetriesExceededReason,
		"Max retries exceeded for pod %s: %s", podName, reason)
}

// getPodNames extracts pod names from a list of pods
func getPodNames(pods []corev1.Pod) []string {
	names := make([]string, len(pods))
	for i, pod := range pods {
		names[i] = pod.Name
	}
	return names
}

// setScheduledPods persists the list of scheduled pods in annotations
func (r *ModelAdapterReconciler) setScheduledPods(instance *modelv1alpha1.ModelAdapter, podNames []string) {
	if instance.Annotations == nil {
		instance.Annotations = make(map[string]string)
	}
	instance.Annotations[ScheduledPodsAnnotationKey] = strings.Join(podNames, ",")
}

// getScheduledPods retrieves the list of scheduled pods from annotations
func (r *ModelAdapterReconciler) getScheduledPods(instance *modelv1alpha1.ModelAdapter) []string {
	if instance.Annotations == nil {
		return nil
	}

	scheduledPodsStr, exists := instance.Annotations[ScheduledPodsAnnotationKey]
	if !exists || scheduledPodsStr == "" {
		return nil
	}

	return strings.Split(scheduledPodsStr, ",")
}

// clearScheduledPods removes the scheduled pods annotation
func (r *ModelAdapterReconciler) clearScheduledPods(instance *modelv1alpha1.ModelAdapter) {
	if instance.Annotations == nil {
		return
	}
	delete(instance.Annotations, ScheduledPodsAnnotationKey)
}

// calculateExponentialBackoff calculates the backoff duration using exponential backoff strategy
func (r *ModelAdapterReconciler) calculateExponentialBackoff(retryCount int32) time.Duration {
	// Exponential backoff: baseInterval * 2^retries
	// Cap the maximum backoff to avoid excessive delays
	maxBackoffSeconds := 300 // 5 minutes max

	if retryCount == 0 {
		return 0 // No backoff for first attempt
	}

	// Calculate 2^retryCount, but cap it to prevent overflow
	multiplier := int32(1 << uint(retryCount))
	if multiplier > int32(maxBackoffSeconds/RetryBackoffSeconds) {
		multiplier = int32(maxBackoffSeconds / RetryBackoffSeconds)
	}

	backoffSeconds := RetryBackoffSeconds * int(multiplier)
	return time.Duration(backoffSeconds) * time.Second
}
