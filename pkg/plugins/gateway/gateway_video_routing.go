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

package gateway

// vLLM-Omni's Videos API (/v1/videos) is asynchronous: a create returns a job id
// and the generated file then lives on the local disk of the one pod that
// produced it, so every follow-up has to land on that same pod. The gateway
// therefore hands the client an opaque, owner-scoped public job id and keeps the
// (public id -> pod identity, backend job id) mapping in the async job registry
// (see async_job_registry.go). The backend's own id is never exposed: it is a
// bare, guessable handle with no ownership attached, and the gateway rewrites it
// into the request path -- and back out of the response body -- on every hop.
//
// The gateway deliberately stays out of the job's business: it does not poll the
// backend, does not track status transitions, and is not an HTTP client for the
// Videos API. It resolves, pins, rewrites ids, and nothing else.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
)

// videoJobAffinityLabel is the routing-strategy header value used to pin a
// resolved video job follow-up. It is not a real routing algorithm: the pod is
// already decided by the registry, and this only tells Envoy to honour the
// target-pod header instead of re-routing.
const videoJobAffinityLabel = "video-job-affinity"

// canonicalVideoPath removes only the query string. The Videos API deliberately
// does not accept trailing-slash aliases: accepting them in response rewriting
// but not multipart parsing made POST /v1/videos/ appear supported while it
// actually failed before routing.
func canonicalVideoPath(requestPath string) string {
	return pathWithoutQuery(requestPath)
}

// extractVideoIDFromPath returns the {id} of /v1/videos/{id} or
// /v1/videos/{id}/content. Bare /v1/videos and /v1/videos/sync have no id to pin.
// Post-migration this id is the public job id, minted by aibrix.
func extractVideoIDFromPath(requestPath string) (videoID string, ok bool) {
	const prefix = PathVideos + "/"
	requestPath = canonicalVideoPath(requestPath)
	if strings.HasSuffix(requestPath, "/") {
		return "", false
	}
	if !strings.HasPrefix(requestPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(requestPath, prefix)
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		rest = rest[:idx]
	}
	if rest == "" || rest == "sync" {
		return "", false
	}
	return rest, true
}

// rewriteVideoPathID swaps the public job id in requestPath for the backend's
// own id, which is the only identifier the engine recognizes. Everything else is
// preserved verbatim: the /content suffix decides whether the response is JSON
// or a video stream, and the query string carries backend options the gateway
// does not interpret.
func rewriteVideoPathID(requestPath, backendJobID string) (string, bool) {
	if backendJobID == "" {
		return "", false
	}
	if _, ok := extractVideoIDFromPath(requestPath); !ok {
		return "", false
	}

	const prefix = PathVideos + "/"
	path, query, hasQuery := strings.Cut(requestPath, "?")
	rest := strings.TrimPrefix(path, prefix)
	suffix := ""
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		suffix = rest[idx:]
	}

	rewritten := prefix + backendJobID + suffix
	if hasQuery {
		rewritten += "?" + query
	}
	return rewritten, true
}

// isVideoListRequest reports whether requestPath is the public catalog request,
// GET /v1/videos. The registry knows every job the caller owns, across models;
// its OpenAI-compatible cursor parameters are parsed separately.
func isVideoListRequest(requestPath, method string) bool {
	return method == http.MethodGet && canonicalVideoPath(requestPath) == PathVideos
}

func isUnsupportedVideoTrailingSlash(requestPath string) bool {
	path := pathWithoutQuery(requestPath)
	return path != "/" && strings.HasPrefix(path, PathVideos+"/") && strings.HasSuffix(path, "/")
}

func parseVideoListOptions(requestPath string) (AsyncJobListOptions, error) {
	_, rawQuery, hasQuery := strings.Cut(requestPath, "?")
	options := AsyncJobListOptions{Limit: defaultAsyncJobListLimit, Order: "desc"}
	if !hasQuery || rawQuery == "" {
		return options, nil
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return AsyncJobListOptions{}, fmt.Errorf("invalid video list query: %w", err)
	}
	for key := range values {
		if key != "after" && key != "limit" && key != "order" {
			return AsyncJobListOptions{}, fmt.Errorf("unsupported video list query parameter %q", key)
		}
		if len(values[key]) != 1 {
			return AsyncJobListOptions{}, fmt.Errorf("video list query parameter %q must be supplied once", key)
		}
	}
	options.After = values.Get("after")
	if rawLimit, ok := values["limit"]; ok {
		limit, err := strconv.Atoi(rawLimit[0])
		if err != nil {
			return AsyncJobListOptions{}, fmt.Errorf("video list limit must be an integer")
		}
		options.Limit = limit
	}
	if rawOrder, ok := values["order"]; ok {
		options.Order = rawOrder[0]
	}
	return normalizeAsyncJobListOptions(options)
}

