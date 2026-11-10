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

package prefixcache

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// Keep the fixture in this file so the same benchmark can run on pre-reuse revisions.
func newSeededMatchRouter(tb testing.TB, n int, kvSync bool) (*prefixCacheRouter, *utils.PodArray) {
	tb.Helper()
	require.NoError(tb, initializePrefixCacheMetrics())
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("seed-pod-%04d", i), Namespace: "default",
				Labels: map[string]string{constants.ModelLabelName: "m1", constants.ModelLabelPort: "8000"}},
			Status: v1.PodStatus{PodIP: fmt.Sprintf("10.0.%d.%d", i/256, i%256)},
		}
	}
	tok := tokenizer.NewCharacterTokenizer()
	c := cache.NewWithPodsForTest(pods, "m1")
	tb.Cleanup(c.Close)
	r := &prefixCacheRouter{cache: c, tokenizer: tok}
	if kvSync {
		r.kvSyncRouter = &kvSyncPrefixCacheRouter{cache: c, tokenizerPool: &mockTokenizerPool{tokenizer: tok}, syncIndexer: syncindexer.NewSyncPrefixHashTable()}
		tb.Cleanup(r.kvSyncRouter.syncIndexer.Close)
	} else {
		r.prefixCacheIndexer = prefixcacheindexer.NewPrefixHashTable()
	}
	return r, &utils.PodArray{Pods: pods}
}

func newSeededMatchContext(prompt string) *types.RoutingContext {
	return types.NewRoutingContext(context.Background(), RouterPrefixCache, "m1", prompt, "seed-match", "")
}

func addSeededMatch(tb testing.TB, r *prefixCacheRouter, pods types.PodList, pod *v1.Pod, prompt string) {
	tb.Helper()
	ctx := newSeededMatchContext(prompt)
	defer ctx.Delete()
	require.NoError(tb, r.PostRouteUpdate(ctx, pods, pod))
}

// Tokens are pre-cached: this measures the repeated prefix matching after
// seeding, rather than the tokenization that both versions already share.
func BenchmarkPrefixCandidateScores(b *testing.B) {
	for _, kvSync := range []bool{false, true} {
		for _, n := range []int{50, 500, 2000} {
			r, all := newSeededMatchRouter(b, n, kvSync)
			prompt := strings.Repeat("shared conversation ", 64)
			for _, pod := range all.Pods[:16] {
				addSeededMatch(b, r, all, pod, prompt)
			}
			setup := newSeededMatchContext(prompt)
			_, _, err := r.ScoreAll(setup, all)
			if err != nil {
				b.Fatal(err)
			}
			tokens := append([]byte(nil), setup.PrefixTokens()...)
			setup.Delete()
			for _, mode := range []string{"estimate", "estimate-and-score", "full-score"} {
				b.Run(fmt.Sprintf("kvSync=%v/N=%d/%s", kvSync, n, mode), func(b *testing.B) {
					b.ReportAllocs()
					candidates := &utils.PodArray{Pods: all.Pods[:32]}
					for i := 0; i < b.N; i++ {
						ctx := newSeededMatchContext(prompt)
						ctx.SetPrefixTokens(tokens)
						if mode == "full-score" {
							if _, _, err := r.ScoreAll(ctx, all); err != nil {
								b.Fatal(err)
							}
						} else {
							if _, _, err := r.CandidateScores(ctx, all); err != nil {
								b.Fatal(err)
							}
							if mode == "estimate-and-score" {
								if _, _, err := r.ScoreAll(ctx, candidates); err != nil {
									b.Fatal(err)
								}
							}
						}
						ctx.Delete()
					}
				})
			}
		}
	}
}
