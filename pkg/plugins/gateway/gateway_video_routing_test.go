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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/bytedance/sonic"
	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func newTestVideoJobRedisServer(t *testing.T) (*Server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &Server{redisClient: client}, mr
}

func TestExtractVideoIDFromPath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wantID string
		wantOK bool
	}{
		{"base list/create path has no id", "/v1/videos", "", false},
		{"sync path is not an id", "/v1/videos/sync", "", false},
		{"status poll", "/v1/videos/video_gen_abc123", "video_gen_abc123", true},
		{"content download", "/v1/videos/video_gen_abc123/content", "video_gen_abc123", true},
		{"status poll with query", "/v1/videos/video_gen_abc123?foo=bar", "video_gen_abc123", true},
		{"content download with variant query", "/v1/videos/video_gen_abc123/content?variant=mp4", "video_gen_abc123", true},
		{"trailing slash with empty id", "/v1/videos/", "", false},
		{"unrelated path", "/v1/chat/completions", "", false},
		{"unrelated path sharing prefix", "/v1/videosomethingelse", "", false},
		{"nested sub-path beyond content still extracts leading id", "/v1/videos/video_gen_abc123/content/extra", "video_gen_abc123", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotOK := extractVideoIDFromPath(tt.path)
			assert.Equal(t, tt.wantOK, gotOK)
			assert.Equal(t, tt.wantID, gotID)
		})
	}
}

func TestVideoJobRedisKey(t *testing.T) {
	assert.Equal(t, "aibrix:gateway:video_job:video-1", videoJobRedisKey("video-1"))
}

// TestServer_VideoJobPodTracking exercises the local-cache-only path (redisClient
// is nil, matching standalone/local dev mode) -- every redis-touching branch in
// rememberVideoJobPod/lookupVideoJobPod/forgetVideoJobPod short-circuits on that
// nil check. Redis-backed cross-replica behavior itself is covered separately by
// the TestSyncVideoJobCacheFromRedis_* tests below, which use a real (miniredis)
// client.
func TestServer_VideoJobPodTracking(t *testing.T) {
	s := &Server{}
	ctx := context.Background()

	// Unknown id.
	_, _, _, ok := s.lookupVideoJobPod(ctx, "missing")
	assert.False(t, ok)

	// Set then get.
	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "wan2.1-vace-1.3b", time.Hour)
	podName, podNamespace, model, ok := s.lookupVideoJobPod(ctx, "video-1")
	assert.True(t, ok)
	assert.Equal(t, "pod-a", podName)
	assert.Equal(t, "ns-a", podNamespace)
	assert.Equal(t, "wan2.1-vace-1.3b", model)

	// Non-positive TTL falls back to the default rather than being treated as
	// "already expired".
	s.rememberVideoJobPod(ctx, "video-2", "pod-b", "ns-b", "m", 0)
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-2")
	assert.True(t, ok)

	// Expired entries are evicted on read. rememberVideoJobPod itself can't produce
	// an already-expired entry (it coerces ttl<=0 to the default, per the video-2
	// case above), so store one directly to exercise the expiry path.
	s.videoJobCache.Store("video-3", videoJobCacheEntry{
		PodName: "pod-c", PodNamespace: "ns-c", Model: "m",
		ExpiresAt: time.Now().Add(-time.Second),
	})
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-3")
	assert.False(t, ok)
	_, found := s.videoJobCache.Load("video-3")
	assert.False(t, found, "expired entry should have been evicted on read")

	// Explicit forget.
	s.forgetVideoJobPod(ctx, "video-1")
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-1")
	assert.False(t, ok)

	// Empty videoID/podName are no-ops, not stored.
	s.rememberVideoJobPod(ctx, "", "pod-x", "ns-x", "m", time.Hour)
	s.rememberVideoJobPod(ctx, "video-4", "", "ns-x", "m", time.Hour)
	_, _, _, ok = s.lookupVideoJobPod(ctx, "")
	assert.False(t, ok)
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-4")
	assert.False(t, ok)
}

