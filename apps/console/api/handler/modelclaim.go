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
	"strings"
	"unicode"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	modelClaimAPIPath      = "/api/v1/model-claims"
	modelClaimPoolAPIPath  = "/api/v1/model-claim-pools"
	modelClaimAPIVersion   = "model.aibrix.ai/v1alpha1"
	modelClaimVLLMEngine   = "vllm"
	modelClaimSGLangEngine = "sglang"
)

// modelClaimLauncherFlags are the engine flags the runtime launcher sets for
// every engine it starts. It passes them after engineConfig.args, so its own
// values win, and a claim that sets one again is refused rather than ignored.
var modelClaimLauncherFlags = map[string]bool{
	"--model":               true,
	"--model-path":          true,
	"--served-model-name":   true,
	"--host":                true,
	"--port":                true,
	"--enable-sleep-mode":   true,
	"--enable-memory-saver": true,
}

// modelClaimConditionOrder lists the condition types in lifecycle order, so
// the console shows them the way a claim moves through them.
var modelClaimConditionOrder = map[string]int{
	string(modelv1alpha1.ModelClaimConditionTypeInitialized): 0,
	string(modelv1alpha1.ModelClaimConditionTypeScheduled):   1,
	string(modelv1alpha1.ModelClaimConditionTypeLoaded):      2,
	string(modelv1alpha1.ModelClaimConditionReady):           3,
}

// ModelClaimHandler exposes ModelClaims and the warm runtime pools they are
// placed on as a narrow REST BFF over Kubernetes. Kubernetes is the source of
// truth, so claims applied with kubectl are listed alongside the ones created
// here.
type ModelClaimHandler struct {
	clients modelAdapterClientProvider
}

type modelClaimPerGPURequest struct {
	MaximumFootprint string `json:"maximum_footprint"`
	KVFloor          string `json:"kv_floor"`
}

type createModelClaimRequest struct {
	Name        string                   `json:"name"`
	ModelName   string                   `json:"model_name"`
	ArtifactURL string                   `json:"artifact_url"`
	Engine      string                   `json:"engine"`
	PoolName    string                   `json:"pool_name"`
	PerGPU      *modelClaimPerGPURequest `json:"per_gpu"`
	Priority    int32                    `json:"priority"`
	EngineArgs  map[string]string        `json:"engine_args"`
}

type updateModelClaimRequest struct {
	PerGPU *modelClaimPerGPURequest `json:"per_gpu"`
}

type modelClaimResponse struct {
	Name             string                        `json:"name"`
	Namespace        string                        `json:"namespace"`
	APIVersion       string                        `json:"api_version"`
	ModelName        string                        `json:"model_name"`
	ArtifactURL      string                        `json:"artifact_url"`
	Engine           string                        `json:"engine"`
	PoolName         string                        `json:"pool_name"`
	PodSelector      string                        `json:"pod_selector"`
	PerGPU           *modelClaimPerGPUResponse     `json:"per_gpu,omitempty"`
	Priority         int32                         `json:"priority"`
	EngineArgs       map[string]string             `json:"engine_args"`
	Phase            string                        `json:"phase"`
	Candidates       int32                         `json:"candidates"`
	ReadyReplicas    int32                         `json:"ready_replicas"`
	DesiredReplicas  int32                         `json:"desired_replicas"`
	CreatedAt        string                        `json:"created_at"`
	Deleting         bool                          `json:"deleting"`
	ManagedByConsole bool                          `json:"managed_by_console"`
	Conditions       []modelClaimConditionResponse `json:"conditions"`
	Instances        []modelClaimInstanceResponse  `json:"instances"`
}

type modelClaimPerGPUResponse struct {
	MaximumFootprint string `json:"maximum_footprint"`
	KVFloor          string `json:"kv_floor"`
}

