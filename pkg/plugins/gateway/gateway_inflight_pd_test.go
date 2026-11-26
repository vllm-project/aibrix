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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/alicebob/miniredis/v2"
	envoyTypePb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	pdrouter "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/router"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/inflight"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func makePDInflightTestPod(name, roleset, role, host, port string, extraLabels map[string]string) *v1.Pod {
	labels := map[string]string{
		pdrouter.LLMEngineIdentifier: pdrouter.VLLMEngine,
		pd.RoleSetLabel:              roleset,
		pd.RoleLabel:                 role,
		constants.ModelLabelPort:     port,
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    labels,
		},
		Status: v1.PodStatus{
			PodIP:      host,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		},
	}
}

func newPDInflightPrefillServer(t *testing.T) (host, port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Hostname(), u.Port()
}

// TestSelectTargetPod_PDInflightAllDecodeSaturatedReturns429 verifies the core
// #3619 regression end-to-end against miniredis and the real pdRouter:
// once every routable decode pod hits requestsInflight, the next request is
// rejected with errReplicaInflightExceeded (HTTP 429) and records an inflight
// rejection rather than leaking prefill/encode/non-rank-0 workers into pdRouter
// for a spurious 503. It also verifies that Redis aibrix:rrq:* HGETALLs are
// issued only for routable decode pods.
func TestSelectTargetPod_PDInflightAllDecodeSaturatedReturns429(t *testing.T) {
	mr := miniredis.RunT(t)
	reads := &fleetReadCounter{}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(context.Background()).Err())

	host, port := newPDInflightPrefillServer(t)
	prefillPod := makePDInflightTestPod("prefill-0", "rs-a", pd.RolePrefill, host, port, nil)
	decodePod := makePDInflightTestPod("decode-0", "rs-a", pd.RoleDecode, host, port, map[string]string{
		pd.PodGroupIndexLabel: "0",
	})
	decodeWorkerPod := makePDInflightTestPod("decode-0-worker-1", "rs-a", pd.RoleDecode, host, port, map[string]string{
		pd.PodGroupIndexLabel: "1",
	})
	encodePod := makePDInflightTestPod("encode-0", "rs-a", encode.Role, host, port, nil)
	pods := []*v1.Pod{prefillPod, decodePod, decodeWorkerPod, encodePod}

	const model = "pd-inflight-429-model"
	store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
	t.Cleanup(store.Close)
	rm := routing.NewRouterManagerWithCache(store)
	rm.Init()
	s := &Server{cache: store, routerManager: rm}

	newCtx := func(reqID string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello world", reqID, "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"pd-inflight-429-model","messages":[{"role":"user","content":"hello world"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{RequestsInflight: 1}
		return ctx
	}

	// Request 1: decode-0 is admissible; Redis rrq HGETALL must query only decode-0 (1 pod, not 4).
	rrqBefore := reads.rrqHGetAlls.Load()
	ctx1 := newCtx("req-pd-1")
	addr, err := s.selectTargetPod(context.Background(), ctx1, &utils.PodArray{Pods: pods}, "")
	require.NoError(t, err)
	require.NotEmpty(t, addr)
	require.Equal(t, decodePod.Name, ctx1.TargetPod().Name)
	assert.EqualValues(t, 1, reads.rrqHGetAlls.Load()-rrqBefore,
		"inflight filter must read aibrix:rrq:* only for the 1 routable decode pod, skipping prefill/encode/rank-1 worker")

	// Simulate HandleRequestBody recording the active request on decode-0.
	store.AddRequestCount(ctx1, ctx1.RequestID, model)

	// Request 2: decode-0 is now at cap (1/1). Prefill, encode, and rank-1 TP worker
	// remain at 0, but must not keep the request from returning HTTP 429.
	beforeRejections := inflightRejectionCount(t, model, inflight.InflightScopeFleet)
	rrqBefore = reads.rrqHGetAlls.Load()
	ctx2 := newCtx("req-pd-2")
	_, err = s.selectTargetPod(context.Background(), ctx2, &utils.PodArray{Pods: pods}, "")
	require.ErrorIs(t, err, errReplicaInflightExceeded, "all routable decode pods saturated must return errReplicaInflightExceeded, not a downstream PD not-ready error")
	assert.EqualValues(t, 1, reads.rrqHGetAlls.Load()-rrqBefore,
		"saturated PD rejection must still query aibrix:rrq:* only for the 1 routable decode pod")
	assert.Equal(t, beforeRejections+1, inflightRejectionCount(t, model, inflight.InflightScopeFleet),
		"saturated PD rejection must increment the fleet inflight rejection counter")

	resp := buildTargetSelectionErrorResponse(ctx2, ctx2.RequestID, model, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.GetImmediateResponse())
	assert.Equal(t, envoyTypePb.StatusCode_TooManyRequests, resp.GetImmediateResponse().GetStatus().GetCode(),
		"saturated PD request must map to HTTP 429 Too Many Requests, not 503")
}

// TestSelectTargetPod_PDInflightPreservesHelpersAndDropsOrphanDecode verifies
// two boundary invariants of partitionPDInflightTargets:
//  1. When a routable decode pod has capacity, prefill, encode, and non-rank-0 TP
//     workers are reattached and reach the PD router intact.
//  2. An idle orphan decode pod in a roleset without any prefill pod is excluded
//     from inflight target checks so it neither triggers a Redis read nor masks
//     the 429 when all prefill-backed decode pods are saturated.
func TestSelectTargetPod_PDInflightPreservesHelpersAndDropsOrphanDecode(t *testing.T) {
	t.Run("reattaches non-target helpers when decode has capacity", func(t *testing.T) {
		prefill := makePDInflightTestPod("prefill-0", "rs-a", pd.RolePrefill, "10.0.0.1", "8000", nil)
		decode := makePDInflightTestPod("decode-0", "rs-a", pd.RoleDecode, "10.0.0.2", "8000", map[string]string{
			pd.PodGroupIndexLabel: "0",
		})
		worker := makePDInflightTestPod("decode-0-w1", "rs-a", pd.RoleDecode, "10.0.0.3", "8000", map[string]string{
			pd.PodGroupIndexLabel: "1",
		})
		enc := makePDInflightTestPod("encode-0", "rs-a", encode.Role, "10.0.0.4", "8000", nil)
		pods := []*v1.Pod{prefill, decode, worker, enc}

		var queried []*v1.Pod
		mockCache := &MockCache{}
		mockCache.On("GetPodsRunningRequests", mock.Anything).Run(func(args mock.Arguments) {
			queried = append([]*v1.Pod(nil), args.Get(0).([]*v1.Pod)...)
		}).Return(map[string]int64{"ns/decode-0": 0}, nil)

		var routedNames []string
		rm := routing.NewRouterManager()
		rm.RegisterProvider(routing.RouterPD, func(*types.RoutingContext) (types.Router, error) {
			return routeFunc(func(ctx *types.RoutingContext, list types.PodList) (string, error) {
				for _, p := range list.All() {
					routedNames = append(routedNames, p.Name)
				}
				ctx.SetTargetPod(decode)
				return ctx.TargetAddress(), nil
			}), nil
		})
		s := &Server{cache: mockCache, routerManager: rm}

		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m-epd", "hi", "req-epd", "")
		t.Cleanup(ctx.Delete)
		ctx.ConfigProfile = inflightProfile(2, 0)

		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		require.Len(t, queried, 1)
		assert.Equal(t, "decode-0", queried[0].Name, "only routable decode pod is queried")
		assert.ElementsMatch(t, []string{"prefill-0", "decode-0", "decode-0-w1", "encode-0"}, routedNames,
			"prefill, encode, and TP worker helpers must be reattached before entering PD router")
	})

	t.Run("idle orphan decode pod does not mask 429 on saturated complete roleset", func(t *testing.T) {
		prefillA := makePDInflightTestPod("prefill-a", "rs-a", pd.RolePrefill, "10.0.0.1", "8000", nil)
		decodeA := makePDInflightTestPod("decode-a", "rs-a", pd.RoleDecode, "10.0.0.2", "8000", nil)
		orphanDecodeB := makePDInflightTestPod("decode-b", "rs-b", pd.RoleDecode, "10.0.0.3", "8000", nil)
		pods := []*v1.Pod{prefillA, decodeA, orphanDecodeB}

		var queried []*v1.Pod
		mockCache := &MockCache{}
		mockCache.On("GetPodsRunningRequests", mock.Anything).Run(func(args mock.Arguments) {
			queried = append([]*v1.Pod(nil), args.Get(0).([]*v1.Pod)...)
		}).Return(map[string]int64{"ns/decode-a": 1, "ns/decode-b": 0}, nil)

		rm := routing.NewRouterManager()
		rm.RegisterProvider(routing.RouterPD, func(*types.RoutingContext) (types.Router, error) {
			return routeFunc(func(*types.RoutingContext, types.PodList) (string, error) {
				t.Error("PD router must not run when every prefill-backed decode pod is saturated")
				return "", errors.New("unexpected route call")
			}), nil
		})
		s := &Server{cache: mockCache, routerManager: rm}

		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, "m-orphan", "hi", "req-orphan", "")
		t.Cleanup(ctx.Delete)
		ctx.ConfigProfile = inflightProfile(1, 0)

		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.ErrorIs(t, err, errReplicaInflightExceeded)
		require.Len(t, queried, 1)
		assert.Equal(t, "decode-a", queried[0].Name, "orphan decode pod without prefill peer must not be queried")
	})
}

