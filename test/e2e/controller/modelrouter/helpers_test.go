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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
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

	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	modelclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	framework "github.com/vllm-project/aibrix/test/e2e/framework"
)

const (
	modelRouterGatewayNamespace        = "aibrix-system"
	modelRouterGatewayName             = "aibrix-eg"
	modelRouterControllerNamespaceEnv  = "AIBRIX_ROLESET_CONTROLLER_NAMESPACE"
	modelRouterControllerDeploymentEnv = "AIBRIX_ROLESET_CONTROLLER_DEPLOYMENT"
	modelRouterControllerDeployment    = "controller-manager"
	modelRouterBackendPort             = int32(8000)
	modelRouterPollInterval            = time.Second
	modelRouterPollTimeout             = 2 * time.Minute
	modelRouterCleanupTimeout          = time.Minute
	modelRouterKeepOnFailureEnv        = "AIBRIX_E2E_KEEP_RESOURCES_ON_FAILURE"
)

type modelRouterHarness struct {
	namespace     string
	config        framework.Config
	kubeClient    *kubernetes.Clientset
	modelClient   *modelclient.Clientset
	gatewayClient *gatewayclient.Clientset
	models        map[string]struct{}
}

func newModelRouterHarness(t *testing.T, ctx context.Context) *modelRouterHarness {
	t.Helper()

	kubeConfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
	if kubeConfig == "" {
		t.Fatal("KUBECONFIG must be set for ModelRouter E2E tests")
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
	if err != nil {
		t.Fatalf("build Kubernetes config: %v", err)
	}
	restConfig.QPS = 50
	restConfig.Burst = 100

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	aibrixClient, err := modelclient.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create AIBrix client: %v", err)
	}
	gatewayAPIClient, err := gatewayclient.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create Gateway API client: %v", err)
	}

	h := &modelRouterHarness{
		namespace:     "modelrouter-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		config:        framework.LoadConfig(),
		kubeClient:    kubeClient,
		modelClient:   aibrixClient,
		gatewayClient: gatewayAPIClient,
		models:        make(map[string]struct{}),
	}
	_, err = h.kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: h.namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create test namespace %s: %v", h.namespace, err)
	}
	t.Cleanup(func() { h.cleanup(t) })
	return h
}

func (h *modelRouterHarness) cleanup(t *testing.T) {
	t.Helper()
	keepNamespace := false
	if t.Failed() {
		h.logDiagnostics(t)
		keepNamespace = strings.EqualFold(strings.TrimSpace(os.Getenv(modelRouterKeepOnFailureEnv)), "true")
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelRouterCleanupTimeout)
	defer cancel()
	h.deleteHTTPRoutes(t, ctx)
	if keepNamespace {
		t.Logf("preserving ModelRouter E2E namespace %s after failure", h.namespace)
		return
	}

	err := h.kubeClient.CoreV1().Namespaces().Delete(ctx, h.namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete ModelRouter E2E namespace %s: %v", h.namespace, err)
	}
}

func (h *modelRouterHarness) deleteHTTPRoutes(t *testing.T, ctx context.Context) {
	t.Helper()
	for model := range h.models {
		err := h.gatewayClient.GatewayV1().HTTPRoutes(modelRouterGatewayNamespace).
			Delete(ctx, utils.ModelRouterName(model), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete HTTPRoute for model %s: %v", model, err)
		}
	}
}

func (h *modelRouterHarness) createBackend(
	t *testing.T,
	ctx context.Context,
	name, model string,
	customPaths []string,
) string {
	t.Helper()
	h.models[model] = struct{}{}

	labels := map[string]string{
		"app":                      name,
		constants.ModelLabelName:   model,
		constants.ModelLabelEngine: "vllm",
		constants.ModelLabelPort:   strconv.Itoa(int(modelRouterBackendPort)),
	}
	annotations := map[string]string{constants.ModelAnnoServiceName: name}
	if len(customPaths) > 0 {
		annotations[constants.ModelAnnoRouterCustomPath] = strings.Join(customPaths, ",")
	}

	_, err := h.kubeClient.CoreV1().Services(h.namespace).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       modelRouterBackendPort,
				TargetPort: intstr.FromInt32(modelRouterBackendPort),
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create backend Service %s/%s: %v", h.namespace, name, err)
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   h.namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To[int64](1),
					Containers: []corev1.Container{{
						Name:            "llm-engine",
						Image:           "aibrix/vllm-mock:nightly",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{
							"python3", "app.py", "--api_key", h.config.APIKey,
						},
						Env: []corev1.EnvVar{
							{Name: "MODEL_NAME", Value: model},
							{Name: "DEPLOYMENT_NAME", Value: name},
							{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
							}},
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							}},
							{Name: "SERVER_PORT", Value: strconv.Itoa(int(modelRouterBackendPort))},
							{Name: "STANDALONE_MODE", Value: "true"},
							{Name: "SIMULATION", Value: "disabled"},
							{Name: "MOCK_REQUEST_DURATION_SECONDS", Value: "0"},
						},
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: modelRouterBackendPort}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(modelRouterBackendPort),
							}},
							PeriodSeconds:    1,
							FailureThreshold: 60,
						},
					}},
				},
			},
		},
	}
	_, err = h.kubeClient.AppsV1().Deployments(h.namespace).
		Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create backend Deployment %s/%s: %v", h.namespace, name, err)
	}
	return h.waitForReadyPod(t, ctx, name)
}

