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

package blacklist

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	ctrlconstants "github.com/vllm-project/aibrix/pkg/controller/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
)

// stormServiceTestPod is a ready pod rendered by a StormService.
func stormServiceTestPod(name string) *v1.Pod {
	pod := blacklistTestPod(name)
	pod.Labels = map[string]string{ctrlconstants.StormServiceNameLabelKey: "svc"}
	return pod
}

// pdRoleTestPod returns a ready pod, created by anything, that carries the given
// PD role label.
func pdRoleTestPod(role string) func(name string) *v1.Pod {
	return func(name string) *v1.Pod {
		pod := blacklistTestPod(name)
		pod.Labels = map[string]string{pd.RoleLabel: role}
		return pod
	}
}

// modelClaimTestPod is a ready warm pod with an engine bound by a ModelClaim.
func modelClaimTestPod(name string) *v1.Pod {
	pod := blacklistTestPod(name)
	pod.Annotations = map[string]string{
		constants.ModelClaimPodAnnotationPrefix + "claim-a": `{"model":"test-model","port":9001,"state":"active"}`,
	}
	return pod
}

// ignoredPodKinds are the kinds of pod the blacklist leaves alone. Each test
// that must hold for all of them runs over this list next to a plain pod as the
// control, so it fails if the pod were tracked.
var ignoredPodKinds = []struct {
	name string
	pod  func(name string) *v1.Pod
}{
	{"stormservice", stormServiceTestPod},
	{"modelclaim", modelClaimTestPod},
	{"pd-prefill", pdRoleTestPod(pd.RolePrefill)},
	{"pd-decode", pdRoleTestPod(pd.RoleDecode)},
}

func TestIgnoredPod(t *testing.T) {
	withAnnotation := blacklistTestPod("1")
	withAnnotation.Annotations = map[string]string{"example.com/other": "x"}
	emptyStormLabel := blacklistTestPod("2")
	emptyStormLabel.Labels = map[string]string{ctrlconstants.StormServiceNameLabelKey: ""}

	assert.False(t, ignoredPod(nil))
	assert.False(t, ignoredPod(blacklistTestPod("1")), "a plain pod is tracked")
	assert.False(t, ignoredPod(blacklistTestEnginePod("1", "vllm")), "an engine label alone does not make a pod ignored")
	assert.False(t, ignoredPod(withAnnotation), "an unrelated annotation does not make a pod ignored")
	assert.True(t, ignoredPod(stormServiceTestPod("1")))
	assert.True(t, ignoredPod(emptyStormLabel), "the label's presence marks the pod, whatever its value")
	assert.True(t, ignoredPod(modelClaimTestPod("1")))
	assert.True(t, ignoredPod(pdRoleTestPod(pd.RolePrefill)("1")), "a prefill pod is ignored whoever created it")
	assert.True(t, ignoredPod(pdRoleTestPod(pd.RoleDecode)("1")))
	assert.False(t, ignoredPod(pdRoleTestPod("other")("1")), "only the prefill and decode roles are ignored")
}

func TestFilterBlacklisted_KeepsIgnoredPods(t *testing.T) {
	b := &PodBlacklist{HealthCheck: alwaysUnhealthy}
	plain := blacklistTestPod("1")
	storm := stormServiceTestPod("2")
	claim := modelClaimTestPod("3")
	tripBreaker(b, plain)
	tripBreaker(b, storm)
	tripBreaker(b, claim)

	got := b.FilterBlacklisted([]*v1.Pod{plain, storm, claim})

	assert.Equal(t, []*v1.Pod{storm, claim}, got, "the tripped plain pod is dropped; the ignored pods are not")
	assert.Equal(t, int64(1), b.tracked.Load(), "only the plain pod has an entry")
}

func TestIgnoredPods_OtherEntryPointsCreateNoState(t *testing.T) {
	for _, kind := range ignoredPodKinds {
		t.Run(kind.name, func(t *testing.T) {
			b := &PodBlacklist{HealthCheck: alwaysUnhealthy}
			pod := kind.pod("1")

			assert.False(t, b.SetLoadExcluded(pod, true), "a load verdict is not applied")
			b.OnPodFlakyReAdd(pod)
			for i := 0; i < 2*podProbeFailureIterations; i++ {
				assert.False(t, b.RecordProbeResult(pod, ProbeDead, probeFailure), "a dead probe verdict is not applied")
			}

			assert.Zero(t, b.tracked.Load())
			assert.True(t, b.Allow(pod))
			assert.False(t, b.IsBlacklisted(pod))
			assert.False(t, b.IsQuarantined(pod))
		})
	}
}

