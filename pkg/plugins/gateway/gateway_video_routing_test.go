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

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/asyncjob"
	"github.com/vllm-project/aibrix/pkg/types"
)

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
		{"trailing slash is not a valid job path", "/v1/videos/", "", false},
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

// TestRewriteVideoPathID pins the one rewrite the gateway performs on a video
// follow-up: the public job id in the path becomes the backend's own id. Only
// that segment may change -- the /content suffix decides whether the response is
// JSON or a video stream, and the query string carries backend options the
// gateway does not understand.
func TestRewriteVideoPathID(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		backendID string
		want      string
		wantOK    bool
	}{
		{"status path", "/v1/videos/aibrixjob-abc", "video_gen_1", "/v1/videos/video_gen_1", true},
		{"content path keeps its suffix", "/v1/videos/aibrixjob-abc/content", "video_gen_1", "/v1/videos/video_gen_1/content", true},
		{"query string is preserved", "/v1/videos/aibrixjob-abc?foo=bar", "video_gen_1", "/v1/videos/video_gen_1?foo=bar", true},
		{"content query string is preserved", "/v1/videos/aibrixjob-abc/content?variant=mp4", "video_gen_1", "/v1/videos/video_gen_1/content?variant=mp4", true},
		{"deeper sub-path is preserved", "/v1/videos/aibrixjob-abc/content/extra", "video_gen_1", "/v1/videos/video_gen_1/content/extra", true},
		{"trailing slash is refused", "/v1/videos/aibrixjob-abc/", "video_gen_1", "", false},
		{"list path has no id to rewrite", "/v1/videos", "video_gen_1", "", false},
		{"sync path has no id to rewrite", "/v1/videos/sync", "video_gen_1", "", false},
		{"empty backend id is refused", "/v1/videos/aibrixjob-abc", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rewriteVideoPathID(tt.path, tt.backendID)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAsyncJobOwnerFromRoutingContext covers scope derivation from the only
// identity the gateway actually has: the user request header. A request without
// one is shared scope, never a wildcard over other users' jobs.
func TestAsyncJobOwnerFromRoutingContext(t *testing.T) {
	assert.Equal(t, asyncjob.OwnerShared, asyncJobOwnerFromRoutingContext(nil))

	anonymous := types.NewRoutingContext(context.Background(), "", "", "", "req-1", "")
	assert.Equal(t, asyncjob.OwnerShared, asyncJobOwnerFromRoutingContext(anonymous))

	named := types.NewRoutingContext(context.Background(), "", "", "", "req-2", "alice")
	assert.Equal(t, "scope:user:alice", asyncJobOwnerFromRoutingContext(named))
}

// TestIsVideoListRequest verifies the public catalog is answered only for its
// exact, non-trailing-slash path. The query parameters are pagination controls,
// not a model selector.
func TestIsVideoListRequest(t *testing.T) {
	assert.True(t, isVideoListRequest(PathVideos, http.MethodGet))
	assert.True(t, isVideoListRequest(PathVideos+"?limit=20", http.MethodGet))
	assert.False(t, isVideoListRequest(PathVideos+"/", http.MethodGet))
	assert.False(t, isVideoListRequest(PathVideos+"/?limit=20", http.MethodGet))
	assert.False(t, isVideoListRequest(PathVideos, http.MethodPost))
	assert.False(t, isVideoListRequest(PathVideosSync, http.MethodGet))
	assert.False(t, isVideoListRequest(PathVideos+"/aibrixjob-abc", http.MethodGet))
	assert.False(t, isVideoListRequest(PathChatCompletions, http.MethodGet))
}

func TestParseVideoListOptions(t *testing.T) {
	tests := []struct {
		path    string
		want    asyncjob.AsyncJobListOptions
		wantErr bool
	}{
		{PathVideos, asyncjob.AsyncJobListOptions{Limit: 20, Order: "desc"}, false},
		{PathVideos + "?limit=100&order=asc&after=aibrixjob-cursor", asyncjob.AsyncJobListOptions{After: "aibrixjob-cursor", Limit: 100, Order: "asc"}, false},
		{PathVideos + "?limit=0", asyncjob.AsyncJobListOptions{}, true},
		{PathVideos + "?limit=101", asyncjob.AsyncJobListOptions{}, true},
		{PathVideos + "?order=newest", asyncjob.AsyncJobListOptions{}, true},
		{PathVideos + "?model=wan2.1", asyncjob.AsyncJobListOptions{}, true},
		{PathVideos + "?limit=1&limit=2", asyncjob.AsyncJobListOptions{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, err := parseVideoListOptions(tt.path)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestVideoJobResponseNeedsBuffering fixes which responses the gateway is
// allowed to hold and mutate. Create, status, and delete can carry an id;
// /content is a video stream that must keep flowing through Envoy untouched.
func TestVideoJobResponseNeedsBuffering(t *testing.T) {
	assert.True(t, videoJobResponseNeedsBuffering(http.MethodPost, PathVideos))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodPost, PathVideos+"/"))
	assert.True(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos+"/aibrixjob-abc"))
	assert.True(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos+"/aibrixjob-abc?foo=bar"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos+"/aibrixjob-abc/"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos+"/aibrixjob-abc/content"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos+"/aibrixjob-abc/content/"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodGet, PathVideos))
	assert.True(t, videoJobResponseNeedsBuffering(http.MethodDelete, PathVideos+"/aibrixjob-abc"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodDelete, PathVideos+"/aibrixjob-abc/"))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodPost, PathVideosSync))
	assert.False(t, videoJobResponseNeedsBuffering(http.MethodPost, PathChatCompletions))
}

