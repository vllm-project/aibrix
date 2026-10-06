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

package modelrouter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// TODO (varun): cleanup model related identifiers and establish common consensus
	modelHeaderIdentifier = "model"
	modelPortIdentifier   = constants.ModelLabelPort
	// TODO (varun): parameterize it or dynamically resolve it
	aibrixEnvoyGateway          = "aibrix-eg"
	aibrixEnvoyGatewayNamespace = "aibrix-system"

	defaultModelServingPort = 8000

	modelRouterCustomPath = constants.ModelAnnoRouterCustomPath

	// defaultResyncInterval is how often missing HTTPRoutes are recreated.
	defaultResyncInterval = 30 * time.Second
)

var watchedWorkloads = []schema.GroupVersionKind{
	{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"},
}

var modelPaths = []string{
	"/v1/completions",
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/messages",
	"/v1/embeddings",
	"/v1/rerank",
	"/v1/classify",
	"/v1/decisions",
	"/generate",
	"/generatevideo",
	"/v1/video",
	"/v1/videos",
	"/v1/audio/transcriptions",
	"/v1/audio/translations",
	"/tokenize",
	"/pooling",
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=orchestration.aibrix.ai,resources=rayclusterfleets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete

func Add(mgr manager.Manager, runtimeConfig config.RuntimeConfig) error {
	klog.InfoS("Starting modelrouter controller")
	cacher := mgr.GetCache()

	deploymentInformer, err := cacher.GetInformer(context.TODO(), &appsv1.Deployment{})
	if err != nil {
		return err
	}

	modelInformer, err := cacher.GetInformer(context.TODO(), &modelv1alpha1.ModelAdapter{})
	if err != nil {
		return err
	}

	fleetInformer, err := cacher.GetInformer(context.TODO(), &orchestrationv1alpha1.RayClusterFleet{})
	if err != nil {
		return err
	}

	utilruntime.Must(gatewayv1.AddToScheme(mgr.GetClient().Scheme()))
	utilruntime.Must(gatewayv1beta1.AddToScheme(mgr.GetClient().Scheme()))

	modelRouter := &ModelRouter{
		Client:         mgr.GetClient(),
		RuntimeConfig:  runtimeConfig,
		cacheReader:    cacher,
		resyncInterval: defaultResyncInterval,
	}

	_, err = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromDeployment,
		UpdateFunc: modelRouter.updateRouteFromWorkload,
		DeleteFunc: modelRouter.deleteRouteFromDeployment,
	})
	if err != nil {
		return err
	}

	_, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromModelAdapter,
		UpdateFunc: modelRouter.updateRouteFromWorkload,
		DeleteFunc: modelRouter.deleteRouteFromModelAdapter,
	})
	if err != nil {
		return err
	}

	_, err = fleetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromRayClusterFleet,
		UpdateFunc: modelRouter.updateRouteFromWorkload,
		DeleteFunc: modelRouter.deleteRouteFromRayClusterFleet,
	})
	if err != nil {
		return err
	}

	// add dynamic informer for all workloads
	for _, gvk := range watchedWorkloads {
		exists, err := utils.GVKCheckExists(mgr.GetConfig(), gvk)
		if err != nil || !exists {
			klog.InfoS("skip informer (optional CRD)", "GVK", gvk, "exists", exists, "error", err)
			continue
		}
		if err := addInformerForGVK(mgr, modelRouter, gvk); err != nil {
			return err
		}
		modelRouter.workloadGVKs = append(modelRouter.workloadGVKs, gvk)
	}

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		modelRouter.Run(ctx)
		return nil
	})); err != nil {
		return err
	}

	return nil
}

type ModelRouter struct {
	client.Client
	Scheme        *runtime.Scheme
	RuntimeConfig config.RuntimeConfig

	// cacheReader lists unstructured workloads from the informer cache, since
	// the manager client reads unstructured objects from the API server.
	cacheReader    client.Reader
	resyncInterval time.Duration
	// workloadGVKs are the optional workload kinds whose informers were registered.
	workloadGVKs []schema.GroupVersionKind
	// routeMu serializes route deletion with the resync's create step, so the
	// resync cannot recreate a route for a workload that was just deleted.
	routeMu sync.Mutex
}

