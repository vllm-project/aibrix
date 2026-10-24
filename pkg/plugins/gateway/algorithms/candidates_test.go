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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/types"
)

type seedingRouter struct {
	seeds []*v1.Pod
}

func (r *seedingRouter) Route(*types.RoutingContext, types.PodList) (string, error) { return "", nil }

func (r *seedingRouter) SeedCandidates(_ *types.RoutingContext, _ types.PodList, max int) []*v1.Pod {
	if len(r.seeds) > max {
		return r.seeds[:max]
	}
	return r.seeds
}

func candidatePods(n int) []*v1.Pod {
	pods := make([]*v1.Pod, n)
	for i := range pods {
		pods[i] = makeLBPod(fmt.Sprintf("pod-%03d", i), fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	return pods
}

func candidateCtx(t *testing.T, resolved string) *types.RoutingContext {
	ctx := types.NewRoutingContext(context.Background(), types.RoutingAlgorithm(resolved), "m1", "hello", "req", "")
	t.Cleanup(ctx.Delete)
	ctx.ResolvedStrategy = resolved
	return ctx
}

func requireDistinctSubset(t *testing.T, got, ready []*v1.Pod) {
	t.Helper()
	inReady := make(map[*v1.Pod]bool, len(ready))
	for _, pod := range ready {
		inReady[pod] = true
	}
	seen := make(map[*v1.Pod]bool, len(got))
	for _, pod := range got {
		require.True(t, inReady[pod], "candidate %s is not a ready pod", pod.Name)
		require.False(t, seen[pod], "candidate %s appears twice", pod.Name)
		seen[pod] = true
	}
}

func TestSelectCandidates_KeepsAllPodsWhenNotApplicable(t *testing.T) {
	pods := candidatePods(40)
	router := &seedingRouter{}
	cases := []struct {
		name     string
		resolved string
		k, m     int
	}{
		{"disabled", "least-request:1,load-balance:1", 0, 4},
		{"set would not shrink", "load-balance", 36, 4},
		{"global-view strategy", "vtc-basic", 8, 4},
		{"blend with a global-view strategy", "least-request:1,vtc-basic:1", 8, 4},
		{"session pin", "session-affinity", 8, 4},
		{"unparseable strategy", "", 8, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectCandidates(candidateCtx(t, tc.resolved), router, pods, tc.k, tc.m)
			assert.Equal(t, pods, got)
		})
	}
}

func TestSelectCandidates_SamplesKDistinctPodsPlusSeeds(t *testing.T) {
	pods := candidatePods(200)
	seeds := []*v1.Pod{pods[7], pods[150], pods[7]} // a duplicate seed counts once
	router := &seedingRouter{seeds: seeds}
	for trial := 0; trial < 200; trial++ {
		got := selectCandidates(candidateCtx(t, "prefix-cache:1,load-balance:1"), router, pods, 16, 4)
		requireDistinctSubset(t, got, pods)
		require.Len(t, got, 18, "16 random pods plus 2 distinct seeds")
		assert.Same(t, pods[7], got[0], "seeds come first")
		assert.Same(t, pods[150], got[1])
	}
}

func TestSelectCandidates_SeedsCappedAtM(t *testing.T) {
	pods := candidatePods(100)
	router := &seedingRouter{seeds: []*v1.Pod{pods[1], pods[2], pods[3], pods[4]}}
	got := selectCandidates(candidateCtx(t, "prefix-cache"), router, pods, 10, 2)
	requireDistinctSubset(t, got, pods)
	require.Len(t, got, 12)
	assert.Same(t, pods[1], got[0])
	assert.Same(t, pods[2], got[1])

	// M=0 turns seeding off.
	got = selectCandidates(candidateCtx(t, "prefix-cache"), router, pods, 10, 0)
	requireDistinctSubset(t, got, pods)
	require.Len(t, got, 10)
}

// TestSelectCandidates_LargeSample covers the branch that draws from a permutation when the
// sample is more than half of the ready pods.
func TestSelectCandidates_LargeSample(t *testing.T) {
	pods := candidatePods(30)
	router := &seedingRouter{seeds: []*v1.Pod{pods[0]}}
	for trial := 0; trial < 200; trial++ {
		got := selectCandidates(candidateCtx(t, "least-request"), router, pods, 25, 4)
		requireDistinctSubset(t, got, pods)
		require.Len(t, got, 26)
	}
}

// TestSelectCandidates_SamplesUniformly checks that every pod is about equally likely to be a
// candidate, so the stage does not bias load toward part of the fleet.
func TestSelectCandidates_SamplesUniformly(t *testing.T) {
	const n, k, trials = 50, 5, 20000
	pods := candidatePods(n)
	router := &seedingRouter{}
	counts := make(map[*v1.Pod]int, n)
	for trial := 0; trial < trials; trial++ {
		for _, pod := range selectCandidates(candidateCtx(t, "load-balance"), router, pods, k, 0) {
			counts[pod]++
		}
	}
	want := float64(trials*k) / n // 2000
	for _, pod := range pods {
		assert.InDelta(t, want, float64(counts[pod]), want*0.15, "pod %s", pod.Name)
	}
}
