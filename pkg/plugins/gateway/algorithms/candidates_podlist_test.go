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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
)

type spyCandidateFinder struct {
	plainRouter
	findList  types.PodList
	scoreList types.PodList
}

func (s *spyCandidateFinder) Polarity() types.Polarity { return types.PolarityMost }
func (s *spyCandidateFinder) ScoreAll(_ *types.RoutingContext, ready types.PodList) ([]float64, []bool, error) {
	return s.CandidateScores(nil, ready)
}
func (s *spyCandidateFinder) CandidateScores(_ *types.RoutingContext, ready types.PodList) ([]float64, []bool, error) {
	s.scoreList = ready
	n := ready.Len()
	scores := make([]float64, n)
	scored := make([]bool, n)
	for i := range scored {
		scored[i] = true
	}
	return scores, scored, nil
}
func (s *spyCandidateFinder) FindCandidates(_ *types.RoutingContext, ready types.PodList, max int) []*v1.Pod {
	s.findList = ready
	return nil
}

func TestSelectCandidatesFromPodList_PreservesSnapshotSafely(t *testing.T) {
	raw := json.RawMessage(`{"candidate":{"k":8}}`)
	profile := &types.ResolvedConfigProfile{
		RoutingConfig: raw,
		Routing:       configprofiles.ParseRoutingConfig(raw),
	}
	newCtx := func() *types.RoutingContext {
		ctx := candidateCtx(t, "prefix-cache")
		ctx.ConfigProfile = profile
		ResolveRoutingOverrides(ctx)
		return ctx
	}

	t.Run("nil PodList returns nil", func(t *testing.T) {
		assert.Nil(t, SelectCandidatesFromPodList(newCtx(), nil, &spyCandidateFinder{}, nil, nil))
	})

	t.Run("large fleet hands cached PodArray to FindCandidates and fresh scout to CandidateScores", func(t *testing.T) {
		// K=8 -> 2*4K = 64; N=80 > 64 invokes FindCandidates.
		podArr := &utils.PodArray{Pods: candidatePods(80)}
		spy := &spyCandidateFinder{}
		got := SelectCandidatesFromPodList(newCtx(), nil, spy, podArr, nil)
		require.Len(t, got, 8)
		assert.Same(t, podArr, spy.findList, "FindCandidates must receive the cached *utils.PodArray snapshot")
		require.NotNil(t, spy.scoreList)
		assert.NotSame(t, podArr, spy.scoreList, "CandidateScores on a 4K scout must see only the scout")
		assert.Equal(t, 4*8, spy.scoreList.Len())
	})

	t.Run("small fleet hands cached PodArray to CandidateScores when unfiltered", func(t *testing.T) {
		// K=8 -> 2*4K = 64; N=20 <= 64 skips FindCandidates and scores the full fleet.
		podArr := &utils.PodArray{Pods: candidatePods(20)}
		spy := &spyCandidateFinder{}
		got := SelectCandidatesFromPodList(newCtx(), nil, spy, podArr, nil)
		require.Len(t, got, 8)
		assert.Nil(t, spy.findList)
		assert.Same(t, podArr, spy.scoreList, "unfiltered full-fleet CandidateScores should reuse cached *PodArray")
	})

	t.Run("small fleet drops PodArray reuse when filterScout excludes pods", func(t *testing.T) {
		podArr := &utils.PodArray{Pods: candidatePods(20)}
		spy := &spyCandidateFinder{}
		dropFirst := func(scout []*v1.Pod) []*v1.Pod {
			return scout[1:]
		}
		got := SelectCandidatesFromPodList(newCtx(), nil, spy, podArr, dropFirst)
		require.Len(t, got, 8)
		require.NotNil(t, spy.scoreList)
		assert.NotSame(t, podArr, spy.scoreList, "filtered scout must not hand the full cached PodArray to CandidateScores")
		assert.Equal(t, 19, spy.scoreList.Len())
	})
}