func (h *modelRouterHarness) createRouteProbeDeployment(
	t *testing.T,
	ctx context.Context,
	name, model, serviceName string,
) {
	t.Helper()
	h.models[model] = struct{}{}
	_, err := h.kubeClient.AppsV1().Deployments(h.namespace).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: h.namespace,
			Labels: map[string]string{
				constants.ModelLabelName: model,
				constants.ModelLabelPort: strconv.Itoa(int(modelRouterBackendPort)),
			},
			Annotations: map[string]string{constants.ModelAnnoServiceName: serviceName},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](0),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "unused", Image: "aibrix/vllm-mock:nightly",
				}}},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create route probe Deployment %s/%s: %v", h.namespace, name, err)
	}
}

func (h *modelRouterHarness) createModelAdapter(
	t *testing.T,
	ctx context.Context,
	name, model, serviceName string,
) {
	t.Helper()
	h.models[model] = struct{}{}
	_, err := h.modelClient.ModelV1alpha1().ModelAdapters(h.namespace).Create(ctx, &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: h.namespace,
			Labels: map[string]string{
				constants.ModelLabelName: model,
				constants.ModelLabelPort: strconv.Itoa(int(modelRouterBackendPort)),
			},
			Annotations: map[string]string{constants.ModelAnnoServiceName: serviceName},
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			ArtifactURL: "huggingface://aibrix/" + model,
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"modelrouter-e2e/fixture": h.namespace,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create ModelAdapter %s/%s: %v", h.namespace, name, err)
	}
}

func (h *modelRouterHarness) waitForReadyPod(t *testing.T, ctx context.Context, deployment string) string {
	t.Helper()
	var podName string
	err := wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app=" + deployment,
			})
			if err != nil {
				if isModelRouterRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			if len(pods.Items) != 1 {
				return false, nil
			}
			for _, condition := range pods.Items[0].Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					podName = pods.Items[0].Name
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("wait for backend Deployment %s/%s: %v", h.namespace, deployment, err)
	}
	return podName
}

func (h *modelRouterHarness) waitForRouteReady(t *testing.T, ctx context.Context, model string) *gatewayv1.HTTPRoute {
	t.Helper()
	var route *gatewayv1.HTTPRoute
	err := wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			current, err := h.gatewayClient.GatewayV1().HTTPRoutes(modelRouterGatewayNamespace).
				Get(ctx, utils.ModelRouterName(model), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				if isModelRouterRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			route = current
			return httpRouteReady(current), nil
		})
	if err != nil {
		t.Fatalf("wait for ready HTTPRoute for model %s: %v; latest=%+v", model, err, route)
	}
	return route
}

func (h *modelRouterHarness) waitForRouteDeleted(t *testing.T, ctx context.Context, model string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := h.gatewayClient.GatewayV1().HTTPRoutes(modelRouterGatewayNamespace).
				Get(ctx, utils.ModelRouterName(model), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if isModelRouterRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		})
	if err != nil {
		t.Fatalf("wait for HTTPRoute deletion for model %s: %v", model, err)
	}
}

func (h *modelRouterHarness) waitForReferenceGrant(
	t *testing.T,
	ctx context.Context,
) *gatewayv1beta1.ReferenceGrant {
	t.Helper()
	name := fmt.Sprintf("%s-reserved-referencegrant-in-%s", modelRouterGatewayNamespace, h.namespace)
	var grant *gatewayv1beta1.ReferenceGrant
	err := wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			current, err := h.gatewayClient.GatewayV1beta1().ReferenceGrants(h.namespace).
				Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				if isModelRouterRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			grant = current
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for ReferenceGrant %s/%s: %v", h.namespace, name, err)
	}
	return grant
}

func (h *modelRouterHarness) waitForReferenceGrantDeleted(t *testing.T, ctx context.Context) {
	t.Helper()
	name := fmt.Sprintf("%s-reserved-referencegrant-in-%s", modelRouterGatewayNamespace, h.namespace)
	err := wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := h.gatewayClient.GatewayV1beta1().ReferenceGrants(h.namespace).
				Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if isModelRouterRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		})
	if err != nil {
		t.Fatalf("wait for ReferenceGrant deletion %s/%s: %v", h.namespace, name, err)
	}
}