func (m *ModelRouter) addRouteFromDeployment(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)
	m.createHTTPRoute(deployment.Namespace, deployment.Labels, deployment.Annotations)
}

func (m *ModelRouter) deleteRouteFromDeployment(obj interface{}) {
	deployment, ok := obj.(*appsv1.Deployment)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		deployment, ok = tombstone.Obj.(*appsv1.Deployment)
		if !ok {
			return
		}
	}
	m.deleteHTTPRoute(deployment.Namespace, deployment.Labels, deployment.Annotations)
}

func (m *ModelRouter) addRouteFromModelAdapter(obj interface{}) {
	modelAdapter := obj.(*modelv1alpha1.ModelAdapter)
	m.createHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels, modelAdapter.Annotations)
}

func (m *ModelRouter) deleteRouteFromModelAdapter(obj interface{}) {
	modelAdapter, ok := obj.(*modelv1alpha1.ModelAdapter)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		modelAdapter, ok = tombstone.Obj.(*modelv1alpha1.ModelAdapter)
		if !ok {
			return
		}
	}
	m.deleteHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels, modelAdapter.Annotations)
}

func (m *ModelRouter) addRouteFromRayClusterFleet(obj interface{}) {
	fleet := obj.(*orchestrationv1alpha1.RayClusterFleet)
	m.createHTTPRoute(fleet.Namespace, fleet.Labels, fleet.Annotations)
}

func (m *ModelRouter) deleteRouteFromRayClusterFleet(obj interface{}) {
	fleet, ok := obj.(*orchestrationv1alpha1.RayClusterFleet)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		fleet, ok = tombstone.Obj.(*orchestrationv1alpha1.RayClusterFleet)
		if !ok {
			return
		}
	}
	m.deleteHTTPRoute(fleet.Namespace, fleet.Labels, fleet.Annotations)
}

func (m *ModelRouter) addRouteFromUnstructuredObj(obj interface{}) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		klog.Errorf("Failed to get unstructured lws")
		return
	}
	m.createHTTPRoute(u.GetNamespace(), u.GetLabels(), u.GetAnnotations())
}

func (m *ModelRouter) deleteRouteFromUnstructuredObj(obj interface{}) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		u, ok = tombstone.Obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
	}
	m.deleteHTTPRoute(u.GetNamespace(), u.GetLabels(), u.GetAnnotations())
}

func (m *ModelRouter) updateRouteFromWorkload(oldObj, newObj interface{}) {
	oldMeta, err := meta.Accessor(oldObj)
	if err != nil {
		klog.ErrorS(err, "Failed to get old object metadata")
		return
	}
	newMeta, err := meta.Accessor(newObj)
	if err != nil {
		klog.ErrorS(err, "Failed to get new object metadata")
		return
	}
	// A workload being deleted must not recreate the route that the delete
	// handler is about to remove.
	if newMeta.GetDeletionTimestamp() != nil {
		return
	}
	// Informer resyncs and status/spec updates leave the model metadata
	// unchanged and must not trigger route recreation.
	if !modelRouteMetadataChanged(oldMeta, newMeta) {
		return
	}
	m.createHTTPRoute(newMeta.GetNamespace(), newMeta.GetLabels(), newMeta.GetAnnotations())
}

// modelRouteMetadataChanged reports whether any label or annotation that
// createHTTPRoute reads differs between the two objects.
func modelRouteMetadataChanged(oldObj, newObj metav1.Object) bool {
	oldName, _ := constants.ModelNameFromMetadata(oldObj.GetLabels(), oldObj.GetAnnotations())
	newName, _ := constants.ModelNameFromMetadata(newObj.GetLabels(), newObj.GetAnnotations())
	if oldName != newName {
		return true
	}
	if oldObj.GetLabels()[modelPortIdentifier] != newObj.GetLabels()[modelPortIdentifier] {
		return true
	}
	oldAnno, newAnno := oldObj.GetAnnotations(), newObj.GetAnnotations()
	return oldAnno[constants.ModelAnnoServiceName] != newAnno[constants.ModelAnnoServiceName] ||
		oldAnno[modelRouterCustomPath] != newAnno[modelRouterCustomPath]
}

