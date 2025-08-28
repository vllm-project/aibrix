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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
)

const (
	ModelIdentifierKey                = constants.ModelLabelName
	ModelAdapterKey                   = "adapter.model.aibrix.ai/name"
	ModelAdapterFinalizer             = "adapter.model.aibrix.ai/finalizer"
	ModelAdapterPodTemplateLabelKey   = "adapter.model.aibrix.ai/enabled"
	ModelAdapterPodTemplateLabelValue = "true"

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

	// Available:

	// ModelAdapterAvailable is added in a ModelAdapter when it has replicas available.
	ModelAdapterAvailable = "ModelAdapterAvailable"
	// ModelAdapterUnavailable is added in a ModelAdapter when it doesn't have any pod hosting it.
	ModelAdapterUnavailable = "ModelAdapterUnavailable"

	// Inference Service path and ports
	DefaultInferenceEnginePort      = "8000"
	DefaultDebugInferenceEnginePort = "30081"
	DefaultRuntimeAPIPort           = "8080"

	ModelListPath            = "/v1/models"
	ModelListRuntimeAPIPath  = "/v1/models"
	LoadLoraAdapterPath      = "/v1/load_lora_adapter"
	LoadLoraRuntimeAPIPath   = "/v1/lora_adapter/load"
	UnloadLoraAdapterPath    = "/v1/unload_lora_adapter"
	UnloadLoraRuntimeAPIPath = "/v1/lora_adapter/unload"

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
		Complete(r)
	if err != nil {
		return err
	}

	klog.V(4).InfoS("Finished to add model-adapter-controller")
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

	// Step 1: Reconcile Pod instances for ModelAdapter based on desired replicas
	if ctrlResult, err := r.reconcileReplicas(ctx, instance); err != nil || ctrlResult.Requeue || ctrlResult.RequeueAfter > 0 {
		return ctrlResult, err
	}

	// Step 2: Reconcile Loading
	if err := r.reconcileLoading(ctx, instance); err != nil {
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

	// Get desired replicas (default to 1 if not specified)
	desiredReplicas := int32(1)
	if instance.Spec.Replicas != nil {
		desiredReplicas = *instance.Spec.Replicas
	}

	currentReplicas := int32(len(instance.Status.Instances))

	// Scale up if needed
	if currentReplicas < desiredReplicas {
		// Get pods that are not yet scheduled
		unscheduledPods := []corev1.Pod{}
		for _, pod := range activePods {
			if !StringInSlice(instance.Status.Instances, pod.Name) {
				unscheduledPods = append(unscheduledPods, pod)
			}
		}

		// Schedule additional pods
		neededReplicas := int(desiredReplicas - currentReplicas)
		if len(unscheduledPods) >= neededReplicas {
			newPods, err := r.schedulePods(ctx, instance, unscheduledPods, neededReplicas)
			if err != nil {
				return ctrl.Result{}, err
			}

			for _, pod := range newPods {
				instance.Status.Instances = append(instance.Status.Instances, pod.Name)
			}

			instance.Status.Phase = modelv1alpha1.ModelAdapterScheduled
			condition := NewCondition(string(modelv1alpha1.ModelAdapterConditionTypeScheduled), metav1.ConditionTrue,
				"Scheduled", fmt.Sprintf("ModelAdapter %s has been allocated to %d pods: %v", klog.KObj(instance), len(instance.Status.Instances), instance.Status.Instances))
			if err := r.updateStatus(ctx, instance, condition); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		} else if len(unscheduledPods) > 0 {
			// Not enough pods available, schedule what we can
			klog.Warningf("Only %d pods available for model adapter %s, need %d more", len(unscheduledPods), klog.KObj(instance), neededReplicas)
		}
	} else if currentReplicas > desiredReplicas {
		// Scale down - remove excess instances
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

func (r *ModelAdapterReconciler) reconcileLoading(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	if len(instance.Status.Instances) == 0 {
		return nil
	}

	for _, podName := range instance.Status.Instances {
		targetPod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: podName}, targetPod); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("pod %s/%s can not be found, skip loading", instance.GetName(), podName)
			}
			return err
		}

		if targetPod.DeletionTimestamp != nil {
			klog.V(4).Infof("Skipping pod %s/%s because it is being deleted", targetPod.Namespace, targetPod.Name)
			continue
		}

		urls := BuildURLs(targetPod.Status.PodIP, r.RuntimeConfig)

		exists, err := r.modelAdapterExists(urls.ListModelsURL, instance)
		if err != nil {
			return err
		}
		if exists {
			klog.V(4).Info("LoRA model has been registered previously, skipping registration")
			continue
		}

		if err := r.loadModelAdapter(urls.LoadAdapterURL, instance); err != nil {
			return err
		}
	}

	return nil
}

// Separate method to check if the model already exists
func (r *ModelAdapterReconciler) modelAdapterExists(url string, instance *modelv1alpha1.ModelAdapter) (bool, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, err
	}
	// Check if "api-key" exists in the map and set the Authorization header accordingly
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	c := &http.Client{}
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("failed to get models: %s", body)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return false, err
	}

	data, ok := response["data"].([]interface{})
	if !ok {
		return false, errors.New("invalid data format")
	}

	for _, item := range data {
		model, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if model["id"] == instance.Name {
			return true, nil
		}
	}

	return false, nil
}

// Separate method to load the LoRA adapter
func (r *ModelAdapterReconciler) loadModelAdapter(url string, instance *modelv1alpha1.ModelAdapter) error {
	artifactURL := instance.Spec.ArtifactURL
	if strings.HasPrefix(instance.Spec.ArtifactURL, "huggingface://") {
		var err error
		artifactURL, err = extractHuggingFacePath(instance.Spec.ArtifactURL)
		if err != nil {
			// Handle error, e.g., log it and return
			klog.ErrorS(err, "Invalid artifact URL", "artifactURL", artifactURL)
			return err
		}
	}
	// TODO: extend to other artifacts

	payload := map[string]string{
		"lora_name": instance.Name,
		"lora_path": artifactURL,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Check if "api-key" exists in the map and set the Authorization header accordingly
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to load LoRA adapter: %s", body)
	}

	return nil
}

// unloadModelAdapter unloads the loras from inference engines
// base model pod could be deleted, in this case, we just do optimistic unloading. It only returns some necessary errors and http errors should not be returned.
func (r *ModelAdapterReconciler) unloadModelAdapter(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	if len(instance.Status.Instances) == 0 {
		klog.Warningf("model adapter %s/%s has not been deployed to any pods yet, skip unloading", instance.GetNamespace(), instance.GetName())
		return nil
	}

	payload := map[string]string{
		"lora_name": instance.Name,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
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

		urls := BuildURLs(targetPod.Status.PodIP, r.RuntimeConfig)
		req, err := http.NewRequest("POST", urls.UnloadAdapterURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		}

		httpClient := &http.Client{}
		resp, err := httpClient.Do(req)
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

	payload := map[string]string{
		"lora_name": instance.Name,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	urls := BuildURLs(targetPod.Status.PodIP, r.RuntimeConfig)
	req, err := http.NewRequest("POST", urls.UnloadAdapterURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token, ok := instance.Spec.AdditionalConfig["api-key"]; ok {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
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

	return false
}
