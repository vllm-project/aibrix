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

package router

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	pdBenchmarkRolesetCount = 8
	pdBenchmarkReplicaCount = 2
)

func BenchmarkScorePrefillPods(b *testing.B) {
	for _, engine := range []string{VLLMEngine} {
		b.Run(engine, func(b *testing.B) {
			pods := benchmarkPDPods(engine, "prefill", pdBenchmarkRolesetCount, pdBenchmarkReplicaCount)
			tok := tokenizer.NewCharacterTokenizer()
			benchmarkPrefixTable := prefixcacheindexer.NewPrefixHashTable()
			router := &pdRouter{
				prefillPolicy:         newPrefixCachePrefillPolicy(benchmarkPrefixTable),
				prefixCacheIndexer:    benchmarkPrefixTable,
				prefillRequestTracker: pd.NewPrefillRequestTracker(),
			}

			ctx := types.NewRoutingContext(
				context.Background(),
				RouterPD,
				"benchmark-model",
				strings.Repeat("prefill benchmark prompt ", 128),
				"bench-score-prefill",
				"bench-user",
			)
			defer ctx.Delete()
			ctx.Engine = engine

			tokens, err := tok.TokenizeInputText(ctx.Message)
			if err != nil {
				b.Fatalf("tokenize prompt: %v", err)
			}
			prefixHashes := router.prefixCacheIndexer.GetPrefixHashes(tokens)
			for i, pod := range pods {
				if i%2 == 0 {
					router.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, utils.GeneratePodKey(pod.Namespace, pod.Name))
				}
				for req := 0; req < (i%4)+1; req++ {
					router.prefillRequestTracker.AddPrefillRequest(fmt.Sprintf("%s-%d", pod.Name, req), utils.GeneratePodKey(pod.Namespace, pod.Name))
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				scores, maxScore, matchedHashes := router.scorePrefillPods(ctx, pods, router.prefillPolicy)
				if len(scores) == 0 {
					b.Fatal("scorePrefillPods returned no scores")
				}
				if maxScore <= 0 {
					b.Fatal("scorePrefillPods returned non-positive max score")
				}
				if len(matchedHashes) == 0 {
					b.Fatal("scorePrefillPods returned no prefix hashes")
				}
			}
		})
	}
}

func BenchmarkScoreDecodePods(b *testing.B) {
	for _, engine := range []string{VLLMEngine} {
		b.Run(engine, func(b *testing.B) {
			pods := benchmarkPDPods(engine, "decode", pdBenchmarkRolesetCount, pdBenchmarkReplicaCount)
			ctx := types.NewRoutingContext(context.Background(), RouterPD, "benchmark-model", "", "bench-score-decode", "bench-user")
			defer ctx.Delete()
			ctx.Engine = engine

			podRequestCounts := make(map[string]float64, len(pods))
			podThroughputs := make(map[string]float64, len(pods))
			podFreeGPUUsage := make(map[string]float64, len(pods))
			maxRequestCount := float64(1)
			maxThroughput := float64(1)
			maxFreeGPUUsage := float64(1)

			for i, pod := range pods {
				requestCount := float64((i % 6) + 1)
				throughput := float64(512 + i*64)
				freeGPUUsage := float64(25 + (i%5)*10)

				podRequestCounts[pod.Name] = requestCount
				podThroughputs[pod.Name] = throughput
				podFreeGPUUsage[pod.Name] = freeGPUUsage

				if requestCount > maxRequestCount {
					maxRequestCount = requestCount
				}
				if throughput > maxThroughput {
					maxThroughput = throughput
				}
				if freeGPUUsage > maxFreeGPUUsage {
					maxFreeGPUUsage = freeGPUUsage
				}
			}

			router := &pdRouter{}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				run := router.scoreDecodePods(
					ctx,
					pods,
					maxRequestCount,
					maxThroughput,
					maxFreeGPUUsage,
					podRequestCounts,
					podThroughputs,
					podFreeGPUUsage,
					nil,
				)
				if len(run.PerRoleset) == 0 {
					b.Fatal("scoreDecodePods returned no scores")
				}
				if run.MaxScore <= 0 {
					b.Fatal("scoreDecodePods returned non-positive max score")
				}
			}
		})
	}
}