// TestSelectTargetPod_PDInflightNotReadyFleetPreservesNotReadyError ensures
// that an unready PD fleet (no decode pods or no prefill pods) does not return
// a false HTTP 429 or issue wasted Redis reads, and instead reaches pdRouter to
// report its genuine not-ready error (HTTP 503).
func TestSelectTargetPod_PDInflightNotReadyFleetPreservesNotReadyError(t *testing.T) {
	host, port := newPDInflightPrefillServer(t)
	cases := []struct {
		name       string
		pods       []*v1.Pod
		wantErrSub string
	}{
		{
			name:       "prefill only (no decode pods)",
			pods:       []*v1.Pod{makePDInflightTestPod("prefill-0", "rs-a", pd.RolePrefill, host, port, nil)},
			wantErrSub: "prefill pods are not ready",
		},
		{
			name:       "decode only (no prefill pods)",
			pods:       []*v1.Pod{makePDInflightTestPod("decode-0", "rs-a", pd.RoleDecode, host, port, nil)},
			wantErrSub: "prefill pods are not ready",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := "m-not-ready-" + tc.name
			mr := miniredis.RunT(t)
			reads := &fleetReadCounter{}
			client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
			t.Cleanup(func() { _ = client.Close() })
			require.NoError(t, client.Ping(context.Background()).Err())

			store := cache.InitWithPods(cache.New(client, nil, nil), tc.pods, model)
			t.Cleanup(store.Close)
			rm := routing.NewRouterManagerWithCache(store)
			rm.Init()
			s := &Server{cache: store, routerManager: rm}

			ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello", "req-not-ready", "user")
			t.Cleanup(ctx.Delete)
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
			ctx.ConfigProfile = inflightProfile(1, 0)

			beforeRejections := inflightRejectionCount(t, model, inflight.InflightScopeFleet)
			rrqBefore := reads.rrqHGetAlls.Load()

			_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: tc.pods}, "")
			require.Error(t, err)
			assert.False(t, errors.Is(err, errReplicaInflightExceeded), "unready PD fleet must not report 429 overload")
			assert.Contains(t, err.Error(), tc.wantErrSub)
			assert.Nil(t, buildTargetSelectionErrorResponse(ctx, ctx.RequestID, model, err),
				"not-ready error should remain unmapped so HandleRequestBody returns HTTP 503")
			assert.EqualValues(t, 0, reads.rrqHGetAlls.Load()-rrqBefore, "no inflight Redis reads when no admissible PD target exists")
			assert.Equal(t, beforeRejections, inflightRejectionCount(t, model, inflight.InflightScopeFleet),
				"not-ready PD fleet must not increment inflight rejection metrics")
		})
	}
}