func (m *ModelRouter) createHTTPRoute(namespace string, labels map[string]string, annotations map[string]string) {
	modelName, ok := constants.ModelNameFromMetadata(labels, annotations)
	if !ok {
		return
	}
	serviceName := modelName
	if annotatedServiceName := annotations[constants.ModelAnnoServiceName]; annotatedServiceName != "" {
		serviceName = annotatedServiceName
	}

	modelPort, err := strconv.ParseInt(labels[modelPortIdentifier], 10, 32)
	if err != nil {
		klog.Warningf("failed to parse model port: %v", err)
		klog.Infof("please ensure %s is configured, default port %d will be used", modelPortIdentifier, defaultModelServingPort)
		modelPort = defaultModelServingPort
	}

	modelHeaderMatch := gatewayv1.HTTPHeaderMatch{
		Type:  ptr.To(gatewayv1.HeaderMatchExact),
		Name:  modelHeaderIdentifier,
		Value: modelName,
	}

	matches := make([]gatewayv1.HTTPRouteMatch, len(modelPaths))
	for i, p := range modelPaths {
		matches[i] = gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{
				Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
				Value: ptr.To(p),
			},
			Headers: []gatewayv1.HTTPHeaderMatch{
				modelHeaderMatch,
			},
		}
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        utils.ModelRouterName(modelName),
			Namespace:   aibrixEnvoyGatewayNamespace,
			Labels:      consoleRouteLabels(labels),
			Annotations: consoleRouteAnnotations(annotations),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      aibrixEnvoyGateway,
						Namespace: ptr.To(gatewayv1.Namespace(aibrixEnvoyGatewayNamespace)),
					},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: matches,
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      gatewayv1.ObjectName(serviceName),
									Namespace: (*gatewayv1.Namespace)(&namespace),
									Port:      ptr.To(gatewayv1.PortNumber(modelPort)),
								},
							},
						},
					},
					Timeouts: &gatewayv1.HTTPRouteTimeouts{
						Request: ptr.To(gatewayv1.Duration(fmt.Sprintf("%ds", utils.LoadEnvInt("AIBRIX_GATEWAY_TIMEOUT_SECONDS", 600)))),
					},
				},
			},
		},
	}

	appendCustomModelRouterPaths(&httpRoute, modelHeaderMatch, annotations)

	err = m.Create(context.Background(), &httpRoute)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(4).Infof("httproute: %v already exists in namespace: %v", httpRoute.Name, namespace)
		} else {
			klog.ErrorS(err, "Failed to create httproute", "namespace", namespace, "name", httpRoute.Name)
			return
		}
	} else {
		klog.Infof("httproute: %v created for model: %v", httpRoute.Name, modelName)
	}

	if aibrixEnvoyGatewayNamespace != namespace {
		m.createReferenceGrant(namespace)
	}
}

func (m *ModelRouter) createReferenceGrant(namespace string) {
	referenceGrantName := fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, namespace)
	referenceGrant := gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      referenceGrantName,
			Namespace: namespace,
		},
	}

	if err := m.Get(context.Background(), client.ObjectKeyFromObject(&referenceGrant), &referenceGrant); err == nil {
		klog.V(4).InfoS("reference grant already exists", "referencegrant", referenceGrant.Name)
		return
	}

	referenceGrant = gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      referenceGrantName,
			Namespace: namespace,
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: aibrixEnvoyGatewayNamespace,
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  "Service",
				},
			},
		},
	}
	if err := m.Create(context.Background(), &referenceGrant); err != nil {
		klog.ErrorS(err, "error on creating referencegrant", "referencegrant", referenceGrant)
		return
	}
	klog.InfoS("referencegrant created", "referencegrant", referenceGrant.Name)
}