// readyPod builds a routable pod. The UID matters as much as the name here:
// async job records pin a pod identity, and a pod recreated under the same name
// must not be mistaken for the one that holds the job's output.
func readyPod(name, namespace, ip string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: k8stypes.UID("uid-" + namespace + "-" + name)},
		Status: v1.PodStatus{
			PodIP:      ip,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

// newTestVideoJobServer wires a Server to an in-memory-store registry so the
// Video layer is exercised against the real registry code (scope checks, UID
// verification, expiry) without a Redis dependency.
func newTestVideoJobServer(t *testing.T) (*Server, *MockCache, asyncjob.AsyncJobRegistry) {
	t.Helper()
	mockCache := new(MockCache)
	registry := asyncjob.NewRegistryForClient(nil, mockCache)
	return &Server{cache: mockCache, asyncJobs: registry}, mockCache, registry
}

func registerTestVideoJob(t *testing.T, registry asyncjob.AsyncJobRegistry, owner, model, backendJobID string, pod *v1.Pod) asyncjob.AsyncJobRecord {
	t.Helper()
	record, err := registry.Register(context.Background(), asyncjob.AsyncJobRegistration{
		JobType:      asyncjob.TypeVideo,
		Owner:        owner,
		Model:        model,
		BackendJobID: backendJobID,
		Pod:          pod,
	})
	require.NoError(t, err)
	return record
}

func videoJobRecordCount(t *testing.T, registry asyncjob.AsyncJobRegistry, owner string) int {
	t.Helper()
	records, err := listTestAsyncJobs(context.Background(), registry, owner, asyncjob.TypeVideo)
	require.NoError(t, err)
	return len(records)
}

// failingAsyncJobRegistry injects registry errors for the Video layer's HTTP mapping tests.
type failingAsyncJobRegistry struct {
	asyncjob.AsyncJobRegistry
	putErr    error
	deleteErr error
}

func (f *failingAsyncJobRegistry) Register(ctx context.Context, reg asyncjob.AsyncJobRegistration) (asyncjob.AsyncJobRecord, error) {
	if f.putErr != nil {
		return asyncjob.AsyncJobRecord{}, f.putErr
	}
	return f.AsyncJobRegistry.Register(ctx, reg)
}

func (f *failingAsyncJobRegistry) Delete(ctx context.Context, owner, publicJobID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.AsyncJobRegistry.Delete(ctx, owner, publicJobID)
}

func listTestAsyncJobs(ctx context.Context, registry asyncjob.AsyncJobRegistry, owner, jobType string) ([]asyncjob.AsyncJobRecord, error) {
	page, err := registry.List(ctx, owner, jobType, asyncjob.AsyncJobListOptions{Limit: 100, Order: "desc"})
	return page.Records, err
}

// TestHandleVideoJobSubResourceHeaders_PinsAndRewritesPath covers the whole
// point of the registry: an opaque public id sent by the client is resolved to
// the pod that owns the job, and the path is rewritten to the backend's own id
// so the engine still recognizes it. Bodyless GET/DELETE must be pinned here,
// at RequestHeaders, because ext_proc never sends a RequestBody message for a
// request with no body.
func TestHandleVideoJobSubResourceHeaders_PinsAndRewritesPath(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1-vace-1.3b", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1-vace-1.3b").Return(int64(7))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID + "?variant=mp4"
	routingCtx.ReqPath = requestPath

	resp, term := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetRequestHeaders())
	assert.Nil(t, resp.GetImmediateResponse())
	assert.EqualValues(t, 7, term)

	headersResp := resp.GetRequestHeaders().GetResponse()
	assert.True(t, headersResp.GetClearRouteCache(), "must clear route cache so Envoy re-evaluates both the rewritten path and the routing-strategy header match")

	set := headersResp.GetHeaderMutation().GetSetHeaders()
	assertHeaderRawValue(t, set, HeaderRoutingStrategy, videoJobAffinityLabel)
	assertHeaderRawValue(t, set, HeaderTargetPod, "10.0.0.5:8000")
	assertHeaderRawValue(t, set, pathKey, PathVideos+"/video_gen_abc?variant=mp4")
	assert.Equal(t, "wan2.1-vace-1.3b", routingCtx.Model)

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResource_PinsAndClearsRouteCache covers the RequestBody
// variant used when a video follow-up carries a body. The pin headers and path
// rewrite arrive after Envoy's initial route selection, so the response must
// explicitly request a rematch onto the ORIGINAL_DST route.
func TestHandleVideoJobSubResource_PinsAndClearsRouteCache(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1").Return(int64(3))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath
	reqBody := []byte(`{"reason":"cleanup"}`)

	resp, model, stream, term := s.handleVideoJobSubResource(ctx, routingCtx, "req-1", requestPath, record.PublicJobID, reqBody)

	require.NotNil(t, resp.GetRequestBody())
	assert.Nil(t, resp.GetImmediateResponse())
	assert.Equal(t, "wan2.1", model)
	assert.False(t, stream)
	assert.EqualValues(t, 3, term)

	common := resp.GetRequestBody().GetResponse()
	assert.True(t, common.GetClearRouteCache(), "bodyful follow-up must rematch after applying its pin headers and rewritten path")
	assert.Equal(t, reqBody, common.GetBodyMutation().GetBody())
	set := common.GetHeaderMutation().GetSetHeaders()
	assertHeaderRawValue(t, set, HeaderRoutingStrategy, videoJobAffinityLabel)
	assertHeaderRawValue(t, set, HeaderTargetPod, "10.0.0.5:8000")
	assertHeaderRawValue(t, set, pathKey, PathVideos+"/video_gen_abc")
	assertHeaderRawValue(t, set, "content-length", strconv.Itoa(len(reqBody)))

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_ContentPathStaysStreaming asserts a
// /content download is pinned and rewritten like any other follow-up but is not
// switched to a buffered response: the body is a video file, not JSON.
func TestHandleVideoJobSubResourceHeaders_ContentPathStaysStreaming(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1").Return(int64(1))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID + "/content"
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetRequestHeaders())
	assertHeaderRawValue(t, resp.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders(), pathKey, PathVideos+"/video_gen_abc/content")
	assert.Nil(t, resp.GetModeOverride(), "/content is routed to the streaming policy; nothing here may ask for buffering")

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_SendsNoModeOverride: the response body
// mode comes from the route's EnvoyExtensionPolicy (see
// TestGatewayPluginManifest_VideoJSONResponsesAreBuffered), not from the plugin.
// Envoy Gateway v1.2.8 leaves ext_proc's allow_mode_override off, so an override
// sent here would be ignored - and asserting one would only look like buffering
// was arranged when nothing had been.
func TestHandleVideoJobSubResourceHeaders_SendsNoModeOverride(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, "req-1", "wan2.1").Return(int64(1))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetRequestHeaders())
	assert.Nil(t, resp.GetModeOverride(), "the response body mode is configured per route, not overridden per request")

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_UnknownPublicIDReturns404 verifies an
// unknown or expired public id gets a proper 404 rather than a hang or a bare
// route-not-found from Envoy.
func TestHandleVideoJobSubResourceHeaders_UnknownPublicIDReturns404(t *testing.T) {
	s, _, _ := newTestVideoJobServer(t)
	ctx := context.Background()

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/aibrixjob-does-not-exist"
	routingCtx.ReqPath = requestPath

	resp, term := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, "aibrixjob-does-not-exist")

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.EqualValues(t, 0, term)
}

