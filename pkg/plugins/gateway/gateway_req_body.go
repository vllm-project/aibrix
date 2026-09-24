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

package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"go.opentelemetry.io/otel/attribute"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func (s *Server) HandleRequestBody(ctx context.Context, routingCtx *types.RoutingContext, requestID string, req *extProcPb.ProcessingRequest, user utils.User) (*extProcPb.ProcessingResponse, string, bool, int64) {
	var term int64 // Identify the trace window

	requestPath := routingCtx.ReqPath

	body := req.Request.(*extProcPb.ProcessingRequest_RequestBody)

	ctx, span := tracer.Start(ctx, "process.handle_request_body")
	defer span.End()

	// Async video job follow-ups (GET status/content, DELETE) carry their routing
	// key -- the public job id -- in the path, not the (often empty) body. The
	// generated video only exists on the pod that created it, so this bypasses the
	// normal model-based routing below and pins directly back to that pod.
	if publicJobID, isSubResource := extractVideoIDFromPath(requestPath); isSubResource {
		return s.handleVideoJobSubResource(ctx, routingCtx, requestID, requestPath, publicJobID, body.RequestBody.GetBody())
	}

	var model, message string
	var stream bool
	var routingAlgorithm types.RoutingAlgorithm
	var errRes *extProcPb.ProcessingResponse

	// Check if this is a multipart request (audio endpoints, vLLM-Omni video generation)
	contentType := routingCtx.ReqHeaders[contentTypeKey]
	if isMultipartFormPath(requestPath) && isMultipartRequest(contentType) {
		// Parse multipart form data for audio/video endpoints
		model, stream, errRes = parseMultipartFormData(requestID, requestPath, contentType, body.RequestBody.GetBody())
		if errRes != nil {
			return errRes, model, stream, term
		}
		message = "" // Audio/video requests don't have a text message for token counting
	} else {
		// Use existing JSON validation for other endpoints
		model, message, stream, errRes = validateRequestBody(requestID, requestPath, body.RequestBody.GetBody(), user)
		if errRes != nil {
			return errRes, model, stream, term
		}
	}

	routingCtx.Model = model
	routingCtx.Message = message
	routingCtx.Stream = stream
	routingCtx.ReqBody = body.RequestBody.GetBody()
	if base, ok := s.cache.ModelBaseModel(model); ok {
		routingCtx.BaseModel = base
	}

	// early reject if model doesn't exist or no pods are ready
	var podsArr types.PodList
	podsArr, errRes = s.validateModelAvailability(requestID, model)
	if errRes != nil {
		return errRes, model, stream, term
	}

	// Read engine label from pods and assign to routing context
	if pods := podsArr.All(); len(pods) > 0 {
		routingCtx.Engine = pods[0].Labels[constants.ModelLabelEngine]
		if routingCtx.Engine == "" {
			routingCtx.Engine = pods[0].Annotations[constants.ModelLabelEngine]
		}
	}

	// Resolve model config profile from annotation and apply overrides
	applyConfigProfile(routingCtx, podsArr.All())

	// Derive and validate routing strategy (headers -> profile -> env); return 400 on invalid
	if strategy, enabled := deriveRoutingStrategyFromContext(routingCtx); enabled {
		var ok bool
		// Some legacy unit tests construct Server literals directly instead of
		// using NewServerWithOptions. Keep those callers compatible while all
		// production-constructed servers still receive a manager at construction.
		if s.routerManager == nil {
			s.routerManager = routing.DefaultRouterManager()
		}
		if routingAlgorithm, ok = s.routerManager.Validate(strategy); !ok {
			klog.ErrorS(nil, "incorrect routing strategy", "requestID", requestID, "routing-strategy", strategy)
			return buildErrorResponse(envoyTypePb.StatusCode_BadRequest, fmt.Sprintf("incorrect routing strategy %s", strategy), "", "", HeaderErrorRouting, "true"), model, stream, term
		}
		routingCtx.Algorithm = routingAlgorithm
	}

	// The async video job (POST /v1/videos) must always be pinned to the pod that
	// creates it: the generated video lives on that pod's local disk, and
	// registerVideoJobFromCreateResponse (called from HandleResponseBody) records
	// routingCtx's target pod identity so follow-up GET/DELETE calls can be routed
	// back to it. RouterNotSet delegates routing to the HTTPRoute/k8s Service
	// below and never calls SetTargetPod, which would leave the job unregisterable
	// and block that TargetPod() call until the request's context is done. Force a
	// real algorithm here regardless of the client's routing-strategy header (or
	// lack of one).
	if routingAlgorithm == routing.RouterNotSet && pathWithoutQuery(requestPath) == PathVideos {
		routingAlgorithm = routing.RouterLeastRequest
		routingCtx.Algorithm = routingAlgorithm
	}

	// The tier the caller declared is mapped onto the body that is forwarded
	// upstream. This runs before a pod is chosen because the PD router builds the
	// prefill leg out of the routing context while it routes (see
	// pd/prefill.PreparePayload): both legs of a PD request have to carry the
	// same priority, so the rewrite cannot wait for the routing decision.
	s.applyPriorityTier(routingCtx)

	// Pre-allocate for the routing path (4 headers: strategy, target-pod, content-length, X-Request-Id).
	headers := make([]*configPb.HeaderValueOption, 0, 4)

	// Path rewriting for image/video generation based on engine type
	// xdit engine uses /generate and /generatevideo endpoints
	// vllm/vllm-omni uses OpenAI-compatible /v1/images/generations
	if rewritePath := getEngineBasedPathRewrite(requestPath, podsArr.All()); rewritePath != "" {
		headers = buildEnvoyProxyHeaders(headers, ":path", rewritePath)
	}

	if errRes = s.enforceModelRPS(ctx, model, routingCtx); errRes != nil {
		return errRes, model, stream, term
	}
	needsRollback := true
	defer func() {
		if needsRollback {
			s.decrModelRPS(ctx, model, routingCtx)
		}
	}()

	if routingAlgorithm == routing.RouterNotSet {
		if err := s.validateHTTPRouteStatus(ctx, model); err != nil {
			return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable, err.Error(), ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true"), model, stream, term
		}
		// The response replaces the upstream body with routingCtx.ReqBody, and
		// routing as well as the priority mapping above may have changed its
		// size. Envoy validates the upstream request against this header, so it
		// always describes the body that is actually forwarded rather than the
		// one that arrived.
		headers = buildEnvoyProxyHeaders(headers,
			HeaderModel, model,
			"content-length", strconv.Itoa(len(routingCtx.ReqBody)))
		klog.InfoS("request_start", "request_id", requestID, "request_path", requestPath, "model", model, "stream", stream)
	} else {
		externalFilter := routingCtx.ReqHeaders[HeaderExternalFilter]
		targetPodIP, err := s.selectTargetPod(ctx, routingCtx, podsArr, externalFilter)
		if targetPodIP == "" || err != nil {
			var invalidReqErr *engine.InvalidRequestError
			if errors.As(err, &invalidReqErr) {
				return buildRoutingErrorResponse(routingCtx, requestID, envoyTypePb.StatusCode_BadRequest,
					invalidReqErr.Error(), "", "", HeaderErrorRouting, "true"), model, stream, term
			}
			if errors.Is(err, errReplicaInflightExceeded) {
				limit := replicaInflightLimit(routingCtx)
				klog.InfoS("replica_inflight_exceeded", "requestID", requestID, "model", model, "limit", limit, "reason", "all_replicas_saturated")
				return replicaInflightExceededResponse(model, limit), model, stream, term
			}
			klog.ErrorS(err, "failed to select target pod", "requestID", requestID, "routingStrategy", routingAlgorithm, "model", model, "routingDuration", routingCtx.GetRoutingDelay())
			return buildRoutingErrorResponse(routingCtx, requestID, envoyTypePb.StatusCode_ServiceUnavailable,
				"error on selecting target pod", ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true"), model, stream, term
		}
		if errRes = s.enforceReplicaInflight(ctx, model, routingCtx); errRes != nil {
			return errRes, model, stream, term
		}
		headers = buildEnvoyProxyHeaders(headers,
			HeaderRoutingStrategy, string(routingAlgorithm),
			HeaderTargetPod, targetPodIP,
			"content-length", strconv.Itoa(len(routingCtx.ReqBody)),
			"X-Request-Id", routingCtx.RequestID)

		var targetPodName, targetNamespace string
		var request_count float64
		if routingCtx.HasRouted() && routingCtx.TargetPod() != nil {
			targetPodName = routingCtx.TargetPod().Name
			targetNamespace = routingCtx.TargetPod().Namespace
			request_count = getRunningRequestsByPod(s, targetPodName, targetNamespace)
		}

		routingDelay := routingCtx.GetRoutingDelay()
		if routingAlgorithm == routing.RouterPD && !routingCtx.PrefillStartTime.IsZero() {
			routingDelay = routingCtx.PrefillStartTime.Sub(routingCtx.RequestTime)
		}
		if routingCtx.Span != nil {
			routingCtx.Span.SetAttributes(
				attribute.String("request_id", requestID),
				attribute.String("request_path", requestPath),
				attribute.String("model", model),
				attribute.Bool("stream", stream),
				attribute.String("target_pod", targetPodName),
				attribute.String("target_pod_ip", targetPodIP),
				attribute.Float64("outstanding_requests_at_start", request_count),
				attribute.Int64("routing_time_taken_ms", routingDelay.Milliseconds()),
			)
		}
		klog.InfoS("request_start", "request_id", requestID, "request_path", requestPath, "model", model, "stream", stream, "routing_strategy", routingAlgorithm,
			"target_pod", targetPodName, "target_pod_ip", targetPodIP, "outstanding_requests", request_count, "routing_time_taken", routingDelay)
	}

	needsRollback = false
	routingCtx.RequestEndTime = time.Now()
	term = s.cache.AddRequestCount(routingCtx, requestID, model)

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestBody{
			RequestBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: routingCtx.ReqBody,
						},
					},
				},
			},
		},
	}

	// No ModeOverride is sent: the response body mode this create needs (Buffered,
	// so the backend job id can be replaced before anything reaches the client)
	// comes from the Videos route's EnvoyExtensionPolicy. Envoy Gateway v1.2.8
	// never sets ext_proc's allow_mode_override, so a per-request override here
	// would be silently ignored and would only read as if it did something.
	return resp, model, stream, term
}