func BenchmarkDoPrefillRequest(b *testing.B) {
	suppressKlogForBenchmark(b)

	for _, engineName := range []string{VLLMEngine, TensorRTLLM} {
		b.Run(engineName, func(b *testing.B) {
			server, port := newPDBenchmarkServer(b, engineName)
			defer server.Close()

			router := &pdRouter{
				prefillRequestTracker: pd.NewPrefillRequestTracker(),
				httpClient:            server.Client(),
			}
			prefillPod := benchmarkPDPods(engineName, "prefill", 1, 1)[0]
			prefillPod.Labels[constants.ModelLabelPort] = strconv.Itoa(port)

			ctx := benchmarkPrefillRoutingContext(engineName, 0)
			handler := engine.Resolve(engineName)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.RequestID = fmt.Sprintf("bench-prefill-%s-%d", engineName, i)
				ctx.RequestTime = time.Now()
				router.prefillRequestTracker.AddPrefillRequest(ctx.RequestID, utils.GeneratePodKey(prefillPod.Namespace, prefillPod.Name))
				if err := router.doPrefillRequest(ctx, prefillPod, handler); err != nil {
					b.Fatalf("doPrefillRequest failed: %v", err)
				}
			}
		})
	}
}

func benchmarkPDPods(engine, role string, rolesetCount, replicaCount int) []*v1.Pod {
	pods := make([]*v1.Pod, 0, rolesetCount*replicaCount)
	for roleset := 0; roleset < rolesetCount; roleset++ {
		for replica := 0; replica < replicaCount; replica++ {
			pods = append(pods, &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("%s-%s-rs%d-rep%d", engine, role, roleset, replica),
					Labels: map[string]string{
						LLMEngineIdentifier:      engine,
						PDRoleIdentifier:         role,
						PDRoleSetIdentifier:      fmt.Sprintf("%s-rs-%d", engine, roleset),
						constants.ModelLabelPort: "8000",
					},
				},
				Status: v1.PodStatus{
					PodIP: "127.0.0.1",
					Conditions: []v1.PodCondition{{
						Type:   v1.PodReady,
						Status: v1.ConditionTrue,
					}},
					ContainerStatuses: []v1.ContainerStatus{{
						Name:  "model",
						Ready: true,
					}},
				},
			})
		}
	}
	return pods
}

func benchmarkPrefillRoutingContext(engine string, requestID int) *types.RoutingContext {
	user := "bench-user"
	return &types.RoutingContext{
		Context:     context.Background(),
		Algorithm:   RouterPD,
		Model:       "benchmark-model",
		Engine:      engine,
		Message:     strings.Repeat("disaggregated inference benchmark ", 32),
		RequestID:   fmt.Sprintf("bench-prefill-%s-%d", engine, requestID),
		User:        &user,
		RequestTime: time.Now(),
		ReqPath:     "/v1/chat/completions",
		ReqHeaders: map[string]string{
			"Authorization": "Bearer bench-token",
		},
		ReqBody: randReqBody(8000),
	}
}

const randReqBodyChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "

// randReqBody returns a JSON chat-completions body whose total length is exactly targetLen bytes.
// The content field is filled with a deterministic character pattern for stable, low-overhead benchmarks.
func randReqBody(targetLen int) []byte {
	const prefix = `{"messages":[{"role":"user","content":"`
	const suffix = `"}],"stream":true}`
	contentLen := targetLen - len(prefix) - len(suffix)
	if contentLen < 0 {
		contentLen = 0
	}
	buf := make([]byte, len(prefix)+contentLen+len(suffix))
	copy(buf, prefix)
	for i := range contentLen {
		buf[len(prefix)+i] = randReqBodyChars[i%len(randReqBodyChars)]
	}
	copy(buf[len(prefix)+contentLen:], suffix)
	return buf
}