// TestHandleVideoJobSubResourceHeaders_ForeignOwnerReturns404 is the scope
// isolation guarantee: another user's public id must be indistinguishable from
// one that never existed, so a 404 cannot be used to probe for valid ids.
func TestHandleVideoJobSubResourceHeaders_ForeignOwnerReturns404(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, "scope:user:alice", "wan2.1", "video_gen_abc", pod)

	// bob, not alice.
	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "bob")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, 1, videoJobRecordCount(t, registry, "scope:user:alice"), "a foreign read must not touch the owner's record")

	mockCache.AssertNotCalled(t, "GetPod", mock.Anything, mock.Anything)
}

// TestHandleVideoJobSubResourceHeaders_SharedScopeIsNotAWildcard makes sure the
// fallback scope used by unauthenticated requests cannot read a named user's
// jobs.
func TestHandleVideoJobSubResourceHeaders_SharedScopeIsNotAWildcard(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, "scope:user:alice", "wan2.1", "video_gen_abc", pod)

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
}

// TestHandleVideoJobSubResourceHeaders_ReplacedPodUIDReturns404 covers the
// reason the record stores a pod UID at all: a pod recreated under the same
// name is a different pod with an empty disk, so the job it used to hold is
// gone and the record must go with it.
func TestHandleVideoJobSubResourceHeaders_ReplacedPodUIDReturns404(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	replacement := readyPod("pod-a", "ns-a", "10.0.0.9")
	replacement.UID = k8stypes.UID("uid-recreated")
	mockCache.On("GetPod", "pod-a", "ns-a").Return(replacement, nil)

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncjob.OwnerShared), "a record pinned to a replaced pod is dead and must be dropped")
	assert.Equal(t, "wan2.1", routingCtx.Model, "the model must be attributed even on the failure path so the fail-metric is not silently dropped")

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_MissingPodReturns404AndDeletesRecord
// covers a pod that is not in the informer cache at all: as confirmed-gone as
// this gateway can observe.
func TestHandleVideoJobSubResourceHeaders_MissingPodReturns404AndDeletesRecord(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return((*v1.Pod)(nil), errors.New("pod not found"))

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncjob.OwnerShared))
	assert.Equal(t, "wan2.1", routingCtx.Model)

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_TerminatingPodReturns404AndDeletesRecord
// covers a pod confirmed going away (DeletionTimestamp set): it will not come
// back under this name, so the outcome is terminal like the cache miss above.
func TestHandleVideoJobSubResourceHeaders_TerminatingPodReturns404AndDeletesRecord(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	terminating := readyPod("pod-a", "ns-a", "10.0.0.5")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	mockCache.On("GetPod", "pod-a", "ns-a").Return(terminating, nil)

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncjob.OwnerShared))

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_NotReadyPodReturns503AndKeepsRecord
// covers a transiently unavailable pod (readiness flap, restart, startup probe
// still pending). The generated video lives on that pod's disk, so the record
// must survive for a retry instead of collapsing into a permanent-looking 404.
func TestHandleVideoJobSubResourceHeaders_NotReadyPodReturns503AndKeepsRecord(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1-vace-1.3b", "video_gen_abc", pod)

	notReady := readyPod("pod-a", "ns-a", "10.0.0.5")
	notReady.Status.Conditions = nil
	mockCache.On("GetPod", "pod-a", "ns-a").Return(notReady, nil)

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, term := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.EqualValues(t, 0, term)
	assert.Equal(t, 1, videoJobRecordCount(t, registry, asyncjob.OwnerShared), "record must survive a transient NotReady pod so a retry can still resolve it")
	assert.Equal(t, "wan2.1-vace-1.3b", routingCtx.Model)

	mockCache.AssertExpectations(t)
}