func readyPod(name, namespace, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// TestHandleVideoJobSubResourceHeaders_PinsKnownJob covers the fix for the bug
// where a GET/DELETE video sub-resource request with no body never got pinned
// to its owning pod: Envoy's ext_proc filter (request_body_mode: BUFFERED)
// never sends a RequestBody message for a bodyless request, so pinning must
// happen at RequestHeaders instead of (only) RequestBody.
func TestHandleVideoJobSubResourceHeaders_PinsKnownJob(t *testing.T) {
	mockCache := new(MockCache)
	s := &Server{cache: mockCache}
	ctx := context.Background()

	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "wan2.1-vace-1.3b", time.Hour)

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1-vace-1.3b").Return(int64(7))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: "GET"}

	resp, term := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", "/v1/videos/video-1", "video-1")

	require.NotNil(t, resp.GetRequestHeaders())
	assert.Nil(t, resp.GetImmediateResponse())
	assert.EqualValues(t, 7, term)

	headersResp := resp.GetRequestHeaders().GetResponse()
	assert.True(t, headersResp.GetClearRouteCache(), "must clear route cache so Envoy re-evaluates the routing-strategy header match")

	set := headersResp.GetHeaderMutation().GetSetHeaders()
	assertHeaderRawValue(t, set, HeaderRoutingStrategy, videoJobAffinityLabel)
	assertHeaderRawValue(t, set, HeaderTargetPod, "10.0.0.5:8000")

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_UnknownVideoReturns404 verifies an
// unknown/expired video_id still gets a proper 404 (not a hang or a bare
// route-not-found from Envoy) when resolved at the headers phase.
func TestHandleVideoJobSubResourceHeaders_UnknownVideoReturns404(t *testing.T) {
	s := &Server{cache: new(MockCache)}
	ctx := context.Background()
	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: "GET"}

	resp, term := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", "/v1/videos/does-not-exist", "does-not-exist")

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.EqualValues(t, 0, term)
}

// TestHandleRequestHeaders_VideoStatusPoll_Bodyless reproduces the reported
// gateway bug end-to-end through the real entry point: a GET status-poll
// request with EndOfStream=true (Envoy's signal that no body follows) must be
// pinned to its owning pod during header processing, since HandleRequestBody
// will never be invoked for it.
func TestHandleRequestHeaders_VideoStatusPoll_Bodyless(t *testing.T) {
	mockCache := new(MockCache)
	s := &Server{cache: mockCache}
	ctx := context.Background()

	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "wan2.1-vace-1.3b", time.Hour)
	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "wan2.1-vace-1.3b").Return(int64(1))

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte("/v1/videos/video-1")},
					{Key: methodKey, RawValue: []byte("GET")},
				}},
				EndOfStream: true,
			},
		},
	}

	rootSpan := trace.SpanFromContext(ctx)
	resp, _, _, routingCtx, term := s.HandleRequestHeaders(ctx, "req-1", rootSpan, req)

	require.NotNil(t, resp.GetRequestHeaders(), "GET with no body must be pinned at the headers phase, not left for a body message that never arrives")
	assert.EqualValues(t, 1, term)
	set := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	assertHeaderRawValue(t, set, HeaderRoutingStrategy, videoJobAffinityLabel)
	assertHeaderRawValue(t, set, HeaderTargetPod, "10.0.0.5:8000")
	assert.Equal(t, "wan2.1-vace-1.3b", routingCtx.Model)

	mockCache.AssertExpectations(t)
}

// TestHandleRequestHeaders_VideoStatusPoll_WithBody ensures the new
// EndOfStream-gated header-phase pinning doesn't fire when a body IS coming
// (EndOfStream false): that case is left to HandleRequestBody, unchanged, so
// this must NOT pin at headers time or set routing headers.
func TestHandleRequestHeaders_VideoStatusPoll_WithBody(t *testing.T) {
	mockCache := new(MockCache)
	s := &Server{cache: mockCache}
	ctx := context.Background()
	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "wan2.1-vace-1.3b", time.Hour)

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte("/v1/videos/video-1")},
					{Key: methodKey, RawValue: []byte("DELETE")},
				}},
				EndOfStream: false,
			},
		},
	}

	rootSpan := trace.SpanFromContext(ctx)
	resp, _, _, routingCtx, term := s.HandleRequestHeaders(ctx, "req-1", rootSpan, req)

	require.NotNil(t, resp.GetRequestHeaders())
	assert.EqualValues(t, 0, term)
	set := resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()
	for _, h := range set {
		assert.NotEqual(t, HeaderRoutingStrategy, h.GetHeader().GetKey(), "pinning must be deferred to the body phase when a body is coming")
		assert.NotEqual(t, HeaderTargetPod, h.GetHeader().GetKey())
	}
	assert.Equal(t, "", routingCtx.Model)

	mockCache.AssertNotCalled(t, "GetPod", mock.Anything, mock.Anything)
}

