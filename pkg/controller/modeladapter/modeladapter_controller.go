/*
Copyright 2024.

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
	"fmt"
	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"net/http"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"
)

const (
	ControllerUIDLabelKey = "modeladapter-controller-uid"
)

var (
	controllerKind         = modelv1alpha1.GroupVersion.WithKind("ModelAdapter")
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
	ctrl.NewControllerManagedBy(mgr).
		For(&modelv1alpha1.ModelAdapter{}).
		Watches(&corev1.Service{}, &handler.EnqueueRequestForObject{}).
		Watches(&discoveryv1.EndpointSlice{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Pod{}, &handler.EnqueueRequestForObject{}).
		Complete(r)

	klog.V(4).InfoS("Finished to add model-adapter-controller")
	return nil
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

	// TOOD: consider to use control way (kubernetes way) to manage the resources
}

//+kubebuilder:rbac:groups=discovery,resources=endpointslices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=discovery,resources=endpointslices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
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

	// TODO: handle one more case, unload model adapter from pods later.

	// Fetch the ModelAdapter instance
	modelAdapter := &modelv1alpha1.ModelAdapter{}
	err := r.Get(ctx, req.NamespacedName, modelAdapter)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.
			// For service, endpoint objects, clean up the resources using finalizers/
			klog.V(3).InfoS("ModelAdapter has been deleted", "modelAdapter", req)
			return reconcile.Result{}, nil
		}

		// Error reading the onbject and let's requeue the request
		klog.ErrorS(err, "Failed to get ModelAdapter", "modelAdapter", klog.KObj(modelAdapter))
		return reconcile.Result{}, err
	}

	return r.DoReconcile(ctx, req, modelAdapter)
}

func (r *ModelAdapterReconciler) DoReconcile(ctx context.Context, req ctrl.Request, instance *modelv1alpha1.ModelAdapter) (ctrl.Result, error) {
	oldInstance := instance.DeepCopy()

	// Step 1: Schedule Pod for ModelAdapter
	selectedPod, err := r.schedulePod(ctx, instance)
	if err != nil {
		klog.ErrorS(err, "Failed to schedule Pod for ModelAdapter", "modelAdapter", instance.Name)
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}

	// Step 2: Reconcile Loading
	if err := r.reconcileLoading(ctx, instance, selectedPod); err != nil {
		if updateErr := r.updateModelAdapterState(ctx, instance, modelv1alpha1.ModelAdapterConfiguring); updateErr != nil {
			klog.ErrorS(updateErr, "ModelAdapter update state error", "cluster name", req.Name)
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}

	// Step 3: Reconcile Service
	if err := r.reconcileService(ctx, instance); err != nil {
		if updateErr := r.updateModelAdapterState(ctx, instance, modelv1alpha1.ModelAdapterBinding); updateErr != nil {
			klog.ErrorS(updateErr, "ModelAdapter update state error", "cluster name", req.Name)
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}

	// Step 4: Reconcile EndpointSlice
	if err := r.reconcileEndpointSlice(ctx, instance, selectedPod); err != nil {
		if updateErr := r.updateModelAdapterState(ctx, instance, modelv1alpha1.ModelAdapterConfiguring); updateErr != nil {
			klog.ErrorS(updateErr, "ModelAdapter update state error", "cluster name", req.Name)
		}
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}

	// Calculate the new status for the ModelAdapter. Note that the function will deep copy `instance` instead of mutating it.
	newInstance, err := r.calculateStatus(ctx, instance)
	if err != nil {
		klog.InfoS("Got error when calculating new status", "cluster name", req.Name, "error", err)
		return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
	}

	// Check if need to update the status.
	if r.inconsistentModelAdapterStatus(ctx, oldInstance.Status, newInstance.Status) {
		klog.InfoS("model adapter reconcile", "Update CR status", req.Name, "status", newInstance.Status)
		if err := r.Status().Update(ctx, newInstance); err != nil {
			klog.InfoS("Got error when updating status", "cluster name", req.Name, "error", err, "ModelAdapter", newInstance)
			return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, err
		}
	}

	//klog.InfoS("Unconditional requeue after", "cluster name", req.Name, "seconds", DefaultRequeueDuration)
	//return ctrl.Result{RequeueAfter: DefaultRequeueDuration}, nil
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

	return &podList.Items[0], nil // Returning the first Pod for simplicity
}

func (r *ModelAdapterReconciler) reconcileLoading(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) error {
	payload := map[string]string{
		"id":   instance.Name,
		"root": instance.Spec.AdditionalConfig["model-artifact"],
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	// TODO: We need to make it mac can access this pod ip.
	// TODO: without sidecar, it's hard to know user's port and metrics here.
	// TODO: Need to finish the vLLM Lora MR and then finalize this one.
	// We need to make sure the mocked app.py has exact api endpoints with vLLM

	//url := fmt.Sprintf("http://%s:8000/v1/load", pod.Status.PodIP)
	url := fmt.Sprintf("http://%s:8000/v1/load", "localhost")
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

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("failed to load model adapters: %s", string(bodyBytes))
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

func (r *ModelAdapterReconciler) reconcileService(ctx context.Context, instance *modelv1alpha1.ModelAdapter) error {
	// Retrieve the Service from the Kubernetes cluster with the name and namespace.
	svc := &corev1.Service{}

	objectKey := types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      instance.Name,
	}

	err := r.Get(ctx, objectKey, svc)
	if errors.IsNotFound(err) {
		// Service does not exist, create it
		svc, err = buildModelAdapterService(ctx, *instance)
		if err != nil {
			return err
		}
		// Set the owner reference
		if err := ctrl.SetControllerReference(instance, svc, r.Scheme); err != nil {
			return err
		}
		// create service
		return r.Create(ctx, svc)
	}
	return err
}

func buildModelAdapterService(context context.Context, instance modelv1alpha1.ModelAdapter) (*corev1.Service, error) {
	labels := map[string]string{
		"model.aibrix.ai/base-model":    instance.Spec.BaseModel,
		"model.aibrix.ai/model-adapter": instance.Name,
	}

	ports := []corev1.ServicePort{
		{
			Name: "http",
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

func (r *ModelAdapterReconciler) reconcileEndpointSlice(ctx context.Context, instance *modelv1alpha1.ModelAdapter, pod *corev1.Pod) error {
	eps := &discoveryv1.EndpointSlice{}

	objectKey := types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      instance.Name,
	}

	err := r.Get(ctx, objectKey, eps)
	if err == nil {
		// Check if the Pod IP is already in the EndpointSlice
		podIP := pod.Status.PodIP
		alreadyExists := false
		for _, endpoint := range eps.Endpoints {
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
			eps.Endpoints = append(eps.Endpoints, discoveryv1.Endpoint{
				Addresses: []string{podIP},
			})

			if err := r.Update(ctx, eps); err != nil {
				klog.ErrorS(err, "Failed to update EndpointSlice", "EndpointSlice", eps.Name)
				return err
			}
			klog.InfoS("Successfully updated EndpointSlice", "EndpointSlice", eps.Name)
		} else {
			klog.InfoS("Pod IP already exists in EndpointSlice", "PodIP", podIP)
		}

		return nil
	} else if errors.IsNotFound(err) {
		// EndpointSlice does not exist, create it
		eps, err = buildModelAdapterEndpointSlice(ctx, *instance, *pod)
		if err != nil {
			return err
		}
		// Set the owner reference
		if err := ctrl.SetControllerReference(instance, eps, r.Scheme); err != nil {
			return err
		}
		// Create endpoint slice
		return r.Create(ctx, eps)
	}
	return err
}

func buildModelAdapterEndpointSlice(context context.Context, instance modelv1alpha1.ModelAdapter, pod corev1.Pod) (*discoveryv1.EndpointSlice, error) {
	labels := map[string]string{
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
			Labels:    labels,
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

func (r *ModelAdapterReconciler) calculateStatus(ctx context.Context, instance *modelv1alpha1.ModelAdapter) (*modelv1alpha1.ModelAdapter, error) {
	// Implement your logic to calculate the status of the ModelAdapter
	// TODO: we need better control here.
	instance.Status.Phase = modelv1alpha1.ModelAdapterRunning
	return instance, nil
}

func (r *ModelAdapterReconciler) inconsistentModelAdapterStatus(ctx context.Context, oldStatus, newStatus modelv1alpha1.ModelAdapterStatus) bool {
	// Implement your logic to check if the status is inconsistent
	if oldStatus.Phase != newStatus.Phase || !equalStringSlices(oldStatus.Instances, newStatus.Instances) {
		return true
	}

	return false
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