// TestHandleVideoJobSubResourceHeaders_StoreUnavailableReturns503 asserts an
// exhausted-retry Redis failure surfaces as a retryable 503, not as a 404 that
// would tell the client its job is gone.
func TestHandleVideoJobSubResourceHeaders_StoreUnavailableReturns503(t *testing.T) {
	mockCache := new(MockCache)
	store := &failingAsyncJobRegistry{AsyncJobRegistry: asyncjob.NewRegistryForClient(nil, mockCache)}
	s := &Server{cache: mockCache, asyncJobs: store}
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, s.asyncJobs, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)
	store.AsyncJobRegistry = storeUnavailableAsyncJobRegistry{}

	routingCtx := types.NewRoutingContext(ctx, "", "", "", "req-1", "")
	routingCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}
	requestPath := PathVideos + "/" + record.PublicJobID
	routingCtx.ReqPath = requestPath

	resp, _ := s.handleVideoJobSubResourceHeaders(ctx, routingCtx, "req-1", requestPath, record.PublicJobID)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
}

// storeUnavailableAsyncJobRegistry reports an exhausted transient store failure.
type storeUnavailableAsyncJobRegistry struct{}

func (storeUnavailableAsyncJobRegistry) Register(context.Context, asyncjob.AsyncJobRegistration) (asyncjob.AsyncJobRecord, error) {
	return asyncjob.AsyncJobRecord{}, fmt.Errorf("%w: put", asyncjob.ErrStoreUnavailable)
}

func (storeUnavailableAsyncJobRegistry) Get(context.Context, string, string) (asyncjob.AsyncJobRecord, *v1.Pod, error) {
	return asyncjob.AsyncJobRecord{}, nil, fmt.Errorf("%w: get", asyncjob.ErrStoreUnavailable)
}

func (storeUnavailableAsyncJobRegistry) List(context.Context, string, string, asyncjob.AsyncJobListOptions) (asyncjob.AsyncJobListPage, error) {
	return asyncjob.AsyncJobListPage{}, fmt.Errorf("%w: list", asyncjob.ErrStoreUnavailable)
}

func (storeUnavailableAsyncJobRegistry) Delete(context.Context, string, string) error {
	return fmt.Errorf("%w: delete", asyncjob.ErrStoreUnavailable)
}

// TestHandleRequestHeaders_VideoStatusPoll_Bodyless drives the bodyless pin
// through the real entry point: EndOfStream=true is Envoy's signal that no body
// follows, so HandleRequestBody will never run for this request.
func TestHandleRequestHeaders_VideoStatusPoll_Bodyless(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1-vace-1.3b", "video_gen_abc", pod)

	mockCache.On("GetPod", "pod-a", "ns-a").Return(pod, nil)
	mockCache.On("AddRequestCount", mock.Anything, mock.Anything, "wan2.1-vace-1.3b").Return(int64(1))

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos + "/" + record.PublicJobID)},
					{Key: methodKey, RawValue: []byte(http.MethodGet)},
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
	assertHeaderRawValue(t, set, pathKey, PathVideos+"/video_gen_abc")
	assert.Equal(t, "wan2.1-vace-1.3b", routingCtx.Model)

	mockCache.AssertExpectations(t)
}

// TestHandleRequestHeaders_VideoStatusPoll_WithBody ensures the EndOfStream-gated
// header-phase pin does not fire when a body IS coming: that case belongs to
// HandleRequestBody.
func TestHandleRequestHeaders_VideoStatusPoll_WithBody(t *testing.T) {
	s, mockCache, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", pod)

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos + "/" + record.PublicJobID)},
					{Key: methodKey, RawValue: []byte(http.MethodDelete)},
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
		assert.NotEqual(t, pathKey, h.GetHeader().GetKey())
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

