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
	"time"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	"github.com/aibrix/aibrix/pkg/cache"
	"github.com/aibrix/aibrix/pkg/controller/modeladapter/scheduling"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	//ControllerUIDLabelKey = "model-adapter-controller-uid"
	ModelAdapterFinalizer = "modeladapter.aibrix.ai/finalizer"
)

var (
	controllerKind                     = modelv1alpha1.GroupVersion.WithKind("ModelAdapter")
	controllerName                     = "model-adapter-controller"
	defaultModelAdapterSchedulerPolicy = "leastAdapters"
	defaultRequeueDuration             = 1 * time.Second
)

// Add creates a new ModelAdapter Controller and adds it to the Manager with default RBAC.
// The Manager will set fields on the Controller and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
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
	c, err := cache.GetCache()
	if err != nil {
		klog.Fatal(err)
	}

	// TODO: policy should be configured by users
	scheduler, err := scheduling.NewScheduler(defaultModelAdapterSchedulerPolicy, c)
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
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		Named(controllerName).
		For(&modelv1alpha1.ModelAdapter{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add model-adapter-controller")
	return err
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
			// For service, endpoint objects, clean up the resources using finalizers/
			klog.InfoS("ModelAdapter resource not found. Ignoring since object mush be deleted", "modelAdapter", req)
			return reconcile.Result{}, nil
		}

		// Error reading the object and let's requeue the request
		klog.ErrorS(err, "Failed to get ModelAdapter", "modelAdapter", klog.KObj(modelAdapter))
		return reconcile.Result{}, err
	}

	if modelAdapter.ObjectMeta.DeletionTimestamp.IsZero() {
		// the object is not being deleted, so if it does not have the finalizer,
		// then lets add the finalizer and update the object.
		if !controllerutil.ContainsFinalizer(modelAdapter, ModelAdapterFinalizer) {
			klog.InfoS("Adding finalizer for ModelAdapter")
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
			if err := r.unloadModelAdapter(modelAdapter); err != nil {
				// if fail to delete unload lora here, return the error so it can be retried.
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
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionTypeInitialized),
			Status:             metav1.ConditionUnknown,
			Reason:             "Reconciling",
			Message:            "Starting reconciliation",
			LastTransitionTime: metav1.Now()})

		if err := r.Status().Update(ctx, instance); err != nil {
			klog.ErrorS(err, "Failed to update ModelAdapter status", "modelAdapter", klog.KObj(instance))
			return reconcile.Result{}, err
		}

		// re-fetch the custom resource after updating the status to avoid 409 error here.
		if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
			klog.Error(err, "Failed to re-fetch modelAdapter")
			return ctrl.Result{}, err
		}
	}

	oldInstance := instance.DeepCopy()

	// Step 0: Validate ModelAdapter configurations
	if err := validateModelAdapter(instance); err != nil {
		klog.Error(err, "Failed to validate the ModelAdapter")

		instance.Status.Phase = modelv1alpha1.ModelAdapterFailed
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated),
			Status:             metav1.ConditionFalse,
			Reason:             "ValidationFailed",
			Message:            "ModelAdapter resource is not valid",
			LastTransitionTime: metav1.Now()})

		if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
			klog.ErrorS(err, "Failed to update ModelAdapter status", "modelAdapter", klog.KObj(instance))
			return reconcile.Result{}, updateErr
		}

		return ctrl.Result{}, err
	}

	// Step 1: Schedule Pod for ModelAdapter
	selectedPod := &corev1.Pod{}
	existPods := false
	var err error
	if instance.Status.Instances != nil && len(instance.Status.Instances) != 0 {
		// model adapter has already been scheduled to some pods
		// check the scheduled pod first, verify the mapping is still valid.
		// TODO: this needs to be changed once we support multiple lora adapters
		selectedPodName := instance.Status.Instances[0]
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: selectedPodName}, selectedPod); err != nil && apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to get selected pod but pod is still in ModelAdapter instance list", "modelAdapter", klog.KObj(instance))

			// instance.Status.Instances has been outdated, and we need to clear the pod list
			// after the pod list is cleaned up, let's reconcile the instance object again in the next loop
			return ctrl.Result{}, r.clearModelAdapterInstanceList(ctx, instance)
		} else if err != nil {
			// failed to fetch the pod, let's requeue
			return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
		} else {
			// compare instance and model adapter labels.
			selector, err := metav1.LabelSelectorAsSelector(instance.Spec.PodSelector)
			if err != nil {
				// TODO: this should barely happen, let's move this logic to earlier validation logics.
				return ctrl.Result{}, fmt.Errorf("failed to convert pod selector: %v", err)
			}

			if !selector.Matches(labels.Set(selectedPod.Labels)) {
				klog.Warning("current assigned pod selector doesn't match model adapter selector")
				return ctrl.Result{}, r.clearModelAdapterInstanceList(ctx, instance)
			}

			existPods = true
		}
	}

	if !existPods {
		// TODO: as we plan to support lora replicas, it needs some corresponding changes.
		// it should return a list of pods in future, otherwise, it should be invoked by N times.
		selectedPod, err = r.schedulePod(ctx, instance)
		if err != nil {
			klog.ErrorS(err, "Failed to schedule Pod for ModelAdapter", "modelAdapter", instance.Name)
			return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
		}

		instance.Status.Phase = modelv1alpha1.ModelAdapterScheduling
		instance.Status.Instances = []string{selectedPod.Name}
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionTypeSelectorMatched),
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciling",
			Message:            fmt.Sprintf("ModelAdapter %s has been allocated to pod %s", klog.KObj(instance), selectedPod.Name),
			LastTransitionTime: metav1.Now(),
		})

		if err := r.Status().Update(ctx, instance); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Step 2: Reconcile Loading
	if err := r.reconcileLoading(ctx, instance, selectedPod); err != nil {
		// retry any of the failure.
		instance.Status.Phase = modelv1alpha1.ModelAdapterFailed
		if err := r.Status().Update(ctx, instance); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
		}

		return ctrl.Result{RequeueAfter: defaultRequeueDuration}, err
	}

	// Step 3: Reconcile Service
	if ctrlResult, err := r.reconcileService(ctx, instance); err != nil {
		if updateErr := r.updateModelAdapterState(ctx, instance, modelv1alpha1.ModelAdapterFailed); updateErr != nil {
			klog.ErrorS(updateErr, "ModelAdapter update state error", "cluster name", req.Name)
		}
		return ctrlResult, err
	}

	// Step 4: Reconcile EndpointSlice
	if ctrlResult, err := r.reconcileEndpointSlice(ctx, instance, selectedPod); err != nil {
		if updateErr := r.updateModelAdapterState(ctx, instance, modelv1alpha1.ModelAdapterFailed); updateErr != nil {
			klog.ErrorS(updateErr, "ModelAdapter update state error", "cluster name", req.Name)
		}
		return ctrlResult, err
	}

	// Check if need to update the status.
	if r.inconsistentModelAdapterStatus(oldInstance.Status, instance.Status) {
		klog.InfoS("model adapter reconcile", "Update CR status", req.Name, "status", instance.Status)
		instance.Status.Phase = modelv1alpha1.ModelAdapterRunning
		if err = r.updateStatus(ctx, instance); err != nil {
			return reconcile.Result{}, fmt.Errorf("update modelAdapter status error: %v", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) updateStatus(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               string(modelv1alpha1.ModelAdapterConditionReady),
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciling",
		Message:            fmt.Sprintf("ModelAdapter %s is ready", klog.KObj(instance)),
		LastTransitionTime: metav1.Now(),
	})

	return r.Status().Update(ctx, instance)
}

