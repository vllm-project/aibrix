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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// These benchmarks measure how the per-request routing cost grows with the number
// of ready replicas (RFC #3188). Each iteration runs the part of the gateway's
// selectTargetPod that follows the cheap pod filters: the load-imbalance gate,
// the shuffle, router selection (including the load-balance auto-blend), and
// Route. The candidate-K cases score only the first K pods after the shuffle,
// as an upper bound on what a sampled candidate set would save.

var fleetBenchSizes = []int{50, 500, 2000}

var fleetBenchStrategies = []string{
	string(RouterLeastRequest),
	string(RouterLoadBalance),
	string(RouterPrefixCache),
}

func newFleetBenchCache(n int) (*cache.Store, []*v1.Pod) {
	pods := make([]*v1.Pod, n)
	podMetrics := make(map[string]map[string]metrics.MetricValue, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("pod-%04d", i)
		pods[i] = makeLBPod(name, fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
		pods[i].Labels = map[string]string{constants.ModelLabelPort: "8000"}
		// Spread load, capacity and KV usage so that scores do not tie.
		podMetrics[name] = map[string]metrics.MetricValue{
			metrics.RealtimeNumRequestsRunning:  &metrics.SimpleMetricValue{Value: float64(i % 17)},
			metrics.RealtimeOutputTokenRateEWMA: &metrics.SimpleMetricValue{Value: float64(1000 + (i*37)%3000)},
			metrics.KVCacheUsagePerc:            &metrics.SimpleMetricValue{Value: float64((i*13)%80) / 100},
		}
	}
	c := cache.NewWithPodsMetricsForTest(pods, "m1", podMetrics)
	c = cache.InitWithPodsModelMetrics(c, podMetrics)
	return c, c.ListPods()
}

func routeFleetOnce(b *testing.B, rm *RouterManager, c cache.Cache, all []*v1.Pod, scratch []*v1.Pod, strategy, message string, k int, shuffle func([]*v1.Pod)) {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(strategy), "m1", message, "req", "")
	defer ctx.Delete()
	ResolveRoutingOverrides(ctx)

	pods := append(scratch[:0], all...)
	if shuffle != nil {
		shuffle(pods)
	}
	if k > 0 && k < len(pods) {
		pods = pods[:k]
	}
	pods = ApplyLoadImbalanceGate(ctx, c, pods)

	router, err := rm.Select(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := router.Route(ctx, &utils.PodArray{Pods: pods}); err != nil {
		b.Fatal(err)
	}
}

// routeFleetCandidatesOnce follows selectTargetPod with the candidate stage on: Select, then
// SelectCandidates (K pods ranked on in-memory estimates, a quarter of them random), then the
// gate, the shuffle and Route over the candidates only. The cost of choosing the candidates,
// including estimating every ready pod, is included.
func routeFleetCandidatesOnce(b *testing.B, rm *RouterManager, c cache.Cache, all []*v1.Pod, strategy, message string, k int) {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(strategy), "m1", message, "req", "")
	defer ctx.Delete()
	ResolveRoutingOverrides(ctx)

	router, err := rm.Select(ctx)
	if err != nil {
		b.Fatal(err)
	}
	pods := selectCandidates(ctx, router, all, k)
	pods = ApplyLoadImbalanceGate(ctx, c, pods)
	utils.Shuffle(pods)
	if _, err := router.Route(ctx, &utils.PodArray{Pods: pods}); err != nil {
		b.Fatal(err)
	}
}

// fleetBenchPrompts rotates a fixed set of distinct prompts so that the prefix
// table reaches a steady size instead of matching one prompt on every pod.
var fleetBenchPrompts = func() []string {
	prompts := make([]string, 64)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("session %d: ", i) + strings.Repeat("the quick brown fox jumps over the lazy dog ", 64)
	}
	return prompts
}()

// silenceKlog keeps per-request routing logs out of the benchmark output.
func silenceKlog(b *testing.B) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	klog.SetOutput(io.Discard)
	b.Cleanup(func() {
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("stderrthreshold", "ERROR")
		klog.SetOutput(os.Stderr)
	})
}