// TestHandleVideoListHeaders_ReturnsOwnerScopedPublicCatalog replaces the old
// per-model HTTP fan-out: the catalog now comes from the registry, so it is
// scoped to the caller, contains only public ids, and never exposes where the
// job is running.
func TestHandleVideoListHeaders_ReturnsOwnerScopedPublicCatalog(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	mine := registerTestVideoJob(t, registry, "scope:user:alice", "wan2.1", "video_gen_mine", readyPod("pod-a", "ns-a", "10.0.0.5"))
	theirs := registerTestVideoJob(t, registry, "scope:user:bob", "wan2.1", "video_gen_theirs", readyPod("pod-b", "ns-a", "10.0.0.6"))

	resp := s.handleVideoListHeaders(ctx, "req-1", "scope:user:alice", asyncjob.AsyncJobListOptions{Limit: asyncjob.DefaultListLimit, Order: "desc"})

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_OK, resp.GetImmediateResponse().GetStatus().GetCode())

	body := resp.GetImmediateResponse().GetBody()
	assert.Equal(t, "list", gjson.Get(body, "object").String())
	data := gjson.Get(body, "data").Array()
	require.Len(t, data, 1, "the catalog is scoped to its owner")
	assert.Equal(t, mine.PublicJobID, data[0].Get("id").String())
	assert.Equal(t, "wan2.1", data[0].Get("model").String())
	assert.Equal(t, mine.PublicJobID, gjson.Get(body, "first_id").String())
	assert.Equal(t, mine.PublicJobID, gjson.Get(body, "last_id").String())
	assert.False(t, gjson.Get(body, "has_more").Bool())

	assert.NotContains(t, body, "video_gen_mine", "backend job ids must never reach the client")
	assert.NotContains(t, body, theirs.PublicJobID)
	assert.NotContains(t, body, "pod-a", "routing-target details must never reach the client")
	assert.NotContains(t, body, "ns-a")
}

// TestHandleVideoListHeaders_StoreUnavailableReturns503 asserts the catalog
// reports a Redis outage as retryable rather than as an empty list, which a
// client would read as "all my jobs are gone".
func TestHandleVideoListHeaders_StoreUnavailableReturns503(t *testing.T) {
	mockCache := new(MockCache)
	s := &Server{cache: mockCache, asyncJobs: storeUnavailableAsyncJobRegistry{}}

	resp := s.handleVideoListHeaders(context.Background(), "req-1", asyncjob.OwnerShared, asyncjob.AsyncJobListOptions{Limit: asyncjob.DefaultListLimit, Order: "desc"})

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
}

// TestHandleRequestHeaders_VideoList_NoModelRequired covers the removal of the
// model query requirement: the registry knows the caller's jobs across models,
// so a bare GET /v1/videos is answered directly.
func TestHandleRequestHeaders_VideoList_NoModelRequired(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))

	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos)},
					{Key: methodKey, RawValue: []byte(http.MethodGet)},
				}},
				EndOfStream: true,
			},
		},
	}

	resp, _, _, _, _ := s.HandleRequestHeaders(ctx, "req-1", trace.SpanFromContext(ctx), req)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_OK, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.Contains(t, resp.GetImmediateResponse().GetBody(), record.PublicJobID)
}

func TestHandleRequestHeaders_VideoListValidationDoesNotExposeInternalError(t *testing.T) {
	s, _, _ := newTestVideoJobServer(t)
	ctx := context.Background()
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos + "?limit=0")},
					{Key: methodKey, RawValue: []byte(http.MethodGet)},
				}},
				EndOfStream: true,
			},
		},
	}

	resp, _, _, _, _ := s.HandleRequestHeaders(ctx, "req-invalid-video-list", trace.SpanFromContext(ctx), req)

	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_BadRequest, resp.GetImmediateResponse().GetStatus().GetCode())
	body := resp.GetImmediateResponse().GetBody()
	assert.Contains(t, body, "list limit must be between 1 and 100")
	assert.NotContains(t, body, asyncjob.ErrInvalidRecord.Error())
}

func TestHandleRequestHeaders_RejectsVideoTrailingSlash(t *testing.T) {
	s, _, _ := newTestVideoJobServer(t)
	ctx := context.Background()
	req := &extProcPb.ProcessingRequest{
		Request: &extProcPb.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extProcPb.HttpHeaders{
				Headers: &configPb.HeaderMap{Headers: []*configPb.HeaderValue{
					{Key: pathKey, RawValue: []byte(PathVideos + "/")},
					{Key: methodKey, RawValue: []byte(http.MethodPost)},
				}},
			},
		},
	}

	resp, _, _, _, _ := s.HandleRequestHeaders(ctx, "req-video-trailing-slash", trace.SpanFromContext(ctx), req)
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_NotFound, resp.GetImmediateResponse().GetStatus().GetCode())
}