// asyncJobOwnerFromRoutingContext derives the job scope from the only principal
// the gateway actually has: the user request header, carried on the routing
// context. Requests without one share a single scope rather than being granted a
// view over everyone else's jobs.
func asyncJobOwnerFromRoutingContext(routingCtx *types.RoutingContext) string {
	if routingCtx == nil || routingCtx.User == nil {
		return asyncJobOwnerShared
	}
	return asyncJobOwnerFromUserName(*routingCtx.User)
}

// videoJobResponseNeedsBuffering reports whether a response body must be held
// and mutated. The finite JSON create, status, and delete responses can carry a
// job id that must be translated before it reaches the client. A /content
// download is a video stream that must keep flowing through Envoy untouched.
//
// The deployed configuration puts these endpoints on a route whose ext_proc
// response body mode is Buffered (config/gateway/gateway-plugin), so the body
// normally arrives as one message with EndOfStream set. This predicate, and the
// accumulation in handleVideoJobResponseBody, also cover the streamed shape: the
// mode is a property of the route the request matched, and the rewriting must not
// depend on which one that was.
func videoJobResponseNeedsBuffering(method, requestPath string) bool {
	path := canonicalVideoPath(requestPath)
	switch method {
	case http.MethodPost:
		return path == PathVideos
	case http.MethodGet:
		videoID, ok := extractVideoIDFromPath(path)
		return ok && path == PathVideos+"/"+videoID
	case http.MethodDelete:
		videoID, ok := extractVideoIDFromPath(path)
		return ok && path == PathVideos+"/"+videoID
	default:
		return false
	}
}

// videoNotFoundResponse builds the 404 returned when a public job id is unknown,
// expired, owned by somebody else, or pinned to a pod that is confirmed gone.
// All of those are deliberately indistinguishable: a distinct "exists but not
// yours" answer would turn the 404 into an id oracle.
func videoNotFoundResponse(publicJobID string) *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_NotFound,
		fmt.Sprintf("video %s not found", publicJobID), ErrorCodeVideoNotFound, "",
		HeaderErrorVideoNotFound, "true")
}

// videoJobPodUnavailableResponse builds the 503 returned when the pinned pod is
// only transiently unavailable (NotReady, or no routable address yet) rather
// than confirmed gone. The record is left in place: the generated video is on
// that pod's local disk, so pinning elsewhere is not an option.
func videoJobPodUnavailableResponse(publicJobID string) *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
		fmt.Sprintf("video %s's pod is temporarily unavailable, please retry", publicJobID),
		ErrorCodeVideoJobPodUnavailable, "",
		HeaderErrorVideoJobPodUnavailable, "true")
}

// videoJobStoreUnavailableResponse builds the 503 for a job store that stayed
// unreachable across the registry's bounded retries. This must not collapse into
// a 404: the job most likely still exists, and telling the client otherwise
// would make it abandon a job it is still paying for.
func videoJobStoreUnavailableResponse(publicJobID string) *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
		fmt.Sprintf("video %s could not be resolved right now, please retry", publicJobID),
		ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true")
}

// videoJobErrorResponse maps a registry error onto the HTTP answer the client
// gets: absent or dead means terminal 404, transient means retryable 503.
func videoJobErrorResponse(publicJobID string, err error) *extProcPb.ProcessingResponse {
	switch {
	case errors.Is(err, errAsyncJobNotFound):
		return videoNotFoundResponse(publicJobID)
	case errors.Is(err, errAsyncJobTargetUnavailable):
		return videoJobPodUnavailableResponse(publicJobID)
	case errors.Is(err, errAsyncJobStoreUnavailable):
		return videoJobStoreUnavailableResponse(publicJobID)
	default:
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			fmt.Sprintf("video %s could not be resolved", publicJobID), "", "",
			HeaderErrorRouting, "true")
	}
}

// videoJobRegistrationFailedResponse is the 503 for a create whose record could
// not be persisted. It carries no id at all: the public id was never durable and
// the backend id must not become the client's handle on the job.
func videoJobRegistrationFailedResponse() *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
		"video job could not be registered, please retry",
		ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true")
}

