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

package pd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tokenLoadTestPod(name string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
}

func TestTokenLoadPrefillPolicy_ScoresFromTracker(t *testing.T) {
	tracker := newTokenLoadTracker(TokenLoadConfig{KVWeight: 0.5, RequestCost: 0}, nil)
	policy := NewTokenLoadPrefillPolicy(tracker)
	assert.Equal(t, PrefillScorePolicyTokenLoad, policy.Name())
	assert.True(t, UsesTokenLoad(policy))
	assert.False(t, UsesTokenLoad(NewLeastRequestPrefillPolicy()))
	assert.False(t, UsesTokenLoad(nil))

	podA, podB := tokenLoadTestPod("pod-a"), tokenLoadTestPod("pod-b")
	tracker.AcquirePrefill("long", podA.Name, 8000)
	tracker.AcquirePrefill("short-1", podB.Name, 100)
	tracker.AcquirePrefill("short-2", podB.Name, 100)
	tracker.ReleaseTokens("short-1")

	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, testMessage, "req-1", "")
	scorer, err := policy.Prepare(ctx, []*v1.Pod{podA, podB}, map[string]struct{}{podA.Name: {}, podB.Name: {}})
	require.NoError(t, err)
	assert.Nil(t, scorer.PrefixHashes(), "token_load does not use the prefix cache")

	// Request counts passed by the router are ignored: only the token ledger
	// decides, and the pod with one long prompt scores worse than the pod
	// with two short ones.
	assert.Equal(t, float64(8000+0.5*8000), scorer.ScorePod(podA, 1, 2))
	assert.Equal(t, float64(100+0.5*200), scorer.ScorePod(podB, 2, 2))
	assert.Equal(t, float64(0), scorer.ScorePod(tokenLoadTestPod("idle"), 0, 2))
}

func TestTokenLoadPrefillPolicy_NilTrackerScoresZero(t *testing.T) {
	policy := NewTokenLoadPrefillPolicy(nil)
	ctx := types.NewRoutingContext(context.Background(), "pd", testModelName, testMessage, "req-1", "")
	scorer, err := policy.Prepare(ctx, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, float64(0), scorer.ScorePod(tokenLoadTestPod("pod-a"), 3, 3))
}