// TestHandleVideoJobResponseBody_RegistersCreateAndRewritesID is the create
// contract: the backend id is recorded against the pod that produced it and
// replaced in the response by the opaque public id, so the client never learns
// the backend id in the first place.
func TestHandleVideoJobResponseBody_RegistersCreateAndRewritesID(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()
	requestID := "req-create-1"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "alice")
	routerCtx.ReqPath = PathVideos
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodPost}
	routerCtx.SetTargetPod(pod)

	full := `{"id":"video_gen_abc","status":"queued","model":"wan2.1"}`
	mid := len(full) / 2

	resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(full[:mid])},
	})
	require.True(t, handled)
	assert.False(t, complete)
	assert.Empty(t, resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody(), "a partial chunk must be held back, not forwarded with a backend id in it")
	assert.Equal(t, 0, videoJobRecordCount(t, registry, "scope:user:alice"), "nothing is registered until the whole body has arrived")

	resp, complete, handled = s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(full[mid:]), EndOfStream: true},
	})
	require.True(t, handled)
	assert.True(t, complete)

	out := string(resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody())
	assert.NotContains(t, out, "video_gen_abc", "the backend id must not survive into the client's response")
	publicID := gjson.Get(out, "id").String()
	assert.True(t, strings.HasPrefix(publicID, "aibrixjob-"), "public ids are minted by aibrix, not taken from the backend: %q", publicID)
	assert.Equal(t, "queued", gjson.Get(out, "status").String(), "the rest of the create response must pass through unchanged")

	records, err := listTestAsyncJobs(ctx, registry, "scope:user:alice", asyncjob.TypeVideo)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, publicID, records[0].PublicJobID)
	assert.Equal(t, "video_gen_abc", records[0].BackendJobID)
	assert.Equal(t, "pod", records[0].RoutingTarget.Kind)
	assert.Equal(t, "pod-a", records[0].RoutingTarget.Pod.Name)
	assert.Equal(t, "ns-a", records[0].RoutingTarget.Pod.Namespace)
	assert.EqualValues(t, pod.UID, records[0].RoutingTarget.Pod.UID)
	assert.False(t, HasRequestBuffers(requestID), "the buffer must be released once the body is complete")
}

// TestHandleVideoJobResponseBody_AppliesBackendExpiry checks the backend's own
// expires_at wins over the default TTL, so aibrix never advertises a job for
// longer than the pod will keep it.
func TestHandleVideoJobResponseBody_AppliesBackendExpiry(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()
	requestID := "req-create-expiry"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	pod := readyPod("pod-a", "ns-a", "10.0.0.5")
	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodPost}
	routerCtx.SetTargetPod(pod)

	expires := time.Now().Add(2 * time.Hour).Unix()
	body := fmt.Sprintf(`{"id":"video_gen_abc","expires_at":%d}`, expires)

	_, _, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(body), EndOfStream: true},
	})
	require.True(t, handled)

	records, err := listTestAsyncJobs(ctx, registry, asyncjob.OwnerShared, asyncjob.TypeVideo)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, expires, records[0].ExpiresAt.Unix())
}

// TestHandleVideoJobResponseBody_RejectsElapsedBackendExpiry makes the
// create-response bridge preserve a supplied expiry even when it is invalid.
// Zero is an explicit Unix epoch, not the registry's absent-expiry sentinel.
func TestHandleVideoJobResponseBody_RejectsElapsedBackendExpiry(t *testing.T) {
	for _, expires := range []int64{0, time.Now().Add(-time.Hour).Unix()} {
		t.Run(fmt.Sprintf("expires_at_%d", expires), func(t *testing.T) {
			s, _, registry := newTestVideoJobServer(t)
			ctx := context.Background()
			requestID := fmt.Sprintf("req-create-expired-%d", expires)
			t.Cleanup(func() { requestBuffers.Delete(requestID) })

			routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
			routerCtx.ReqPath = PathVideos
			routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodPost}
			routerCtx.SetTargetPod(readyPod("pod-a", "ns-a", "10.0.0.5"))

			body := fmt.Sprintf(`{"id":"video_gen_abc","expires_at":%d}`, expires)
			resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
				ResponseBody: &extProcPb.HttpBody{Body: []byte(body), EndOfStream: true},
			})

			require.True(t, handled)
			assert.True(t, complete)
			require.NotNil(t, resp.GetImmediateResponse())
			assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
			assert.NotContains(t, resp.GetImmediateResponse().GetBody(), "video_gen_abc")
			assert.Empty(t, videoJobRecordCount(t, registry, asyncjob.OwnerShared), "an elapsed backend expiry must not fall back to the default TTL")
		})
	}
}

// TestHandleVideoJobResponseBody_ForwardsBodyWithoutIDUntouched keeps error and
// non-JSON create responses intact: there is nothing to register and nothing to
// rewrite, and corrupting them would hide the backend's own error from the client.
func TestHandleVideoJobResponseBody_ForwardsBodyWithoutIDUntouched(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()
	requestID := "req-create-err"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodPost}
	routerCtx.SetTargetPod(readyPod("pod-a", "ns-a", "10.0.0.5"))

	body := `{"error":{"message":"prompt too long","type":"invalid_request_error"}}`
	resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(body), EndOfStream: true},
	})

	require.True(t, handled)
	assert.True(t, complete)
	assert.Nil(t, resp.GetImmediateResponse())
	assert.Equal(t, body, string(resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody()))
	assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncjob.OwnerShared))
}