// TestSelectTargetPod_PDInflightCombinedSpillover verifies PromptLengthBucketing
// with combined pods:
//  1. When all decode pods are saturated but a combined pod still has capacity,
//     the combined pod is kept as an inflight target and serves the request.
//  2. When both decode pods and combined pods are saturated, the request returns
//     errReplicaInflightExceeded (HTTP 429).
func TestSelectTargetPod_PDInflightCombinedSpillover(t *testing.T) {
	host, port := newPDInflightPrefillServer(t)

	pdBucketRaw := json.RawMessage(`{"promptLengthBucketing":true,"promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096}`)
	pdBucketAnno := `{"defaultProfile":"pd-bucket","profiles":{"pd-bucket":{"routingConfig":{"promptLengthBucketing":true,"promptLenBucketMinLength":0,"promptLenBucketMaxLength":4096}}}}`
	combinedAnno := `{"defaultProfile":"combined-profile","profiles":{"combined-profile":{"routingConfig":{"promptLengthBucketing":true,"combined":true}}}}`

	prefillPod := makePDInflightTestPod("prefill-0", "rs-a", pd.RolePrefill, host, port, nil)
	prefillPod.Annotations = map[string]string{constants.ModelAnnoConfig: pdBucketAnno}
	decodePod := makePDInflightTestPod("decode-0", "rs-a", pd.RoleDecode, host, port, nil)
	decodePod.Annotations = map[string]string{constants.ModelAnnoConfig: pdBucketAnno}
	combinedPod := makePDInflightTestPod("combined-0", "rs-c", "combined", host, port, nil)
	combinedPod.Annotations = map[string]string{constants.ModelAnnoConfig: combinedAnno}
	pods := []*v1.Pod{prefillPod, decodePod, combinedPod}

	newBucketCtx := func(model, reqID string) *types.RoutingContext {
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello world", reqID, "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello world"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    pdBucketRaw,
			Routing:          configprofiles.ParseRoutingConfig(pdBucketRaw),
		}
		return ctx
	}

	t.Run("spills over to idle combined pod when decode is saturated", func(t *testing.T) {
		const model = "pd-combined-spillover"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Saturate decode-0 via a real prior request count.
		satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "req-seed-dec", "user")
		t.Cleanup(satCtx.Delete)
		satCtx.SetTargetPod(decodePod)
		store.AddRequestCount(satCtx, satCtx.RequestID, model)

		rrqBefore := reads.rrqHGetAlls.Load()
		ctx := newBucketCtx(model, "req-spillover")
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err, "idle combined pod must stay eligible when decode pod is saturated")
		require.NotEmpty(t, addr)
		require.Equal(t, combinedPod.Name, ctx.TargetPod().Name)
		assert.EqualValues(t, 2, reads.rrqHGetAlls.Load()-rrqBefore,
			"inflight filter must read aibrix:rrq:* for decode-0 and combined-0 only (skipping prefill-0)")
	})

	t.Run("returns 429 when both decode and combined pods are saturated", func(t *testing.T) {
		const model = "pd-combined-both-saturated"
		mr := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		for _, target := range []*v1.Pod{decodePod, combinedPod} {
			satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "req-seed-"+target.Name, "user")
			t.Cleanup(satCtx.Delete)
			satCtx.SetTargetPod(target)
			store.AddRequestCount(satCtx, satCtx.RequestID, model)
		}

		ctx := newBucketCtx(model, "req-both-saturated")
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.ErrorIs(t, err, errReplicaInflightExceeded,
			"when both decode and combined pods are saturated, request must fail with 429")
	})
}