func newPDBenchmarkServer(b *testing.B, engine string) (*httptest.Server, int) {
	b.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("listen on benchmark port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		var response []byte
		switch engine {
		case VLLMEngine:
			response, err = sonic.Marshal(map[string]any{
				"kv_transfer_params": map[string]any{
					"do_remote_decode":  true,
					"do_remote_prefill": false,
					"remote_engine_id":  "bench-engine",
					"remote_block_ids":  []string{"b1", "b2"},
					"remote_host":       "127.0.0.1",
					"remote_port":       "8080",
				},
			})
		case TensorRTLLM:
			response, err = sonic.Marshal(map[string]any{
				"disaggregated_params": map[string]any{
					"request_type":      "context_only",
					"disagg_request_id": int64(123456789),
					"first_gen_tokens":  []int{42},
					"opaque_state":      "bench-state",
				},
			})
		default:
			response = []byte(`{"choices":[{"message":{"content":"ok"}}]}`)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(response)
	}))

	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	return server, port
}

func suppressKlogForBenchmark(b *testing.B) {
	b.Helper()

	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	b.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
}

// BenchmarkFilterPrefillDecodePods times one full PD selection for a request:
// policy Prepare (outside the model's selection lock) plus everything the
// router does while it holds the stripe lock (imbalance checks, prefill and
// decode scoring, final pairing, tracker registration). With least_request the
// Prepare step is trivial, so that sub-benchmark is effectively the cost of
// the selection critical section.
// 4 rolesets x 2 replicas = 8 prefill + 8 decode pods, all equally loaded so
// no imbalance fast path fires and every pod is scored.
func BenchmarkFilterPrefillDecodePods(b *testing.B) {
	suppressKlogForBenchmark(b)

	const (
		rolesetCount = 4
		replicaCount = 2
		model        = "benchmark-model"
	)
	prefillPods := benchmarkPDPods(VLLMEngine, "prefill", rolesetCount, replicaCount)
	decodePods := benchmarkPDPods(VLLMEngine, "decode", rolesetCount, replicaCount)
	readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePods...)

	podMetrics := make(map[string]map[string]metrics.MetricValue, len(decodePods))
	for _, pod := range decodePods {
		podMetrics[pod.Name] = map[string]metrics.MetricValue{
			metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 2},
			metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 512},
			metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.3},
		}
	}
	c := cache.NewWithPodsMetricsForTest(readyPods, model, podMetrics)

	prefixTable := prefixcacheindexer.NewPrefixHashTable()
	policies := []struct {
		name   string
		policy pd.PrefillScorePolicy
	}{
		{name: pd.PrefillScorePolicyLeastRequest, policy: pd.NewLeastRequestPrefillPolicy()},
		{name: pd.PrefillScorePolicyPrefixCache, policy: newPrefixCachePrefillPolicy(prefixTable)},
	}

	for _, tc := range policies {
		b.Run(tc.name, func(b *testing.B) {
			tracker := pd.NewPrefillRequestTracker()
			pending := pd.NewPendingDecodeTracker()
			router := &pdRouter{
				cache:                 c,
				prefillPolicy:         tc.policy,
				prefixCacheIndexer:    prefixTable,
				prefillRequestTracker: tracker,
				pendingDecodeTracker:  pending,
				prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
				selectionCounts:       map[string]int64{},
			}

			// In production startPrefixUpdater() runs a goroutine that drains
			// prefixUpdateCh. Without a consumer the 1024-slot buffer fills up and
			// every later enqueuePrefixUpdate takes the drop branch and logs a
			// warning, which is pure benchmark artefact. Drain and discard the jobs
			// so the send stays on the normal (non-dropping) path without adding
			// concurrent indexer writes to what is being measured.
			updaterStop := make(chan struct{})
			updaterDone := make(chan struct{})
			go func() {
				defer close(updaterDone)
				for {
					select {
					case <-router.prefixUpdateCh:
					case <-updaterStop:
						return
					}
				}
			}()
			b.Cleanup(func() {
				close(updaterStop)
				<-updaterDone
			})

			ctx := types.NewRoutingContext(context.Background(), RouterPD, model,
				strings.Repeat("filter prefill decode benchmark prompt ", 32),
				"bench-filter-pd", "bench-user")
			defer ctx.Delete()
			ctx.Engine = VLLMEngine
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = randReqBody(4000)

			// Seed the shared prefix table so the prefix_cache sub-benchmark measures
			// the match path instead of scoring every prefill pod as a cache miss.
			// prefixcache.NewTokenizer() is what newPrefixCachePrefillPolicy hands the policy, so
			// these are the hashes the policy looks up.
			seedTokens, err := prefixcache.NewTokenizer().TokenizeInputText(ctx.Message)
			if err != nil {
				b.Fatalf("tokenize prompt: %v", err)
			}
			seedHashes := prefixTable.GetPrefixHashes(seedTokens)
			for i, pod := range prefillPods {
				if i%2 == 0 {
					prefixTable.AddPrefix(seedHashes, ctx.Model, utils.GeneratePodKey(pod.Namespace, pod.Name))
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				prefillPod, decodePod, err := router.filterPrefillDecodePods(ctx, readyPods)
				if err != nil {
					b.Fatalf("filterPrefillDecodePods: %v", err)
				}
				if prefillPod == nil || decodePod == nil {
					b.Fatal("filterPrefillDecodePods returned a nil pod")
				}
				tracker.RemovePrefillRequest(ctx.RequestID)
				pending.RemovePendingDecode(ctx.RequestID)
			}
		})
	}
}