func TestRewriteVideoJobErrorBody_CreateFailureWithoutPublicIDIsUnchanged(t *testing.T) {
	routerCtx := types.NewRoutingContext(context.Background(), "", "wan2.1", "", "req-create-error", "")
	routerCtx.ReqPath = PathVideos
	// Even if a future caller records the backend ID early, the create path has
	// no public ID that can safely replace it.
	routerCtx.AsyncJobBackendID = "video_gen_abc"
	body := []byte(`{"error":{"message":"Video video_gen_abc failed"}}`)

	assert.Equal(t, body, rewriteVideoJobErrorBody(routerCtx, body))
}

func TestRewriteVideoJobErrorBody_RewritesOnlyKnownJSONFields(t *testing.T) {
	routerCtx := types.NewRoutingContext(context.Background(), "", "wan2.1", "", "req-status-error", "")
	routerCtx.ReqPath = PathVideos + "/aibrixjob-public"
	routerCtx.AsyncJobBackendID = "video_gen_abc"
	body := []byte(`{
		"id":"video_gen_abc",
		"error":{
			"message":"Video video_gen_abc was not found",
			"param":"video_gen_abc",
			"details":{"id":"video_gen_abc"}
		},
		"trace_id":"trace-video_gen_abc",
		"extension":"video_gen_abc"
	}`)

	rewritten := rewriteVideoJobErrorBody(routerCtx, body)
	assert.Equal(t, "aibrixjob-public", gjson.GetBytes(rewritten, "id").String())
	assert.Equal(t, "Video aibrixjob-public was not found", gjson.GetBytes(rewritten, "error.message").String())
	assert.Equal(t, "aibrixjob-public", gjson.GetBytes(rewritten, "error.param").String())
	assert.Equal(t, "video_gen_abc", gjson.GetBytes(rewritten, "error.details.id").String())
	assert.Equal(t, "trace-video_gen_abc", gjson.GetBytes(rewritten, "trace_id").String())
	assert.Equal(t, "video_gen_abc", gjson.GetBytes(rewritten, "extension").String())

	plainText := []byte("Video video_gen_abc was not found")
	assert.Equal(t, plainText, rewriteVideoJobErrorBody(routerCtx, plainText))
}

// TestHandleVideoJobResponseBody_RegistrationFailureReturns503 covers the rule
// that there is no create success without a durable record: if the registry
// write cannot complete, the client gets a retryable 503 and -- crucially --
// not the backend id it would otherwise have to keep polling with.
func TestHandleVideoJobResponseBody_RegistrationFailureReturns503(t *testing.T) {
	mockCache := new(MockCache)
	store := &failingAsyncJobRegistry{
		AsyncJobRegistry: asyncjob.NewRegistryForClient(nil, mockCache),
		putErr:           fmt.Errorf("%w: register", asyncjob.ErrStoreUnavailable),
	}
	s := &Server{cache: mockCache, asyncJobs: store}
	ctx := context.Background()
	requestID := "req-create-fail"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodPost}
	routerCtx.SetTargetPod(readyPod("pod-a", "ns-a", "10.0.0.5"))

	resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(`{"id":"video_gen_abc","status":"queued"}`), EndOfStream: true},
	})

	require.True(t, handled)
	assert.True(t, complete)
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
	assert.NotContains(t, resp.GetImmediateResponse().GetBody(), "video_gen_abc", "a failed registration must not leak the backend id the client could not otherwise use")
	assert.False(t, HasRequestBuffers(requestID))
}

// TestHandleVideoJobResponseBody_StatusRewritesIDBackToPublic closes the loop:
// the status body the backend produced names its own id, which has to become the
// public id again before the client sees it.
func TestHandleVideoJobResponseBody_StatusRewritesIDBackToPublic(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()
	requestID := "req-status-1"
	t.Cleanup(func() { requestBuffers.Delete(requestID) })

	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos + "/" + record.PublicJobID
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}

	resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte(`{"id":"video_gen_abc","status":"completed","progress":100}`), EndOfStream: true},
	})

	require.True(t, handled)
	assert.True(t, complete)
	out := string(resp.GetResponseBody().GetResponse().GetBodyMutation().GetBody())
	assert.Equal(t, record.PublicJobID, gjson.Get(out, "id").String())
	assert.NotContains(t, out, "video_gen_abc")
	assert.Equal(t, "completed", gjson.Get(out, "status").String())
	assert.EqualValues(t, 100, gjson.Get(out, "progress").Int())
}

func TestUnsupportedVideoTrailingSlash(t *testing.T) {
	for _, path := range []string{
		PathVideos + "/",
		PathVideos + "/aibrixjob-public/",
		PathVideos + "/aibrixjob-public/content/",
	} {
		assert.True(t, isUnsupportedVideoTrailingSlash(path), path)
	}
	assert.False(t, isUnsupportedVideoTrailingSlash(PathVideos))
	assert.False(t, isUnsupportedVideoTrailingSlash(PathVideos+"/aibrixjob-public/content"))
}