// videoJobCleanupFailedResponse is the 503 returned when the backend deleted the
// job but its record could not be removed. Reporting success would leave a
// public id that still resolves to a job the client believes is gone.
func videoJobCleanupFailedResponse(publicJobID string) *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
		fmt.Sprintf("video %s was deleted but its record could not be cleaned up, please retry", publicJobID),
		ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true")
}

// videoListItem is the public shape of a catalog entry. It carries the opaque id
// and the model only -- never the backend id, never where the job runs.
type videoListItem struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model,omitempty"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// videoListResponse is the OpenAI-shaped cursor envelope for one owner's live
// jobs. It intentionally contains no backend id or routing target.
type videoListResponse struct {
	Object  string          `json:"object"`
	Data    []videoListItem `json:"data"`
	FirstID *string         `json:"first_id"`
	HasMore bool            `json:"has_more"`
	LastID  *string         `json:"last_id"`
}

// handleVideoListHeaders answers GET /v1/videos from the registry. It is an
// ImmediateResponse because there is no backend to ask: the catalog is aibrix's
// own record of what the caller owns, deliberately without live status
// enrichment, which would mean fanning out HTTP calls to every pinned pod.
func (s *Server) handleVideoListHeaders(ctx context.Context, requestID, owner string, options AsyncJobListOptions) *extProcPb.ProcessingResponse {
	page, err := s.asyncJobRegistry().List(ctx, owner, asyncJobTypeVideo, options)
	if err != nil {
		if errors.Is(err, errAsyncJobNotFound) || errors.Is(err, errAsyncJobInvalidRecord) {
			return buildErrorResponse(envoyTypePb.StatusCode_BadRequest,
				"invalid video list cursor or parameters", "", "", HeaderErrorRequestBodyProcessing, "true")
		}
		klog.ErrorS(err, "failed to list async video jobs", "requestID", requestID, "owner", owner)
		return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			"video jobs could not be listed right now, please retry",
			ErrorCodeServiceUnavailable, "", HeaderErrorRouting, "true")
	}

	items := make([]videoListItem, 0, len(page.Records))
	for _, record := range page.Records {
		items = append(items, videoListItem{
			ID:        record.PublicJobID,
			Object:    "video",
			Model:     record.Model,
			CreatedAt: record.CreatedAt.Unix(),
			ExpiresAt: record.ExpiresAt.Unix(),
		})
	}

	var firstID, lastID *string
	if len(items) > 0 {
		firstID = &items[0].ID
		lastID = &items[len(items)-1].ID
	}
	respBody, err := sonic.Marshal(videoListResponse{
		Object:  "list",
		Data:    items,
		FirstID: firstID,
		HasMore: page.HasMore,
		LastID:  lastID,
	})
	if err != nil {
		klog.ErrorS(err, "failed to marshal video job list", "requestID", requestID, "owner", owner)
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError,
			"failed to marshal video job list", "", "", HeaderErrorResponseUnknown, "true")
	}

	klog.InfoS("video job list served from registry", "requestID", requestID, "owner", owner,
		"jobs", len(items), "hasMore", page.HasMore, "order", options.Order)

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extProcPb.ImmediateResponse{
				Status: &envoyTypePb.HttpStatus{Code: envoyTypePb.StatusCode_OK},
				Headers: &extProcPb.HeaderMutation{
					SetHeaders: buildEnvoyProxyHeaders(nil, "Content-Type", "application/json"),
				},
				Body: string(respBody),
			},
		},
	}
}

