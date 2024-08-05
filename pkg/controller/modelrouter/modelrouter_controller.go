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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// TODO (varun): cleanup model related identifiers and establish common concensus
	modelHeaderIdentifier = "model"
	modelIdentifier       = "model.aibrix.ai"
	modelPortIdentifier   = "model.aibrix.ai/port"
	// TODO (varun): parameterize it or dynamically resolve it
	aibrixEnvoyGateway = "eg"
)

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

func Add(mgr manager.Manager) error {
	klog.Info("Starting modelrouter controller")
	cacher := mgr.GetCache()

	deploymentInformer, err := cacher.GetInformer(context.TODO(), &appsv1.Deployment{})
	if err != nil {
		return err
	}

	utilruntime.Must(gatewayv1.AddToScheme(mgr.GetClient().Scheme()))

	modelRouter := &ModelRouter{
		Client: mgr.GetClient(),
	}
	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    modelRouter.addModel,
		DeleteFunc: modelRouter.deleteModel,
	})

	return nil
}

type ModelRouter struct {
	client.Client
	Scheme *runtime.Scheme
}

func (m *ModelRouter) addModel(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)

	modelName, ok := deployment.Labels[modelIdentifier]
	if !ok {
		fmt.Printf("deployment %s does not have a model, labels: %s\n", deployment.Name, deployment.Labels)
		return
	}

	modelPort, _ := strconv.ParseInt(deployment.Labels[modelPortIdentifier], 10, 32)

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: deployment.Namespace,
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
	err := m.Client.Create(context.Background(), &httpRoute)
	klog.Errorln(err)
}

func (m *ModelRouter) deleteModel(obj interface{}) {
	deployment := obj.(*appsv1.Deployment)

	modelName, ok := deployment.Labels[modelIdentifier]
	if !ok {
		fmt.Printf("deployment %s does not have a model, labels: %s\n", deployment.Name, deployment.Labels)
		return
	}

	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-router", modelName),
			Namespace: deployment.Namespace,
		},
	}

	err := m.Client.Delete(context.Background(), &httpRoute)
	klog.Errorln(err)
}