func (r *ModelAdapterReconciler) clearModelAdapterInstanceList(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	stalePodName := instance.Status.Instances[0]
	instance.Status.Instances = []string{}
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               string(modelv1alpha1.ModelAdapterConditionCleanup),
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciling",
		Message:            fmt.Sprintf("Pod (%s) can not be fetched for model adapter (%s), clean up the list", stalePodName, instance.Name),
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, instance); err != nil {
		klog.Error(err, "Failed to update modelAdapter status")
		return err
	}

	return nil
}

func (r *ModelAdapterReconciler) schedulePod(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (*corev1.Pod, error) {
	// Implement your scheduling logic here to select a Pod based on the instance.Spec.PodSelector
	// For the sake of example, we will just list the Pods matching the selector and pick the first one
	podList := &corev1.PodList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.Namespace),
		client.MatchingLabels(instance.Spec.PodSelector.MatchLabels),
	}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, err
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found matching selector")
	}

	return r.scheduler.SelectPod(ctx, podList.Items)
}

func (r *ModelAdapterReconciler) reconcileLoading(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) error {
	// Define the key you want to check
	key := "DEBUG_MODE"
	value, exists := getEnvKey(key)
	host := fmt.Sprintf("http://%s:8000", pod.Status.PodIP)
	if exists && value == "on" {
		// 30080 is the nodePort of the base model service.
		host = fmt.Sprintf("http://%s:30080", "localhost")
	}

	// Check if the model is already loaded
	exists, err := r.modelAdapterExists(host, instance.Name)
	if err != nil {
		return err
	}
	if exists {
		klog.V(4).Info("LoRA model has been registered previously, skipping registration")
		return nil
	}

	// Load the Model adapter
	err = r.loadModelAdapter(host, instance)
	if err != nil {
		return err
	}

	// Update the instance status
	instance.Status.Phase = modelv1alpha1.ModelAdapterBinding
	meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               string(modelv1alpha1.ModelAdapterConditionTypeScheduled),
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciling",
		Message:            fmt.Sprintf("ModelAdapter %s is loaded", klog.KObj(instance)),
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, instance); err != nil {
		klog.InfoS("Got error when updating status", "error", err, "ModelAdapter", instance)
		return err
	}

	return nil
}

