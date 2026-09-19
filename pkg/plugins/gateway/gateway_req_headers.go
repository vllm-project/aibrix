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
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/klog/v2"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// the key in request headers
const (
	userKey          = "user"
	pathKey          = ":path"
	methodKey        = ":method"
	authorizationKey = "authorization"
	contentTypeKey   = "content-type"
)

// videoCreateRematchHeaders prepares the headers-phase route rematch for an
// asynchronous create whose real routing strategy and target pod can only be
// selected after its multipart body has been decoded.
func videoCreateRematchHeaders(reqHeaders map[string]string, requestPath string, endOfStream bool) []*configPb.HeaderValueOption {
	if endOfStream ||
		!strings.EqualFold(reqHeaders[methodKey], http.MethodPost) ||
		pathWithoutQuery(requestPath) != PathVideos ||
		strings.TrimSpace(reqHeaders[HeaderRoutingStrategy]) != "" {
		return nil
	}
	return buildEnvoyProxyHeaders(nil, HeaderRoutingStrategy, string(routing.RouterLeastRequest))
}

func (s *Server) HandleRequestHeaders(ctx context.Context, requestID string, rootSpan trace.Span, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, utils.User, int64, *types.RoutingContext, int64) {
	var username, requestPath string
	var user utils.User
	var rpm, term int64
	var err error
	var errRes *extProcPb.ProcessingResponse
	var routingCtx *types.RoutingContext
	var reqConfigProfile string

	_, span := tracer.Start(ctx, "process.handle_request_headers")
	defer span.End()

	h := req.Request.(*extProcPb.ProcessingRequest_RequestHeaders)
	reqHeaders := map[string]string{}
	for _, n := range h.RequestHeaders.Headers.Headers {
		switch strings.ToLower(n.Key) {
		case userKey:
			username = string(n.RawValue)
		case pathKey:
			requestPath = string(n.RawValue)
		case methodKey:
			reqHeaders[n.Key] = string(n.RawValue)
		case authorizationKey:
			reqHeaders[n.Key] = string(n.RawValue)
		case HeaderExternalFilter:
			reqHeaders[HeaderExternalFilter] = string(n.RawValue)
		case contentTypeKey:
			reqHeaders[contentTypeKey] = string(n.RawValue)
		case HeaderRoutingStrategy:
			reqHeaders[HeaderRoutingStrategy] = string(n.RawValue)
		case HeaderConfigProfile:
			reqConfigProfile = strings.TrimSpace(string(n.RawValue))
		case constants.HeaderSessionID:
			reqHeaders[constants.HeaderSessionID] = string(n.RawValue)
		case constants.HeaderSessionKey, HeaderMockPDFailure:
			reqHeaders[strings.ToLower(n.Key)] = string(n.RawValue)
		case HeaderTraceParent: // Preserve the trace context for requests initiated by the gateway plugin. like PD
			reqHeaders[HeaderTraceParent] = string(n.RawValue)
			if !rootSpan.SpanContext().HasTraceID() { // prefers rootSpan traceID over traceparent
				requestID = GetTraceID(string(n.RawValue), requestID)
			}
		}
	}

	// check API key
	if s.apiKeyAuth != nil && s.apiKeyAuth.token != "" {
		authHeaderValue := ""
		for key, value := range reqHeaders {
			if strings.EqualFold(key, authorizationKey) {
				authHeaderValue = value
				break
			}
		}
		if !s.apiKeyAuth.isAuthorized(authHeaderValue) {
			klog.V(2).InfoS("rejecting request with invalid gateway API key", "requestID", requestID, "header", "Authorization")
			return generateErrorResponse(
				envoyTypePb.StatusCode_Unauthorized,
				nil,
				"Incorrect API key provided",
				ErrorCodeInvalidAPIKey,
				"api_key",
			), utils.User{}, rpm, nil, term
		}
	}

	if s.rateLimitingEnabled {
		user.Name = username
	}
	if user.Name != "" && s.redisClient != nil {
		user, err = utils.GetUser(ctx, utils.User{Name: username}, s.redisClient)
		if err != nil {
			klog.ErrorS(err, "unable to process user info", "requestID", requestID, "username", username)
			return generateErrorResponse(
				envoyTypePb.StatusCode_InternalServerError,
				[]*configPb.HeaderValueOption{{Header: &configPb.HeaderValue{
					Key: HeaderErrorUser, RawValue: []byte("true"),
				}}},
				err.Error(), "", ""), utils.User{}, rpm, routingCtx, term
		}

		rpm, errRes, err = s.checkLimits(ctx, user)
		if errRes != nil {
			klog.ErrorS(err, "error on checking limits", "requestID", requestID, "username", username)
			return errRes, utils.User{}, rpm, routingCtx, term
		}
	}

	routingCtx = types.NewRoutingContext(ctx, "", "", "", requestID, user.Name)
	// Async-job ownership is an access boundary, not a rate-limit or routing
	// identity. Preserve it independently so disabled mode cannot merge named
	// users into the shared job scope or feed user-aware routers.
	routingCtx.AsyncJobOwner = asyncJobOwnerFromUserName(username)
	routingCtx.ReqPath = requestPath
	routingCtx.ReqHeaders = reqHeaders
	routingCtx.ReqConfigProfile = reqConfigProfile

	// Do not create a second, subtly different Videos API under a trailing-slash
	// alias. In particular POST /v1/videos/ must not reach response rewriting
	// after having missed multipart parsing and create pinning.
	if isUnsupportedVideoTrailingSlash(requestPath) {
		return buildErrorResponse(
			envoyTypePb.StatusCode_NotFound,
			"video paths do not support a trailing slash",
			"", "", HeaderErrorRequestBodyProcessing, "true"), user, rpm, routingCtx, term
	}

	// Async video job follow-ups (GET status/content, DELETE) carry their routing
	// key -- the public job id -- in the path, not the (often empty/absent) body.
	// Envoy's ext_proc filter only invokes RequestBody processing when the request
	// actually has a body, so a bodyless request must be pinned here, at
	// RequestHeaders, or it never gets pinned at all (see
	// handleVideoJobSubResourceHeaders for the full explanation). When a body IS
	// coming (EndOfStream false), HandleRequestBody's existing handling covers it.
	if h.RequestHeaders.EndOfStream {
		if publicJobID, isSubResource := extractVideoIDFromPath(requestPath); isSubResource {
			resp, videoTerm := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, requestID, requestPath, publicJobID)
			return resp, user, rpm, routingCtx, videoTerm
		}
		// GET /v1/videos is the caller's own job catalog, which only the gateway's
		// registry knows -- there is no backend to route it to.
		if isVideoListRequest(requestPath, reqHeaders[methodKey]) {
			options, err := parseVideoListOptions(requestPath)
			if err != nil {
				return buildErrorResponse(envoyTypePb.StatusCode_BadRequest,
					err.Error(), "", "", HeaderErrorRequestBodyProcessing, "true"), user, rpm, routingCtx, term
			}
			return s.handleVideoListHeaders(ctx, requestID, asyncJobOwnerFromRoutingContext(routingCtx), options), user, rpm, routingCtx, term
		}
	}

	headers := videoCreateRematchHeaders(reqHeaders, requestPath, h.RequestHeaders.EndOfStream)
	// The initial /v1/videos route is selected before ext_proc sees the request.
	// When the client supplies no routing strategy, stamp a temporary valid value
	// while the headers-phase ClearRouteCache below can still rematch the request
	// onto the Videos ORIGINAL_DST route. Do not put this synthetic value in
	// routingCtx.ReqHeaders: HandleRequestBody must still resolve the real strategy
	// from the model profile or environment before it selects the concrete pod and
	// overwrites this header together with target-pod.
	headers = append(headers, &configPb.HeaderValueOption{
		Header: &configPb.HeaderValue{
			Key:      HeaderWentIntoReqHeaders,
			RawValue: []byte("true"),
		},
	})

	outCarrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, outCarrier)
	for k, val := range outCarrier {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:   k,
				Value: val, // OTel generates a string, which is assigned directly to Envoy's Value field
			},
			AppendAction: configPb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}

	// Note: Path rewriting for /v1/images/generations and /v1/video/generations
	// is handled in HandleRequestBody based on the engine type (model.aibrix.ai/engine label).
	// - xdit engine: rewrites to /generate or /generatevideo
	// - vllm/vllm-omni engine: keeps the original OpenAI-compatible path

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					ClearRouteCache: true,
				},
			},
		},
	}, user, rpm, routingCtx, term
}