func (h *modelRouterHarness) waitForGatewayRequest(
	t *testing.T,
	ctx context.Context,
	model, podName string,
) framework.MockRequestRecord {
	t.Helper()
	// Envoy replaces external request IDs. Put the same unique marker in the
	// request body so the isolated backend record can still be correlated, then
	// separately assert that Envoy supplied a non-empty recorder request ID.
	requestID := uuid.New().String()
	body, err := json.Marshal(map[string]interface{}{
		"model": model,
		"messages": []map[string]string{{
			"role": "user", "content": "modelrouter e2e reachability check " + requestID,
		}},
		"max_tokens": 1,
	})
	if err != nil {
		t.Fatalf("marshal gateway request: %v", err)
	}

	var lastErr error
	err = wait.PollUntilContextTimeout(ctx, 2*time.Second, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, lastErr = sendModelRouteRequest(ctx, h.config, model, requestID, body)
			return lastErr == nil, nil
		})
	if err != nil {
		t.Fatalf("gateway request for model %s did not succeed: %v; last error: %v", model, err, lastErr)
	}

	var matched framework.MockRequestRecord
	err = wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			records, err := queryAllMockRequests(ctx, h.kubeClient, h.namespace, podName)
			if err != nil {
				return false, nil
			}
			for _, record := range records {
				if record.Path == "/v1/chat/completions" &&
					record.Outcome == "success" &&
					record.StatusCode == http.StatusOK &&
					bytes.Contains(record.ParsedJSON, []byte(requestID)) {
					matched = record
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("mock pod %s did not record successful request marker %s: %v", podName, requestID, err)
	}
	if matched.RequestID == "" {
		t.Fatalf("mock pod %s recorded request marker %s without a request ID", podName, requestID)
	}
	return matched
}

func queryAllMockRequests(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, podName string,
) ([]framework.MockRequestRecord, error) {
	body, err := client.CoreV1().RESTClient().Get().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("proxy").
		Suffix("debug/requests").
		Do(ctx).
		Raw()
	if err != nil {
		return nil, fmt.Errorf("query all mock requests for pod %q: %w", podName, err)
	}
	return framework.DecodeMockRequestRecords(body)
}

func sendModelRouteRequest(
	ctx context.Context,
	config framework.Config,
	model, requestID string,
	body []byte,
) (framework.PDRequestResult, error) {
	result := framework.PDRequestResult{RequestID: requestID}
	url := strings.TrimRight(config.GatewayURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("model", model)
	req.Header.Set("x-request-id", requestID)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return result, err
	}
	defer func() { _ = resp.Body.Close() }()

	result.StatusCode = resp.StatusCode
	result.Headers = resp.Header.Clone()
	result.Body, err = io.ReadAll(resp.Body)
	if err != nil {
		return result, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf("model route request failed with status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(result.Body)))
	}
	return result, nil
}

func (h *modelRouterHarness) restartController(t *testing.T, ctx context.Context) {
	t.Helper()
	controllerNamespace := modelRouterEnvOrDefault(modelRouterControllerNamespaceEnv, modelRouterGatewayNamespace)
	controllerDeployment := modelRouterEnvOrDefault(modelRouterControllerDeploymentEnv, modelRouterControllerDeployment)
	deployments := h.kubeClient.AppsV1().Deployments(controllerNamespace)
	podsClient := h.kubeClient.CoreV1().Pods(controllerNamespace)
	deployment, err := deployments.Get(ctx, controllerDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get controller deployment: %v", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		t.Fatalf("build controller pod selector: %v", err)
	}
	pods, err := podsClient.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		t.Fatalf("list controller pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatal("controller deployment has no pods to restart")
	}
	oldUIDs := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		oldUIDs[string(pods.Items[i].UID)] = struct{}{}
		if err := podsClient.Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete controller pod %s: %v", pods.Items[i].Name, err)
		}
	}

	err = wait.PollUntilContextTimeout(ctx, modelRouterPollInterval, modelRouterPollTimeout, true,
		func(ctx context.Context) (bool, error) {
			current, err := deployments.Get(ctx, controllerDeployment, metav1.GetOptions{})
			if err != nil {
				if isModelRouterRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			currentPods, err := podsClient.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
			if err != nil {
				if isModelRouterRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}
			desired := int32(1)
			if current.Spec.Replicas != nil {
				desired = *current.Spec.Replicas
			}
			if int32(len(currentPods.Items)) < desired || current.Status.AvailableReplicas < desired {
				return false, nil
			}
			ready := int32(0)
			for i := range currentPods.Items {
				if _, old := oldUIDs[string(currentPods.Items[i].UID)]; old {
					return false, nil
				}
				for _, condition := range currentPods.Items[i].Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						ready++
						break
					}
				}
			}
			return ready >= desired, nil
		})
	if err != nil {
		t.Fatalf("wait for controller restart: %v", err)
	}
}