// BenchmarkFilterPrefillDecodePods_ParallelMultiModel measures concurrent PD
// selection throughput across multiple independent models sharing one pdRouter.
func BenchmarkFilterPrefillDecodePods_ParallelMultiModel(b *testing.B) {
	suppressKlogForBenchmark(b)

	const (
		modelCount   = 8
		rolesetCount = 8
		replicaCount = 4
	)

	modelNames := make([]string, modelCount)
	modelPods := make([][]*v1.Pod, modelCount)
	allPods := make([]*v1.Pod, 0, modelCount*rolesetCount*replicaCount*2)
	podMetrics := make(map[string]map[string]metrics.MetricValue, modelCount*rolesetCount*replicaCount)

	for m := 0; m < modelCount; m++ {
		modelNames[m] = fmt.Sprintf("benchmark-model-%d", m)
		prefills := benchmarkPDPods(VLLMEngine, "prefill", rolesetCount, replicaCount)
		decodes := benchmarkPDPods(VLLMEngine, "decode", rolesetCount, replicaCount)
		for _, p := range prefills {
			p.Name = fmt.Sprintf("m%d-%s", m, p.Name)
		}
		for _, p := range decodes {
			p.Name = fmt.Sprintf("m%d-%s", m, p.Name)
			podMetrics[p.Name] = map[string]metrics.MetricValue{
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: 2},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: 512},
				metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.3},
			}
		}
		ready := append(append([]*v1.Pod{}, prefills...), decodes...)
		modelPods[m] = ready
		allPods = append(allPods, ready...)
	}

	c := cache.NewWithPodsMetricsForTest(allPods, modelNames[0], podMetrics)
	tracker := pd.NewPrefillRequestTracker()
	pending := pd.NewPendingDecodeTracker()
	router := &pdRouter{
		cache:                 c,
		prefillPolicy:         pd.NewLeastRequestPrefillPolicy(),
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pending,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}

	var workerSeq atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := workerSeq.Add(1) - 1
		m := int(w % modelCount)
		model := modelNames[m]
		ready := modelPods[m]
		var iter uint64
		for pb.Next() {
			iter++
			reqID := fmt.Sprintf("bench-mm-%d-%d", w, iter)
			ctx := types.NewRoutingContext(context.Background(), RouterPD, model, "hello", reqID, "bench-user")
			ctx.Engine = VLLMEngine
			prefillPod, decodePod, err := router.filterPrefillDecodePods(ctx, ready)
			if err != nil {
				b.Fatalf("filterPrefillDecodePods: %v", err)
			}
			if prefillPod == nil || decodePod == nil {
				b.Fatal("filterPrefillDecodePods returned a nil pod")
			}
			tracker.RemovePrefillRequest(reqID)
			pending.RemovePendingDecode(reqID)
			ctx.Delete()
		}
	})
}