func (m *ModelRouter) deleteHTTPRoute(namespace string, labels, annotations map[string]string) {
	modelName, ok := constants.ModelNameFromMetadata(labels, annotations)
	if !ok {
		return
	}

	m.routeMu.Lock()
	defer m.routeMu.Unlock()

	ctx := context.Background()
	hasModel, hasAnyModel, err := m.namespaceModelWorkloadState(ctx, namespace, modelName)
	if err != nil {
		klog.ErrorS(err, "Failed to check remaining workloads before deleting HTTPRoute",
			"namespace", namespace, "model", modelName)
		return
	}
	if hasModel {
		return
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.ModelRouterName(modelName),
			Namespace: aibrixEnvoyGatewayNamespace,
		},
	}

	err = m.Delete(ctx, &httpRoute)
	if err != nil {
		klog.Errorln(err)
	}
	if err == nil {
		klog.Infof("httproute: %v deleted for model: %v", httpRoute.Name, modelName)
	}

	if aibrixEnvoyGatewayNamespace != namespace {
		if err := m.deleteReferenceGrant(ctx, namespace, hasAnyModel); err != nil {
			klog.ErrorS(err, "Failed to delete ReferenceGrant after checking remaining workloads",
				"namespace", namespace)
		}
	}
}

func (m *ModelRouter) deleteReferenceGrant(ctx context.Context, namespace string, hasModelWorkload bool) error {
	if hasModelWorkload {
		return nil
	}

	referenceGrantName := fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, namespace)
	referenceGrant := gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      referenceGrantName,
			Namespace: namespace,
		},
	}
	if err := m.Delete(ctx, &referenceGrant); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete ReferenceGrant", "name", referenceGrantName, "namespace", namespace)
			return err
		}
	}
	klog.InfoS("delete reference grant", "referencegrant", referenceGrantName)
	return nil
}

func (m *ModelRouter) namespaceHasModelWorkload(ctx context.Context, namespace string) (bool, error) {
	hasModel, _, err := m.namespaceModelWorkloadState(ctx, namespace, "")
	return hasModel, err
}

// namespaceModelWorkloadState reports whether the namespace contains the
// requested model and whether it contains any model workload. An empty
// modelName matches any labeled workload for ReferenceGrant cleanup.
func (m *ModelRouter) namespaceModelWorkloadState(ctx context.Context, namespace, modelName string) (hasModel, hasAnyModel bool, err error) {
	matches := func(labels, annotations map[string]string) bool {
		workloadModelName, ok := constants.ModelNameFromMetadata(labels, annotations)
		if !ok {
			return false
		}
		hasAnyModel = true
		return modelName == "" || workloadModelName == modelName
	}

	var deploymentList appsv1.DeploymentList
	if err := m.List(ctx, &deploymentList, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "Failed to list model deployments", "namespace", namespace)
		return false, hasAnyModel, err
	}
	for i := range deploymentList.Items {
		deployment := &deploymentList.Items[i]
		if matches(deployment.Labels, deployment.Annotations) {
			klog.InfoS("found labeled model deployment in namespace",
				"namespace", namespace, "deployment", deployment.Name)
			return true, true, nil
		}
	}

	var adapterList modelv1alpha1.ModelAdapterList
	if err := m.List(ctx, &adapterList, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "Failed to list model adapters", "namespace", namespace)
		return false, hasAnyModel, err
	}
	for i := range adapterList.Items {
		adapter := &adapterList.Items[i]
		if matches(adapter.Labels, adapter.Annotations) {
			klog.InfoS("found labeled model adapter in namespace",
				"namespace", namespace, "modeladapter", adapter.Name)
			return true, true, nil
		}
	}

	var fleetList orchestrationv1alpha1.RayClusterFleetList
	if err := m.List(ctx, &fleetList, client.InNamespace(namespace)); err != nil {
		klog.ErrorS(err, "Failed to list ray cluster fleets", "namespace", namespace)
		return false, hasAnyModel, err
	}
	for i := range fleetList.Items {
		fleet := &fleetList.Items[i]
		if matches(fleet.Labels, fleet.Annotations) {
			klog.InfoS("found labeled ray cluster fleet in namespace",
				"namespace", namespace, "rayclusterfleet", fleet.Name)
			return true, true, nil
		}
	}

	// LeaderWorkerSet (and any other optional watched workload) is listed as
	// unstructured so clusters without the CRD do not leak ReferenceGrants.
	for _, gvk := range watchedWorkloads {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind + "List",
		})
		if err := m.List(ctx, list, client.InNamespace(namespace)); err != nil {
			if meta.IsNoMatchError(err) {
				// CRD not installed; treat as "not present", not as "has workload".
				klog.V(4).InfoS("optional workload CRD not present", "GVK", gvk, "namespace", namespace)
				continue
			}
			klog.ErrorS(err, "Failed to list optional model workloads", "GVK", gvk, "namespace", namespace)
			return false, hasAnyModel, err
		}
		for i := range list.Items {
			u := &list.Items[i]
			if matches(u.GetLabels(), u.GetAnnotations()) {
				klog.InfoS("found labeled model workload in namespace",
					"namespace", namespace, "gvk", gvk.String(), "name", u.GetName())
				return true, true, nil
			}
		}
	}
	return false, hasAnyModel, nil
}

