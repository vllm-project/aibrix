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
	"os"
	"time"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	clientv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	//ControllerUIDLabelKey = "model-adapter-controller-uid"
	ModelAdapterFinalizer = "model-adapter-finalizer"
)

var (
	//controllerKind         = modelv1alpha1.GroupVersion.WithKind("ModelAdapter")
	DefaultRequeueDuration = 1 * time.Second
)

// Add creates a new ModelAdapter Controller and adds it to the Manager with default RBAC.
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

	// TODO: mgr.GetClient() gives us the controller-runtime client but here we need a client-go client. Find other ways instead.
	// get kubernetes client from manager
	config := mgr.GetConfig()
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Unable to create Kubernetes client: %v", err)
	}

	// Do we still need this?
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&clientv1core.EventSinkImpl{Interface: k8sClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(mgr.GetScheme(), corev1.EventSource{Component: "model-adapter-controller"})

	reconciler := &ModelAdapterReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		PodLister:           podLister,
		ServiceLister:       serviceLister,
		EndpointSliceLister: endpointSliceLister,
		Recorder:            recorder,
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// use the builder fashion. If we need more fine grain control later, we can switch to `controller.New()`
	err := ctrl.NewControllerManagedBy(mgr).
		For(&modelv1alpha1.ModelAdapter{}).
		Owns(&corev1.Service{}).
		Owns(&discoveryv1.EndpointSlice{}).
		Owns(&corev1.Pod{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add model-adapter-controller")
	return err
}

var _ reconcile.Reconciler = &ModelAdapterReconciler{}

// ModelAdapterReconciler reconciles a ModelAdapter object
type ModelAdapterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// PodLister is able to list/get pods from a shared informer's cache store
	PodLister corelisters.PodLister
	// ServiceLister is able to list/get services from a shared informer's cache store
	ServiceLister corelisters.ServiceLister
	// EndpointSliceLister is able to list/get services from a shared informer's cache store
	EndpointSliceLister discoverylisters.EndpointSliceLister

	// TODO: consider to use control way (kubernetes way) to manage the resources
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ModelAdapter object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *ModelAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO: handle unload model adapter from pods later.

	// Fetch the ModelAdapter instance
	modelAdapter := &modelv1alpha1.ModelAdapter{}
	err := r.Get(ctx, req.NamespacedName, modelAdapter)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.
			// For service, endpoint objects, clean up the resources using finalizers/
			klog.V(3).InfoS("ModelAdapter resource not found. Ignoring since object mush be deleted", "modelAdapter", req)
			return reconcile.Result{}, nil
		}

		// Error reading the onbject and let's requeue the request
		klog.ErrorS(err, "Failed to get ModelAdapter", "modelAdapter", klog.KObj(modelAdapter))
		return reconcile.Result{}, err
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

	// TODO: better handle finalizer here. Then we can define some operations which should
	// occur before the custom resource to be deleted.
	// TODO: handle DeletionTimeStamp later.
	if controllerutil.ContainsFinalizer(instance, ModelAdapterFinalizer) {
		klog.Info("Adding finalizer for ModelAdapter")

		if ok := controllerutil.AddFinalizer(instance, ModelAdapterFinalizer); !ok {
			klog.Error("Failed to add finalizer for ModelAdapter")
			return ctrl.Result{Requeue: true}, nil
		}

		if err := r.Update(ctx, instance); err != nil {
			klog.Error("Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	oldInstance := instance.DeepCopy()

	// Step 0: Validate ModelAdapter configurations
	if ctrlResult, err := r.validateModelAdapter(ctx, instance); err != nil {
		return ctrlResult, err
	}

	// Step 1: Schedule Pod for ModelAdapter
	selectedPod := &corev1.Pod{}
	existPods := false
	var err error
	if instance.Status.Instances != nil && len(instance.Status.Instances) != 0 {
		// model adapter has already been scheduled to some pods
		// check the lora scheduled pod first, verify the mapping is still valid.
		selectedPodName := instance.Status.Instances[0]
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: selectedPodName}, selectedPod); err != nil && apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to get selected pod but pod is still in ModelAdapter instance list", "modelAdapter", klog.KObj(instance))

			// clear the pod list
			ctrlResult, err := r.clearModelAdapterInstanceList(ctx, instance)
			if err != nil {
				return ctrlResult, err
			}
		} else if err != nil {
			// failed to fetch the pod, let's requeue
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
		} else {
			existPods = true
			// compare instance and model adapter labels.
			selector, err := metav1.LabelSelectorAsSelector(instance.Spec.PodSelector)
			if err != nil {
				// TODO: this should barely happen, let's move this logic to earlier validation logics.
				return ctrl.Result{}, fmt.Errorf("failed to convert pod selector: %v", err)
			}

			if !selector.Matches(labels.Set(selectedPod.Labels)) {
				klog.Warning("current assigned pod selector doesn't match model adapter selector")
				ctrlResult, err := r.clearModelAdapterInstanceList(ctx, instance)
				if err != nil {
					return ctrlResult, err
				}
			}
		}
	}

	if !existPods {
		selectedPod, err = r.schedulePod(ctx, instance)
		if err != nil {
			klog.ErrorS(err, "Failed to schedule Pod for ModelAdapter", "modelAdapter", instance.Name)
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
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
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// Step 2: Reconcile Loading
	if err := r.reconcileLoading(ctx, instance, selectedPod); err != nil {
		// retry any of the failure.
		instance.Status.Phase = modelv1alpha1.ModelAdapterFailed
		if err := r.Status().Update(ctx, instance); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
		}

		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
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
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               string(modelv1alpha1.ModelAdapterConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciling",
			Message:            fmt.Sprintf("ModelAdapter %s is ready", klog.KObj(instance)),
			LastTransitionTime: metav1.Now(),
		})

		if err := r.Status().Update(ctx, instance); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", instance)
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *ModelAdapterReconciler) clearModelAdapterInstanceList(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
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
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

	// TODO: let's build the scheduling algorithm later
	// we should also fetch <pod, list<lora>> mappings later.

	return &podList.Items[0], nil // Returning the first Pod for simplicity
}

// GetEnvKey retrieves the value of the environment variable named by the key.
// If the variable is present, the function returns the value and a boolean true.
// If the variable is not present, the function returns an empty string and a boolean false.
func GetEnvKey(key string) (string, bool) {
	value, exists := os.LookupEnv(key)
	return value, exists
}

// make sure it only called once.
func (r *ModelAdapterReconciler) reconcileLoading(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) error {
	// Define the key you want to check
	key := "DEBUG_MODE"
	value, exists := GetEnvKey(key)
	host := fmt.Sprintf("http://%s:8000", pod.Status.PodIP)
	if exists && value == "on" {
		// 30080 is the nodePort of the base model service.
		host = fmt.Sprintf("http://%s:30080", "localhost")
	}

	// Get the list of existing models from /v1/models, parse the payload, if the instance already exist, we should return nil directly.
	url := fmt.Sprintf("%s/v1/models", host)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Handle the error here. For example, log it or take appropriate corrective action.
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to get models: %s", body)
	}

	var response map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}

	data, ok := response["data"].([]interface{})
	if !ok {
		return errors.New("invalid data format")
	}

	// Check if the instance already exists
	for _, item := range data {
		model, ok := item.(map[string]interface{})
		if !ok {
			fmt.Println("Invalid model format")
			continue
		}

		if model["id"] == instance.Name {
			// Instance already exists, return nil
			klog.V(4).Info("lora model has been registered previous, skip registration")
			return nil
		}
	}

	payload := map[string]string{
		"lora_name": instance.Name,
		"lora_path": instance.Spec.AdditionalConfig["modelArtifact"],
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	// TODO: We need to make it mac can access this pod ip.
	// TODO: without sidecar, it's hard to know user's port and metrics here.
	url = fmt.Sprintf("%s/v1/load_lora_adapter", host)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	resp, err = client.Do(req)
	if err != nil {
		return err
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			// Handle the error here. For example, log it or take appropriate corrective action.
			klog.InfoS("Error closing response body:", err)
		}
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, _ = io.ReadAll(resp.Body)
		return err
	}

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
		svc, err := buildModelAdapterService(*instance)
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