func buildRoutingErrorResponse(
	routingCtx *types.RoutingContext,
	requestID string,
	statusCode envoyTypePb.StatusCode,
	errBody, errorCode, param string,
	headers ...string,
) *extProcPb.ProcessingResponse {
	response := buildErrorResponse(statusCode, errBody, errorCode, param, headers...)
	setHeaders := response.GetImmediateResponse().GetHeaders().GetSetHeaders()
	setHeaders = buildEnvoyProxyHeaders(setHeaders, HeaderRequestID, requestID)
	if routingCtx != nil {
		for key, value := range routingCtx.RespHeaders {
			setHeaders = buildEnvoyProxyHeaders(setHeaders, key, value)
		}
	}
	response.GetImmediateResponse().GetHeaders().SetHeaders = setHeaders
	return response
}

// getEngineBasedPathRewrite returns the rewritten path for image/video generation endpoints
// based on the engine type specified in the pod labels/annotations.
// Returns empty string if no rewrite is needed (e.g., for vllm/vllm-omni which uses OpenAI-compatible paths).
func getEngineBasedPathRewrite(requestPath string, pods []*v1.Pod) string {
	if len(pods) == 0 {
		return ""
	}

	// Get engine type from the first pod (all pods for a model should have the same engine)
	pod := pods[0]
	engine := pod.Labels[constants.ModelLabelEngine]
	if engine == "" {
		engine = pod.Annotations[constants.ModelLabelEngine]
	}

	// Only xdit engine needs path rewriting to its native endpoints
	if engine == EngineXdit {
		switch pathWithoutQuery(requestPath) {
		case PathImagesGenerations:
			return PathXditGenerate
		case PathVideoGenerations:
			return PathXditGenerateVideo
		}
	}

	// vllm, vllm-omni, sglang, and other engines use OpenAI-compatible paths
	return ""
}