func TestPodProbeTargets_SkipsIgnoredPods(t *testing.T) {
	plain := blacklistTestPod("1")
	src := fakePods{"test-model": {plain, stormServiceTestPod("2"), modelClaimTestPod("3")}}

	targets := (&PodBlacklist{}).podProbeTargets(src)

	require.Len(t, targets, 1)
	assert.Equal(t, plain, targets[0].pod)
}

func TestSweepPodProbes_NeverProbesOrBlacklistsIgnoredPods(t *testing.T) {
	plain := blacklistTestPod("1")
	storm := stormServiceTestPod("2")
	claim := modelClaimTestPod("3")
	var probesOfIgnored atomic.Int32
	b, src := newProbeSweep(func(addr string) bool {
		if !podIPIs("10.0.0.1")(addr) {
			probesOfIgnored.Add(1)
		}
		return false // every pod is dead
	}, nil, map[string][]*v1.Pod{"test-model": {plain, storm, claim}})

	for i := 0; i < podProbeFailureIterations; i++ {
		b.sweepPodProbes(src)
	}

	assert.True(t, b.IsBlacklisted(plain), "control: the plain pod that stays dead is blacklisted")
	assert.True(t, b.IsQuarantined(plain))
	assert.Zero(t, probesOfIgnored.Load(), "no probe is sent to an ignored pod")
	for _, pod := range []*v1.Pod{storm, claim} {
		assert.True(t, b.Allow(pod))
		assert.False(t, b.IsBlacklisted(pod))
		assert.False(t, b.IsQuarantined(pod))
	}
}

func TestSweepLoadOutliers_IgnoredPodIsNeverExcluded(t *testing.T) {
	loads := loadTable{"test-model/1": 20, "test-model/2": 20, "test-model/3": 20, "test-model/4": 100, "test-model/5": 100}

	t.Run("control: the same load on a plain pod is excluded", func(t *testing.T) {
		var b PodBlacklist
		plainOutlier := blacklistTestPod("4")
		src := fakePods{"test-model": {blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3"), plainOutlier}}

		b.sweepLoadOutliers(src, loads.depth, nil)

		assert.True(t, b.IsBlacklisted(plainOutlier))
	})

	for _, kind := range ignoredPodKinds {
		t.Run(kind.name, func(t *testing.T) {
			var b PodBlacklist
			ignoredOutlier := kind.pod("4")
			pods := []*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3"), ignoredOutlier}

			b.sweepLoadOutliers(fakePods{"test-model": pods}, loads.depth, nil)

			assert.False(t, b.IsBlacklisted(ignoredOutlier), "an ignored pod is not excluded however high its load")
			assert.Zero(t, b.tracked.Load())
			for _, pod := range pods[:3] {
				assert.False(t, b.IsBlacklisted(pod), "its peers are scored without it")
			}
		})
	}
}

// An ignored pod is not a peer: it does not count toward loadOutlierMinPods
// and its load is not part of the baseline the others are scored against.
func TestSweepLoadOutliers_IgnoredPodsAreNotPeers(t *testing.T) {
	t.Run("not counted toward the minimum replica count", func(t *testing.T) {
		var b PodBlacklist
		src := fakePods{"test-model": {blacklistTestPod("1"), blacklistTestPod("2"), stormServiceTestPod("3")}}

		b.sweepLoadOutliers(src, func(*v1.Pod, string) (float64, bool) {
			t.Error("load must not be read: two scorable pods are fewer than loadOutlierMinPods")
			return 0, false
		}, nil)

		assert.Zero(t, b.tracked.Load())
	})

	t.Run("not part of the baseline", func(t *testing.T) {
		// Pod 3 is 20 above its two plain peers, past the minimum gap. Counted as
		// peers, the two ignored pods at 100 would lift its baseline to 60 and
		// hide it.
		var b PodBlacklist
		loads := loadTable{"test-model/1": 20, "test-model/2": 20, "test-model/3": 40, "test-model/4": 100, "test-model/5": 100}
		outlier := blacklistTestPod("3")
		src := fakePods{"test-model": {blacklistTestPod("1"), blacklistTestPod("2"), outlier, stormServiceTestPod("4"), modelClaimTestPod("5")}}

		b.sweepLoadOutliers(src, loads.depth, nil)

		assert.True(t, b.IsBlacklisted(outlier))
	})
}