// Run periodically recreates missing HTTPRoutes until ctx is cancelled. Add and
// Update events cannot recover a route that was deleted out of band.
func (m *ModelRouter) Run(ctx context.Context) {
	ticker := time.NewTicker(m.resyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.ensureHTTPRoutes(ctx); err != nil {
				klog.ErrorS(err, "Failed to resync model httproutes")
			}
		case <-ctx.Done():
			klog.Info("context done, stopping model httproute resync")
			return
		}
	}
}

// ensureHTTPRoutes creates the HTTPRoute of every labeled model workload whose
// route is missing. Existing routes are left unchanged.
func (m *ModelRouter) ensureHTTPRoutes(ctx context.Context) error {
	var workloads []client.Object

	var deploymentList appsv1.DeploymentList
	if err := m.List(ctx, &deploymentList); err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}
	for i := range deploymentList.Items {
		workloads = append(workloads, &deploymentList.Items[i])
	}

	var adapterList modelv1alpha1.ModelAdapterList
	if err := m.List(ctx, &adapterList); err != nil {
		return fmt.Errorf("failed to list model adapters: %w", err)
	}
	for i := range adapterList.Items {
		workloads = append(workloads, &adapterList.Items[i])
	}

	var fleetList orchestrationv1alpha1.RayClusterFleetList
	if err := m.List(ctx, &fleetList); err != nil {
		return fmt.Errorf("failed to list ray cluster fleets: %w", err)
	}
	for i := range fleetList.Items {
		workloads = append(workloads, &fleetList.Items[i])
	}

	for _, gvk := range m.workloadGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := m.cacheReader.List(ctx, list); err != nil {
			return fmt.Errorf("failed to list %s: %w", gvk, err)
		}
		for i := range list.Items {
			workloads = append(workloads, &list.Items[i])
		}
	}

	checked := make(map[string]struct{})
	for _, workload := range workloads {
		if workload.GetDeletionTimestamp() != nil {
			continue
		}
		modelName, ok := constants.ModelNameFromMetadata(workload.GetLabels(), workload.GetAnnotations())
		if !ok {
			continue
		}
		if _, ok := checked[modelName]; ok {
			continue
		}
		if m.ensureHTTPRouteForWorkload(ctx, workload, modelName) {
			checked[modelName] = struct{}{}
		}
	}
	return nil
}

// ensureHTTPRouteForWorkload creates the model's HTTPRoute if it is missing and
// the workload still serves the model. It reports whether the workload is still
// current, so the caller can skip other workloads of the same model.
//
// The workload comes from a List snapshot and may have been deleted since, with
// DeleteFunc already removing the route. Re-reading it under routeMu closes that
// window: the informer updates its cache before calling DeleteFunc, so either
// the re-read sees the deletion, or the route is created first and the pending
// DeleteFunc removes it once the lock is released.
func (m *ModelRouter) ensureHTTPRouteForWorkload(ctx context.Context, workload client.Object, modelName string) bool {
	m.routeMu.Lock()
	defer m.routeMu.Unlock()

	current, err := m.getWorkload(ctx, workload)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to get model workload",
				"namespace", workload.GetNamespace(), "name", workload.GetName())
		}
		return false
	}
	if current.GetDeletionTimestamp() != nil {
		return false
	}
	if name, ok := constants.ModelNameFromMetadata(current.GetLabels(), current.GetAnnotations()); !ok || name != modelName {
		return false
	}

	var route gatewayv1.HTTPRoute
	key := client.ObjectKey{Namespace: aibrixEnvoyGatewayNamespace, Name: utils.ModelRouterName(modelName)}
	err = m.Get(ctx, key, &route)
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to get httproute", "model", modelName)
		return true
	}
	klog.InfoS("httproute is missing, recreating it", "model", modelName, "namespace", current.GetNamespace())
	m.createHTTPRoute(current.GetNamespace(), current.GetLabels(), current.GetAnnotations())
	return true
}

