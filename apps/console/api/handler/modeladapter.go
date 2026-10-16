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

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/apps/console/api/middleware"
	aibrixclient "github.com/vllm-project/aibrix/pkg/client/clientset/versioned"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	modelAdapterAPIPath             = "/api/v1/model-adapters"
	modelAdapterTargetAPIPath       = "/api/v1/model-adapter-targets"
	modelAdapterAPIVersion          = "model.aibrix.ai/v1alpha1"
	deploymentAPIVersion            = "apps/v1"
	defaultModelAdapterScheduler    = "default"
	defaultModelServingPort         = int32(8000)
	placementAll                    = "all"
	placementSingle                 = "single"
	targetDeploymentAnnotation      = "console.aibrix.ai/target-deployment"
	modelAdapterManagedByLabel      = "app.kubernetes.io/managed-by"
	modelAdapterManagedByLabelValue = "aibrix-console"
	modelSourceEnvName              = "AIBRIX_MODEL_SOURCE_URI"
	engineTypeEnvName               = "AIBRIX_ENGINE_TYPE"
	modelAdapterAdminRole           = "admin"
	modelAdapterVLLMEngine          = "vllm"
	modelAdapterSGLangEngine        = "sglang"
)

type modelAdapterClientProvider interface {
	Client() (kubernetes.Interface, string, error)
	ModelClient() (aibrixclient.Interface, string, error)
}

// ModelAdapterHandler exposes the open-source ModelAdapter workflow as a
// narrow REST BFF over Kubernetes.
type ModelAdapterHandler struct {
	clients modelAdapterClientProvider
}

type createModelAdapterRequest struct {
	Name           string `json:"name"`
	ArtifactURL    string `json:"artifact_url"`
	DeploymentName string `json:"deployment_name"`
	Placement      string `json:"placement"`
}

type modelAdapterResponse struct {
	Name            string              `json:"name"`
	Namespace       string              `json:"namespace"`
	APIVersion      string              `json:"api_version"`
	ArtifactURL     string              `json:"artifact_url"`
	BaseModel       string              `json:"base_model"`
	SchedulerName   string              `json:"scheduler_name"`
	Placement       string              `json:"placement"`
	Phase           string              `json:"phase"`
	ReadyReplicas   int32               `json:"ready_replicas"`
	DesiredReplicas int32               `json:"desired_replicas"`
	Candidates      int32               `json:"candidates"`
	CreatedAt       string              `json:"created_at"`
	PodSelector     string              `json:"pod_selector"`
	Target          *modelAdapterTarget `json:"target,omitempty"`
	Instances       []boundPod          `json:"instances"`
}

type modelAdapterTarget struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	Kind            string `json:"kind"`
	APIVersion      string `json:"api_version"`
	BaseModel       string `json:"base_model"`
	Engine          string `json:"engine"`
	Port            int32  `json:"port"`
	ReadyReplicas   int32  `json:"ready_replicas"`
	DesiredReplicas int32  `json:"desired_replicas"`
	Selector        string `json:"selector"`
	UpdateStrategy  string `json:"update_strategy"`
	CreatedAt       string `json:"created_at"`
}

type boundPod struct {
	Name      string `json:"name"`
	Ready     string `json:"ready"`
	Status    string `json:"status"`
	Restarts  int32  `json:"restarts"`
	CreatedAt string `json:"created_at"`
	PodIP     string `json:"pod_ip"`
	Node      string `json:"node"`
}

type listModelAdaptersResponse struct {
	ModelAdapters []modelAdapterResponse `json:"model_adapters"`
}

type listModelAdapterTargetsResponse struct {
	Targets []modelAdapterTarget `json:"targets"`
}

type modelAdapterErrorResponse struct {
	Error string `json:"error"`
}

func NewModelAdapterHandler(clients modelAdapterClientProvider) *ModelAdapterHandler {
	return &ModelAdapterHandler{clients: clients}
}

