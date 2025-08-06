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
	"errors"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/config"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
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
)

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

	klog.Info("Waiting for caches to sync")
	if ok := cacher.WaitForCacheSync(context.TODO()); !ok {
		return errors.New("modelrouter controller: failed to sync cache")
	}
	klog.Info("All caches synced")

	return err
}

type ModelRouter struct {
	client.Client
	Scheme        *runtime.Scheme
	RuntimeConfig config.RuntimeConfig
}

func (m *ModelRouter) addRouteFromDeployment(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)
	m.createHTTPRoute(deployment.Namespace, deployment.Labels)
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
	m.createHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels)
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
	m.createHTTPRoute(fleet.Namespace, fleet.Labels)
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

func (m *ModelRouter) createHTTPRoute(namespace string, labels map[string]string) {
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
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
								Value: ptr.To("/v1/completions"),
							},
							Headers: []gatewayv1.HTTPHeaderMatch{
								modelHeaderMatch,
							},
						},
						{
							Path: &gatewayv1.HTTPPathMatch{
								Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
								Value: ptr.To("/v1/chat/completions"),
							},
							Headers: []gatewayv1.HTTPHeaderMatch{
								modelHeaderMatch,
							},
						},
					},
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
		klog.InfoS("reference grant already exists", "referencegrant", referenceGrant.Name)
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
	// one reference grant per namespace is shared by all envoy gateway objects
	// only delete reference grant object if all model deployments are deleted in the namespace
	deploymentList := &appsv1.DeploymentList{}
	err := m.Client.List(context.Background(), deploymentList, client.InNamespace(namespace))
	if err != nil {
		klog.ErrorS(err, "deleteReferenceGrant: unable to list all deployments")
		return
	}
	for _, deployment := range deploymentList.Items {
		_, ok := deployment.Labels[modelIdentifier]
		if !ok {
			continue
		}
		klog.InfoS("ignore delete reference grant, at least one model deployment shares same reference grant in the namesapce",
			"namespace", namespace, "deployment", deployment.Name)
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
		klog.ErrorS(err, "fail to delete reference grant", "referencegrant", referenceGrantName)
		return
	}
	klog.InfoS("delete reference grant", "referencegrant", referenceGrantName)
}