type modelClaimConditionResponse struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"last_transition_time"`
}

type modelClaimInstanceResponse struct {
	Pod          string `json:"pod"`
	Port         int32  `json:"port"`
	Phase        string `json:"phase"`
	KVLimitBytes int64  `json:"kv_limit_bytes"`
	Reason       string `json:"reason"`
	// Route is the routing annotation the controller wrote on the Pod for this
	// claim, which is what the gateway routes by.
	Route *modelClaimRouteResponse `json:"route,omitempty"`
	// WakeRequestedAt is set while a request for the sleeping engine waits on
	// the controller to wake it.
	WakeRequestedAt string   `json:"wake_requested_at,omitempty"`
	PodStatus       boundPod `json:"pod_status"`
}

type modelClaimRouteResponse struct {
	State         string `json:"state"`
	Port          int    `json:"port"`
	Reason        string `json:"reason,omitempty"`
	WakeByRequest bool   `json:"wake_by_request"`
}

type modelClaimPoolResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Workloads are the Deployments whose Pod template joins the pool.
	Workloads []string `json:"workloads"`
	// Pods carry the pool name; EnabledPods also carry the enabled label that
	// placement requires; ReadyPods are enabled, Ready and not terminating.
	Pods        int32 `json:"pods"`
	EnabledPods int32 `json:"enabled_pods"`
	ReadyPods   int32 `json:"ready_pods"`
	// GPUCounts are the distinct nvidia.com/gpu counts of the pool's enabled
	// Pods, or of its workload templates while it has no Pods. A
	// topology-homogeneous pool has exactly one.
	GPUCounts []int64  `json:"gpu_counts"`
	Images    []string `json:"images"`
	Claims    int32    `json:"claims"`
	Policy    string   `json:"policy,omitempty"`
}

type listModelClaimsResponse struct {
	ModelClaims []modelClaimResponse `json:"model_claims"`
}

type listModelClaimPoolsResponse struct {
	Pools []modelClaimPoolResponse `json:"pools"`
}

func NewModelClaimHandler(clients modelAdapterClientProvider) *ModelClaimHandler {
	return &ModelClaimHandler{clients: clients}
}

func (h *ModelClaimHandler) RegisterRoutes(mux *runtime.ServeMux) error {
	routes := []struct {
		method  string
		path    string
		handler runtime.HandlerFunc
	}{
		{http.MethodGet, modelClaimAPIPath, h.handleList},
		{http.MethodPost, modelClaimAPIPath, h.handleCreate},
		{http.MethodGet, modelClaimAPIPath + "/{name}", h.handleGet},
		{http.MethodPatch, modelClaimAPIPath + "/{name}", h.handleUpdate},
		{http.MethodDelete, modelClaimAPIPath + "/{name}", h.handleDelete},
		{http.MethodGet, modelClaimPoolAPIPath, h.handleListPools},
	}
	for _, route := range routes {
		if err := mux.HandlePath(route.method, route.path, route.handler); err != nil {
			return fmt.Errorf("register %s %s: %w", route.method, route.path, err)
		}
	}
	return nil
}

func (h *ModelClaimHandler) handleList(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	kubeClient, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	claims, err := modelClient.ModelV1alpha1().ModelClaims(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	podsByName, err := listModelClaimInstancePods(r.Context(), kubeClient, namespace, claims.Items)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	sort.Slice(claims.Items, func(i, j int) bool {
		if claims.Items[i].CreationTimestamp.Equal(&claims.Items[j].CreationTimestamp) {
			return claims.Items[i].Name < claims.Items[j].Name
		}
		return claims.Items[i].CreationTimestamp.After(claims.Items[j].CreationTimestamp.Time)
	})
	response := listModelClaimsResponse{
		ModelClaims: make([]modelClaimResponse, 0, len(claims.Items)),
	}
	for i := range claims.Items {
		response.ModelClaims = append(response.ModelClaims, buildModelClaimResponse(&claims.Items[i], podsByName))
	}
	writeModelAdapterJSON(w, http.StatusOK, response)
}

func (h *ModelClaimHandler) handleGet(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	kubeClient, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}

	claim, err := modelClient.ModelV1alpha1().ModelClaims(namespace).Get(
		r.Context(),
		pathParams["name"],
		metav1.GetOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	podsByName, err := getModelClaimInstancePods(r.Context(), kubeClient, namespace, claim)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	writeModelAdapterJSON(w, http.StatusOK, buildModelClaimResponse(claim, podsByName))
}

func (h *ModelClaimHandler) handleCreate(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	if err := requireAdminRole(r.Context(), "ModelClaims"); err != nil {
		writeModelAdapterError(w, err)
		return
	}

	var request createModelClaimRequest
	if err := decodeModelClaimRequest(r, &request); err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}
	normalizeCreateModelClaimRequest(&request)
	claim, parallelism, err := buildModelClaim(&request)
	if err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}

	kubeClient, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	claim.Namespace = namespace

	existing, err := modelClient.ModelV1alpha1().ModelClaims(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	for i := range existing.Items {
		if modelClaimServedName(&existing.Items[i]) == modelClaimServedName(claim) {
			writeModelAdapterJSON(w, http.StatusConflict, modelAdapterErrorResponse{
				Error: fmt.Sprintf(
					"model name %q is already served by ModelClaim %q",
					modelClaimServedName(claim),
					existing.Items[i].Name,
				),
			})
			return
		}
	}

	pools, err := discoverModelClaimPools(r.Context(), kubeClient, namespace, existing.Items)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	pool := findModelClaimPool(pools, request.PoolName)
	if pool == nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{
			Error: fmt.Sprintf("warm runtime pool %q was not found in namespace %q", request.PoolName, namespace),
		})
		return
	}
	// A pool is held to one GPU count per Pod. The controller does not place
	// a vLLM claim whose TP * PP differs from it, so refuse it here instead of
	// leaving it Pending.
	if parallelism > 0 && len(pool.GPUCounts) == 1 && pool.GPUCounts[0] > 0 && pool.GPUCounts[0] != parallelism {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{
			Error: fmt.Sprintf(
				"tensor-parallel-size * pipeline-parallel-size is %d, but Pods in pool %q have %s",
				parallelism,
				pool.Name,
				formatGPUCount(pool.GPUCounts[0]),
			),
		})
		return
	}

	created, err := modelClient.ModelV1alpha1().ModelClaims(namespace).Create(
		r.Context(),
		claim,
		metav1.CreateOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	writeModelAdapterJSON(w, http.StatusCreated, buildModelClaimResponse(created, nil))
}

// handleUpdate changes spec.perGPU, the only field the API server lets a
// claim change after it is created.
func (h *ModelClaimHandler) handleUpdate(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	if err := requireAdminRole(r.Context(), "ModelClaims"); err != nil {
		writeModelAdapterError(w, err)
		return
	}

	var request updateModelClaimRequest
	if err := decodeModelClaimRequest(r, &request); err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}
	if request.PerGPU == nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: "per_gpu is required"})
		return
	}
	normalizeModelClaimPerGPURequest(request.PerGPU)
	perGPU, err := parseModelClaimPerGPU(request.PerGPU)
	if err != nil {
		writeModelAdapterJSON(w, http.StatusBadRequest, modelAdapterErrorResponse{Error: err.Error()})
		return
	}

	kubeClient, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{"perGPU": perGPU},
	})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	updated, err := modelClient.ModelV1alpha1().ModelClaims(namespace).Patch(
		r.Context(),
		pathParams["name"],
		types.MergePatchType,
		patch,
		metav1.PatchOptions{},
	)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	podsByName, err := getModelClaimInstancePods(r.Context(), kubeClient, namespace, updated)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	writeModelAdapterJSON(w, http.StatusOK, buildModelClaimResponse(updated, podsByName))
}

func (h *ModelClaimHandler) handleDelete(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	if err := requireAdminRole(r.Context(), "ModelClaims"); err != nil {
		writeModelAdapterError(w, err)
		return
	}

	_, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	if err := modelClient.ModelV1alpha1().ModelClaims(namespace).Delete(
		r.Context(),
		pathParams["name"],
		metav1.DeleteOptions{},
	); err != nil {
		writeModelAdapterError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ModelClaimHandler) handleListPools(w http.ResponseWriter, r *http.Request, _ map[string]string) {
	kubeClient, modelClient, namespace, err := resolveClusterClients(h.clients)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	claims, err := modelClient.ModelV1alpha1().ModelClaims(namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	pools, err := discoverModelClaimPools(r.Context(), kubeClient, namespace, claims.Items)
	if err != nil {
		writeModelAdapterError(w, err)
		return
	}
	writeModelAdapterJSON(w, http.StatusOK, listModelClaimPoolsResponse{Pools: pools})
}

func decodeModelClaimRequest(r *http.Request, request interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func normalizeCreateModelClaimRequest(request *createModelClaimRequest) {
	request.Name = strings.TrimSpace(request.Name)
	request.ModelName = strings.TrimSpace(request.ModelName)
	request.ArtifactURL = strings.TrimSpace(request.ArtifactURL)
	request.Engine = strings.ToLower(strings.TrimSpace(request.Engine))
	request.PoolName = strings.TrimSpace(request.PoolName)
	if request.PerGPU != nil {
		normalizeModelClaimPerGPURequest(request.PerGPU)
	}
}

func normalizeModelClaimPerGPURequest(request *modelClaimPerGPURequest) {
	request.MaximumFootprint = strings.TrimSpace(request.MaximumFootprint)
	request.KVFloor = strings.TrimSpace(request.KVFloor)
}

// buildModelClaim validates a create request and renders the claim it asks
// for. It also returns the GPU group size a vLLM engine needs, or zero for an
// engine the pool topology does not constrain.
func buildModelClaim(request *createModelClaimRequest) (*modelv1alpha1.ModelClaim, int64, error) {
	if errs := validation.IsDNS1123Subdomain(request.Name); len(errs) > 0 {
		return nil, 0, fmt.Errorf("name must be a valid Kubernetes DNS subdomain: %s", strings.Join(errs, ", "))
	}
	// The controller writes one routing annotation per claim on its Pod, keyed
	// by the claim name, so the name has to fit an annotation key.
	if errs := validation.IsQualifiedName(constants.ModelClaimPodAnnotationPrefix + request.Name); len(errs) > 0 {
		return nil, 0, fmt.Errorf("name must fit a Pod annotation key: %s", strings.Join(errs, ", "))
	}
	if strings.IndexFunc(request.ModelName, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return nil, 0, fmt.Errorf("model_name must not contain whitespace")
	}
	if request.ArtifactURL == "" {
		return nil, 0, fmt.Errorf("artifact_url is required")
	}
	if _, err := url.ParseRequestURI(request.ArtifactURL); err != nil {
		return nil, 0, fmt.Errorf("artifact_url is invalid: %w", err)
	}
	if err := utils.ValidateArtifactURL(request.ArtifactURL); err != nil {
		return nil, 0, fmt.Errorf("artifact_url uses an unsupported scheme: %w", err)
	}
	engine := request.Engine
	if engine == "" {
		engine = modelClaimVLLMEngine
	}
	if engine != modelClaimVLLMEngine && engine != modelClaimSGLangEngine {
		return nil, 0, fmt.Errorf("engine must be %q or %q", modelClaimVLLMEngine, modelClaimSGLangEngine)
	}
	if request.PoolName == "" {
		return nil, 0, fmt.Errorf("pool_name is required")
	}
	if errs := validation.IsValidLabelValue(request.PoolName); len(errs) > 0 {
		return nil, 0, fmt.Errorf("pool_name must be a valid Kubernetes label value: %s", strings.Join(errs, ", "))
	}
	// The schema leaves perGPU optional for claims stored before it existed,
	// but the controller never places a claim without it.
	if request.PerGPU == nil {
		return nil, 0, fmt.Errorf("per_gpu is required: a claim that does not declare its GPU cost is never placed")
	}
	perGPU, err := parseModelClaimPerGPU(request.PerGPU)
	if err != nil {
		return nil, 0, err
	}
	// Priority is immutable once the claim exists, so it is only set here.
	if request.Priority < 0 {
		return nil, 0, fmt.Errorf("priority must not be negative")
	}
	args, err := normalizeModelClaimEngineArgs(request.EngineArgs)
	if err != nil {
		return nil, 0, err
	}

	claim := &modelv1alpha1.ModelClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: modelClaimAPIVersion,
			Kind:       "ModelClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: request.Name,
			Labels: map[string]string{
				modelAdapterManagedByLabel: modelAdapterManagedByLabelValue,
			},
		},
		Spec: modelv1alpha1.ModelClaimSpec{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{constants.ModelPoolLabelName: request.PoolName},
			},
			ArtifactURL: request.ArtifactURL,
			Engine:      engine,
			PerGPU:      perGPU,
			Priority:    request.Priority,
		},
	}
	if request.ModelName != "" {
		claim.Spec.ModelName = &request.ModelName
	}
	if len(args) > 0 {
		claim.Spec.EngineConfig = &modelv1alpha1.ModelClaimEngineConfig{Args: args}
	}

	parallelism := int64(0)
	if engine == modelClaimVLLMEngine {
		parallelism, err = utils.ModelClaimVLLMParallelism(claim.Spec.EngineConfig)
		if err != nil {
			return nil, 0, fmt.Errorf("engine_args: %w", err)
		}
	}
	return claim, parallelism, nil
}

func parseModelClaimPerGPU(request *modelClaimPerGPURequest) (*modelv1alpha1.ModelClaimPerGPU, error) {
	maximumFootprint, err := parseModelClaimQuantity("maximumFootprint", request.MaximumFootprint)
	if err != nil {
		return nil, err
	}
	kvFloor, err := parseModelClaimQuantity("kvFloor", request.KVFloor)
	if err != nil {
		return nil, err
	}
	return &modelv1alpha1.ModelClaimPerGPU{MaximumFootprint: maximumFootprint, KVFloor: kvFloor}, nil
}

func parseModelClaimQuantity(name, value string) (resource.Quantity, error) {
	if value == "" {
		return resource.Quantity{}, fmt.Errorf("spec.perGPU.%s is required", name)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("spec.perGPU.%s must be a quantity such as 30Gi: %w", name, err)
	}
	if _, err := utils.ModelClaimDeclaredBytes(name, quantity); err != nil {
		return resource.Quantity{}, err
	}
	return quantity, nil
}

// normalizeModelClaimEngineArgs checks each flag the way vLLM reads it, so a
// flag written as --flag=value or with "_" for "-" is still caught.
func normalizeModelClaimEngineArgs(args map[string]string) (map[string]string, error) {
	normalized := make(map[string]string, len(args))
	flags := make(map[string]bool, len(args))
	for rawKey, rawValue := range args {
		key := strings.TrimSpace(rawKey)
		value := strings.TrimSpace(rawValue)
		flag, _, hasValue := utils.ModelClaimEngineFlag(key)
		if !strings.HasPrefix(key, "--") || len(flag) <= len("--") {
			return nil, fmt.Errorf("engine_args key %q must be an engine flag such as --max-model-len", rawKey)
		}
		if strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			return nil, fmt.Errorf("engine_args key %q must not contain whitespace", rawKey)
		}
		if hasValue && value != "" {
			return nil, fmt.Errorf("engine_args key %q already gives a value after \"=\"; leave its value empty", rawKey)
		}
		if owned := modelClaimLauncherFlag(flag); owned != "" {
			return nil, fmt.Errorf("engine_args must not set %s; the runtime sets %s for every engine", key, owned)
		}
		if flags[flag] {
			return nil, fmt.Errorf("engine_args sets %s more than once", flag)
		}
		flags[flag] = true
		normalized[key] = value
	}
	return normalized, nil
}

// modelClaimLauncherFlag returns the launcher flag that flag sets, or turns off
// in vLLM's --no- form, or "" for any other flag.
func modelClaimLauncherFlag(flag string) string {
	if modelClaimLauncherFlags[flag] {
		return flag
	}
	if name, negated := strings.CutPrefix(flag, "--no-"); negated && modelClaimLauncherFlags["--"+name] {
		return "--" + name
	}
	return ""
}

// getModelClaimInstancePods reads the Pods that run one claim's instances.
func getModelClaimInstancePods(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	claim *modelv1alpha1.ModelClaim,
) (map[string]*corev1.Pod, error) {
	names := modelClaimInstancePodNames([]modelv1alpha1.ModelClaim{*claim})
	podsByName := make(map[string]*corev1.Pod, len(names))
	if err := readMissingPods(ctx, kubeClient, namespace, names, podsByName); err != nil {
		return nil, err
	}
	return podsByName, nil
}

// listModelClaimInstancePods reads the Pods that run the claims' instances.
// The controller places an instance only on a Pod with the enabled label, so
// one List by that label finds them; the pool name label is not enough, since
// a claim's selector need not use it. A Pod that has since lost the enabled
// label, such as one that left its pool, still runs its instances and is read
// on its own.
func listModelClaimInstancePods(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	claims []modelv1alpha1.ModelClaim,
) (map[string]*corev1.Pod, error) {
	names := modelClaimInstancePodNames(claims)
	podsByName := make(map[string]*corev1.Pod, len(names))
	if len(names) == 0 {
		return podsByName, nil
	}
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: constants.ModelPoolLabelEnabled,
	})
	if err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if names[pods.Items[i].Name] {
			podsByName[pods.Items[i].Name] = &pods.Items[i]
		}
	}
	if err := readMissingPods(ctx, kubeClient, namespace, names, podsByName); err != nil {
		return nil, err
	}
	return podsByName, nil
}

func modelClaimInstancePodNames(claims []modelv1alpha1.ModelClaim) map[string]bool {
	names := map[string]bool{}
	for i := range claims {
		for _, instance := range claims[i].Status.Instances {
			if instance.Pod != "" {
				names[instance.Pod] = true
			}
		}
	}
	return names
}

// readMissingPods gets each named Pod that podsByName does not hold yet. A Pod
// that no longer exists is left out.
func readMissingPods(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	names map[string]bool,
	podsByName map[string]*corev1.Pod,
) error {
	for name := range names {
		if _, found := podsByName[name]; found {
			continue
		}
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			podsByName[name] = pod
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func buildModelClaimResponse(
	claim *modelv1alpha1.ModelClaim,
	podsByName map[string]*corev1.Pod,
) modelClaimResponse {
	engine := claim.Spec.Engine
	if engine == "" {
		engine = modelClaimVLLMEngine
	}
	phase := string(claim.Status.Phase)
	if phase == "" {
		phase = string(modelv1alpha1.ModelClaimPending)
	}
	engineArgs := map[string]string{}
	if claim.Spec.EngineConfig != nil {
		for key, value := range claim.Spec.EngineConfig.Args {
			engineArgs[key] = value
		}
	}

	response := modelClaimResponse{
		Name:             claim.Name,
		Namespace:        claim.Namespace,
		APIVersion:       modelClaimAPIVersion,
		ModelName:        modelClaimServedName(claim),
		ArtifactURL:      claim.Spec.ArtifactURL,
		Engine:           engine,
		PoolName:         modelClaimPoolName(claim),
		PodSelector:      formatLabelSelector(claim.Spec.PodSelector),
		Priority:         claim.Spec.Priority,
		EngineArgs:       engineArgs,
		Phase:            phase,
		Candidates:       claim.Status.Candidates,
		ReadyReplicas:    claim.Status.ReadyReplicas,
		DesiredReplicas:  claim.Status.DesiredReplicas,
		CreatedAt:        formatKubernetesTime(claim.CreationTimestamp),
		Deleting:         claim.DeletionTimestamp != nil,
		ManagedByConsole: claim.Labels[modelAdapterManagedByLabel] == modelAdapterManagedByLabelValue,
		Conditions:       buildModelClaimConditions(claim.Status.Conditions),
		Instances:        make([]modelClaimInstanceResponse, 0, len(claim.Status.Instances)),
	}
	if claim.Spec.PerGPU != nil {
		response.PerGPU = &modelClaimPerGPUResponse{
			MaximumFootprint: claim.Spec.PerGPU.MaximumFootprint.String(),
			KVFloor:          claim.Spec.PerGPU.KVFloor.String(),
		}
	}
	for _, instance := range claim.Status.Instances {
		response.Instances = append(response.Instances, buildModelClaimInstance(claim.Name, instance, podsByName[instance.Pod]))
	}
	return response
}

func buildModelClaimConditions(conditions []metav1.Condition) []modelClaimConditionResponse {
	sorted := make([]metav1.Condition, len(conditions))
	copy(sorted, conditions)
	sort.SliceStable(sorted, func(i, j int) bool {
		rankI, knownI := modelClaimConditionOrder[sorted[i].Type]
		rankJ, knownJ := modelClaimConditionOrder[sorted[j].Type]
		if knownI != knownJ {
			return knownI
		}
		if knownI {
			return rankI < rankJ
		}
		return sorted[i].Type < sorted[j].Type
	})
	response := make([]modelClaimConditionResponse, 0, len(sorted))
	for _, condition := range sorted {
		response = append(response, modelClaimConditionResponse{
			Type:               condition.Type,
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: formatKubernetesTime(condition.LastTransitionTime),
		})
	}
	return response
}

func buildModelClaimInstance(
	claimName string,
	instance modelv1alpha1.ModelClaimInstance,
	pod *corev1.Pod,
) modelClaimInstanceResponse {
	response := modelClaimInstanceResponse{
		Pod:          instance.Pod,
		Port:         instance.Port,
		Phase:        string(instance.Phase),
		KVLimitBytes: instance.KVLimitBytes,
		Reason:       instance.Reason,
		PodStatus:    buildBoundPod(instance.Pod, pod),
	}
	if pod == nil {
		return response
	}
	for _, binding := range utils.ModelClaimBindingsFromPod(pod) {
		if binding.Claim == claimName {
			response.Route = &modelClaimRouteResponse{
				State:         binding.State,
				Port:          binding.Port,
				Reason:        binding.Reason,
				WakeByRequest: binding.WakeByRequest,
			}
			break
		}
	}
	response.WakeRequestedAt = pod.Annotations[constants.ModelClaimWakeAnnotationPrefix+claimName]
	return response
}

func modelClaimServedName(claim *modelv1alpha1.ModelClaim) string {
	if claim.Spec.ModelName != nil && *claim.Spec.ModelName != "" {
		return *claim.Spec.ModelName
	}
	return claim.Name
}

// modelClaimPoolName is the pool a claim selects, when its selector is the
// plain pool-name match the console creates. A claim applied with another
// selector has no single pool, and its selector is shown instead.
func modelClaimPoolName(claim *modelv1alpha1.ModelClaim) string {
	if claim.Spec.PodSelector == nil {
		return ""
	}
	return claim.Spec.PodSelector.MatchLabels[constants.ModelPoolLabelName]
}

type modelClaimPoolAccumulator struct {
	response     modelClaimPoolResponse
	podGPUs      map[int64]bool
	templateGPUs map[int64]bool
	images       map[string]bool
}

// discoverModelClaimPools groups the namespace's warm runtime Pods and the
// Deployments that create them by pool name.
func discoverModelClaimPools(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	claims []modelv1alpha1.ModelClaim,
) ([]modelClaimPoolResponse, error) {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: constants.ModelPoolLabelName,
	})
	if err != nil {
		return nil, err
	}
	deployments, err := kubeClient.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	pools := map[string]*modelClaimPoolAccumulator{}
	poolFor := func(name string) *modelClaimPoolAccumulator {
		pool, found := pools[name]
		if !found {
			pool = &modelClaimPoolAccumulator{
				response: modelClaimPoolResponse{
					Name:      name,
					Namespace: namespace,
					Workloads: []string{},
				},
				podGPUs:      map[int64]bool{},
				templateGPUs: map[int64]bool{},
				images:       map[string]bool{},
			}
			pools[name] = pool
		}
		return pool
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		name := pod.Labels[constants.ModelPoolLabelName]
		if name == "" {
			continue
		}
		pool := poolFor(name)
		pool.response.Pods++
		if pod.Labels[constants.ModelPoolLabelEnabled] != constants.ModelPoolLabelEnabledValue {
			continue
		}
		pool.response.EnabledPods++
		if pod.DeletionTimestamp == nil && utils.IsPodReady(pod) {
			pool.response.ReadyPods++
		}
		pool.podGPUs[utils.ModelClaimPodGPUCount(&pod.Spec)] = true
		for _, container := range pod.Spec.Containers {
			pool.images[container.Image] = true
		}
	}

	sort.Slice(deployments.Items, func(i, j int) bool {
		return deployments.Items[i].Name < deployments.Items[j].Name
	})
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		name := deployment.Spec.Template.Labels[constants.ModelPoolLabelName]
		if name == "" || deployment.DeletionTimestamp != nil {
			continue
		}
		pool := poolFor(name)
		pool.response.Workloads = append(pool.response.Workloads, deployment.Name)
		if pool.response.Policy == "" {
			pool.response.Policy = deployment.Annotations[constants.ModelPoolPolicyAnnotationKey]
		}
		addModelClaimPoolTemplate(pool, deployment)
	}

	for i := range claims {
		if pool, found := pools[modelClaimPoolName(&claims[i])]; found {
			pool.response.Claims++
		}
	}

	response := make([]modelClaimPoolResponse, 0, len(pools))
	for _, pool := range pools {
		gpuCounts := pool.podGPUs
		if pool.response.EnabledPods == 0 {
			gpuCounts = pool.templateGPUs
		}
		pool.response.GPUCounts = sortedInt64Keys(gpuCounts)
		pool.response.Images = sortedStringKeys(pool.images)
		response = append(response, pool.response)
	}
	sort.Slice(response, func(i, j int) bool {
		return response[i].Name < response[j].Name
	})
	return response, nil
}

func addModelClaimPoolTemplate(pool *modelClaimPoolAccumulator, deployment *appsv1.Deployment) {
	pool.templateGPUs[utils.ModelClaimPodGPUCount(&deployment.Spec.Template.Spec)] = true
	if pool.response.EnabledPods > 0 {
		return
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		pool.images[container.Image] = true
	}
}

func findModelClaimPool(pools []modelClaimPoolResponse, name string) *modelClaimPoolResponse {
	for i := range pools {
		if pools[i].Name == name {
			return &pools[i]
		}
	}
	return nil
}

func formatGPUCount(count int64) string {
	if count == 1 {
		return "1 GPU"
	}
	return fmt.Sprintf("%d GPUs", count)
}

func sortedInt64Keys(values map[int64]bool) []int64 {
	keys := make([]int64, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func sortedStringKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