// BenchmarkFilterPrefillDecodePods_LargeFleet measures single-model PD selection
// throughput (serial and parallel) on 500-pod fleets (250P + 250D) across two
// contrasting topologies (50 rolesets x 5 replicas and 1 roleset x 250 replicas)
// and three policy pairings (prefix_cache+load_balancing,
// hybrid_cache_load+token_load, and conductor+conductor).
func BenchmarkFilterPrefillDecodePods_LargeFleet(b *testing.B) {
	suppressKlogForBenchmark(b)
	const model = "large-fleet-model"

	topologies := []struct {
		name         string
		rolesetCount int
		replicaCount int
	}{
		{"50rsx5", 50, 5},
		{"1rsx250", 1, 250},
	}

	prompt := strings.Repeat("disaggregated scale benchmark prompt ", 32)
	body := randReqBody(4000)

	for _, topo := range topologies {
		prefillPods := benchmarkPDPods(VLLMEngine, "prefill", topo.rolesetCount, topo.replicaCount)
		decodePods := benchmarkPDPods(VLLMEngine, "decode", topo.rolesetCount, topo.replicaCount)
		readyPods := append(append([]*v1.Pod{}, prefillPods...), decodePods...)

		podMetrics := make(map[string]map[string]metrics.MetricValue, len(decodePods))
		for i, pod := range decodePods {
			podMetrics[pod.Name] = map[string]metrics.MetricValue{
				metrics.RealtimeNumRequestsRunning:      &metrics.SimpleMetricValue{Value: float64(2 + i%3)},
				metrics.AvgGenerationThroughputToksPerS: &metrics.SimpleMetricValue{Value: float64(500 + (i%5)*20)},
				metrics.KVCacheUsagePerc:                &metrics.SimpleMetricValue{Value: 0.3},
			}
		}
		c := cache.NewWithPodsMetricsForTest(readyPods, model, podMetrics)
		prefixTable := prefixcacheindexer.NewPrefixHashTable()
		tokenTracker := pd.NewTokenLoadTracker()

		seedTokens, err := prefixcache.NewTokenizer().TokenizeInputText(prompt)
		if err != nil {
			b.Fatalf("tokenize prompt: %v", err)
		}
		seedHashes := prefixTable.GetPrefixHashes(seedTokens)
		for i := 0; i < 4 && i < len(prefillPods); i++ {
			prefixTable.AddPrefix(seedHashes, model, utils.GeneratePodKey(prefillPods[i].Namespace, prefillPods[i].Name))
		}

		policies := []struct {
			name    string
			prefill pd.PrefillScorePolicy
			decode  pd.DecodeScorePolicy
		}{
			{"prefix_cache+load_balancing", newPrefixCachePrefillPolicy(prefixTable), pd.LoadBalancingDecodePolicy{}},
			{"hybrid_cache_load+token_load", newHybridCacheLoadPrefillPolicy(prefixTable, tokenTracker), pd.TokenLoadDecodePolicy{}},
			{"conductor+conductor", newConductorPrefillPolicy(prefixTable, c), pd.ConductorDecodePolicy{}},
		}

		for _, pol := range policies {
			b.Run(topo.name+"/"+pol.name+"/serial", func(b *testing.B) {
				runLargeFleetPDBenchSerial(b, c, prefixTable, tokenTracker, pol.prefill, pol.decode, model, prompt, body, readyPods)
			})
			b.Run(topo.name+"/"+pol.name+"/parallel_single_model", func(b *testing.B) {
				runLargeFleetPDBenchParallel(b, c, prefixTable, tokenTracker, pol.prefill, pol.decode, model, prompt, body, readyPods)
			})
		}
	}
}

