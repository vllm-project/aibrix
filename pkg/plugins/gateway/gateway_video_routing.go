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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// defaultVideoJobTTL is used when the create response omits expires_at. Pinning
// also forgets the mapping as soon as the owning pod is gone; this TTL only
// covers a job that is never polled or deleted.
const defaultVideoJobTTL = 7 * 24 * time.Hour

// videoJobAffinityLabel is the routing-strategy header value for a request
// already pinned to a video job's pod. It is not a registered
// types.RoutingAlgorithm and never goes through the algorithm registry.
const videoJobAffinityLabel = "video-job-affinity"

// videoJobRedisKeyPrefix namespaces video-job pod mappings in the shared Redis
// keyspace.
const videoJobRedisKeyPrefix = "aibrix:gateway:video_job:"

func videoJobRedisKey(videoID string) string {
	return videoJobRedisKeyPrefix + videoID
}

const (
	// videoJobCacheSyncInterval bounds how stale this replica's local cache can
	// be vs Redis. A still-valid local hit skips the Redis fallback, so without
	// this a forget on another replica (DELETE, pod-gone) would stay invisible
	// until the local entry's own TTL — which can be days.
	videoJobCacheSyncInterval = 60 * time.Second
	// videoJobCacheSyncBatchSize bounds how many keys share one MGET round-trip.
	videoJobCacheSyncBatchSize = 200
)

// startVideoJobCacheSync periodically reconciles this replica's local
// videoJobCache against Redis: missing keys are evicted, changed values are
// refreshed. It only walks IDs already cached locally. A whole-batch Redis
// error leaves the local cache untouched (a failed read is not a miss). Stops
// when stopCh is closed.
func (s *Server) startVideoJobCacheSync(stopCh <-chan struct{}) {
	if s.redisClient == nil {
		return
	}
	ticker := time.NewTicker(videoJobCacheSyncInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.syncVideoJobCacheFromRedis()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

func (s *Server) syncVideoJobCacheFromRedis() {
	var videoIDs []string
	s.videoJobCache.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			videoIDs = append(videoIDs, id)
		}
		return true
	})
	if len(videoIDs) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), videoJobCacheSyncInterval)
	defer cancel()

	for batchStart := 0; batchStart < len(videoIDs); batchStart += videoJobCacheSyncBatchSize {
		idChunk := videoIDs[batchStart:min(batchStart+videoJobCacheSyncBatchSize, len(videoIDs))]

		keys := make([]string, len(idChunk))
		for i, id := range idChunk {
			keys[i] = videoJobRedisKey(id)
		}
		vals, err := s.redisClient.MGet(ctx, keys...).Result()
		if err != nil {
			klog.V(4).ErrorS(err, "failed to refresh video job cache from redis", "batchStart", batchStart, "batchLen", len(idChunk))
			continue
		}

		for i, id := range idChunk {
			raw, ok := vals[i].(string)
			if !ok {
				// Nil or unexpected type: either Redis genuinely no longer vouches
				// for this mapping, or this replica's own write to it is still
				// pending/failed -- handleVideoJobCacheSyncMiss tells those apart.
				s.handleVideoJobCacheSyncMiss(ctx, id)
				continue
			}
			var entry videoJobCacheEntry
			if err := sonic.UnmarshalString(raw, &entry); err != nil {
				klog.V(4).ErrorS(err, "failed to unmarshal video job entry during cache sync", "videoID", id)
				continue
			}
			s.videoJobCache.Store(id, videoJobCacheItem{entry: entry, confirmed: true})
		}
	}
}