func buildModelAdapterService(instance modelv1alpha1.ModelAdapter) (*corev1.Service, error) {
	labels := map[string]string{
		"model.aibrix.ai/base-model":    instance.Spec.BaseModel,
		"model.aibrix.ai/model-adapter": instance.Name,
	}

	ports := []corev1.ServicePort{
		{
			Name: "http",
			// it should use the base model service port.
			// make sure this can be dynamically configured later.
			Port: 8000,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: 8000,
			},
			Protocol: corev1.ProtocolTCP,
		},
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Ports:                    ports,
		},
	}, nil
}

func (r *ModelAdapterReconciler) reconcileEndpointSlice(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) (ctrl.Result, error) {
	// check if the endpoint slice already exists, if not create a new one.
	found := &discoveryv1.EndpointSlice{}
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// EndpointSlice does not exist, create it
		eps, err := buildModelAdapterEndpointSlice(*instance, *pod)
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

func buildModelAdapterEndpointSlice(instance modelv1alpha1.ModelAdapter, pod corev1.Pod) (*discoveryv1.EndpointSlice, error) {
	serviceLabels := map[string]string{
		"kubernetes.io/service-name": instance.Name,
	}

	addresses := []discoveryv1.Endpoint{
		{
			Addresses: []string{pod.Status.PodIP},
		},
	}

	ports := []discoveryv1.EndpointPort{
		{
			Name:     stringPtr("http"),
			Protocol: protocolPtr(corev1.ProtocolTCP),
			Port:     int32Ptr(8000),
		},
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    serviceLabels,
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   addresses,
		Ports:       ports,
	}, nil
}

func stringPtr(s string) *string {
	return &s
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func int32Ptr(i int32) *int32 {
	return &i
}

func (r *ModelAdapterReconciler) inconsistentModelAdapterStatus(oldStatus, newStatus modelv1alpha1.ModelAdapterStatus) bool {
	// Implement your logic to check if the status is inconsistent
	if oldStatus.Phase != newStatus.Phase || !equalStringSlices(oldStatus.Instances, newStatus.Instances) {
		return true
	}

	return false
}

func (r *ModelAdapterReconciler) validateModelAdapter(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	// TODO: Do more validation on the model adapter object
	// for example, check the base model mapping etc.
	return ctrl.Result{}, nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	aSet := make(map[string]struct{}, len(a))
	for _, item := range a {
		aSet[item] = struct{}{}
	}

	for _, item := range b {
		if _, exists := aSet[item]; !exists {
			return false
		}
	}

	return true
}
