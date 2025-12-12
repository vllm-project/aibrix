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

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
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
	modelIdentifier       = constants.ModelLabelName
	modelPortIdentifier   = constants.ModelLabelPort
	// TODO (varun): parameterize it or dynamically resolve it
	aibrixEnvoyGateway          = "aibrix-eg"
	aibrixEnvoyGatewayNamespace = "aibrix-system"

	defaultModelServingPort = 8000

	modelRouterCustomPath = constants.ModelAnnoRouterCustomPath
	lwsIdentifier         = "leaderworkersets.leaderworkerset.x-k8s.io"
)

var modelPaths = []string{
	"/v1/completions",
	"/v1/chat/completions",
	"/v1/embeddings",
	"/v1/rerank",
	"/generate",
	"/generatevideo",
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
		Client:        mgr.GetClient(),
		RuntimeConfig: runtimeConfig,
	}

	_, err = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromDeployment,
		DeleteFunc: modelRouter.deleteRouteFromDeployment,
	})
	if err != nil {
		return err
	}

	_, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromModelAdapter,
		DeleteFunc: modelRouter.deleteRouteFromModelAdapter,
	})
	if err != nil {
		return err
	}

	_, err = fleetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromRayClusterFleet,
		DeleteFunc: modelRouter.deleteRouteFromRayClusterFleet,
	})
	if err != nil {
		return err
	}

	exists, err := utils.CheckCRDExists(mgr.GetAPIReader(), lwsIdentifier)
	if err != nil {
		klog.ErrorS(err, "Failed to check CRD leaderworkersets")
		return err
	}
	if !exists {
		klog.Infof("%s CRD not found, skipping add informer for lws. "+
			"This is optional - install %s CRD if you need ", lwsIdentifier, lwsIdentifier)
	} else {
		err = addInformerForLWS(mgr, modelRouter)
		if err != nil {
			return err
		}
	}

	return nil
}

type ModelRouter struct {
	client.Client
	Scheme        *runtime.Scheme
	RuntimeConfig config.RuntimeConfig
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
	m.deleteHTTPRoute(deployment.Namespace, deployment.Labels)
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
	m.deleteHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels)
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
	m.deleteHTTPRoute(fleet.Namespace, fleet.Labels)
}

func (m *ModelRouter) addRouteFromLeaderWorkerSet(obj interface{}) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		klog.Errorf("Failed to get unstructured lws")
		return
	}
	m.createHTTPRoute(u.GetNamespace(), u.GetLabels(), u.GetAnnotations())
}

func (m *ModelRouter) deleteRouteFromLeaderWorkerSet(obj interface{}) {
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
	m.deleteHTTPRoute(u.GetNamespace(), u.GetLabels())
}

func (m *ModelRouter) createHTTPRoute(namespace string, labels map[string]string, annotations map[string]string) {
	modelName, ok := labels[modelIdentifier]
	if !ok {
		return
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
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: aibrixEnvoyGatewayNamespace,
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
									// TODO (varun): resolve service name from deployment
									Name:      gatewayv1.ObjectName(modelName),
									Namespace: (*gatewayv1.Namespace)(&namespace),
									Port:      ptr.To(gatewayv1.PortNumber(modelPort)),
								},
							},
						},
					},
					Timeouts: &gatewayv1.HTTPRouteTimeouts{
						Request: ptr.To(gatewayv1.Duration(fmt.Sprintf("%ds", utils.LoadEnvInt("AIBRIX_GATEWAY_TIMEOUT_SECONDS", 120)))),
					},
				},
			},
		},
	}

	appendCustomModelRouterPaths(&httpRoute, modelHeaderMatch, annotations)

	err = m.Client.Create(context.Background(), &httpRoute)
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

	if err := m.Client.Get(context.Background(), client.ObjectKeyFromObject(&referenceGrant), &referenceGrant); err == nil {
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
	if err := m.Client.Create(context.Background(), &referenceGrant); err != nil {
		klog.ErrorS(err, "error on creating referencegrant", "referencegrant", referenceGrant)
		return
	}
	klog.InfoS("referencegrant created", "referencegrant", referenceGrant.Name)
}

func (m *ModelRouter) deleteHTTPRoute(namespace string, labels map[string]string) {
	modelName, ok := labels[modelIdentifier]
	if !ok {
		return
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: aibrixEnvoyGatewayNamespace,
		},
	}

	err := m.Client.Delete(context.Background(), &httpRoute)
	if err != nil {
		klog.Errorln(err)
	}
	if err == nil {
		klog.Infof("httproute: %v deleted for model: %v", httpRoute.Name, modelName)
	}

	if aibrixEnvoyGatewayNamespace != namespace {
		m.deleteReferenceGrant(namespace)
	}
}

func (m *ModelRouter) deleteReferenceGrant(namespace string) {
	selector, err := labels.NewRequirement(modelIdentifier, selection.Exists, nil)
	if err != nil {
		klog.ErrorS(err, "Failed to create label requirement", "namespace", namespace)
		return
	}

	listOpts := &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labels.SelectorFromSet(labels.Set{}).Add(*selector),
	}

	var deploymentList appsv1.DeploymentList
	if err := m.Client.List(context.Background(), &deploymentList, listOpts); err != nil {
		klog.ErrorS(err, "Failed to list model deployments", "namespace", namespace)
		return
	}
	if len(deploymentList.Items) > 0 {
		klog.InfoS("Skip deleting ReferenceGrant: model deployment still exists",
			"namespace", namespace, "existingDeployments", len(deploymentList.Items))
		return
	}

	referenceGrantName := fmt.Sprintf("%s-reserved-referencegrant-in-%s", aibrixEnvoyGatewayNamespace, namespace)
	referenceGrant := gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      referenceGrantName,
			Namespace: namespace,
		},
	}
	if err := m.Client.Delete(context.Background(), &referenceGrant); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete ReferenceGrant", "name", referenceGrantName, "namespace", namespace)
			return
		}
	}
	klog.InfoS("delete reference grant", "referencegrant", referenceGrantName)
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

func addInformerForLWS(mgr manager.Manager, modelRouter *ModelRouter) error {
	// create dynamic Informer
	lwsObj := &unstructured.Unstructured{}

	// define LWS GVK (Group, Version, Kind)
	lwsObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "leaderworkerset.x-k8s.io",
		Version: "v1",
		Kind:    "LeaderWorkerSet",
	})

	lwsInformer, err := mgr.GetCache().GetInformer(context.TODO(), lwsObj)
	if err != nil {
		klog.ErrorS(err, "Failed to get informer for LWS")
		return err
	}

	// add Event Handler
	// note: modelRouter.addRouteFromLeaderWorkerSet obj will be *unstructured.Unstructured
	_, err = lwsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addRouteFromLeaderWorkerSet,
		DeleteFunc: modelRouter.deleteRouteFromLeaderWorkerSet,
	})
	if err != nil {
		return err
	}
	klog.Infof("Added model router informer for LWS")
	return nil
}