// handleVideoJobCacheSyncMiss reacts to videoID being absent from Redis during
// a sync pass. A previously confirmed entry (this replica successfully wrote
// or read it from Redis at some point) is evicted: Redis no longer vouches
// for it, most likely a DELETE or a forgotten pod on another replica. An
// entry that was never confirmed means this replica's own write to Redis is
// still pending or failed outright -- a nil read says nothing about whether
// that mapping is still valid, so it's retried here instead of being dropped
// as the only surviving copy (see the confirmed field on videoJobCacheItem).
func (s *Server) handleVideoJobCacheSyncMiss(ctx context.Context, videoID string) {
	cached, found := s.videoJobCache.Load(videoID)
	if !found {
		return
	}
	item, ok := cached.(videoJobCacheItem)
	if !ok || item.confirmed || !time.Now().Before(item.entry.ExpiresAt) {
		s.videoJobCache.Delete(videoID)
		return
	}
	if s.persistVideoJobToRedis(ctx, videoID, item.entry, time.Until(item.entry.ExpiresAt)) {
		s.videoJobCache.Store(videoID, videoJobCacheItem{entry: item.entry, confirmed: true})
	}
}

// videoJobCacheEntry is the video_id -> owning-pod mapping. The generated
// video lives on that pod's local disk, so follow-up GET/DELETE must return
// there. JSON tags are the Redis value shared across gateway replicas.
type videoJobCacheEntry struct {
	PodName      string    `json:"pod_name"`
	PodNamespace string    `json:"pod_namespace"`
	Model        string    `json:"model"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// videoJobCacheItem is what's actually stored in Server.videoJobCache.
// confirmed is local-only bookkeeping (never marshaled to Redis) recording
// whether entry is known to exist in Redis. See handleVideoJobCacheSyncMiss
// for why that distinction matters.
type videoJobCacheItem struct {
	entry     videoJobCacheEntry
	confirmed bool
}

// persistVideoJobToRedis write-throughs entry to Redis under videoID's key
// and reports whether Redis now has it. A false return (no redisClient, a
// marshal error, or a Set error -- both logged here) means the caller's local
// copy is this replica's only copy, and a later Redis miss for videoID must
// not be read as proof the mapping was deleted elsewhere.
func (s *Server) persistVideoJobToRedis(ctx context.Context, videoID string, entry videoJobCacheEntry, ttl time.Duration) bool {
	if s.redisClient == nil {
		return false
	}
	payload, err := sonic.Marshal(entry)
	if err != nil {
		klog.ErrorS(err, "failed to marshal video job entry for redis", "videoID", videoID)
		return false
	}
	if err := s.redisClient.Set(ctx, videoJobRedisKey(videoID), string(payload), ttl).Err(); err != nil {
		klog.ErrorS(err, "failed to persist video job pod to redis", "videoID", videoID)
		return false
	}
	return true
}

// rememberVideoJobPod stores videoID's owning pod locally and, when Redis is
// configured, write-through with the same TTL. ttl is expires_at when the
// backend reported one, otherwise defaultVideoJobTTL.
func (s *Server) rememberVideoJobPod(ctx context.Context, videoID, podName, podNamespace, model string, ttl time.Duration) {
	if videoID == "" || podName == "" {
		return
	}
	if ttl <= 0 {
		ttl = defaultVideoJobTTL
	}
	entry := videoJobCacheEntry{
		PodName:      podName,
		PodNamespace: podNamespace,
		Model:        model,
		ExpiresAt:    time.Now().Add(ttl),
	}

	// Write-through to Redis before the local Store below: syncVideoJobCacheFromRedis
	// walks locally-cached IDs and evicts unconfirmed entries when this fails, so
	// storing locally first would open a window where a concurrent sync tick sees
	// this videoID still missing from Redis and evicts/retries prematurely.
	confirmed := s.persistVideoJobToRedis(ctx, videoID, entry, ttl)
	s.videoJobCache.Store(videoID, videoJobCacheItem{entry: entry, confirmed: confirmed})
}

// lookupVideoJobPod returns the pod that owns videoID, or ok=false if the
// mapping is unknown or expired. Expired local entries are evicted on read.
// A local miss falls back to Redis (cross-replica) and warms the local cache.
func (s *Server) lookupVideoJobPod(ctx context.Context, videoID string) (podName, podNamespace, model string, ok bool) {
	if cached, found := s.videoJobCache.Load(videoID); found {
		item, itemOK := cached.(videoJobCacheItem)
		if !itemOK {
			s.videoJobCache.Delete(videoID)
		} else if time.Now().Before(item.entry.ExpiresAt) {
			return item.entry.PodName, item.entry.PodNamespace, item.entry.Model, true
		} else {
			s.videoJobCache.Delete(videoID)
		}
	}

	if s.redisClient == nil {
		return "", "", "", false
	}

	val, err := s.redisClient.Get(ctx, videoJobRedisKey(videoID)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			klog.ErrorS(err, "failed to look up video job pod in redis", "videoID", videoID)
		}
		return "", "", "", false
	}
	var entry videoJobCacheEntry
	if err := sonic.UnmarshalString(val, &entry); err != nil {
		klog.ErrorS(err, "failed to unmarshal video job entry from redis", "videoID", videoID)
		return "", "", "", false
	}
	if time.Now().After(entry.ExpiresAt) {
		// Redis EX should already have reaped this; don't resurrect on clock skew.
		return "", "", "", false
	}

	s.videoJobCache.Store(videoID, videoJobCacheItem{entry: entry, confirmed: true})
	return entry.PodName, entry.PodNamespace, entry.Model, true
}

// forgetVideoJobPod drops videoID from the local cache and Redis (pod gone, or
// a DELETE that already succeeded).
func (s *Server) forgetVideoJobPod(ctx context.Context, videoID string) {
	s.videoJobCache.Delete(videoID)
	if s.redisClient == nil {
		return
	}
	if err := s.redisClient.Del(ctx, videoJobRedisKey(videoID)).Err(); err != nil {
		klog.ErrorS(err, "failed to delete video job pod from redis", "videoID", videoID)
	}
}

// extractVideoIDFromPath returns the {id} of /v1/videos/{id} or
// /v1/videos/{id}/content. Bare /v1/videos and /v1/videos/sync have no id to pin.
func extractVideoIDFromPath(requestPath string) (videoID string, ok bool) {
	const prefix = PathVideos + "/"
	requestPath = pathWithoutQuery(requestPath)
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

// videoNotFoundResponse builds the 404 returned when a video_id is unknown,
// expired, or its owning pod is no longer available.
func videoNotFoundResponse(videoID string) *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_NotFound,
		fmt.Sprintf("video %s not found", videoID), ErrorCodeVideoNotFound, "",
		HeaderErrorVideoNotFound, "true")
}

// parseVideoListRequest reports whether requestPath is the bare /v1/videos
// list path (not a {id} sub-resource) and the value of its model query
// parameter, which may be empty.
func parseVideoListRequest(requestPath string) (model string, isListPath bool) {
	path, query, _ := strings.Cut(requestPath, "?")
	if path != PathVideos {
		return "", false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", true
	}
	return values.Get("model"), true
}

// videoListModelRequiredResponse is the 400 for GET /v1/videos without model.
// A standalone vLLM-Omni server has one implicit model; this gateway does not.
func videoListModelRequiredResponse() *extProcPb.ProcessingResponse {
	return buildErrorResponse(envoyTypePb.StatusCode_BadRequest,
		"'model' query parameter is required to list video jobs", "", "model",
		HeaderErrorRequestBodyProcessing, "true")
}

// videoListFanoutTimeout bounds how long GET /v1/videos waits on the slowest
// pod before merging whatever succeeded. Total failure is a 503, not an empty list.
const videoListFanoutTimeout = 5 * time.Second

// videoListFanoutMaxConcurrency caps parallel pod fetches so a large replica
// set cannot open unbounded outbound connections from the gateway plugin.
const videoListFanoutMaxConcurrency = 16

// videoListFanoutMaxBodyBytes caps a single pod's list response. A malformed
// or malicious backend must not be able to OOM the gateway via io.ReadAll.
const videoListFanoutMaxBodyBytes = 1 << 20 // 1 MiB

// videoListFanoutClient is reused across requests (connection pooling) for the
// GET /v1/videos fan-out below; it makes no other outbound calls.
var videoListFanoutClient = &http.Client{Timeout: videoListFanoutTimeout}

// podAddress is the routable ip:port for model on pod (ModelClaim port, then
// the model.aibrix.ai/port label). SetTargetPod/TargetAddress are one-shot per
// request, so the list fan-out cannot reuse them per pod.
func podAddress(requestID, model string, pod *v1.Pod) string {
	if port, ok := utils.ModelClaimPortForPod(pod, model); ok {
		if port > 0 {
			return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port))
		}
		return ""
	}
	return net.JoinHostPort(pod.Status.PodIP, strconv.FormatInt(utils.GetModelPortForPod(requestID, pod), 10))
}

// handleVideoListHeaders serves GET /v1/videos by fanning out to every
// routable pod for model and merging their job lists. List has no owning
// pod — each replica only knows jobs on its own disk — so the plugin acts as
// the HTTP client and returns an ImmediateResponse instead of pinning via
// Envoy. Individual pod failures are skipped; if every routable pod fails
// (or none are routable), this returns 503 so an outage is not an empty list.
func (s *Server) handleVideoListHeaders(requestID, model string) *extProcPb.ProcessingResponse {
	podsArr, errResp := s.validateModelAvailability(requestID, model)
	if errResp != nil {
		return errResp
	}
	pods := utils.FilterRoutablePods(podsArr.All())
	if len(pods) == 0 {
		klog.ErrorS(nil, "video list fan-out has no routable pods", "requestID", requestID, "model", model)
		return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			fmt.Sprintf("no ready pod available to list video jobs for model %s", model),
			ErrorCodeServiceUnavailable, "", HeaderErrorNoModelBackends, "true")
	}

	type podResult struct {
		podName string
		data    []byte
		err     error
	}
	results := make([]podResult, len(pods))

	fanoutCtx, cancel := context.WithTimeout(context.Background(), videoListFanoutTimeout)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, videoListFanoutMaxConcurrency)
	for i, pod := range pods {
		results[i].podName = pod.Name
		addr := podAddress(requestID, model, pod)
		if addr == "" {
			results[i].err = fmt.Errorf("pod %s has no routable address", pod.Name)
			continue
		}
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i].err = fmt.Errorf("panic fetching video list from pod: %v", r)
					klog.ErrorS(nil, "panic recovered in video list fan-out goroutine", "requestID", requestID, "model", model, "pod", results[i].podName, "panic", r)
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-fanoutCtx.Done():
				results[i].err = fanoutCtx.Err()
				return
			}
			data, err := fetchPodVideoList(fanoutCtx, addr)
			results[i].data = data
			results[i].err = err
		}(i, addr)
	}
	wg.Wait()

	merged := make([]json.RawMessage, 0, len(pods))
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			klog.V(2).ErrorS(r.err, "video list fan-out to pod failed, skipping", "requestID", requestID, "model", model, "pod", r.podName)
			continue
		}
		merged = append(merged, extractVideoListItems(r.data)...)
	}

	if failed == len(pods) {
		klog.ErrorS(nil, "video list fan-out failed for every pod", "requestID", requestID, "model", model, "pods", len(pods))
		return buildErrorResponse(envoyTypePb.StatusCode_ServiceUnavailable,
			fmt.Sprintf("failed to list video jobs from any pod for model %s", model),
			ErrorCodeServiceUnavailable, "", HeaderErrorNoModelBackends, "true")
	}

	respBody, err := sonic.Marshal(map[string]any{"object": "list", "data": merged})
	if err != nil {
		klog.ErrorS(err, "failed to marshal merged video list", "requestID", requestID, "model", model)
		return buildErrorResponse(envoyTypePb.StatusCode_InternalServerError, "failed to marshal merged video list", "", "", HeaderErrorResponseUnknown, "true")
	}

	klog.InfoS("video list fan-out complete", "requestID", requestID, "model", model, "pods", len(pods), "failed", failed, "merged", len(merged))

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

// fetchPodVideoList GETs /v1/videos on addr (the pod, not Envoy). No model
// query is sent: a single vLLM-Omni server has one implicit model.
func fetchPodVideoList(ctx context.Context, addr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+PathVideos, nil)
	if err != nil {
		return nil, err
	}
	resp, err := videoListFanoutClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, videoListFanoutMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > videoListFanoutMaxBodyBytes {
		return nil, fmt.Errorf("video list response exceeds %d bytes", videoListFanoutMaxBodyBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return body, nil
}

// extractVideoListItems returns job objects from a pod's list body: an
// OpenAI {"data": [...]} envelope or a bare array. Other shapes yield nil so
// one malformed pod does not fail the merge.
func extractVideoListItems(body []byte) []json.RawMessage {
	data := gjson.GetBytes(body, "data")
	if !data.Exists() {
		data = gjson.ParseBytes(body)
	}
	if !data.IsArray() {
		return nil
	}
	items := data.Array()
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		out[i] = json.RawMessage(item.Raw)
	}
	return out
}

// pinVideoJobSubResource looks up videoID's pod, applies RPS, and returns
// header mutations that pin the request via ORIGINAL_DST. On error, RPS is
// rolled back if it was incremented; AddRequestCount is not called. Used from
// both the RequestBody and RequestHeaders pin paths.
func (s *Server) pinVideoJobSubResource(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, videoID string) (headers []*configPb.HeaderValueOption, model string, term int64, errResp *extProcPb.ProcessingResponse) {
	var podName, podNamespace string
	var ok bool
	podName, podNamespace, model, ok = s.lookupVideoJobPod(ctx, videoID)
	if !ok {
		klog.ErrorS(nil, "unknown or expired video job", "requestID", requestID, "videoID", videoID)
		return nil, "", term, videoNotFoundResponse(videoID)
	}

	// Set as soon as the model is known (even on the error returns below) so
	// gateway.go's st.model, taken from this same routingCtx, isn't left empty --
	// an empty model there skips emitMetricsCounterHelper(GatewayRequestModelFailTotal),
	// which would otherwise make stale/rescheduled-pod failures invisible to
	// gateway_request_fail metrics.
	routingCtx.Model = model

	pod, err := s.cache.GetPod(podName, podNamespace)
	if err != nil || pod == nil || !utils.IsPodReady(pod) {
		s.forgetVideoJobPod(ctx, videoID)
		klog.ErrorS(err, "video job's pod is no longer available", "requestID", requestID, "videoID", videoID, "podName", podName, "podNamespace", podNamespace)
		return nil, model, term, videoNotFoundResponse(videoID)
	}

	routingCtx.SetTargetPod(pod)
	targetPodIP := routingCtx.TargetAddress()
	if targetPodIP == "" {
		s.forgetVideoJobPod(ctx, videoID)
		klog.ErrorS(nil, "video job's pod has no routable address", "requestID", requestID, "videoID", videoID, "podName", podName)
		return nil, model, term, videoNotFoundResponse(videoID)
	}

	applyConfigProfile(routingCtx, []*v1.Pod{pod})

	if errRes := s.enforceModelRPS(ctx, model, routingCtx); errRes != nil {
		return nil, model, term, errRes
	}
	needsRollback := true
	defer func() {
		if needsRollback {
			s.decrModelRPS(ctx, model, routingCtx)
		}
	}()

	headers = buildEnvoyProxyHeaders(make([]*configPb.HeaderValueOption, 0, 3),
		HeaderRoutingStrategy, videoJobAffinityLabel,
		HeaderTargetPod, targetPodIP,
		"X-Request-Id", routingCtx.RequestID)

	klog.InfoS("request_start", "request_id", requestID, "request_path", requestPath, "model", model,
		"routing_strategy", videoJobAffinityLabel, "target_pod", podName, "target_pod_ip", targetPodIP)

	needsRollback = false
	routingCtx.RequestEndTime = time.Now()
	term = s.cache.AddRequestCount(routingCtx, requestID, model)

	return headers, model, term, nil
}

// maybeForgetVideoJobAfterDelete drops the mapping after a DELETE 2xx or 404.
// 5xx keeps it so the client can retry the same sticky route. Called from
// HandleResponseHeaders once upstream status is known, not at pin time.
func (s *Server) maybeForgetVideoJobAfterDelete(ctx context.Context, routerCtx *types.RoutingContext, statusCode int) {
	if routerCtx == nil || routerCtx.ReqHeaders[methodKey] != http.MethodDelete {
		return
	}
	if (statusCode < 200 || statusCode >= 300) && statusCode != http.StatusNotFound {
		return
	}
	videoID, ok := extractVideoIDFromPath(routerCtx.ReqPath)
	if !ok {
		return
	}
	s.forgetVideoJobPod(ctx, videoID)
	klog.InfoS("video job pod mapping evicted on delete", "requestID", routerCtx.RequestID, "videoID", videoID, "status", statusCode)
}

// handleVideoJobSubResource pins a video_id follow-up at the RequestBody
// phase. Bodyless GET/DELETE never reach here; those are pinned in
// handleVideoJobSubResourceHeaders.
func (s *Server) handleVideoJobSubResource(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, videoID string, reqBody []byte) (*extProcPb.ProcessingResponse, string, bool, int64) {
	routingCtx.ReqBody = reqBody

	headers, model, term, errResp := s.pinVideoJobSubResource(ctx, routingCtx, requestID, requestPath, videoID)
	if errResp != nil {
		return errResp, model, false, term
	}

	headers = buildEnvoyProxyHeaders(headers, "content-length", strconv.Itoa(len(routingCtx.ReqBody)))

	return &extProcPb.ProcessingResponse{
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
	}, model, false, term
}

// handleVideoJobSubResourceHeaders pins a bodyless GET/DELETE at RequestHeaders.
// ext_proc BUFFERED mode never sends RequestBody when there is no body, so
// without this the routing-strategy/target-pod headers would never be set.
// Only called when EndOfStream is true; otherwise the body-phase pin runs.
func (s *Server) handleVideoJobSubResourceHeaders(ctx context.Context, routingCtx *types.RoutingContext, requestID, requestPath, videoID string) (*extProcPb.ProcessingResponse, int64) {
	headers, _, term, errResp := s.pinVideoJobSubResource(ctx, routingCtx, requestID, requestPath, videoID)
	if errResp != nil {
		return errResp, term
	}

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
	}, term
}

// recordVideoJobPodFromResponse buffers a POST /v1/videos response and, at
// EndOfStream, records the owning pod from the JSON id. Uses the same
// requestBuffers map as processLanguageResponse; a given requestID is only
// one of those paths.
func (s *Server) recordVideoJobPodFromResponse(ctx context.Context, requestID string, routerCtx *types.RoutingContext, b *extProcPb.ProcessingRequest_ResponseBody) {
	buf, _ := requestBuffers.LoadOrStore(requestID, &bytes.Buffer{})
	buffer := buf.(*bytes.Buffer)
	buffer.Write(b.ResponseBody.GetBody())

	if !b.ResponseBody.EndOfStream {
		return
	}
	requestBuffers.Delete(requestID)

	pod := routerCtx.TargetPod()
	if pod == nil {
		return
	}

	body := buffer.Bytes()
	videoID := gjson.GetBytes(body, "id").String()
	if videoID == "" {
		return
	}

	ttl := defaultVideoJobTTL
	if expiresAt := gjson.GetBytes(body, "expires_at"); expiresAt.Exists() {
		if d := time.Until(time.Unix(expiresAt.Int(), 0)); d > 0 {
			ttl = d
		}
	}

	s.rememberVideoJobPod(ctx, videoID, pod.Name, pod.Namespace, routerCtx.Model, ttl)
	klog.InfoS("video job pod recorded", "requestID", requestID, "videoID", videoID, "podName", pod.Name, "ttl", ttl)
}