// TestSelectTargetPod_PDInflightMultiBucketSuitability verifies that under
// multi-bucket PromptLengthBucketing, inflight checks only consider decode and
// combined pods whose prompt-length range covers the request:
//  1. When the matching short-prompt bucket is saturated, an idle decode pod in
//     another bucket and an idle combined pod restricted to another range must
//     neither trigger Redis reads nor mask HTTP 429.
//  2. When a request falls outside every bucket and no suitable combined pod
//     exists, the inflight filter bypasses Redis (0 reads) so pdRouter returns
//     its genuine bucket-mismatch error (503) rather than a false 429.
func TestSelectTargetPod_PDInflightMultiBucketSuitability(t *testing.T) {
	host, port := newPDInflightPrefillServer(t)

	shortBucketRaw := json.RawMessage(`{"promptLengthBucketing":true,"promptLenBucketMinLength":1,"promptLenBucketMaxLength":100}`)
	shortBucketAnno := `{"defaultProfile":"short","profiles":{"short":{"routingConfig":{"promptLengthBucketing":true,"promptLenBucketMinLength":1,"promptLenBucketMaxLength":100}}}}`
	longBucketAnno := `{"defaultProfile":"long","profiles":{"long":{"routingConfig":{"promptLengthBucketing":true,"promptLenBucketMinLength":101,"promptLenBucketMaxLength":4096}}}}`
	longCombinedAnno := `{"defaultProfile":"long-combined","profiles":{"long-combined":{"routingConfig":{"promptLengthBucketing":true,"combined":true,"promptLenBucketMinLength":101,"promptLenBucketMaxLength":4096}}}}`

	shortPrefill := makePDInflightTestPod("prefill-short", "rs-short", pd.RolePrefill, host, port, nil)
	shortPrefill.Annotations = map[string]string{constants.ModelAnnoConfig: shortBucketAnno}
	shortDecode := makePDInflightTestPod("decode-short", "rs-short", pd.RoleDecode, host, port, nil)
	shortDecode.Annotations = map[string]string{constants.ModelAnnoConfig: shortBucketAnno}

	longPrefill := makePDInflightTestPod("prefill-long", "rs-long", pd.RolePrefill, host, port, nil)
	longPrefill.Annotations = map[string]string{constants.ModelAnnoConfig: longBucketAnno}
	longDecode := makePDInflightTestPod("decode-long", "rs-long", pd.RoleDecode, host, port, nil)
	longDecode.Annotations = map[string]string{constants.ModelAnnoConfig: longBucketAnno}

	longCombined := makePDInflightTestPod("combined-long", "rs-combined", "combined", host, port, nil)
	longCombined.Annotations = map[string]string{constants.ModelAnnoConfig: longCombinedAnno}

	pods := []*v1.Pod{shortPrefill, shortDecode, longPrefill, longDecode, longCombined}

	t.Run("saturated matching bucket returns 429 despite idle other bucket and unsuitable combined pod", func(t *testing.T) {
		const model = "pd-multibucket-429"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Saturate only decode-short (1/1); decode-long and combined-long remain idle (0/1).
		satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "req-seed-short", "user")
		t.Cleanup(satCtx.Delete)
		satCtx.SetTargetPod(shortDecode)
		store.AddRequestCount(satCtx, satCtx.RequestID, model)

		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "short prompt", "req-short", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"short prompt"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    shortBucketRaw,
			Routing:          configprofiles.ParseRoutingConfig(shortBucketRaw),
		}

		rrqBefore := reads.rrqHGetAlls.Load()
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.ErrorIs(t, err, errReplicaInflightExceeded,
			"saturated matching bucket must return 429 rather than being masked by idle non-matching bucket or unsuitable combined pod")
		assert.EqualValues(t, 1, reads.rrqHGetAlls.Load()-rrqBefore,
			"inflight filter must query only the 1 matching-bucket decode pod")
	})

	t.Run("unmatched prompt length bypasses inflight filter and preserves bucket mismatch 503", func(t *testing.T) {
		const model = "pd-multibucket-unmatched"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Empty message gives prompt length 0, outside [1, 100] and [101, 4096].
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "", "req-unmatched", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":""}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    shortBucketRaw,
			Routing:          configprofiles.ParseRoutingConfig(shortBucketRaw),
		}

		rrqBefore := reads.rrqHGetAlls.Load()
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.Error(t, err)
		assert.False(t, errors.Is(err, errReplicaInflightExceeded), "unmatched prompt length must not report 429 overload")
		assert.Contains(t, err.Error(), "no prompt-length bucket matches")
		assert.EqualValues(t, 0, reads.rrqHGetAlls.Load()-rrqBefore,
			"no Redis inflight reads when no pod matches the request's prompt-length bucket")
	})

	t.Run("routes to suitable combined pod while ignoring unsuitable combined and mismatched-prefill roleset", func(t *testing.T) {
		const model = "pd-multibucket-suitable-combined"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		shortCombinedAnno := `{"defaultProfile":"short-combined","profiles":{"short-combined":{"routingConfig":{"promptLengthBucketing":true,"combined":true,"promptLenBucketMinLength":1,"promptLenBucketMaxLength":100}}}}`
		shortCombined := makePDInflightTestPod("combined-short", "rs-combined-short", "combined", host, port, nil)
		shortCombined.Annotations = map[string]string{constants.ModelAnnoConfig: shortCombinedAnno}

		// Mismatched roleset: prefill is long-only [101,4096], decode is short [1,100].
		// Must NOT count as a short-bucket target because its prefill peer cannot serve short prompts.
		mismatchPrefill := makePDInflightTestPod("prefill-mismatch", "rs-mismatch", pd.RolePrefill, host, port, nil)
		mismatchPrefill.Annotations = map[string]string{constants.ModelAnnoConfig: longBucketAnno}
		mismatchDecode := makePDInflightTestPod("decode-mismatch", "rs-mismatch", pd.RoleDecode, host, port, nil)
		mismatchDecode.Annotations = map[string]string{constants.ModelAnnoConfig: shortBucketAnno}

		fleet := append(append([]*v1.Pod{}, pods...), shortCombined, mismatchPrefill, mismatchDecode)
		store := cache.InitWithPods(cache.New(client, nil, nil), fleet, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Saturate decode-short (1/1); combined-short remains idle (0/1).
		satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "req-seed-short-2", "user")
		t.Cleanup(satCtx.Delete)
		satCtx.SetTargetPod(shortDecode)
		store.AddRequestCount(satCtx, satCtx.RequestID, model)

		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "short prompt", "req-short-combined", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"short prompt"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    shortBucketRaw,
			Routing:          configprofiles.ParseRoutingConfig(shortBucketRaw),
		}

		rrqBefore := reads.rrqHGetAlls.Load()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: fleet}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		require.Equal(t, shortCombined.Name, ctx.TargetPod().Name)
		assert.EqualValues(t, 2, reads.rrqHGetAlls.Load()-rrqBefore,
			"inflight filter must query only decode-short and combined-short, skipping decode-long, combined-long, and decode-mismatch")
	})
}
