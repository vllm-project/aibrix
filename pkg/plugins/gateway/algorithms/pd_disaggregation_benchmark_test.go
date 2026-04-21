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

package routingalgorithms

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
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/types"
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
					router.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, pod.Name)
				}
				for req := 0; req < (i%4)+1; req++ {
					router.prefillRequestTracker.AddPrefillRequest(fmt.Sprintf("%s-%d", pod.Name, req), pod.Name)
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

	for _, engine := range []string{VLLMEngine, TensorRTLLM} {
		b.Run(engine, func(b *testing.B) {
			server, port := newPDBenchmarkServer(b, engine)
			defer server.Close()

			router := &pdRouter{
				prefillRequestTracker: pd.NewPrefillRequestTracker(),
				httpClient:            server.Client(),
			}
			prefillPod := benchmarkPDPods(engine, "prefill", 1, 1)[0]
			prefillPod.Labels[constants.ModelLabelPort] = strconv.Itoa(port)

			ctx := benchmarkPrefillRoutingContext(engine, 0)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ctx.RequestID = fmt.Sprintf("bench-prefill-%s-%d", engine, i)
				ctx.RequestTime = time.Now()
				if err := router.doPrefillRequest(ctx, prefillPod, engine); err != nil {
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