func assertHeaderRawValue(t *testing.T, headers []*configPb.HeaderValueOption, key, want string) {
	t.Helper()
	for _, h := range headers {
		if h.GetHeader().GetKey() == key {
			assert.Equal(t, want, string(h.GetHeader().GetRawValue()))
			return
		}
	}
	t.Errorf("header %q not found in %v", key, headers)
}

// TestSyncVideoJobCacheFromRedis_RefreshesChangedEntry covers the case this sync
// exists for: a different replica updated (or re-created) the Redis mapping --
// here simulated by writing a different pod directly to Redis -- and this
// replica's stale local copy must pick up the change without waiting for its
// own (possibly days-long) local ExpiresAt.
func TestSyncVideoJobCacheFromRedis_RefreshesChangedEntry(t *testing.T) {
	s, _ := newTestVideoJobRedisServer(t)
	ctx := context.Background()

	s.rememberVideoJobPod(ctx, "video-1", "pod-old", "ns-a", "m", time.Hour)

	fresh := videoJobCacheEntry{PodName: "pod-new", PodNamespace: "ns-b", Model: "m", ExpiresAt: time.Now().Add(time.Hour)}
	payload, err := sonic.Marshal(fresh)
	require.NoError(t, err)
	require.NoError(t, s.redisClient.Set(ctx, videoJobRedisKey("video-1"), string(payload), time.Hour).Err())

	s.syncVideoJobCacheFromRedis()

	podName, podNamespace, _, ok := s.lookupVideoJobPod(ctx, "video-1")
	require.True(t, ok)
	assert.Equal(t, "pod-new", podName)
	assert.Equal(t, "ns-b", podNamespace)
}

// TestSyncVideoJobCacheFromRedis_EvictsRedisMiss covers the DELETE/pod-gone
// cross-replica case directly: another replica's forgetVideoJobPod removed the
// Redis key, and this replica's local (not-yet-expired) copy must be evicted on
// the next sync rather than continuing to serve/pin against it.
func TestSyncVideoJobCacheFromRedis_EvictsRedisMiss(t *testing.T) {
	s, _ := newTestVideoJobRedisServer(t)

	// Store locally without ever writing to Redis, standing in for "some other
	// replica already called forgetVideoJobPod and deleted the Redis key".
	s.videoJobCache.Store("video-2", videoJobCacheEntry{
		PodName: "pod-a", PodNamespace: "ns-a", Model: "m", ExpiresAt: time.Now().Add(time.Hour),
	})

	s.syncVideoJobCacheFromRedis()

	_, found := s.videoJobCache.Load("video-2")
	assert.False(t, found, "local entry must be evicted once Redis no longer has it")
}

// TestSyncVideoJobCacheFromRedis_LeavesLocalCacheUntouchedOnRedisError ensures a
// Redis outage during the periodic sync doesn't get misread as "every key is
// missing": that would wipe every replica's local cache (and, if it also
// deleted from Redis, cascade into deleting jobs cluster-wide) over a transient
// connectivity blip rather than a real forget/delete.
func TestSyncVideoJobCacheFromRedis_LeavesLocalCacheUntouchedOnRedisError(t *testing.T) {
	s, mr := newTestVideoJobRedisServer(t)
	ctx := context.Background()

	s.rememberVideoJobPod(ctx, "video-3", "pod-a", "ns-a", "m", time.Hour)
	mr.Close()

	s.syncVideoJobCacheFromRedis()

	cached, found := s.videoJobCache.Load("video-3")
	require.True(t, found, "local entry must survive a whole-batch redis error")
	assert.Equal(t, "pod-a", cached.(videoJobCacheEntry).PodName)
}