// Separate method to check if the model already exists
func (r *ModelAdapterReconciler) modelAdapterExists(host, modelName string) (bool, error) {
	// TODO: /v1/models is the vllm entrypoints, let's support multiple engine in future
	url := fmt.Sprintf("%s/v1/models", host)
	resp, err := http.Get(url)
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
		if model["id"] == modelName {
			return true, nil
		}
	}

	return false, nil
}

// Separate method to load the LoRA adapter
func (r *ModelAdapterReconciler) loadModelAdapter(host string, instance *modelv1alpha1.ModelAdapter) error {
	artifactURL, err := extractHuggingFacePath(instance.Spec.ArtifactURL)
	if err != nil {
		// Handle error, e.g., log it and return
		klog.ErrorS(err, "Invalid artifact URL", "artifactURL", artifactURL)
		return err
	}

	payload := map[string]string{
		"lora_name": instance.Name,
		"lora_path": artifactURL,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/v1/load_lora_adapter", host)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

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
func (r *ModelAdapterReconciler) unloadModelAdapter(instance *modelv1alpha1.ModelAdapter) error {
	if len(instance.Status.Instances) == 0 {
		klog.Warning("model adapter has not been deployed to any pods yet, skip unloading")
		return nil
	}

	// TODO:(jiaxin.shan) Support multiple instances

	podName := instance.Status.Instances[0]
	targetPod := &corev1.Pod{}
	if err := r.Get(context.TODO(), types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      podName,
	}, targetPod); err != nil {
		if apierrors.IsNotFound(err) {
			// since the pod doesn't exist, unload is unnecessary
			return nil
		}
		klog.Warning("Error getting Pod from lora instance list", err)
		return err
	}

	payload := map[string]string{
		"lora_name": instance.Name,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s:%d/v1/unload_lora_adapter", targetPod.Status.PodIP, 8000)
	key := "DEBUG_MODE"
	value, exists := getEnvKey(key)
	if exists && value == "on" {
		// 30080 is the nodePort of the base model service.
		url = "http://localhost:30080/v1/unload_lora_adapter"
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

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
		return fmt.Errorf("failed to unload LoRA adapter: %s", body)
	}

	return nil
}

func (r *ModelAdapterReconciler) updateModelAdapterState(ctx context.Context, instance *modelv1alpha1.ModelAdapter, phase modelv1alpha1.ModelAdapterPhase) error {
	if instance.Status.Phase == phase {
		return nil
	}
	instance.Status.Phase = phase
	klog.InfoS("Update CR Status.Phase", "phase", phase)
	return r.Status().Update(ctx, instance)
}

func (r *ModelAdapterReconciler) reconcileService(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	// Retrieve the Service from the Kubernetes cluster with the name and namespace.
	found := &corev1.Service{}

	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Service does not exist, create a new one
		svc, err := buildModelAdapterService(instance)
		if err != nil {
			klog.ErrorS(err, "Failed to define new Service resource for ModelAdapter")
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated),
				Status:             metav1.ConditionFalse,
				Reason:             "Reconciling",
				Message:            fmt.Sprintf("Failed to create Service for the custom resource (%s): (%s)", instance.Name, err),
				LastTransitionTime: metav1.Now(),
			})

			if err := r.Status().Update(ctx, instance); err != nil {
				klog.Error(err, "Failed to update modelAdapter status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Set the owner reference
		if err := ctrl.SetControllerReference(instance, svc, r.Scheme); err != nil {
			klog.Error(err, "Failed to set controller reference to modelAdapter")
			return ctrl.Result{}, err
		}

		// create service
		klog.InfoS("Creating a new service", "service.namespace", svc.Namespace, "service.name", svc.Name)
		if err = r.Create(ctx, svc); err != nil {
			klog.ErrorS(err, "Failed to create new service resource", "service.namespace", svc.Namespace, "service.name", svc.Name)
			return ctrl.Result{}, err
		}
	} else if err != nil {
		klog.ErrorS(err, "Failed to get Service")
		return ctrl.Result{}, err
	}

	// TODO: Now, we are using the name comparison which is not enough,
	// compare the object difference in future.
	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) reconcileEndpointSlice(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) (ctrl.Result, error) {
	// check if the endpoint slice already exists, if not create a new one.
	found := &discoveryv1.EndpointSlice{}
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// EndpointSlice does not exist, create it
		eps, err := buildModelAdapterEndpointSlice(instance, pod)
		if err != nil {
			klog.ErrorS(err, "Failed to define new EndpointSlice resource for ModelAdapter")
			instance.Status.Phase = modelv1alpha1.ModelAdapterFailed
			meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
				Type:               string(modelv1alpha1.ModelAdapterConditionTypeResourceCreated),
				Status:             metav1.ConditionFalse,
				Reason:             "Reconciling",
				Message:            fmt.Sprintf("Failed to create EndpointSlice for the custom resource (%s): (%s)", instance.Name, err),
				LastTransitionTime: metav1.Now(),
			})

			if err := r.Status().Update(ctx, instance); err != nil {
				klog.Error(err, "Failed to update modelAdapter status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}
		// Set the owner reference
		if err := ctrl.SetControllerReference(instance, eps, r.Scheme); err != nil {
			klog.Error(err, "Failed to set controller reference to modelAdapter")
			return ctrl.Result{}, err
		}

		// create service
		klog.InfoS("Creating a new EndpointSlice", "endpointslice.namespace", eps.Namespace, "endpointslice.name", eps.Name)
		if err = r.Create(ctx, eps); err != nil {
			klog.ErrorS(err, "Failed to create new EndpointSlice resource", "endpointslice.namespace", eps.Namespace, "endpointslice.name", eps.Name)
			return ctrl.Result{}, err
		}
	} else if err != nil {
		klog.ErrorS(err, "Failed to get EndpointSlice")
		return ctrl.Result{}, err
	} else {
		// Existing EndpointSlice Found. Check if the Pod IP is already in the EndpointSlice
		podIP := pod.Status.PodIP
		alreadyExists := false
		for _, endpoint := range found.Endpoints {
			for _, address := range endpoint.Addresses {
				if address == podIP {
					alreadyExists = true
					break
				}
			}
			if alreadyExists {
				break
			}
		}

		// Append the Pod IP to the EndpointSlice if it doesn't already exist
		if !alreadyExists {
			found.Endpoints = append(found.Endpoints, discoveryv1.Endpoint{
				Addresses: []string{podIP},
			})

			if err := r.Update(ctx, found); err != nil {
				klog.ErrorS(err, "Failed to update EndpointSlice", "EndpointSlice", found.Name)
				return ctrl.Result{}, err
			}
			klog.InfoS("Successfully updated EndpointSlice", "EndpointSlice", found.Name)
		} else {
			klog.InfoS("Pod IP already exists in EndpointSlice", "PodIP", podIP)
		}
	}

	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) inconsistentModelAdapterStatus(oldStatus, newStatus modelv1alpha1.ModelAdapterStatus) bool {
	// Implement your logic to check if the status is inconsistent
	if oldStatus.Phase != newStatus.Phase || !equalStringSlices(oldStatus.Instances, newStatus.Instances) {
		return true
	}

	return false
}