func (h *modelRouterHarness) routeCount(t *testing.T, ctx context.Context, model string) int {
	t.Helper()
	routes, err := h.gatewayClient.GatewayV1().HTTPRoutes(modelRouterGatewayNamespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list HTTPRoutes: %v", err)
	}
	want := utils.ModelRouterName(model)
	count := 0
	for i := range routes.Items {
		if routes.Items[i].Name == want {
			count++
		}
	}
	return count
}

func routePaths(route *gatewayv1.HTTPRoute) []string {
	if len(route.Spec.Rules) == 0 {
		return nil
	}
	paths := make([]string, 0, len(route.Spec.Rules[0].Matches))
	for _, match := range route.Spec.Rules[0].Matches {
		if match.Path != nil && match.Path.Value != nil {
			paths = append(paths, *match.Path.Value)
		}
	}
	return paths
}

func httpRouteReady(route *gatewayv1.HTTPRoute) bool {
	if route == nil {
		return false
	}
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
			return true
		}
	}
	return false
}

func isModelRouterRetryableAPIError(err error) bool {
	return apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) ||
		utilnet.IsTimeout(err) ||
		utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err) ||
		utilnet.IsConnectionRefused(err)
}

func modelRouterEnvOrDefault(name, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return defaultValue
}

func (h *modelRouterHarness) logDiagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if deployments, err := h.kubeClient.AppsV1().Deployments(h.namespace).List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ModelRouter E2E deployments in %s: %+v", h.namespace, deployments.Items)
	}
	if services, err := h.kubeClient.CoreV1().Services(h.namespace).List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ModelRouter E2E services in %s: %+v", h.namespace, services.Items)
	}
	if slices, err := h.kubeClient.DiscoveryV1().EndpointSlices(h.namespace).List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ModelRouter E2E endpoint slices in %s: %+v", h.namespace, slices.Items)
	}
	if pods, err := h.kubeClient.CoreV1().Pods(h.namespace).List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ModelRouter E2E pods in %s: %+v", h.namespace, pods.Items)
		for i := range pods.Items {
			logs, logErr := h.kubeClient.CoreV1().Pods(h.namespace).
				GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{Container: "llm-engine", TailLines: ptr.To[int64](100)}).
				DoRaw(ctx)
			if logErr != nil {
				t.Logf("failed to get backend logs from %s: %v", pods.Items[i].Name, logErr)
			} else {
				t.Logf("backend logs from %s:\n%s", pods.Items[i].Name, logs)
			}
		}
	}
	if events, err := h.kubeClient.CoreV1().Events(h.namespace).List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ModelRouter E2E events in %s: %+v", h.namespace, events.Items)
	}
	if routes, err := h.gatewayClient.GatewayV1().HTTPRoutes(modelRouterGatewayNamespace).
		List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("HTTPRoutes in %s: %+v", modelRouterGatewayNamespace, routes.Items)
	}
	if grants, err := h.gatewayClient.GatewayV1beta1().ReferenceGrants(h.namespace).
		List(ctx, metav1.ListOptions{}); err == nil {
		t.Logf("ReferenceGrants in %s: %+v", h.namespace, grants.Items)
	}

	controllerNamespace := modelRouterEnvOrDefault(modelRouterControllerNamespaceEnv, modelRouterGatewayNamespace)
	controllerDeployment := modelRouterEnvOrDefault(modelRouterControllerDeploymentEnv, modelRouterControllerDeployment)
	controller, err := h.kubeClient.AppsV1().Deployments(controllerNamespace).
		Get(ctx, controllerDeployment, metav1.GetOptions{})
	if err != nil {
		return
	}
	selector, err := metav1.LabelSelectorAsSelector(controller.Spec.Selector)
	if err != nil {
		return
	}
	pods, err := h.kubeClient.CoreV1().Pods(controllerNamespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return
	}
	for i := range pods.Items {
		logs, err := h.kubeClient.CoreV1().Pods(controllerNamespace).
			GetLogs(pods.Items[i].Name, &corev1.PodLogOptions{
				Container: "manager",
				TailLines: ptr.To[int64](200),
			}).
			DoRaw(ctx)
		if err != nil {
			t.Logf("failed to get controller logs from %s: %v", pods.Items[i].Name, err)
		} else {
			t.Logf("controller logs from %s:\n%s", pods.Items[i].Name, logs)
		}
	}
}