func benchmarkFleetRouting(b *testing.B, k int, shuffle func([]*v1.Pod)) {
	silenceKlog(b)
	for _, n := range fleetBenchSizes {
		c, all := newFleetBenchCache(n)
		scratch := make([]*v1.Pod, 0, len(all))
		for _, strategy := range fleetBenchStrategies {
			b.Run(fmt.Sprintf("N=%d/%s", n, strategy), func(b *testing.B) {
				rm := NewRouterManagerWithCache(c)
				rm.Init()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					prompt := fleetBenchPrompts[i%len(fleetBenchPrompts)]
					routeFleetOnce(b, rm, c, all, scratch, strategy, prompt, k, shuffle)
				}
			})
		}
	}
}

// BenchmarkFleetRouting_Full is today's path: every ready pod is shuffled and scored.
func BenchmarkFleetRouting_Full(b *testing.B) { benchmarkFleetRouting(b, 0, utils.Shuffle[*v1.Pod]) }

// BenchmarkFleetRouting_FullCryptoShuffle shuffles with crypto/rand, as the gateway did
// before it switched to math/rand/v2. Run against older code, it reproduces the "before" cost.
func BenchmarkFleetRouting_FullCryptoShuffle(b *testing.B) {
	benchmarkFleetRouting(b, 0, utils.CryptoShuffle[*v1.Pod])
}

// BenchmarkFleetRouting_NoShuffle isolates the cost of shuffling the full list.
func BenchmarkFleetRouting_NoShuffle(b *testing.B) { benchmarkFleetRouting(b, 0, nil) }

// BenchmarkFleetRouting_K16 scores only 16 shuffled candidates. It still shuffles
// the whole list, so it shows the scoring saving alone.
func BenchmarkFleetRouting_K16(b *testing.B) { benchmarkFleetRouting(b, 16, utils.Shuffle[*v1.Pod]) }

// BenchmarkFleetRouting_CandidateStage runs the candidate stage with K=16, so it can be read
// against BenchmarkFleetRouting_Full.
func BenchmarkFleetRouting_CandidateStage(b *testing.B) {
	silenceKlog(b)
	for _, n := range fleetBenchSizes {
		c, all := newFleetBenchCache(n)
		for _, strategy := range fleetBenchStrategies {
			b.Run(fmt.Sprintf("N=%d/%s", n, strategy), func(b *testing.B) {
				rm := NewRouterManagerWithCache(c)
				rm.Init()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					prompt := fleetBenchPrompts[i%len(fleetBenchPrompts)]
					routeFleetCandidatesOnce(b, rm, c, all, strategy, prompt, 16)
				}
			})
		}
	}
}

// BenchmarkCandidateSelection isolates the candidate stage itself: estimating every ready pod
// from memory, blending and ranking the estimates, and drawing the random slots, with K=16.
func BenchmarkCandidateSelection(b *testing.B) {
	silenceKlog(b)
	for _, n := range fleetBenchSizes {
		c, all := newFleetBenchCache(n)
		for _, strategy := range fleetBenchStrategies {
			b.Run(fmt.Sprintf("N=%d/%s", n, strategy), func(b *testing.B) {
				rm := NewRouterManagerWithCache(c)
				rm.Init()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(strategy), "m1",
						fleetBenchPrompts[i%len(fleetBenchPrompts)], "req", "")
					ResolveRoutingOverrides(ctx)
					router, err := rm.Select(ctx)
					if err != nil {
						b.Fatal(err)
					}
					if got := selectCandidates(ctx, router, all, 16); len(got) != 16 {
						b.Fatalf("got %d candidates", len(got))
					}
					ctx.Delete()
				}
			})
		}
	}
}

// BenchmarkShuffle compares the full-list shuffles: crypto/rand, which the gateway used
// before, and math/rand/v2, which it uses now.
func BenchmarkShuffle(b *testing.B) {
	for _, n := range fleetBenchSizes {
		pods := make([]*v1.Pod, n)
		b.Run(fmt.Sprintf("crypto/N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				utils.CryptoShuffle(pods)
			}
		})
		b.Run(fmt.Sprintf("math/N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				utils.Shuffle(pods)
			}
		})
	}
}
