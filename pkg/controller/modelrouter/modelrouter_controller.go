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

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	modelv1alpha1 "github.com/aibrix/aibrix/api/model/v1alpha1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// TODO (varun): cleanup model related identifiers and establish common consensus
	modelHeaderIdentifier = "model"
	modelIdentifier       = "model.aibrix.ai/name"
	modelPortIdentifier   = "model.aibrix.ai/port"
	// TODO (varun): parameterize it or dynamically resolve it
	aibrixEnvoyGateway = "aibrix-eg"
)

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

func Add(mgr manager.Manager) error {
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

	utilruntime.Must(gatewayv1.AddToScheme(mgr.GetClient().Scheme()))

	modelRouter := &ModelRouter{
		Client: mgr.GetClient(),
	}

	_, err = deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addModel,
		DeleteFunc: modelRouter.deleteModel,
	})
	if err != nil {
		return err
	}

	_, err = modelInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addModelAdapter,
		DeleteFunc: modelRouter.deleteModelAdapter,
	})

	return err
}

type ModelRouter struct {
	client.Client
	Scheme *runtime.Scheme
}

func (m *ModelRouter) addModel(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)
	m.createHTTPRoute(deployment.Namespace, deployment.Labels)
}

func (m *ModelRouter) deleteModel(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)
	m.deleteHTTPRoute(deployment.Namespace, deployment.Labels)
}

func (m *ModelRouter) addModelAdapter(obj interface{}) {
	modelAdapter := obj.(*modelv1alpha1.ModelAdapter)
	m.createHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels)
}

func (m *ModelRouter) deleteModelAdapter(obj interface{}) {
	modelAdapter := obj.(*modelv1alpha1.ModelAdapter)
	m.deleteHTTPRoute(modelAdapter.Namespace, modelAdapter.Labels)
}

func (m *ModelRouter) createHTTPRoute(namespace string, labels map[string]string) {
	modelName, ok := labels[modelIdentifier]
	if !ok {
		return
	}

	modelPort, err := strconv.ParseInt(labels[modelPortIdentifier], 10, 32)
	if err != nil {
		return
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: namespace,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: aibrixEnvoyGateway,
					},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Headers: []gatewayv1.HTTPHeaderMatch{
								{
									Type:  ptr.To(gatewayv1.HeaderMatchExact),
									Name:  modelHeaderIdentifier,
									Value: modelName,
								},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									// TODO (varun): resolve service name from deployment
									Name: gatewayv1.ObjectName(modelName),
									Port: ptr.To(gatewayv1.PortNumber(modelPort)),
								},
							},
						},
					},
				},
			},
		},
	}
	if err := m.Client.Create(context.Background(), &httpRoute); err != nil {
		klog.Errorln(err)
		return
	}
	klog.Infof("httproute: %v created for model: %v", httpRoute.Name, modelName)
}

func (m *ModelRouter) deleteHTTPRoute(namespace string, labels map[string]string) {
	modelName, ok := labels[modelIdentifier]
	if !ok {
		return
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: namespace,
		},
	}

	if err := m.Client.Delete(context.Background(), &httpRoute); err != nil {
		klog.Errorln(err)
		return
	}
	klog.Infof("httproute: %v deleted for model: %v", httpRoute.Name, modelName)
}
