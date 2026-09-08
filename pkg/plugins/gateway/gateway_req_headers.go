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
		case constants.HeaderSessionKey:
			reqHeaders[constants.HeaderSessionKey] = string(n.RawValue)
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

	if username != "" {
		user.Name = username
	}
	if username != "" && s.redisClient != nil {
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
	routingCtx.ReqPath = requestPath
	routingCtx.ReqHeaders = reqHeaders
	routingCtx.ReqConfigProfile = reqConfigProfile

	// Async video job follow-ups (GET status/content, DELETE) carry their routing
	// key -- video_id -- in the path, not the (often empty/absent) body. Envoy's
	// ext_proc filter only invokes RequestBody processing when the request
	// actually has a body, so a bodyless request must be pinned here, at
	// RequestHeaders, or it never gets pinned at all (see
	// handleVideoJobSubResourceHeaders for the full explanation). When a body IS
	// coming (EndOfStream false), HandleRequestBody's existing handling covers it.
	if h.RequestHeaders.EndOfStream {
		if videoID, isSubResource := extractVideoIDFromPath(requestPath); isSubResource {
			resp, videoTerm := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, requestID, requestPath, videoID)
			return resp, user, rpm, routingCtx, videoTerm
		}
		// GET /v1/videos (list, no video_id) is fanned out across all of a
		// model's pods and answered directly here -- see handleVideoListHeaders
		// for why this can't be a normal single-pod routing decision.
		if model, isListPath := parseVideoListRequest(requestPath); isListPath && reqHeaders[methodKey] == http.MethodGet {
			if model == "" {
				return videoListModelRequiredResponse(), user, rpm, routingCtx, term
			}
			return s.handleVideoListHeaders(requestID, model), user, rpm, routingCtx, term
		}
	}

	headers := []*configPb.HeaderValueOption{}
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