func runLargeFleetPDBenchSerial(b *testing.B, c cache.Cache, prefixTable *prefixcacheindexer.PrefixHashTable, tokenTracker *pd.TokenLoadTracker, prefillPol pd.PrefillScorePolicy, decodePol pd.DecodeScorePolicy, model, prompt string, body []byte, readyPods []*v1.Pod) {
	b.Helper()
	tracker := pd.NewPrefillRequestTracker()
	pending := pd.NewPendingDecodeTracker()
	r := &pdRouter{
		cache:                 c,
		prefillPolicy:         prefillPol,
		decodePolicy:          decodePol,
		prefixCacheIndexer:    prefixTable,
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pending,
		tokenLoadTracker:      tokenTracker,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}
	ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, "bench-large-0", "bench-user")
	defer ctx.Delete()
	ctx.Engine = VLLMEngine
	ctx.ReqPath = "/v1/chat/completions"
	ctx.ReqBody = body

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, d, err := r.filterPrefillDecodePods(ctx, readyPods)
		if err != nil || p == nil || d == nil {
			b.Fatalf("filterPrefillDecodePods: err=%v p=%v d=%v", err, p, d)
		}
		r.pendingPrefixCredits.Delete(ctx.TrackerKey())
		tracker.RemovePrefillRequest(ctx.TrackerKey())
		pending.RemovePendingDecode(ctx.TrackerKey())
		tokenTracker.ReleaseAll(ctx.TrackerKey())
	}
}

func runLargeFleetPDBenchParallel(b *testing.B, c cache.Cache, prefixTable *prefixcacheindexer.PrefixHashTable, tokenTracker *pd.TokenLoadTracker, prefillPol pd.PrefillScorePolicy, decodePol pd.DecodeScorePolicy, model, prompt string, body []byte, readyPods []*v1.Pod) {
	b.Helper()
	tracker := pd.NewPrefillRequestTracker()
	pending := pd.NewPendingDecodeTracker()
	r := &pdRouter{
		cache:                 c,
		prefillPolicy:         prefillPol,
		decodePolicy:          decodePol,
		prefixCacheIndexer:    prefixTable,
		prefillRequestTracker: tracker,
		pendingDecodeTracker:  pending,
		tokenLoadTracker:      tokenTracker,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       map[string]int64{},
	}
	var seq atomic.Uint64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := fmt.Sprintf("bench-large-%d", seq.Add(1))
			ctx := types.NewRoutingContext(context.Background(), RouterPD, model, prompt, id, "bench-user")
			ctx.Engine = VLLMEngine
			ctx.ReqPath = "/v1/chat/completions"
			ctx.ReqBody = body
			p, d, err := r.filterPrefillDecodePods(ctx, readyPods)
			if err != nil || p == nil || d == nil {
				b.Errorf("filterPrefillDecodePods: err=%v p=%v d=%v", err, p, d)
			}
			r.pendingPrefixCredits.Delete(ctx.TrackerKey())
			tracker.RemovePrefillRequest(ctx.TrackerKey())
			pending.RemovePendingDecode(ctx.TrackerKey())
			tokenTracker.ReleaseAll(ctx.TrackerKey())
			ctx.Delete()
		}
	})
}