// validateModelAvailability checks that the model exists in cache and has routable pods.
// Returns the pod list and nil on success, or nil and an error response on failure.
func (s *Server) validateModelAvailability(requestID, model string) (types.PodList, *extProcPb.ProcessingResponse) {
	if !s.cache.HasModel(model) {
		if provider, ok := s.cache.(cache.ModelClaimBindingProvider); ok {
			if pod, _, state, found := provider.ModelClaimBinding(model); found {
				klog.InfoS("ModelClaim is known but not routable", "requestID", requestID, "model", model, "state", state)
				if state == constants.ModelClaimRoutingStateSleeping && s.wakeRequester != nil {
					s.wakeRequester.RequestWake(pod, model)
				}
				return nil, modelClaimRetryResponse(model, state, "", state != constants.ModelClaimRoutingStateFailed)
			}
		}
		// A claim that no pod advertises yet has not been placed. Its model is
		// on the way, so it is not a model that does not exist.
		if provider, ok := s.cache.(cache.ModelClaimStatusProvider); ok {
			if phase, reason, found := provider.ModelClaimStatus(model); found {
				klog.InfoS("ModelClaim is known but not placed", "requestID", requestID, "model", model,
					"phase", phase, "reason", reason)
				return nil, modelClaimRetryResponse(model, unplacedModelClaimState(phase), reason,
					!modelClaimMustChange(reason))
			}
		}
		klog.ErrorS(nil, "model doesn't exist in cache, probably wrong model name", "requestID", requestID, "model", model)
		return nil, generateErrorResponse(envoyTypePb.StatusCode_BadRequest,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte(model)}}},
			fmt.Sprintf("model %s does not exist", model), ErrorCodeModelNotFound, "model")
	}

	podsArr, err := s.cache.ListPodsByModel(model)
	if err != nil || podsArr == nil || utils.CountRoutablePods(podsArr.All()) == 0 {
		klog.ErrorS(err, "no ready pod available", "requestID", requestID, "model", model)
		return nil, generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
				Key: HeaderErrorNoModelBackends, RawValue: []byte("true")}}},
			fmt.Sprintf("error on getting pods for model %s", model), ErrorCodeServiceUnavailable, "")
	}

	return podsArr, nil
}