// pinAsyncVideoJob resolves publicJobID for its owner, rewrites the path to the
// backend id, applies RPS, and returns the header mutations that pin the request
// to the recorded pod. On error, RPS is rolled back if it was incremented and
// AddRequestCount is not called. Shared by the RequestHeaders and RequestBody
// pin paths.
func (s *Server) pinAsyncVideoJob(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, publicJobID string) (headers []*configPb.HeaderValueOption, term int64, errResp *extProcPb.ProcessingResponse) {
	owner := asyncJobOwnerFromRoutingContext(routingCtx)
	record, pod, err := s.asyncJobRegistry().Get(ctx, owner, publicJobID)

	// Attribute the model as soon as it is known, including on the error paths
	// below: gateway.go takes st.model from this same routing context, and an
	// empty model there skips emitMetricsCounterHelper(GatewayRequestModelFailTotal),
	// which would make dead-pod and store-outage failures invisible in
	// gateway_request_fail metrics even though the client did get an error.
	if record.Model != "" {
		routingCtx.Model = record.Model
	}
	if err != nil {
		klog.ErrorS(err, "failed to resolve async video job", "requestID", requestID, "publicJobID", publicJobID, "owner", owner)
		return nil, term, videoJobErrorResponse(publicJobID, err)
	}

	rewrittenPath, ok := rewriteVideoPathID(requestPath, record.BackendJobID)
	if !ok {
		klog.ErrorS(nil, "resolved video job path cannot be rewritten", "requestID", requestID, "publicJobID", publicJobID, "requestPath", requestPath)
		return nil, term, videoNotFoundResponse(publicJobID)
	}
	routingCtx.AsyncJobBackendID = record.BackendJobID

	routingCtx.SetTargetPod(pod)
	targetPodIP := routingCtx.TargetAddress()
	if targetPodIP == "" {
		// Ready but not yet routable (e.g. the IP has not propagated to this
		// cache entry). Same reasoning as a NotReady pod: keep the record.
		klog.InfoS("video job's pod has no routable address yet, keeping record for retry", "requestID", requestID, "publicJobID", publicJobID, "podName", pod.Name)
		return nil, term, videoJobPodUnavailableResponse(publicJobID)
	}

	applyConfigProfile(routingCtx, []*v1.Pod{pod})

	if errRes := s.enforceModelRPS(ctx, record.Model, routingCtx); errRes != nil {
		return nil, term, errRes
	}
	needsRollback := true
	defer func() {
		if needsRollback {
			s.decrModelRPS(ctx, record.Model, routingCtx)
		}
	}()

	headers = buildEnvoyProxyHeaders(make([]*configPb.HeaderValueOption, 0, 4),
		HeaderRoutingStrategy, videoJobAffinityLabel,
		HeaderTargetPod, targetPodIP,
		pathKey, rewrittenPath,
		"X-Request-Id", routingCtx.RequestID)

	klog.InfoS("request_start", "request_id", requestID, "request_path", rewrittenPath, "model", record.Model,
		"routing_strategy", videoJobAffinityLabel, "target_pod", pod.Name, "target_pod_ip", targetPodIP)

	needsRollback = false
	routingCtx.RequestEndTime = time.Now()
	term = s.cache.AddRequestCount(routingCtx, requestID, record.Model)

	return headers, term, nil
}

// handleVideoJobSubResource pins a public job id follow-up at the RequestBody
// phase. Bodyless GET/DELETE never reach here; those are pinned in
// handleVideoJobSubResourceHeaders.
func (s *Server) handleVideoJobSubResource(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, publicJobID string, reqBody []byte) (*extProcPb.ProcessingResponse, string, bool, int64) {
	routingCtx.ReqBody = reqBody

	headers, term, errResp := s.pinAsyncVideoJob(ctx, routingCtx, requestID, requestPath, publicJobID)
	if errResp != nil {
		return errResp, routingCtx.Model, false, term
	}

	headers = buildEnvoyProxyHeaders(headers, "content-length", strconv.Itoa(len(routingCtx.ReqBody)))

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
	return resp, routingCtx.Model, false, term
}

// handleVideoJobSubResourceHeaders pins a bodyless GET/DELETE at RequestHeaders.
// ext_proc never sends a RequestBody message when there is no body, so without
// this the routing-strategy, target-pod and rewritten :path headers would never
// be set. Only called when EndOfStream is true; otherwise the body-phase pin runs.
func (s *Server) handleVideoJobSubResourceHeaders(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, publicJobID string) (*extProcPb.ProcessingResponse, int64) {
	headers, term, errResp := s.pinAsyncVideoJob(ctx, routingCtx, requestID, requestPath, publicJobID)
	if errResp != nil {
		return errResp, term
	}

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: headers,
					},
					// The rewritten :path must be re-evaluated by Envoy, along
					// with the routing-strategy header match.
					ClearRouteCache: true,
				},
			},
		},
	}
	return resp, term
}