func (h *ModelAdapterHandler) RegisterRoutes(mux *runtime.ServeMux) error {
	routes := []struct {
		method  string
		path    string
		handler runtime.HandlerFunc
	}{
		{http.MethodGet, modelAdapterAPIPath, h.handleList},
		{http.MethodPost, modelAdapterAPIPath, h.handleCreate},
		{http.MethodGet, modelAdapterAPIPath + "/{name}", h.handleGet},
		{http.MethodDelete, modelAdapterAPIPath + "/{name}", h.handleDelete},
		{http.MethodGet, modelAdapterTargetAPIPath, h.handleListTargets},
	}
	for _, route := range routes {
		if err := mux.HandlePath(route.method, route.path, route.handler); err != nil {
			return fmt.Errorf("register %s %s: %w", route.method, route.path, err)
		}
	}
	return nil
}

func (h *ModelAdapterHandler) handleList(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	kubeClient, modelClient, namespace, err := h.resolveClients()
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	adapters, err := modelClient.ModelV1alpha1().ModelAdapters(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	deployments, err := kubeClient.AppsV1().Deployments(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	pods, err := kubeClient.CoreV1().Pods(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	sort.Slice(adapters.Items, func(i, j int) bool {
		if adapters.Items[i].CreationTimestamp.Equal(&adapters.Items[j].CreationTimestamp) {
			return adapters.Items[i].Name < adapters.Items[j].Name
		}
		return adapters.Items[i].CreationTimestamp.After(adapters.Items[j].CreationTimestamp.Time)
	})
	podsByName := indexPods(pods.Items)
	response := listModelAdaptersResponse{
		ModelAdapters: make([]modelAdapterResponse, 0, len(adapters.Items)),
	}
	for i := range adapters.Items {
		response.ModelAdapters = append(
			response.ModelAdapters,
			buildModelAdapterResponse(&adapters.Items[i], deployments.Items, podsByName),
		)
	}
	writeModelAdapterJSON(w, http.StatusOK, response)
}

func (h *ModelAdapterHandler) handleGet(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	kubeClient, modelClient, namespace, err := h.resolveClients()
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	adapter, err := modelClient.ModelV1alpha1().ModelAdapters(namespace).Get(
		r.Context(),
		pathParams["name"],
		metav1.GetOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	deployments, err := kubeClient.AppsV1().Deployments(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	podsByName := make(map[string]*corev1.Pod, len(adapter.Status.Instances))
	for _, name := range adapter.Status.Instances {
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(r.Context(), name, metav1.GetOptions{})
		if err == nil {
			podsByName[name] = pod
		} else if !apierrors.IsNotFound(err) {
			writeModelAdapterError(w, err)
			return
		}
	}

	writeModelAdapterJSON(
		w,
		http.StatusOK,
		buildModelAdapterResponse(adapter, deployments.Items, podsByName),
	)
}

func (h *ModelAdapterHandler) handleCreate(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if err := requireModelAdapterAdmin(r.Context()); err != nil {
		writeModelAdapterError(w, err)
		return
	}

	var request createModelAdapterRequest
	if err := decodeModelAdapterRequest(r, &request); err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}
	if err := validateCreateModelAdapterRequest(&request); err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}

	kubeClient, modelClient, namespace, err := h.resolveClients()
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	deployment, err := kubeClient.AppsV1().Deployments(namespace).Get(
		r.Context(),
		request.DeploymentName,
		metav1.GetOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	if !isModelAdapterTarget(deployment) {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{
			Error: "deployment is not compatible with the ModelAdapter controller",
		})
		return
	}

	baseModel := modelAdapterBaseModel(deployment)
	adapter := &modelv1alpha1.ModelAdapter{
		TypeMeta: metav1.TypeMeta{
			APIVersion: modelAdapterAPIVersion,
			Kind:       "ModelAdapter",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      request.Name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.ModelLabelName:   request.Name,
				constants.ModelLabelPort:   strconv.Itoa(int(deploymentPort(deployment))),
				modelAdapterManagedByLabel: modelAdapterManagedByLabelValue,
			},
			Annotations: map[string]string{
				targetDeploymentAnnotation: deployment.Name,
			},
		},
		Spec: modelv1alpha1.ModelAdapterSpec{
			BaseModel:     ptr.To(baseModel),
			PodSelector:   deployment.Spec.Selector.DeepCopy(),
			SchedulerName: defaultModelAdapterScheduler,
			ArtifactURL:   request.ArtifactURL,
		},
	}
	if request.Placement == placementSingle {
		adapter.Spec.Replicas = ptr.To[int32](1)
	}

	created, err := modelClient.ModelV1alpha1().ModelAdapters(namespace).Create(
		r.Context(),
		adapter,
		metav1.CreateOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	writeModelAdapterJSON(
		w,
		http.StatusCreated,
		buildModelAdapterResponse(created, []appsv1.Deployment{*deployment}, nil),
	)
}

func (h *ModelAdapterHandler) handleDelete(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	if err := requireModelAdapterAdmin(r.Context()); err != nil {
		writeModelAdapterError(w, err)
		return
	}

	if h.clients == nil {
		writeModelAdapterJSON(w, http.StatusServiceUnavailable, modelAdapterErrorResponse{
			Error: "Kubernetes clients are not configured",
		})
		return
	}
	modelClient, namespace, err := h.clients.ModelClient()
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	if modelClient == nil {
		writeModelAdapterJSON(w, http.StatusServiceUnavailable, modelAdapterErrorResponse{
			Error: "AIBrix client is not configured",
		})
		return
	}
	if err := modelClient.ModelV1alpha1().ModelAdapters(namespace).Delete(
		r.Context(),
		pathParams["name"],
		metav1.DeleteOptions{},
	); err != nil {
		writeModelAdapterError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ModelAdapterHandler) handleListTargets(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if h.clients == nil {
		writeModelAdapterJSON(w, http.StatusServiceUnavailable, modelAdapterErrorResponse{
			Error: "Kubernetes clients are not configured",
		})
		return
	}
	kubeClient, namespace, err := h.clients.Client()
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	if kubeClient == nil {
		writeModelAdapterJSON(w, http.StatusServiceUnavailable, modelAdapterErrorResponse{
			Error: "Kubernetes client is not configured",
		})
		return
	}
	deployments, err := kubeClient.AppsV1().Deployments(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	response := listModelAdapterTargetsResponse{Targets: make([]modelAdapterTarget, 0, len(deployments.Items))}
	for i := range deployments.Items {
		if isModelAdapterTarget(&deployments.Items[i]) {
			response.Targets = append(response.Targets, buildModelAdapterTarget(&deployments.Items[i]))
		}
	}
	sort.Slice(response.Targets, func(i, j int) bool {
		return response.Targets[i].Name < response.Targets[j].Name
	})
	writeModelAdapterJSON(w, http.StatusOK, response)
}

func requireModelAdapterAdmin(ctx context.Context) error {
	user := middleware.GetUser(ctx)
	if user == nil {
		return grpcstatus.Error(codes.Unauthenticated, "authentication is required")
	}
	if !strings.EqualFold(strings.TrimSpace(user.Role), modelAdapterAdminRole) {
		return grpcstatus.Error(codes.PermissionDenied, "admin role is required to manage ModelAdapters")
	}
	return nil
}

func (h *ModelAdapterHandler) resolveClients() (kubernetes.Interface, aibrixclient.Interface, string, error) {
	if h.clients == nil {
		return nil, nil, "", grpcstatus.Error(codes.Unavailable, "Kubernetes clients are not configured")
	}
	kubeClient, namespace, err := h.clients.Client()
	if err != nil {
		return nil, nil, "", err
	}
	modelClient, modelNamespace, err := h.clients.ModelClient()
	if err != nil {
		return nil, nil, "", err
	}
	if kubeClient == nil || modelClient == nil {
		return nil, nil, "", grpcstatus.Error(codes.Unavailable, "Kubernetes clients are not configured")
	}
	if namespace != modelNamespace {
		return nil, nil, "", grpcstatus.Error(codes.Internal, "Kubernetes client namespaces do not match")
	}
	return kubeClient, modelClient, namespace, nil
}

func decodeModelAdapterRequest(r *http.Request, request *createModelAdapterRequest) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	request.Name = strings.TrimSpace(request.Name)
	request.ArtifactURL = strings.TrimSpace(request.ArtifactURL)
	request.DeploymentName = strings.TrimSpace(request.DeploymentName)
	request.Placement = strings.TrimSpace(request.Placement)
	return nil
}

func validateCreateModelAdapterRequest(request *createModelAdapterRequest) error {
	if errors := validation.IsDNS1123Subdomain(request.Name); len(errors) > 0 {
		return fmt.Errorf("name must be a valid Kubernetes DNS subdomain: %s", strings.Join(errors, ", "))
	}
	if errors := validation.IsValidLabelValue(request.Name); len(errors) > 0 {
		return fmt.Errorf("name must be a valid Kubernetes label value: %s", strings.Join(errors, ", "))
	}
	if request.DeploymentName == "" {
		return fmt.Errorf("deployment_name is required")
	}
	if request.Placement != placementAll && request.Placement != placementSingle {
		return fmt.Errorf("placement must be %q or %q", placementAll, placementSingle)
	}
	if request.ArtifactURL == "" {
		return fmt.Errorf("artifact_url is required")
	}
	if _, err := url.ParseRequestURI(request.ArtifactURL); err != nil {
		return fmt.Errorf("artifact_url is invalid: %w", err)
	}
	if err := utils.ValidateArtifactURL(request.ArtifactURL); err != nil {
		return fmt.Errorf("artifact_url uses an unsupported scheme: %w", err)
	}
	return nil
}

func buildModelAdapterResponse(
	adapter *modelv1alpha1.ModelAdapter,
	deployments []appsv1.Deployment,
	podsByName map[string]*corev1.Pod,
) modelAdapterResponse {
	baseModel := ""
	if adapter.Spec.BaseModel != nil {
		baseModel = *adapter.Spec.BaseModel
	}
	placement := placementAll
	if adapter.Spec.Replicas != nil {
		placement = placementSingle
	}
	phase := string(adapter.Status.Phase)
	if phase == "" {
		phase = string(modelv1alpha1.ModelAdapterPending)
	}

	response := modelAdapterResponse{
		Name:            adapter.Name,
		Namespace:       adapter.Namespace,
		APIVersion:      modelAdapterAPIVersion,
		ArtifactURL:     adapter.Spec.ArtifactURL,
		BaseModel:       baseModel,
		SchedulerName:   adapter.Spec.SchedulerName,
		Placement:       placement,
		Phase:           phase,
		ReadyReplicas:   adapter.Status.ReadyReplicas,
		DesiredReplicas: adapter.Status.DesiredReplicas,
		Candidates:      adapter.Status.Candidates,
		CreatedAt:       formatKubernetesTime(adapter.CreationTimestamp),
		PodSelector:     formatLabelSelector(adapter.Spec.PodSelector),
		Instances:       make([]boundPod, 0, len(adapter.Status.Instances)),
	}
	if target := findTargetDeployment(adapter, deployments); target != nil {
		targetResponse := buildModelAdapterTarget(target)
		response.Target = &targetResponse
		response.BaseModel = targetResponse.BaseModel
	}
	for _, name := range adapter.Status.Instances {
		response.Instances = append(response.Instances, buildBoundPod(name, podsByName[name]))
	}
	return response
}

func buildModelAdapterTarget(deployment *appsv1.Deployment) modelAdapterTarget {
	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	return modelAdapterTarget{
		Name:            deployment.Name,
		Namespace:       deployment.Namespace,
		Kind:            "Deployment",
		APIVersion:      deploymentAPIVersion,
		BaseModel:       deploymentBaseModel(deployment),
		Engine:          deploymentEngine(deployment),
		Port:            deploymentPort(deployment),
		ReadyReplicas:   deployment.Status.ReadyReplicas,
		DesiredReplicas: desiredReplicas,
		Selector:        formatLabelSelector(deployment.Spec.Selector),
		UpdateStrategy:  string(deployment.Spec.Strategy.Type),
		CreatedAt:       formatKubernetesTime(deployment.CreationTimestamp),
	}
}

func findTargetDeployment(
	adapter *modelv1alpha1.ModelAdapter,
	deployments []appsv1.Deployment,
) *appsv1.Deployment {
	targetName := adapter.Annotations[targetDeploymentAnnotation]
	if targetName != "" {
		for i := range deployments {
			if deployments[i].Name == targetName {
				return &deployments[i]
			}
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(adapter.Spec.PodSelector)
	if err != nil || selector.Empty() {
		return nil
	}
	for i := range deployments {
		if selector.Matches(labels.Set(deployments[i].Spec.Template.Labels)) {
			return &deployments[i]
		}
	}
	return nil
}

func isModelAdapterTarget(deployment *appsv1.Deployment) bool {
	if deployment == nil || deployment.Spec.Selector == nil {
		return false
	}
	if deployment.DeletionTimestamp != nil ||
		len(deployment.Spec.Selector.MatchLabels) == 0 ||
		len(deployment.Spec.Selector.MatchExpressions) > 0 {
		return false
	}
	if !hasExplicitBaseModel(deployment) {
		return false
	}
	engine := strings.ToLower(strings.TrimSpace(deploymentEngine(deployment)))
	if engine != modelAdapterVLLMEngine && engine != modelAdapterSGLangEngine {
		return false
	}
	if deploymentPort(deployment) != defaultModelServingPort {
		return false
	}
	return modelAdapterEnabled(deployment) || hasExplicitEngine(deployment)
}

func hasExplicitBaseModel(deployment *appsv1.Deployment) bool {
	if _, ok := constants.ModelNameFromMetadata(
		deployment.Spec.Template.Labels,
		deployment.Spec.Template.Annotations,
	); ok {
		return true
	}
	if _, ok := constants.ModelNameFromMetadata(deployment.Labels, deployment.Annotations); ok {
		return true
	}
	return deploymentContainerEnv(deployment, modelSourceEnvName) != ""
}

func hasExplicitEngine(deployment *appsv1.Deployment) bool {
	return deployment.Spec.Template.Labels[constants.ModelLabelEngine] != "" ||
		deployment.Labels[constants.ModelLabelEngine] != "" ||
		deploymentContainerEnv(deployment, engineTypeEnvName) != ""
}

func modelAdapterEnabled(deployment *appsv1.Deployment) bool {
	return strings.EqualFold(
		deployment.Spec.Template.Labels[constants.ModelLabelAdapterEnabled],
		"true",
	) || strings.EqualFold(
		deployment.Labels[constants.ModelLabelAdapterEnabled],
		"true",
	)
}

func deploymentBaseModel(deployment *appsv1.Deployment) string {
	if value, ok := constants.ModelNameFromMetadata(
		deployment.Spec.Template.Labels,
		deployment.Spec.Template.Annotations,
	); ok {
		return value
	}
	if value, ok := constants.ModelNameFromMetadata(deployment.Labels, deployment.Annotations); ok {
		return value
	}
	if value := deploymentContainerEnv(deployment, modelSourceEnvName); value != "" {
		return value
	}
	return deployment.Name
}

func modelAdapterBaseModel(deployment *appsv1.Deployment) string {
	baseModel := deploymentBaseModel(deployment)
	if len(validation.IsValidLabelValue(baseModel)) == 0 {
		return baseModel
	}
	// The controller projects spec.baseModel into a Service label. Model URIs
	// commonly contain slashes, so use the stable workload name when the
	// display identifier cannot be represented as a Kubernetes label value.
	return deployment.Name
}

func deploymentEngine(deployment *appsv1.Deployment) string {
	if value := deployment.Spec.Template.Labels[constants.ModelLabelEngine]; value != "" {
		return value
	}
	if value := deployment.Labels[constants.ModelLabelEngine]; value != "" {
		return value
	}
	if value := deploymentContainerEnv(deployment, engineTypeEnvName); value != "" {
		return value
	}
	return modelAdapterVLLMEngine
}

func deploymentContainerEnv(deployment *appsv1.Deployment, name string) string {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
}

func deploymentPort(deployment *appsv1.Deployment) int32 {
	for _, source := range []map[string]string{deployment.Spec.Template.Labels, deployment.Labels} {
		if value := source[constants.ModelLabelPort]; value != "" {
			if port, err := strconv.ParseInt(value, 10, 32); err == nil && port > 0 && port <= 65535 {
				return int32(port)
			}
		}
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == "http" && port.ContainerPort > 0 {
				return port.ContainerPort
			}
		}
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			if port.ContainerPort > 0 {
				return port.ContainerPort
			}
		}
	}
	return defaultModelServingPort
}

func indexPods(pods []corev1.Pod) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod, len(pods))
	for i := range pods {
		result[pods[i].Name] = &pods[i]
	}
	return result
}

func buildBoundPod(name string, pod *corev1.Pod) boundPod {
	if pod == nil {
		return boundPod{Name: name, Status: "Unknown", Ready: "0/0"}
	}
	readyContainers := 0
	restarts := int32(0)
	for _, container := range pod.Status.ContainerStatuses {
		if container.Ready {
			readyContainers++
		}
		restarts += container.RestartCount
	}
	return boundPod{
		Name:      pod.Name,
		Ready:     fmt.Sprintf("%d/%d", readyContainers, len(pod.Spec.Containers)),
		Status:    string(pod.Status.Phase),
		Restarts:  restarts,
		CreatedAt: formatKubernetesTime(pod.CreationTimestamp),
		PodIP:     pod.Status.PodIP,
		Node:      pod.Spec.NodeName,
	}
}

func formatLabelSelector(selector *metav1.LabelSelector) string {
	if selector == nil {
		return ""
	}
	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return ""
	}
	return parsed.String()
}

func formatKubernetesTime(value metav1.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func writeModelAdapterJSON(w http.ResponseWriter, statusCode int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if statusCode == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(value); err != nil {
		klog.Errorf("write ModelAdapter response: %v", err)
	}
}

func writeModelAdapterError(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	switch {
	case apierrors.IsNotFound(err):
		statusCode = http.StatusNotFound
	case apierrors.IsAlreadyExists(err):
		statusCode = http.StatusConflict
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		statusCode = http.StatusBadRequest
	case apierrors.IsForbidden(err):
		statusCode = http.StatusForbidden
	case apierrors.IsUnauthorized(err):
		statusCode = http.StatusUnauthorized
	default:
		switch grpcstatus.Code(err) {
		case codes.InvalidArgument:
			statusCode = http.StatusBadRequest
		case codes.NotFound:
			statusCode = http.StatusNotFound
		case codes.AlreadyExists:
			statusCode = http.StatusConflict
		case codes.PermissionDenied:
			statusCode = http.StatusForbidden
		case codes.Unauthenticated:
			statusCode = http.StatusUnauthorized
		case codes.FailedPrecondition:
			statusCode = http.StatusPreconditionFailed
		case codes.Unavailable:
			statusCode = http.StatusServiceUnavailable
		}
	}
	writeModelAdapterJSON(w, statusCode, modelAdapterErrorResponse{Error: err.Error()})
}