func TestParseVideoListRequest(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantModel  string
		wantIsList bool
	}{
		{"list with model", "/v1/videos?model=wan2.1", "wan2.1", true},
		{"list without model", "/v1/videos", "", true},
		{"list with empty model param", "/v1/videos?model=", "", true},
		{"list with other params first", "/v1/videos?foo=bar&model=wan2.1", "wan2.1", true},
		{"sub-resource path is not the list", "/v1/videos/video-1", "", false},
		{"sync path is not the list", "/v1/videos/sync", "", false},
		{"unrelated path", "/v1/chat/completions", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, isList := parseVideoListRequest(tt.path)
			assert.Equal(t, tt.wantIsList, isList)
			assert.Equal(t, tt.wantModel, model)
		})
	}
}

func podWithPortLabel(name, ip string, port int) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{constants.ModelLabelPort: strconv.Itoa(port)},
		},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func TestPodAddress(t *testing.T) {
	pod := podWithPortLabel("pod-a", "10.0.0.5", 9000)
	assert.Equal(t, "10.0.0.5:9000", podAddress("req-1", "some-model", pod))
}

func TestExtractVideoListItems(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"openai-style envelope", `{"object":"list","data":[{"id":"v1"},{"id":"v2"}]}`, 2},
		{"bare top-level array", `[{"id":"v1"}]`, 1},
		{"empty envelope", `{"object":"list","data":[]}`, 0},
		{"malformed body", `not json`, 0},
		{"unrelated shape", `{"status":"ok"}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := extractVideoListItems([]byte(tt.body))
			assert.Len(t, items, tt.want)
		})
	}
}

// videoListPod starts an httptest server standing in for a single vLLM-Omni
// pod's own GET /v1/videos endpoint, and returns a *v1.Pod whose
// model.aibrix.ai/port label points podAddress at that server.
func videoListPod(t *testing.T, name string, handler http.HandlerFunc) *v1.Pod {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	var port int
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)
	return podWithPortLabel(name, "127.0.0.1", port)
}

// TestHandleVideoListHeaders_FanOutMergesAcrossPods is the core coverage for
// the fan-out design: GET /v1/videos?model=X has no video_id to pin on and no
// single owning pod (each pod only knows about jobs it created locally), so
// the gateway itself queries every ready pod for the model and merges their
// individual lists into one response -- this verifies that merge actually
// happens across more than one pod, and that a slow/erroring pod doesn't
// block or fail the whole request.
func TestHandleVideoListHeaders_FanOutMergesAcrossPods(t *testing.T) {
	podA := videoListPod(t, "pod-a", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, PathVideos, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video-a1"},{"id":"video-a2"}]}`))
	})
	podB := videoListPod(t, "pod-b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video-b1"}]}`))
	})
	podErr := videoListPod(t, "pod-err", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	mockCache := new(MockCache)
	mockCache.On("HasModel", "wan2.1").Return(true)
	mockCache.On("ListPodsByModel", "wan2.1").Return(types.PodList(&utils.PodArray{Pods: []*v1.Pod{podA, podB, podErr}}), nil)

	s := &Server{cache: mockCache}
	resp := s.handleVideoListHeaders("req-1", "wan2.1")

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_OK, imm.GetStatus().GetCode())

	ids := gjson.GetBytes([]byte(imm.GetBody()), "data.#.id").Array()
	gotIDs := make([]string, len(ids))
	for i, id := range ids {
		gotIDs[i] = id.String()
	}
	assert.ElementsMatch(t, []string{"video-a1", "video-a2", "video-b1"}, gotIDs,
		"merged list must include every successful pod's jobs and skip the erroring one, not fail the whole request")

	mockCache.AssertExpectations(t)
}

func TestHandleVideoListHeaders_AllPodsFailReturnsServiceUnavailable(t *testing.T) {
	podA := videoListPod(t, "pod-a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	podB := videoListPod(t, "pod-b", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	mockCache := new(MockCache)
	mockCache.On("HasModel", "wan2.1").Return(true)
	mockCache.On("ListPodsByModel", "wan2.1").Return(types.PodList(&utils.PodArray{Pods: []*v1.Pod{podA, podB}}), nil)

	s := &Server{cache: mockCache}
	resp := s.handleVideoListHeaders("req-1", "wan2.1")

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, imm.GetStatus().GetCode(),
		"total fan-out failure must not look like an empty job list")

	mockCache.AssertExpectations(t)
}

func TestHandleVideoListHeaders_SuccessfulEmptyListIsOK(t *testing.T) {
	podA := videoListPod(t, "pod-a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})

	mockCache := new(MockCache)
	mockCache.On("HasModel", "wan2.1").Return(true)
	mockCache.On("ListPodsByModel", "wan2.1").Return(types.PodList(&utils.PodArray{Pods: []*v1.Pod{podA}}), nil)

	s := &Server{cache: mockCache}
	resp := s.handleVideoListHeaders("req-1", "wan2.1")

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_OK, imm.GetStatus().GetCode())
	assert.Equal(t, 0, len(gjson.GetBytes([]byte(imm.GetBody()), "data").Array()))

	mockCache.AssertExpectations(t)
}

func TestHandleVideoListHeaders_SkipsNonRoutablePods(t *testing.T) {
	podReady := videoListPod(t, "pod-ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"video-ready"}]}`))
	})
	podNotReady := videoListPod(t, "pod-not-ready", func(w http.ResponseWriter, r *http.Request) {
		t.Error("non-routable pod must not be contacted")
		w.WriteHeader(http.StatusInternalServerError)
	})
	podNotReady.Status.Conditions = nil

	mockCache := new(MockCache)
	mockCache.On("HasModel", "wan2.1").Return(true)
	mockCache.On("ListPodsByModel", "wan2.1").Return(types.PodList(&utils.PodArray{Pods: []*v1.Pod{podReady, podNotReady}}), nil)

	s := &Server{cache: mockCache}
	resp := s.handleVideoListHeaders("req-1", "wan2.1")

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_OK, imm.GetStatus().GetCode())
	ids := gjson.GetBytes([]byte(imm.GetBody()), "data.#.id").Array()
	require.Len(t, ids, 1)
	assert.Equal(t, "video-ready", ids[0].String())

	mockCache.AssertExpectations(t)
}