// getWorkload re-reads a listed workload from the informer cache.
func (m *ModelRouter) getWorkload(ctx context.Context, workload client.Object) (client.Object, error) {
	var current client.Object
	reader := client.Reader(m.Client)
	switch w := workload.(type) {
	case *appsv1.Deployment:
		current = &appsv1.Deployment{}
	case *modelv1alpha1.ModelAdapter:
		current = &modelv1alpha1.ModelAdapter{}
	case *orchestrationv1alpha1.RayClusterFleet:
		current = &orchestrationv1alpha1.RayClusterFleet{}
	case *unstructured.Unstructured:
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(w.GroupVersionKind())
		current = u
		reader = m.cacheReader
	default:
		return nil, fmt.Errorf("unsupported workload type %T", workload)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workload), current); err != nil {
		return nil, err
	}
	return current, nil
}

func consoleRouteLabels(labels map[string]string) map[string]string {
	if labels[constants.AppLabelManagedBy] != constants.ConsoleManagedByValue {
		return nil
	}
	return map[string]string{
		constants.AppLabelManagedBy: constants.ConsoleManagedByValue,
	}
}

func consoleRouteAnnotations(annotations map[string]string) map[string]string {
	result := map[string]string{}
	for _, key := range []string{
		constants.ConsoleDeploymentIDAnnotation,
		constants.ConsoleDeploymentNameAnnotation,
	} {
		if value := annotations[key]; value != "" {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// append matches if model-router-custom-paths is set
func appendCustomModelRouterPaths(httpRoute *gatewayv1.HTTPRoute, modelHeaderMatch gatewayv1.HTTPHeaderMatch, annotations map[string]string) {
	if httpRoute == nil || annotations == nil {
		return
	}

	if len(httpRoute.Spec.Rules) == 0 {
		// This case should not happen in the current workflow, as createHTTPRoute always creates a rule.
		// Creating a rule here without BackendRefs would be incorrect.
		klog.Warningf("Cannot append custom path to HTTPRoute %s with no rules.", httpRoute.Name)
		return
	}

	paths, ok := annotations[modelRouterCustomPath]
	if !ok {
		return
	}

	pathSlice := strings.Split(paths, ",")
	// avoid duplicates
	pathSet := make(map[string]struct{})
	for _, path := range pathSlice {
		// remove illegal space in path
		path = strings.ReplaceAll(path, " ", "")
		if _, exists := pathSet[path]; path == "" || exists {
			continue
		}
		httpRoute.Spec.Rules[0].Matches = append(httpRoute.Spec.Rules[0].Matches,
			gatewayv1.HTTPRouteMatch{
				Path: &gatewayv1.HTTPPathMatch{
					Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
					Value: ptr.To(path),
				},
				Headers: []gatewayv1.HTTPHeaderMatch{
					modelHeaderMatch,
				},
			})
		klog.InfoS("Added custom model router path", "path", path)
	}
}

func addInformerForGVK(mgr manager.Manager, modelRouter *ModelRouter, gvk schema.GroupVersionKind) error {
	// create dynamic Informer
	uObj := &unstructured.Unstructured{}
	uObj.SetGroupVersionKind(gvk)

	uInformer, err := mgr.GetCache().GetInformer(context.Background(), uObj)
	if err != nil {
		klog.ErrorS(err, "Failed to get informer", "GVK", gvk)
		return err
	}

	// add Event Handler
	_, err = uInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromUnstructuredObj,
		UpdateFunc: modelRouter.updateRouteFromWorkload,
		DeleteFunc: modelRouter.deleteRouteFromUnstructuredObj,
	})
	if err != nil {
		return err
	}
	klog.Infof("Added model router informer for %s", gvk)
	return nil
}