// handleVideoJobResponseBody buffers and rewrites the two Videos API responses
// that carry a job id. It reports handled=false for everything else -- including
// /content -- so those bodies stay on the untouched streaming path.
//
// The returned bool pair is (complete, handled): complete drives the caller's
// request-trace finalization, handled says whether this function owns the response.
func (s *Server) handleVideoJobResponseBody(ctx context.Context, requestID string, routerCtx *types.RoutingContext, b *extProcPb.ProcessingRequest_ResponseBody) (*extProcPb.ProcessingResponse, bool, bool) {
	if routerCtx == nil {
		return nil, false, false
	}
	method := routerCtx.ReqHeaders[methodKey]
	if !videoJobResponseNeedsBuffering(method, routerCtx.ReqPath) {
		return nil, false, false
	}

	// Same requestBuffers map as processLanguageResponse; a given requestID only
	// ever takes one of those paths.
	buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
	buffer := buf.(*bytes.Buffer)
	buffer.Write(b.ResponseBody.GetBody())

	if !b.ResponseBody.EndOfStream {
		// Hold this chunk back. The id can only be rewritten once the whole JSON
		// body is in hand, and forwarding a partial body would put the backend id
		// on the wire. Envoy emits the accumulated, rewritten body at EndOfStream.
		return videoJobBodyResponse(nil), false, true
	}
	requestBuffers.Delete(requestID)
	body := buffer.Bytes()

	if method == http.MethodPost {
		return s.registerVideoJobFromCreateResponse(ctx, requestID, routerCtx, body)
	}
	return videoJobBodyResponse(rewriteVideoJobStatusID(requestID, routerCtx, body)), true, true
}

// registerVideoJobFromCreateResponse durably records the job the backend just
// created and replaces its id with the public one. There is no successful create
// without a record: the public id is the client's only handle on the job, and it
// has to survive a gateway restart or a different replica taking the next poll.
func (s *Server) registerVideoJobFromCreateResponse(ctx context.Context, requestID string, routerCtx *types.RoutingContext, body []byte) (*extProcPb.ProcessingResponse, bool, bool) {
	pod := routerCtx.TargetPod()
	backendJobID := gjson.GetBytes(body, "id").String()
	if pod == nil || backendJobID == "" {
		// Nothing to pin: an error body, a non-JSON body, or a create that never
		// reached a pod. Forward it verbatim so the backend's own answer -- error
		// message included -- reaches the client unmangled.
		return videoJobBodyResponse(body), true, true
	}

	var expiresAt time.Time
	if raw := gjson.GetBytes(body, "expires_at"); raw.Type == gjson.Number {
		// A zero time is the registry's "no expiry supplied" sentinel, whereas
		// Unix epoch is an explicitly supplied, already-expired backend value.
		// Preserve that distinction so the registry can reject the latter instead
		// of silently applying its default TTL.
		expiresAt = time.Unix(raw.Int(), 0)
	}

	owner := asyncJobOwnerFromRoutingContext(routerCtx)
	record, err := s.asyncJobRegistry().Register(ctx, AsyncJobRegistration{
		JobType:      asyncJobTypeVideo,
		Owner:        owner,
		Model:        routerCtx.Model,
		BackendJobID: backendJobID,
		Pod:          pod,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		// The job exists on the pod but is unreachable through aibrix and stays
		// orphaned until it expires there. That is the price of never handing out
		// the backend id: with no record, a public id would resolve to nothing,
		// and the backend id is not a handle the gateway is willing to expose.
		klog.ErrorS(err, "failed to register async video job", "requestID", requestID, "owner", owner, "podName", pod.Name)
		return videoJobRegistrationFailedResponse(), true, true
	}

	rewritten, err := sjson.SetBytes(body, "id", record.PublicJobID)
	if err != nil {
		// The record would name a job the client can never learn the id of; drop
		// it rather than leave an unreachable entry occupying the owner's catalog.
		if delErr := s.asyncJobRegistry().Delete(ctx, owner, record.PublicJobID); delErr != nil {
			klog.ErrorS(delErr, "failed to drop unreachable async video job record", "requestID", requestID, "publicJobID", record.PublicJobID)
		}
		klog.ErrorS(err, "failed to rewrite video job id in create response", "requestID", requestID, "publicJobID", record.PublicJobID)
		return videoJobRegistrationFailedResponse(), true, true
	}

	klog.InfoS("async video job registered", "requestID", requestID, "publicJobID", record.PublicJobID,
		"owner", owner, "podName", pod.Name, "podNamespace", pod.Namespace, "expiresAt", record.ExpiresAt)

	return videoJobBodyResponse(rewritten), true, true
}

// rewriteVideoJobStatusID translates the backend id in a status body back to the
// public one. The public id comes from routerCtx.ReqPath, which still holds the
// path the client sent: the backend-id rewrite happened in the header mutation
// handed to Envoy, not on the routing context.
func rewriteVideoJobStatusID(requestID string, routerCtx *types.RoutingContext, body []byte) []byte {
	publicJobID, ok := extractVideoIDFromPath(routerCtx.ReqPath)
	if !ok || !gjson.GetBytes(body, "id").Exists() {
		// An error body or a shape without an id: nothing to translate, and
		// rewriting anything else here would corrupt the backend's answer.
		return body
	}
	rewritten, err := sjson.SetBytes(body, "id", publicJobID)
	if err != nil {
		klog.ErrorS(err, "failed to rewrite video job id in status response", "requestID", requestID, "publicJobID", publicJobID)
		return body
	}
	return rewritten
}

// rewriteVideoJobErrorBody removes the backend-private ID from the known fields
// of an OpenAI-style non-2xx JSON body after a follow-up request has been pinned.
// Restricting the rewrite to top-level id, error.message and error.param avoids
// corrupting unrelated trace ids or extension fields that merely contain the
// backend id as a substring. Non-JSON bodies are left untouched rather than
// subjected to an unsafe text replacement.
//
// Create failures are intentionally not covered: before POST /v1/videos
// succeeds no public ID or registry record exists, so there is no safe ID to
// substitute.
func rewriteVideoJobErrorBody(routerCtx *types.RoutingContext, body []byte) []byte {
	if routerCtx == nil || routerCtx.AsyncJobBackendID == "" {
		return body
	}
	publicJobID, ok := extractVideoIDFromPath(routerCtx.ReqPath)
	if !ok || publicJobID == routerCtx.AsyncJobBackendID {
		return body
	}
	if !gjson.ValidBytes(body) {
		return body
	}

	backendJobID := routerCtx.AsyncJobBackendID
	rewritten := body
	fields := []struct {
		path             string
		replaceSubstring bool
	}{
		{path: "id"},
		{path: "error.message", replaceSubstring: true},
		{path: "error.param"},
	}
	for _, field := range fields {
		value := gjson.GetBytes(rewritten, field.path)
		if !value.Exists() || value.Type != gjson.String {
			continue
		}
		fieldValue := value.String()
		if field.replaceSubstring {
			fieldValue = strings.ReplaceAll(fieldValue, backendJobID, publicJobID)
		} else if fieldValue == backendJobID {
			fieldValue = publicJobID
		} else {
			continue
		}
		if fieldValue == value.String() {
			continue
		}
		var err error
		rewritten, err = sjson.SetBytes(rewritten, field.path, fieldValue)
		if err != nil {
			return body
		}
	}
	return rewritten
}

// videoJobBodyResponse wraps a (possibly empty) body into a ResponseBody
// mutation. An empty body suppresses a chunk that is still being accumulated.
func videoJobBodyResponse(body []byte) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseBody{
			ResponseBody: &extProcPb.BodyResponse{
				Response: &extProcPb.CommonResponse{
					BodyMutation: &extProcPb.BodyMutation{
						Mutation: &extProcPb.BodyMutation_Body{
							Body: body,
						},
					},
				},
			},
		},
	}
}

