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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

// A seed cap of one still retains partial and zero matches for random candidates.
// Changing the live index after seeding makes a second lookup observable: final
// scoring must keep the seeding result until the decision is cleared.
func TestSeededPrefixMatchSharesRawScores(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("kvSync=%v", kvSync), func(t *testing.T) {
			r, all := newSeededMatchRouter(t, 4, kvSync)
			prompt := strings.Repeat("shared conversation ", 64)
			addSeededMatch(t, r, all, all.Pods[0], prompt)
			addSeededMatch(t, r, all, all.Pods[1], prompt[:len(prompt)/2])
			ctx := newSeededMatchContext(prompt)
			defer ctx.Delete()
			before, _, err := r.ScoreAll(ctx, all)
			require.NoError(t, err)
			require.Equal(t, float64(100), before[0])
			require.Greater(t, before[1], float64(0))
			require.Less(t, before[1], float64(100))
			require.Zero(t, before[2])
			require.Nil(t, ctx.SeededPrefixMatch(), "ordinary scoring does not retain a snapshot")

			require.Equal(t, []*v1.Pod{all.Pods[0]}, r.SeedCandidates(ctx, all, 1))
			require.NotNil(t, ctx.SeededPrefixMatch())
			addSeededMatch(t, r, all, all.Pods[1], prompt)
			addSeededMatch(t, r, all, all.Pods[2], prompt)
			// Gate removal and shuffle can change the subset/order; informer updates
			// can also supply a different object for the same namespace/name.
			subset := &utils.PodArray{Pods: []*v1.Pod{all.Pods[2], all.Pods[1].DeepCopy()}}
			scores, scored, err := r.ScoreAll(ctx, subset)
			require.NoError(t, err)
			assert.Equal(t, []float64{before[2], before[1]}, scores, "raw percentages, not fleet-normalized scores")
			assert.Equal(t, []bool{true, true}, scored, "zero prefix matches remain scored")
			ctx.SetSeededPrefixMatch(nil)
			scores, _, err = r.ScoreAll(ctx, subset)
			require.NoError(t, err)
			assert.Equal(t, []float64{100, 100}, scores, "the next decision sees live index updates")
		})
	}
}

func TestSeededPrefixMatchPreservesAdmissionAndPrefixCredit(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("kvSync=%v", kvSync), func(t *testing.T) {
			r, all := newSeededMatchRouter(t, 3, kvSync)
			prompt := strings.Repeat("shared conversation ", 64)
			addSeededMatch(t, r, all, all.Pods[0], prompt)
			ctx := newSeededMatchContext(prompt)
			defer ctx.Delete()
			r.SeedCandidates(ctx, all, 1)
			require.NotNil(t, ctx.SeededPrefixMatch())
			// The matched pod was filtered out; the remaining pod must not inherit
			// its match, and is credited only if it admits the request.
			remaining := &utils.PodArray{Pods: all.Pods[1:2]}
			denied := errors.New("admission denied")
			ctx.SetTargetAdmitter(func(*v1.Pod) error { return denied })
			_, err := r.Route(ctx, remaining)
			require.ErrorIs(t, err, denied)
			fresh := newSeededMatchContext(prompt)
			defer fresh.Delete()
			scores, _, err := r.ScoreAll(fresh, remaining)
			require.NoError(t, err)
			assert.Equal(t, []float64{0}, scores, "failed admission must not add a prefix")
			ctx.SetTargetAdmitter(nil)
			_, err = r.Route(ctx, remaining)
			require.NoError(t, err)
			scores, _, err = r.ScoreAll(fresh, remaining)
			require.NoError(t, err)
			assert.Equal(t, []float64{100}, scores, "successful routing still adds the full prefix")
		})
	}
}

func TestSeededPrefixMatchCoverageAndIdentity(t *testing.T) {
	ctx := newSeededMatchContext("prompt")
	defer ctx.Delete()
	pods := []*v1.Pod{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a"}}, {ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b"}}}
	index := &struct{ id int }{1}
	calls := 0
	read := func(ready map[string]struct{}) (map[string]int, []uint64) {
		calls++
		out := map[string]int{}
		if _, ok := ready["ns/a"]; ok {
			out["ns/a"] = 60
		}
		return out, []uint64{1, 2}
	}
	matchPrefix(ctx, pods, index, "model", -1, true, read)
	stored := ctx.SeededPrefixMatch()
	matched, hashes := matchPrefix(ctx, []*v1.Pod{pods[1].DeepCopy()}, index, "model", -1, false, read)
	assert.Empty(t, matched, "a zero-match pod is covered")
	assert.Equal(t, []uint64{1, 2}, hashes)
	assert.Equal(t, 1, calls)
	matched, _ = matchPrefix(ctx, pods[:1], index, "model", -1, false, read)
	matched["ns/a"] = 99
	assert.Equal(t, 60, stored.Matches["ns/a"], "returned projections cannot mutate the retained map")
	for _, tc := range []struct {
		name   string
		index  any
		model  string
		loraID int64
		pods   []*v1.Pod
	}{
		{"other index", &struct{ id int }{2}, "model", -1, pods},
		{"other model", index, "other", -1, pods},
		{"other adapter", index, "model", 7, pods},
		{"uncovered pod", index, "model", -1, append(append([]*v1.Pod(nil), pods...), &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "a"}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			matchPrefix(ctx, tc.pods, tc.index, tc.model, tc.loraID, false, read)
			assert.Equal(t, before+1, calls)
			assert.Same(t, stored, ctx.SeededPrefixMatch(), "fallback reads never extend the snapshot")
		})
	}
}

type seededMatchTokenizer struct{ fail bool }

func (t *seededMatchTokenizer) TokenizeInputText(text string) ([]byte, error) {
	if t.fail {
		return nil, errors.New("tokenization failed")
	}
	return tokenizer.NewCharacterTokenizer().TokenizeInputText(text)
}

func TestSeededPrefixMatchDoesNotCacheFailures(t *testing.T) {
	for _, kvSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("kvSync=%v", kvSync), func(t *testing.T) {
			r, pods := newSeededMatchRouter(t, 3, kvSync)
			tok := &seededMatchTokenizer{fail: true}
			if kvSync {
				r.kvSyncRouter.tokenizerPool = &mockTokenizerPool{tokenizer: tok}
			} else {
				r.tokenizer = tok
			}
			ctx := newSeededMatchContext("hello prefix")
			defer ctx.Delete()
			assert.Empty(t, r.SeedCandidates(ctx, pods, 2))
			require.Nil(t, ctx.SeededPrefixMatch())
			tok.fail = false
			assert.Empty(t, r.SeedCandidates(ctx, pods, 2), "a successful all-miss lookup seeds no pods")
			require.NotNil(t, ctx.SeededPrefixMatch(), "successful misses are retained, unlike failures")
			addSeededMatch(t, r, pods, pods.Pods[0], "hello prefix")
			scores, _, err := r.ScoreAll(ctx, pods)
			require.NoError(t, err)
			assert.Equal(t, []float64{0, 0, 0}, scores, "all-miss results also avoid another lookup")
		})
	}
}