// modelClaimRetryResponse answers for a model whose ModelClaim cannot serve
// it now. The reason, when there is one, is the controller's own word for why,
// such as NoMatchingPods. A client is asked to retry only when retry is set.
func modelClaimRetryResponse(model, state, reason string, retry bool) *extProcPb.ProcessingResponse {
	headers := []*configPb.HeaderValueOption{
		{Header: &configPb.HeaderValue{Key: HeaderErrorNoModelBackends, RawValue: []byte(model)}},
	}
	message := fmt.Sprintf("model %s is %s", model, state)
	if reason != "" {
		message += fmt.Sprintf(" (%s)", reason)
	}
	if retry {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key: "Retry-After", RawValue: []byte(strconv.Itoa(modelClaimRetryAfterSeconds)),
			},
		})
		message += "; retry shortly"
	}
	return generateErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable, headers,
		message,
		ErrorCodeServiceUnavailable, "model")
}

// modelClaimReasonsThatMustChange are the reasons for which a claim is not
// placed until the claim itself is changed. Waiting does not help, so a client
// is not asked to retry. Any other refusal, including a failed activation, the
// controller tries again by itself.
var modelClaimReasonsThatMustChange = map[string]struct{}{
	"InvalidEngineConfig": {},
	"InvalidPerGPU":       {},
}

func modelClaimMustChange(reason string) bool {
	_, found := modelClaimReasonsThatMustChange[reason]
	return found
}

// unplacedModelClaimState words the phase of a claim that is not placed the
// way routing states are worded. A claim the controller has not looked at yet
// is pending.
func unplacedModelClaimState(phase string) string {
	if phase == "" {
		return "pending"
	}
	return strings.ToLower(phase)
}

// getRunningRequestsByPod fetches the local metric slot for a pod's running-request
// count, with a safe zero fallback. This is the periodically synced cache
// (RealtimeNumRequestsRunning), not a live cross-gateway read -- fine for per-request
// logging (request_start/request_end sit on the ext_proc Send path, where an extra
// Redis round trip is not worth paying), but not for a routing or admission decision.
// Use cache.GetPodRunningRequests (or, for admission, AdmitPodRunningRequest -- see
// gateway_inflight.go's enforceReplicaInflight) for those.
func getRunningRequestsByPod(s *Server, podName, namespace string) float64 {
	mv, err := s.cache.GetMetricValueByPod(podName, namespace, metrics.RealtimeNumRequestsRunning)
	if err != nil || mv == nil {
		return 0
	}
	return mv.GetSimpleValue()
}