// TestHandleVideoJobResponseBody_ContentIsNotBuffered guarantees a video
// download is never held in gateway memory or rewritten: it keeps streaming
// through Envoy exactly as it arrived.
func TestHandleVideoJobResponseBody_ContentIsNotBuffered(t *testing.T) {
	s, _, _ := newTestVideoJobServer(t)
	ctx := context.Background()
	requestID := "req-content-1"

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", requestID, "")
	routerCtx.ReqPath = PathVideos + "/aibrixjob-abc/content"
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}

	resp, complete, handled := s.handleVideoJobResponseBody(ctx, requestID, routerCtx, &extProcPb.ProcessingRequest_ResponseBody{
		ResponseBody: &extProcPb.HttpBody{Body: []byte{0x00, 0x01, 0x02}},
	})

	assert.False(t, handled, "binary content must not enter the buffer-and-mutate path")
	assert.Nil(t, resp)
	assert.False(t, complete)
	assert.False(t, HasRequestBuffers(requestID))
}

// TestMaybeDeleteVideoJobAfterDelete_DeletesOnBackendSuccessAndNotFound checks
// registry cleanup follows the backend: 2xx means it is gone, 404 means it was
// already gone, and both make the record garbage.
func TestMaybeDeleteVideoJobAfterDelete_DeletesOnBackendSuccessAndNotFound(t *testing.T) {
	for _, statusCode := range []int{http.StatusOK, http.StatusNoContent, http.StatusNotFound} {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			s, _, registry := newTestVideoJobServer(t)
			ctx := context.Background()

			record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))

			routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", "req-1", "")
			routerCtx.ReqPath = PathVideos + "/" + record.PublicJobID
			routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}

			resp := s.maybeDeleteVideoJobAfterDelete(ctx, routerCtx, statusCode)

			assert.Nil(t, resp)
			assert.Equal(t, 0, videoJobRecordCount(t, registry, asyncjob.OwnerShared))
		})
	}
}

// TestMaybeDeleteVideoJobAfterDelete_KeepsRecordOnBackendFailure leaves the
// record in place when the backend could not delete: the job may still exist on
// that pod, and the client needs the same sticky route to try again.
func TestMaybeDeleteVideoJobAfterDelete_KeepsRecordOnBackendFailure(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", "req-1", "")
	routerCtx.ReqPath = PathVideos + "/" + record.PublicJobID
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}

	resp := s.maybeDeleteVideoJobAfterDelete(ctx, routerCtx, http.StatusInternalServerError)

	assert.Nil(t, resp)
	assert.Equal(t, 1, videoJobRecordCount(t, registry, asyncjob.OwnerShared))
}

// TestMaybeDeleteVideoJobAfterDelete_IgnoresNonDeleteMethods makes sure a status
// poll that happens to return 404 cannot delete a live record.
func TestMaybeDeleteVideoJobAfterDelete_IgnoresNonDeleteMethods(t *testing.T) {
	s, _, registry := newTestVideoJobServer(t)
	ctx := context.Background()

	record := registerTestVideoJob(t, registry, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", "req-1", "")
	routerCtx.ReqPath = PathVideos + "/" + record.PublicJobID
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodGet}

	assert.Nil(t, s.maybeDeleteVideoJobAfterDelete(ctx, routerCtx, http.StatusNotFound))
	assert.Equal(t, 1, videoJobRecordCount(t, registry, asyncjob.OwnerShared))
}

// TestMaybeDeleteVideoJobAfterDelete_Returns503WhenCleanupFails covers the one
// case where the gateway overrides a successful backend delete: the record
// outlived its job, so the client is told to retry and finish the cleanup rather
// than being handed a 200 over a stale entry.
func TestMaybeDeleteVideoJobAfterDelete_Returns503WhenCleanupFails(t *testing.T) {
	mockCache := new(MockCache)
	store := &failingAsyncJobRegistry{AsyncJobRegistry: asyncjob.NewRegistryForClient(nil, mockCache)}
	s := &Server{cache: mockCache, asyncJobs: store}
	ctx := context.Background()

	record := registerTestVideoJob(t, s.asyncJobs, asyncjob.OwnerShared, "wan2.1", "video_gen_abc", readyPod("pod-a", "ns-a", "10.0.0.5"))
	store.deleteErr = fmt.Errorf("%w: delete", asyncjob.ErrStoreUnavailable)

	routerCtx := types.NewRoutingContext(ctx, "", "wan2.1", "", "req-1", "")
	routerCtx.ReqPath = PathVideos + "/" + record.PublicJobID
	routerCtx.ReqHeaders = map[string]string{methodKey: http.MethodDelete}

	resp := s.maybeDeleteVideoJobAfterDelete(ctx, routerCtx, http.StatusNoContent)

	require.NotNil(t, resp)
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_ServiceUnavailable, resp.GetImmediateResponse().GetStatus().GetCode())
}

func TestIsMultipartFormPath(t *testing.T) {
	assert.True(t, isMultipartFormPath(PathVideos))
	assert.True(t, isMultipartFormPath(PathVideos+"?foo=bar"))
	assert.True(t, isMultipartFormPath(PathVideosSync))
	assert.True(t, isMultipartFormPath(PathAudioTranscriptions))
	assert.True(t, isMultipartFormPath(PathImagesEdits))
	assert.True(t, isMultipartFormPath(PathImagesEdits+"?foo=bar"))
	assert.False(t, isMultipartFormPath(PathImagesGenerations), "image generations take a JSON body")
	assert.False(t, isMultipartFormPath(PathVideos+"/video-1"))
	assert.False(t, isMultipartFormPath(PathChatCompletions))
}
