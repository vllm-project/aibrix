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
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/inflight"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

func newFakePrefillBackend(t *testing.T) (host, port string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"prefill-ok","kv_transfer_params":{}}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Hostname(), u.Port()
}

func makeLargePDFleet(rolesets, replicas int, host, port string, anno string) []*v1.Pod {
	pods := make([]*v1.Pod, 0, rolesets*replicas*2)
	for rs := 0; rs < rolesets; rs++ {
		rsID := fmt.Sprintf("rs-%03d", rs)
		for rep := 0; rep < replicas; rep++ {
			p := makePDInflightTestPod(fmt.Sprintf("p-%03d-%03d", rs, rep), rsID, pd.RolePrefill, host, port, nil)
			d := makePDInflightTestPod(fmt.Sprintf("d-%03d-%03d", rs, rep), rsID, pd.RoleDecode, host, port, nil)
			if anno != "" {
				p.Annotations = map[string]string{constants.ModelAnnoConfig: anno}
				d.Annotations = map[string]string{constants.ModelAnnoConfig: anno}
			}
			pods = append(pods, p, d)
		}
	}
	return pods
}

func TestSelectTargetPod_PDCandidateStageBoundsRedisFanoutAtN500(t *testing.T) {
	host, port := newFakePrefillBackend(t)
	pods := makeLargePDFleet(50, 5, host, port, "") // 250P + 250D

	cases := []struct {
		name          string
		model         string
		routingConfig string
		wantMaxRRQ    int64
		wantMaxDTL    int64
		wantNarrowed  bool
	}{
		{
			name:          "load_balancing_K0_full_fleet_baseline",
			model:         "pd-n500-lb-k0",
			routingConfig: `{"candidate":{"k":0}}`,
			wantMaxRRQ:    250,
			wantMaxDTL:    0,
			wantNarrowed:  false,
		},
		{
			name:          "load_balancing_K64_bounded",
			model:         "pd-n500-lb-k64",
			routingConfig: `{"candidate":{"k":64}}`,
			wantMaxRRQ:    64,
			wantMaxDTL:    0,
			wantNarrowed:  true,
		},
		{
			name:          "token_load_K64_bounded_rrq_and_dtl",
			model:         "pd-n500-tl-k64",
			routingConfig: `{"decodeScorePolicy":"token_load","candidate":{"k":64}}`,
			wantMaxRRQ:    64,
			wantMaxDTL:    64,
			wantNarrowed:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			reads := &fleetReadCounter{}
			client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
			t.Cleanup(func() { _ = client.Close() })
			require.NoError(t, client.Ping(context.Background()).Err())

			store := cache.InitWithPods(cache.New(client, nil, nil), pods, tc.model)
			t.Cleanup(store.Close)
			rm := routing.NewRouterManagerWithCache(store)
			rm.Init()
			s := &Server{cache: store, routerManager: rm}

			raw := json.RawMessage(tc.routingConfig)
			ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, tc.model, "hello world", "req-"+tc.name, "user")
			t.Cleanup(ctx.Delete)
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"hello world"}],"stream":true}`)
			ctx.ConfigProfile = &types.ResolvedConfigProfile{
				RequestsInflight: 1000,
				RoutingConfig:    raw,
				Routing:          configprofiles.ParseRoutingConfig(raw),
			}

			beforeSel := candidateSelectionsFor(t, tc.model)
			rrqBefore := reads.rrqHGetAlls.Load()
			dtlBefore := reads.ledgerHGetAlls.Load()

			addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
			require.NoError(t, err)
			require.NotEmpty(t, addr)

			rrqDelta := reads.rrqHGetAlls.Load() - rrqBefore
			dtlDelta := reads.ledgerHGetAlls.Load() - dtlBefore
			assert.EqualValues(t, tc.wantMaxRRQ, rrqDelta, "rrq HGETALLs for %s", tc.name)
			assert.EqualValues(t, tc.wantMaxDTL, dtlDelta, "dtl HGETALLs for %s", tc.name)

			afterSel := candidateSelectionsFor(t, tc.model)
			if tc.wantNarrowed {
				assert.Equal(t, beforeSel+1, afterSel, "candidate stage metric must record narrowed PD decision")
			} else {
				assert.Equal(t, beforeSel, afterSel, "bypassed PD decision must not increment candidate stage metric")
			}
		})
	}
}

func TestSelectTargetPod_PDCandidateStageInflightScope429AndBucketing(t *testing.T) {
	host, port := newFakePrefillBackend(t)

	t.Run("saturated_narrowed_candidates_return_429_with_candidates_scope", func(t *testing.T) {
		const model = "pd-cand-429-scope"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		pods := makeLargePDFleet(10, 2, host, port, "") // 20P + 20D
		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Saturate every decode pod at cap=1.
		for _, pod := range pods {
			if pod.Labels[pd.RoleLabel] != pd.RoleDecode {
				continue
			}
			satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "seed-"+pod.Name, "user")
			satCtx.SetTargetPod(pod)
			store.AddRequestCount(satCtx, satCtx.RequestID, model)
			satCtx.Delete()
		}

		raw := json.RawMessage(`{"candidate":{"k":4}}`)
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello", "req-429-cand", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    raw,
			Routing:          configprofiles.ParseRoutingConfig(raw),
		}

		beforeCand := inflightRejectionCount(t, model, inflight.InflightScopeCandidates)
		beforeFleet := inflightRejectionCount(t, model, inflight.InflightScopeFleet)
		rrqBefore := reads.rrqHGetAlls.Load()

		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.ErrorIs(t, err, errReplicaInflightExceeded)
		assert.Equal(t, beforeCand+1, inflightRejectionCount(t, model, inflight.InflightScopeCandidates),
			"narrowed PD saturation must record scope=candidates")
		assert.Equal(t, beforeFleet, inflightRejectionCount(t, model, inflight.InflightScopeFleet))
		assert.EqualValues(t, 4, reads.rrqHGetAlls.Load()-rrqBefore,
			"saturated narrowed PD rejection must only query the K=4 candidate decode pods in Redis")
	})

	t.Run("bucketing_combined_and_decode_share_k_redis_budget", func(t *testing.T) {
		const model = "pd-cand-bucket-combined-k"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		matchAnno := `{"defaultProfile":"short","profiles":{"short":{"routingConfig":{"promptLengthBucketing":true,"promptLenBucketMinLength":1,"promptLenBucketMaxLength":500}}}}`
		combAnno := `{"defaultProfile":"comb","profiles":{"comb":{"routingConfig":{"promptLengthBucketing":true,"combined":true,"promptLenBucketMinLength":1,"promptLenBucketMaxLength":500}}}}`
		pods := makeLargePDFleet(10, 2, host, port, matchAnno) // 20P + 20D
		for i := 0; i < 10; i++ {
			c := makePDInflightTestPod(fmt.Sprintf("comb-%02d", i), "rs-comb", "combined", host, port, nil)
			c.Annotations = map[string]string{constants.ModelAnnoConfig: combAnno}
			pods = append(pods, c)
		}

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		raw := json.RawMessage(`{"promptLengthBucketing":true,"candidate":{"k":8}}`)
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "short prompt", "req-bkt-comb", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"short prompt"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1000,
			RoutingConfig:    raw,
			Routing:          configprofiles.ParseRoutingConfig(raw),
		}

		rrqBefore := reads.rrqHGetAlls.Load()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err)
		require.NotEmpty(t, addr)
		assert.LessOrEqual(t, reads.rrqHGetAlls.Load()-rrqBefore, int64(8),
			"decode + combined Redis rrq HGETALLs together must stay <= K")
	})

	t.Run("bucketing_mismatch_on_large_fleet_bypasses_and_preserves_503", func(t *testing.T) {
		const model = "pd-cand-bucket-mismatch-503"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		longOnlyAnno := `{"defaultProfile":"long","profiles":{"long":{"routingConfig":{"promptLengthBucketing":true,"promptLenBucketMinLength":50000,"promptLenBucketMaxLength":100000}}}}`
		pods := makeLargePDFleet(20, 2, host, port, longOnlyAnno) // 40P + 40D

		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		raw := json.RawMessage(`{"promptLengthBucketing":true,"candidate":{"k":4}}`)
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "short", "req-bkt-503", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"short"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 10,
			RoutingConfig:    raw,
			Routing:          configprofiles.ParseRoutingConfig(raw),
		}

		beforeSel := candidateSelectionsFor(t, model)
		rrqBefore := reads.rrqHGetAlls.Load()
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.Error(t, err)
		assert.False(t, errors.Is(err, errReplicaInflightExceeded))
		assert.Contains(t, err.Error(), "no prompt-length bucket matches")
		assert.EqualValues(t, 0, reads.rrqHGetAlls.Load()-rrqBefore)
		assert.Equal(t, beforeSel, candidateSelectionsFor(t, model))
	})

	t.Run("invalid_decodeScorePolicy_still_bounds_redis_to_k_before_error", func(t *testing.T) {
		const model = "pd-cand-invalid-decode-policy"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		pods := makeLargePDFleet(50, 5, host, port, "") // 250P + 250D
		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		raw := json.RawMessage(`{"decodeScorePolicy":"invalid_policy","candidate":{"k":64}}`)
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello", "req-bad-dec", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1000,
			RoutingConfig:    raw,
			Routing:          configprofiles.ParseRoutingConfig(raw),
		}

		rrqBefore := reads.rrqHGetAlls.Load()
		_, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown decodeScorePolicy")
		assert.LessOrEqual(t, reads.rrqHGetAlls.Load()-rrqBefore, int64(64),
			"invalid decodeScorePolicy must not fall through to full-fleet O(N) Redis reads")
	})

	t.Run("partial_saturation_soft_exclude_succeeds_and_sampled_saturation_returns_candidates_429", func(t *testing.T) {
		const model = "pd-cand-partial-sat"
		mr := miniredis.RunT(t)
		reads := &fleetReadCounter{}
		client := redis.NewClient(&redis.Options{Addr: mr.Addr(), Dialer: reads.dial})
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, client.Ping(context.Background()).Err())

		pods := makeLargePDFleet(1, 20, host, port, "") // 1rs x 20P/20D
		store := cache.InitWithPods(cache.New(client, nil, nil), pods, model)
		t.Cleanup(store.Close)
		rm := routing.NewRouterManagerWithCache(store)
		rm.Init()
		s := &Server{cache: store, routerManager: rm}

		// Part A: Saturate 16 of 20 decodes via this gateway's store (known to local approx).
		// Soft-exclude steers ranked slots to the 4 idle decodes -> succeeds with <= 4 HGETALLs.
		satCount := 0
		for _, pod := range pods {
			if pod.Labels[pd.RoleLabel] != pd.RoleDecode {
				continue
			}
			if satCount < 16 {
				satCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "seed", "seed-local-"+pod.Name, "user")
				satCtx.SetTargetPod(pod)
				store.AddRequestCount(satCtx, satCtx.RequestID, model)
				satCtx.Delete()
				satCount++
			}
		}

		raw := json.RawMessage(`{"candidate":{"k":4}}`)
		ctx := types.NewRoutingContext(context.Background(), routing.RouterPD, model, "hello", "req-partial-ok", "user")
		t.Cleanup(ctx.Delete)
		ctx.ReqPath = "/v1/chat/completions"
		ctx.ReqBody = []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		ctx.ConfigProfile = &types.ResolvedConfigProfile{
			RequestsInflight: 1,
			RoutingConfig:    raw,
			Routing:          configprofiles.ParseRoutingConfig(raw),
		}
		rrqBefore := reads.rrqHGetAlls.Load()
		addr, err := s.selectTargetPod(context.Background(), ctx, &utils.PodArray{Pods: pods}, "")
		require.NoError(t, err, "in-memory soft-exclude must steer ranked slots to idle decodes")
		require.NotEmpty(t, addr)
		assert.EqualValues(t, 4, reads.rrqHGetAlls.Load()-rrqBefore)

		// Part B: Initialize store2 before the peer gateway burst so store2's in-memory
		// approx lags the peer's counts (within one 1s remote-running sweep interval).
		// Then saturate 19 of 20 decodes from peerStore sharing mr2, leaving 1 idle
		// unsampled decode in the fleet. When a K=4 sample lands on 4 peer-saturated
		// decodes, narrowPDToInflightCapacity returns 429 with scope="candidates" and
		// only 4 Redis HGETALLs.
		mr2 := miniredis.RunT(t)
		const remoteModel = "pd-cand-partial-sat-remote"

		reads2 := &fleetReadCounter{}
		client2 := redis.NewClient(&redis.Options{Addr: mr2.Addr(), Dialer: reads2.dial})
		t.Cleanup(func() { _ = client2.Close() })
		require.NoError(t, client2.Ping(context.Background()).Err())

		store2 := cache.InitWithPods(cache.New(client2, nil, nil), pods, remoteModel)
		t.Cleanup(store2.Close)
		rm2 := routing.NewRouterManagerWithCache(store2)
		rm2.Init()
		s2 := &Server{cache: store2, routerManager: rm2}

		// Register a distinct live peer gateway in aibrix:rrq:gateways (with a fresh ms
		// heartbeat) and mark 19/20 decodes at cap=1 in Redis under that peer's hash field.
		peerClient := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
		t.Cleanup(func() { _ = peerClient.Close() })
		require.NoError(t, peerClient.ZAdd(context.Background(), "aibrix:rrq:gateways", redis.Z{
			Score:  float64(time.Now().UnixMilli() + 60_000),
			Member: "peer-gw-other",
		}).Err())
		remoteSat := 0
		for _, pod := range pods {
			if pod.Labels[pd.RoleLabel] != pd.RoleDecode {
				continue
			}
			if remoteSat < 19 {
				key := "aibrix:rrq:" + utils.GeneratePodKey(pod.Namespace, pod.Name)
				require.NoError(t, peerClient.HSet(context.Background(), key, "peer-gw-other", 1).Err())
				remoteSat++
			}
		}

		sawCandidates429 := false
		for trial := 0; trial < 20; trial++ {
			tCtx := types.NewRoutingContext(context.Background(), routing.RouterPD, remoteModel, "hello", fmt.Sprintf("req-remote-sat-%d", trial), "user")
			tCtx.ReqPath = "/v1/chat/completions"
			tCtx.ReqBody = []byte(`{"model":"` + remoteModel + `","messages":[{"role":"user","content":"hello"}],"stream":true}`)
			tCtx.ConfigProfile = &types.ResolvedConfigProfile{
				RequestsInflight: 1,
				RoutingConfig:    raw,
				Routing:          configprofiles.ParseRoutingConfig(raw),
			}
			beforeCand := inflightRejectionCount(t, remoteModel, inflight.InflightScopeCandidates)
			rrqB := reads2.rrqHGetAlls.Load()
			_, err := s2.selectTargetPod(context.Background(), tCtx, &utils.PodArray{Pods: pods}, "")
			tCtx.Delete()
			assert.EqualValues(t, 4, reads2.rrqHGetAlls.Load()-rrqB, "must only query K=4 sampled decodes in Redis")
			if errors.Is(err, errReplicaInflightExceeded) {
				assert.Equal(t, beforeCand+1, inflightRejectionCount(t, remoteModel, inflight.InflightScopeCandidates))
				sawCandidates429 = true
				break
			}
		}
		assert.True(t, sawCandidates429, "sampled saturation on a partially saturated fleet must return 429 with scope=candidates")
	})
}
