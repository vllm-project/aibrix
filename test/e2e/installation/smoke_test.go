/*
Copyright 2026 The Aibrix Team.

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

package installation

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	orchestrationv1alpha1 "github.com/vllm-project/aibrix/api/orchestration/v1alpha1"
	orchestrationclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned/typed/orchestration/v1alpha1"
	controllerconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	framework "github.com/vllm-project/aibrix/test/e2e/framework"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	mockImage             = "aibrix/vllm-mock:nightly"
	pdContract            = "vllm-aibrix-shfs"
	modelNameLabel        = "model.aibrix.ai/name"
	modelPortLabel        = "model.aibrix.ai/port"
	modelEngine           = "model.aibrix.ai/engine"
	modelConfigAnnotation = "model.aibrix.ai/config"
	pdRoutingConfig       = `{"defaultProfile":"pd","profiles":{"pd":{"routingStrategy":"pd"}}}`
	gatewayNamespace      = "aibrix-system"
	scenarioTimeout       = 4 * time.Minute
	stormServiceTimeout   = 4 * time.Minute
	cleanupTimeout        = 2 * time.Minute
)

func TestInstallationSmoke(t *testing.T) {
	clientContext, cancelClients := context.WithCancel(context.Background())
	defer cancelClients()

	kubernetesClient, aibrixClient := framework.InitializeClient(clientContext, t)
	gatewayClient := initializeGatewayClient(t)
	verifyStormServiceAPI(t, kubernetesClient)
	config := framework.LoadConfig()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)

	t.Run("single-instance", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		name := "install-smoke-single-" + suffix
		modelName := "smoke-single-" + suffix
		runStormServiceScenario(t, ctx, kubernetesClient,
			aibrixClient.OrchestrationV1alpha1().StormServices(config.Namespace),
			gatewayClient,
			newSingleStormService(config.Namespace, name, modelName),
			map[string]int32{"worker": 1},
			func(t *testing.T) {
				framework.WaitForInference(t, modelName)
			})
	})

	t.Run("pd-disaggregated", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
		defer cancel()
		name := "install-smoke-pd-" + suffix
		modelName := "smoke-pd-" + suffix
		runStormServiceScenario(t, ctx, kubernetesClient,
			aibrixClient.OrchestrationV1alpha1().StormServices(config.Namespace),
			gatewayClient,
			newPDStormService(config.Namespace, name, modelName),
			map[string]int32{"prefill": 1, "decode": 1},
			func(t *testing.T) {
				framework.WaitForPDDisaggregationRouting(t, modelName)
			})
	})
}

func initializeGatewayClient(t *testing.T) gatewayclient.Interface {
	t.Helper()
	kubeConfig := os.Getenv("KUBECONFIG")
	require.NotEmpty(t, kubeConfig, "KUBECONFIG must point to the CI cluster")
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	require.NoError(t, err, "build Gateway API client configuration")
	client, err := gatewayclient.NewForConfig(config)
	require.NoError(t, err, "create Gateway API client")
	return client
}

func verifyStormServiceAPI(t *testing.T, client kubernetes.Interface) {
	t.Helper()
	resources, err := client.Discovery().ServerResourcesForGroupVersion(orchestrationv1alpha1.GroupVersion.String())
	require.NoError(t, err, "StormService API group is not served")
	for _, resource := range resources.APIResources {
		if resource.Name == "stormservices" {
			return
		}
	}
	t.Fatalf("StormService resource is not served in %s", orchestrationv1alpha1.GroupVersion.String())
}

func runStormServiceScenario(
	t *testing.T,
	ctx context.Context,
	kubernetesClient kubernetes.Interface,
	stormServices orchestrationclient.StormServiceInterface,
	gatewayClient gatewayclient.Interface,
	stormService *orchestrationv1alpha1.StormService,
	expectedRoles map[string]int32,
	request func(*testing.T),
) {
	t.Helper()
	service, route, grant := newRoutingResources(stormService.Namespace, stormService.Labels[modelNameLabel])
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if err := deleteStormServiceAndWait(cleanupCtx, kubernetesClient, stormServices, stormService.Name); err != nil {
			t.Errorf("clean up StormService %s: %v", stormService.Name, err)
		}
		err := deleteRoutingResourcesAndWait(
			cleanupCtx, kubernetesClient, gatewayClient, service, route, grant,
		)
		if err != nil {
			t.Errorf("clean up Gateway resources for %s: %v", stormService.Name, err)
		}
	})

	_, err := kubernetesClient.CoreV1().Services(service.Namespace).Create(ctx, service, metav1.CreateOptions{})
	require.NoError(t, err, "create model Service %s", service.Name)
	_, err = gatewayClient.GatewayV1beta1().ReferenceGrants(grant.Namespace).Create(ctx, grant, metav1.CreateOptions{})
	require.NoError(t, err, "create ReferenceGrant %s", grant.Name)
	_, err = gatewayClient.GatewayV1().HTTPRoutes(route.Namespace).Create(ctx, route, metav1.CreateOptions{})
	require.NoError(t, err, "create HTTPRoute %s", route.Name)

	created, err := stormServices.Create(ctx, stormService, metav1.CreateOptions{})
	require.NoError(t, err, "create StormService %s", stormService.Name)

	require.NoError(t, waitForStormServiceReady(ctx, stormServices, created.Name, expectedRoles),
		"StormService %s did not become ready", created.Name)
	require.NoError(t, waitForHTTPRouteReady(ctx, gatewayClient, route.Namespace, route.Name),
		"HTTPRoute %s did not become ready", route.Name)
	request(t)
	require.NoError(t, deleteStormServiceAndWait(ctx, kubernetesClient, stormServices, created.Name),
		"delete StormService %s and its pods", created.Name)
	require.NoError(t, deleteRoutingResourcesAndWait(ctx, kubernetesClient, gatewayClient, service, route, grant),
		"delete Gateway resources for %s", created.Name)
	cleaned = true
}

func waitForStormServiceReady(
	ctx context.Context,
	stormServices orchestrationclient.StormServiceInterface,
	name string,
	expectedRoles map[string]int32,
) error {
	var latest *orchestrationv1alpha1.StormService
	var lastTransientError error
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, stormServiceTimeout, true,
		func(ctx context.Context) (bool, error) {
			var err error
			latest, err = stormServices.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if isRetryableAPIError(err) {
					lastTransientError = err
					return false, nil
				}
				return false, err
			}
			lastTransientError = nil
			return stormServiceReady(latest, expectedRoles), nil
		})
	if err == nil {
		return nil
	}
	if latest == nil {
		if lastTransientError != nil {
			return fmt.Errorf("wait for StormService %s after transient API error %v: %w", name, lastTransientError, err)
		}
		return fmt.Errorf("wait for StormService %s: %w", name, err)
	}
	return fmt.Errorf("wait for StormService %s: %w (generation=%d observed=%d ready=%d roles=%v conditions=%v)",
		name, err, latest.Generation, latest.Status.ObservedGeneration, latest.Status.ReadyReplicas,
		latest.Status.RoleStatuses, latest.Status.Conditions)
}

func deleteStormServiceAndWait(
	ctx context.Context,
	kubernetesClient kubernetes.Interface,
	stormServices orchestrationclient.StormServiceInterface,
	name string,
) error {
	propagation := metav1.DeletePropagationForeground
	err := stormServices.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete StormService %s: %w", name, err)
	}

	selector := fmt.Sprintf("%s=%s", controllerconstants.StormServiceNameLabelKey, name)
	return wait.PollUntilContextTimeout(ctx, time.Second, cleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := stormServices.Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				return false, nil
			}
			if !apierrors.IsNotFound(err) {
				if isRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}

			pods, err := kubernetesClient.CoreV1().Pods(metav1.NamespaceAll).List(ctx,
				metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				if isRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			return len(pods.Items) == 0, nil
		})
}

func newRoutingResources(
	namespace, modelName string,
) (*corev1.Service, *gatewayv1.HTTPRoute, *gatewayv1beta1.ReferenceGrant) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{modelNameLabel: modelName},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8000,
				TargetPort: intstr.FromInt32(8000),
			}},
		},
	}

	backendNamespace := gatewayv1.Namespace(namespace)
	parentNamespace := gatewayv1.Namespace(gatewayNamespace)
	path := "/v1/chat/completions"
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: modelName + "-router", Namespace: gatewayNamespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{
				Name:      "aibrix-eg",
				Namespace: &parentNamespace,
			}}},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
						Value: &path,
					},
					Headers: []gatewayv1.HTTPHeaderMatch{{
						Type:  ptr.To(gatewayv1.HeaderMatchExact),
						Name:  "model",
						Value: modelName,
					}},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name:      gatewayv1.ObjectName(modelName),
						Namespace: &backendNamespace,
						Port:      ptr.To(gatewayv1.PortNumber(8000)),
					},
				}}},
			}},
		},
	}

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: modelName + "-route-grant", Namespace: namespace},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: gatewayNamespace,
			}},
			To: []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	return service, route, grant
}

func waitForHTTPRouteReady(ctx context.Context, client gatewayclient.Interface, namespace, name string) error {
	var lastTransientError error
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true,
		func(ctx context.Context) (bool, error) {
			route, err := client.GatewayV1().HTTPRoutes(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				if isRetryableAPIError(err) {
					lastTransientError = err
					return false, nil
				}
				return false, err
			}
			lastTransientError = nil
			for _, parent := range route.Status.Parents {
				accepted := false
				resolved := false
				for _, condition := range parent.Conditions {
					switch condition.Type {
					case string(gatewayv1.RouteConditionAccepted):
						accepted = condition.Status == metav1.ConditionTrue
					case string(gatewayv1.RouteConditionResolvedRefs):
						resolved = condition.Status == metav1.ConditionTrue
					}
				}
				if accepted && resolved {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil && lastTransientError != nil {
		return fmt.Errorf("wait for HTTPRoute %s/%s after transient API error %v: %w",
			namespace, name, lastTransientError, err)
	}
	return err
}

func deleteRoutingResourcesAndWait(
	ctx context.Context,
	kubernetesClient kubernetes.Interface,
	gatewayClient gatewayclient.Interface,
	service *corev1.Service,
	route *gatewayv1.HTTPRoute,
	grant *gatewayv1beta1.ReferenceGrant,
) error {
	err := gatewayClient.GatewayV1().HTTPRoutes(route.Namespace).Delete(ctx, route.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete HTTPRoute %s: %w", route.Name, err)
	}
	err = gatewayClient.GatewayV1beta1().ReferenceGrants(grant.Namespace).
		Delete(ctx, grant.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ReferenceGrant %s: %w", grant.Name, err)
	}
	err = kubernetesClient.CoreV1().Services(service.Namespace).
		Delete(ctx, service.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Service %s: %w", service.Name, err)
	}

	return wait.PollUntilContextTimeout(ctx, time.Second, cleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, routeErr := gatewayClient.GatewayV1().HTTPRoutes(route.Namespace).Get(ctx, route.Name, metav1.GetOptions{})
			_, grantErr := gatewayClient.GatewayV1beta1().ReferenceGrants(grant.Namespace).
				Get(ctx, grant.Name, metav1.GetOptions{})
			_, serviceErr := kubernetesClient.CoreV1().Services(service.Namespace).Get(ctx, service.Name, metav1.GetOptions{})
			for _, err := range []error{routeErr, grantErr, serviceErr} {
				if err == nil || apierrors.IsNotFound(err) {
					continue
				}
				if isRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			return apierrors.IsNotFound(routeErr) && apierrors.IsNotFound(grantErr) && apierrors.IsNotFound(serviceErr), nil
		})
}

func newSingleStormService(namespace, name, modelName string) *orchestrationv1alpha1.StormService {
	return newStormService(namespace, name, modelName, []orchestrationv1alpha1.RoleSpec{
		newMockRole("worker", modelName),
	})
}

func newPDStormService(namespace, name, modelName string) *orchestrationv1alpha1.StormService {
	prefill := newMockRole("prefill", modelName)
	prefill.Template.Spec.Containers[0].Env = pdEnvironment("prefill")
	prefill.Template.Annotations = map[string]string{modelConfigAnnotation: pdRoutingConfig}
	decode := newMockRole("decode", modelName)
	decode.Template.Spec.Containers[0].Env = pdEnvironment("decode")
	decode.Template.Annotations = map[string]string{modelConfigAnnotation: pdRoutingConfig}

	stormService := newStormService(namespace, name, modelName, []orchestrationv1alpha1.RoleSpec{prefill, decode})
	stormService.Annotations = map[string]string{modelConfigAnnotation: pdRoutingConfig}
	stormService.Spec.Template.Annotations = map[string]string{modelConfigAnnotation: pdRoutingConfig}
	return stormService
}

func newStormService(
	namespace, name, modelName string,
	roles []orchestrationv1alpha1.RoleSpec,
) *orchestrationv1alpha1.StormService {
	labels := map[string]string{
		"app":          name,
		modelNameLabel: modelName,
		modelPortLabel: "8000",
	}

	return &orchestrationv1alpha1.StormService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: orchestrationv1alpha1.GroupVersion.String(),
			Kind:       orchestrationv1alpha1.StormServiceKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: orchestrationv1alpha1.StormServiceSpec{
			Replicas: ptr.To[int32](1),
			Mode:     orchestrationv1alpha1.StormServicePooledMode,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: orchestrationv1alpha1.RoleSetTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       &orchestrationv1alpha1.RoleSetSpec{Roles: roles},
			},
			UpdateStrategy: orchestrationv1alpha1.StormServiceUpdateStrategy{
				Type: orchestrationv1alpha1.InPlaceUpdateStormServiceStrategyType,
			},
		},
	}
}

func newMockRole(name, modelName string) orchestrationv1alpha1.RoleSpec {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/ready",
			Port: intstr.FromInt32(8000),
		}},
		PeriodSeconds:    2,
		FailureThreshold: 30,
	}

	return orchestrationv1alpha1.RoleSpec{
		Name:     name,
		Replicas: ptr.To[int32](1),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"app":                    modelName,
				"app.kubernetes.io/name": modelName,
				modelNameLabel:           modelName,
				modelPortLabel:           "8000",
				modelEngine:              "vllm",
			}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:           "llm-engine",
				Image:          mockImage,
				Ports:          []corev1.ContainerPort{{ContainerPort: 8000}},
				ReadinessProbe: probe,
			}}},
		},
	}
}

func pdEnvironment(role string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MOCK_PD_CONTRACT", Value: pdContract},
		{Name: "MOCK_PD_ROLE", Value: role},
	}
}

func stormServiceReady(stormService *orchestrationv1alpha1.StormService, expectedRoles map[string]int32) bool {
	if stormService.Status.ObservedGeneration < stormService.Generation ||
		stormService.Status.ReadyReplicas < stormService.Spec.ResolvedReplicas() ||
		stormService.Status.Conditions.GetCondition(orchestrationv1alpha1.StormServiceReady).Status != corev1.ConditionTrue {
		return false
	}

	readyRoles := make(map[string]int32, len(stormService.Status.RoleStatuses))
	for _, role := range stormService.Status.RoleStatuses {
		readyRoles[role.Name] = role.ReadyReplicas
	}
	for role, replicas := range expectedRoles {
		if readyRoles[role] < replicas {
			return false
		}
	}
	return true
}

func isRetryableAPIError(err error) bool {
	return apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		utilnet.IsTimeout(err) ||
		utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err)
}