func TestHandleVideoListHeaders_UnknownModelReturnsError(t *testing.T) {
	mockCache := new(MockCache)
	mockCache.On("HasModel", "does-not-exist").Return(false)

	s := &Server{cache: mockCache}
	resp := s.handleVideoListHeaders("req-1", "does-not-exist")

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, imm.GetStatus().GetCode())
}

// TestHandleRequestHeaders_VideoList_RequiresModel covers the real entry
// point: GET /v1/videos with no `model` query parameter must 400 rather than
// silently routing nowhere (the bug this endpoint had before the fan-out was
// added -- see extractVideoIDFromPath, which never matched the bare list path).
func TestHandleRequestHeaders_VideoList_RequiresModel(t *testing.T) {
	s := &Server{cache: new(MockCache)}
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte("/v1/videos")},
					{Key: methodKey, RawValue: []byte("GET")},
				}},
				EndOfStream: true,
			},
		},
	}

	ctx := context.Background()
	rootSpan := trace.SpanFromContext(ctx)
	resp, _, _, _, _ := s.HandleRequestHeaders(ctx, "req-1", rootSpan, req)

	imm := resp.GetImmediateResponse()
	require.NotNil(t, imm)
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, imm.GetStatus().GetCode())
}

func TestIsMultipartFormPath(t *testing.T) {
	assert.True(t, isMultipartFormPath(PathVideos))
	assert.True(t, isMultipartFormPath(PathVideos+"?foo=bar"))
	assert.True(t, isMultipartFormPath(PathVideosSync))
	assert.True(t, isMultipartFormPath(PathAudioTranscriptions))
	assert.False(t, isMultipartFormPath(PathVideos+"/video-1"))
	assert.False(t, isMultipartFormPath(PathChatCompletions))
}