// maybeDeleteVideoJobAfterDelete removes the record once the backend has
// confirmed the job is gone (2xx) or was never there (404). A 5xx leaves it
// alone: the job may still exist on that pod and the client needs the same
// sticky route to try again. Called from HandleResponseHeaders, where the
// upstream status is known -- not at pin time.
//
// A non-nil return replaces the client's response: the backend delete succeeded
// but the record outlived it, so the client is told to retry and finish the
// cleanup instead of being handed a success over a public id that still resolves.
func (s *Server) maybeDeleteVideoJobAfterDelete(ctx context.Context, routerCtx *types.RoutingContext, statusCode int) *extProcPb.ProcessingResponse {
	if routerCtx == nil || routerCtx.ReqHeaders[methodKey] != http.MethodDelete {
		return nil
	}
	if (statusCode < 200 || statusCode >= 300) && statusCode != http.StatusNotFound {
		return nil
	}
	publicJobID, ok := extractVideoIDFromPath(routerCtx.ReqPath)
	if !ok {
		return nil
	}

	owner := asyncJobOwnerFromRoutingContext(routerCtx)
	if err := s.asyncJobRegistry().Delete(ctx, owner, publicJobID); err != nil {
		klog.ErrorS(err, "failed to delete async video job record after backend delete",
			"requestID", routerCtx.RequestID, "publicJobID", publicJobID, "owner", owner, "status", statusCode)
		return videoJobCleanupFailedResponse(publicJobID)
	}

	klog.InfoS("async video job record deleted", "requestID", routerCtx.RequestID, "publicJobID", publicJobID, "owner", owner, "status", statusCode)
	return nil
}