func TestRecordVideoJobPodFromResponse_BuffersUntilEnd(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	requestID := "req-record-1"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos
	routerCtx.SetTargetPod(pod)

	expires := time.Now().Add(time.Hour).Unix()
	full := fmt.Sprintf(`{"id":"video-1","expires_at":%d}`, expires)
	mid := len(full) / 2

	s.recordVideoJobPodFromResponse(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(full[:mid])},
	})
	_, _, _, ok := s.lookupVideoJobPod(ctx, "video-1")
	assert.False(t, ok, "must not record until EndOfStream")

	s.recordVideoJobPodFromResponse(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(full[mid:]), EndOfStream: true},
	})
	podName, ns, model, ok := s.lookupVideoJobPod(ctx, "video-1")
	require.True(t, ok)
	assert.Equal(t, "pod-a", podName)
	assert.Equal(t, "ns-a", ns)
	assert.Equal(t, "wan2.1", model)
}

func TestHandleResponseBody_RecordsVideoJobWithQueryOnPath(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	requestID := "req-record-query"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos + "?foo=bar"
	routerCtx.RequestTime = time.Now()
	routerCtx.SetTargetPod(pod)

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseBody{
			ResponseBody: &extProcPb.HttpBody{
				Body:        []byte(`{"id":"video-query-1"}`),
				EndOfStream: true,
			},
		},
	}
	_, complete, _ := s.HandleResponseBody(ctx, routerCtx, requestID, req, utils.User{}, 0, "wan2.1", false, false)
	assert.True(t, complete)

	podName, _, _, ok := s.lookupVideoJobPod(ctx, "video-query-1")
	require.True(t, ok, "POST /v1/videos?… must still record the owning pod")
	assert.Equal(t, "pod-a", podName)
}

func TestHandleVideoJobSubResourceHeaders_DeleteKeepsMappingUntilResponse(t *testing.T) {
	mockCache := new(MockCache)
	s := &Server{cache: mockCache}
	ctx := context.Background()

	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "wan2.1-vace-1.3b", time.Hour)
	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1-vace-1.3b").Return(int64(1))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqPath = "/v1/videos/video-1"
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", "/v1/videos/video-1", "video-1")
	require.NotNil(t, resp.GetRequestHeaders())
	_, _, _, ok := s.lookupVideoJobPod(ctx, "video-1")
	assert.True(t, ok, "DELETE must not evict the mapping until the backend responds")

	headerReq := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":status", RawValue: []byte("500")},
				}},
			},
		},
	}
	_, isErr, code := s.HandleResponseHeaders(ctx, routingCtx, "req-1", "wan2.1-vace-1.3b", headerReq)
	assert.True(t, isErr)
	assert.Equal(t, 500, code)
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-1")
	assert.True(t, ok, "5xx delete must leave the mapping so the client can retry")

	headerReq = &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: ":status", RawValue: []byte("204")},
				}},
			},
		},
	}
	_, isErr, _ = s.HandleResponseHeaders(ctx, routingCtx, "req-1", "wan2.1-vace-1.3b", headerReq)
	assert.False(t, isErr)
	_, _, _, ok = s.lookupVideoJobPod(ctx, "video-1")
	assert.False(t, ok, "2xx delete must evict the mapping")

	mockCache.AssertExpectations(t)
}

func TestMaybeForgetVideoJobAfterDelete_EvictsOn404(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	s.rememberVideoJobPod(ctx, "video-1", "pod-a", "ns-a", "m", time.Hour)

	routingCtx := types.NewRoutingContext(ctx, "", "m", "", "req-1", "")
	routingCtx.ReqPath = "/v1/videos/video-1?unused=1"
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}

	s.maybeForgetVideoJobAfterDelete(ctx, routingCtx, http.StatusNotFound)
	_, _, _, ok := s.lookupVideoJobPod(ctx, "video-1")
	assert.False(t, ok)
}

func TestFetchPodVideoList_RejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), videoListFanoutMaxBodyBytes+1))
	}))
	t.Cleanup(srv.Close)

	_, err := fetchPodVideoList(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}
