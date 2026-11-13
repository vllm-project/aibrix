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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

func TestMeanStdDev(t *testing.T) {
	mean, stddev := meanStdDev([]float64{20, 25, 100})
	assert.InDelta(t, 48.333, mean, 0.01)
	assert.InDelta(t, 36.591, stddev, 0.01)

	mean, stddev = meanStdDev([]float64{5, 5, 5})
	assert.Equal(t, 5.0, mean)
	assert.Equal(t, 0.0, stddev)

	mean, stddev = meanStdDev(nil)
	assert.Equal(t, 0.0, mean)
	assert.Equal(t, 0.0, stddev)
}

// loadTable maps "model/pod" to a pod's engine running+waiting request count.
type loadTable map[string]float64

func (l loadTable) depth(pod *v1.Pod, model string) float64 { return l[model+"/"+pod.Name] }

func TestSweepLoadOutliers_BlacklistsStatisticalOutlier(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	src := fakePods{"test-model": {pod1, pod2, pod3}}

	// pod1: 20, pod2: 25, pod3: 100 -- pod3 is more than 2 standard
	// deviations above the mean of the other two, and well past the
	// absolute gap floor too.
	loads := loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 100}

	b.sweepLoadOutliers(src, loads.depth, nil)

	assert.False(t, b.IsBlacklisted(pod1))
	assert.False(t, b.IsBlacklisted(pod2))
	assert.True(t, b.IsBlacklisted(pod3), "pod3's load is >2 stddev above the mean and should be excluded")
}

func TestSweepLoadOutliers_ReincludesOnceConverged(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	src := fakePods{"test-model": {pod1, pod2, pod3}}
	var recovered []string
	onRecovered := func(pod *v1.Pod) { recovered = append(recovered, pod.Name) }

	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 100}.depth, onRecovered)
	assert.True(t, b.IsBlacklisted(pod3))
	assert.Empty(t, recovered, "nothing has recovered yet")

	// pod3's load converges back down on the next tick.
	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 22}.depth, onRecovered)
	assert.False(t, b.IsBlacklisted(pod3), "pod3 should be un-blacklisted once its load converges")
	assert.Equal(t, []string{"3"}, recovered, "the recovery callback fires once, for the pod that left exclusion")

	// A tick that changes nothing must not report a recovery again.
	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 22}.depth, onRecovered)
	assert.Equal(t, []string{"3"}, recovered)
}

func TestSweepLoadOutliers_RecoveryCallbackIsOptional(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	src := fakePods{"test-model": {pod1, pod2, pod3}}

	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 100}.depth, nil)
	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 25, "test-model/3": 22}.depth, nil)

	assert.False(t, b.IsBlacklisted(pod3))
}

func TestSweepLoadOutliers_SkipsModelsWithTooFewPods(t *testing.T) {
	var b PodBlacklist
	pod1, pod2 := blacklistTestPod("1"), blacklistTestPod("2")
	src := fakePods{"test-model": {pod1, pod2}}

	b.sweepLoadOutliers(src, func(*v1.Pod, string) float64 {
		t.Error("load must not be read for a model with too few replicas")
		return 0
	}, nil)

	assert.False(t, b.IsBlacklisted(pod1))
	assert.False(t, b.IsBlacklisted(pod2))
}

// sweepScoringTest sets up a single model with 3 pods reporting the given
// running+waiting loads, so the calibration tests below can focus on just the
// numbers.
func sweepScoringTest(loads [3]float64) (*PodBlacklist, PodSource, QueueDepthFunc, [3]*v1.Pod) {
	pods := [3]*v1.Pod{blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")}
	table := loadTable{}
	for i, pod := range pods {
		table["test-model/"+pod.Name] = loads[i]
	}
	return &PodBlacklist{}, fakePods{"test-model": {pods[0], pods[1], pods[2]}}, table.depth, pods
}

// TestSweepLoadOutliers_TiedPeersWithClearOutlier_Excludes covers the case a
// bare "stddev == 0 means skip" guard used to miss: two peers tied at the
// exact same load give a reference stddev of 0, but a third pod wildly above
// them (gap of 80, well past loadOutlierMinGap) is still a real outlier and
// must be excluded.
func TestSweepLoadOutliers_TiedPeersWithClearOutlier_Excludes(t *testing.T) {
	b, src, depth, pods := sweepScoringTest([3]float64{20, 20, 100})
	b.sweepLoadOutliers(src, depth, nil)

	assert.False(t, b.IsBlacklisted(pods[0]))
	assert.False(t, b.IsBlacklisted(pods[1]))
	assert.True(t, b.IsBlacklisted(pods[2]), "a pod 80 requests above two tied peers must be excluded even though the peers' stddev is 0")
}

// TestSweepLoadOutliers_SmallBlipOffIdleBaseline_NotExcluded covers the flip
// side: with an idle-fleet baseline of 0,0 the stddev-scaled threshold alone
// is 0, so without an absolute floor any positive gap -- including a single
// request -- would qualify. loadOutlierMinGap exists so this trivial blip
// isn't flagged.
func TestSweepLoadOutliers_SmallBlipOffIdleBaseline_NotExcluded(t *testing.T) {
	b, src, depth, pods := sweepScoringTest([3]float64{0, 0, 1})
	b.sweepLoadOutliers(src, depth, nil)

	for _, pod := range pods {
		assert.False(t, b.IsBlacklisted(pod), "a single request above an idle baseline must not be flagged as an outlier")
	}
}

// TestSweepLoadOutliers_SmallGapAtThreePods_NotExcluded covers small-n
// miscalibration: at n=3, leave-one-out scores each pod against just 2
// peers, whose population stddev is |a-b|/2 -- so the 2-sigma threshold
// alone sits only ~|a-b| above their mean, and ordinary variance between two
// healthy pods can cross it. loadOutlierMinGap keeps this gap (5.5) from
// being flagged.
func TestSweepLoadOutliers_SmallGapAtThreePods_NotExcluded(t *testing.T) {
	b, src, depth, pods := sweepScoringTest([3]float64{20, 25, 28})
	b.sweepLoadOutliers(src, depth, nil)

	for _, pod := range pods {
		assert.False(t, b.IsBlacklisted(pod), "an ordinary 3-pod variance must not be flagged as a 2-sigma outlier")
	}
}

// TestSweepLoadOutliers_NoCascadeAcrossRepeatedTicks guards against a
// miscalibrated detector's cascade failure mode: excluding one pod piles its
// share of traffic onto its peers, which (if the detector is too sensitive)
// makes one of *them* look like the next outlier, and so on until the whole
// replica set is excluded. Running the same borderline, ordinary-variance
// loads across several consecutive sweeps must never start that cascade.
func TestSweepLoadOutliers_NoCascadeAcrossRepeatedTicks(t *testing.T) {
	b, src, depth, pods := sweepScoringTest([3]float64{20, 25, 28})

	for tick := 0; tick < 3; tick++ {
		b.sweepLoadOutliers(src, depth, nil)

		for i, pod := range pods {
			assert.False(t, b.IsBlacklisted(pod), "tick %d: pod%d must not cascade into exclusion", tick, i+1)
		}
	}
}

// quarantinedPod is a pod the probe sweep found dead, so the cache no longer
// scrapes it and its last-reported load is frozen.
func quarantinedPod(t *testing.T, b *PodBlacklist, name string) *v1.Pod {
	t.Helper()
	pod := blacklistTestPod(name)
	failDead(b, pod, podProbeFailureIterations)
	assert.True(t, b.IsQuarantined(pod))
	return pod
}

// A quarantined pod's frozen load must not be one of the peers the live pods are
// scored against. Here pod3 is 40 above two idle peers, a clear outlier, but the
// 400 the dead pod last reported would pull its peer mean to ~133 and hide it.
func TestSweepLoadOutliers_QuarantinedPodIsNotAPeer(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	dead := quarantinedPod(t, &b, "dead")
	src := fakePods{"test-model": {pod1, pod2, pod3, dead}}
	loads := loadTable{"test-model/1": 0, "test-model/2": 0, "test-model/3": 40, "test-model/dead": 400}

	b.sweepLoadOutliers(src, loads.depth, nil)

	assert.True(t, b.IsBlacklisted(pod3), "the dead pod's frozen load must not mask a real outlier among the live pods")
	assert.False(t, b.IsBlacklisted(pod1))
	assert.False(t, b.IsBlacklisted(pod2))
}

// Outlier detection needs loadOutlierMinPods replicas to mean anything. A dead pod
// is not one of them, so two live pods and a quarantined one are not scored.
func TestSweepLoadOutliers_QuarantinedPodDoesNotCountTowardTheMinimumReplicas(t *testing.T) {
	var b PodBlacklist
	pod1, pod2 := blacklistTestPod("1"), blacklistTestPod("2")
	dead := quarantinedPod(t, &b, "dead")
	src := fakePods{"test-model": {pod1, pod2, dead}}

	b.sweepLoadOutliers(src, func(*v1.Pod, string) float64 {
		t.Error("load must not be read for a model with too few live replicas")
		return 0
	}, nil)

	assert.False(t, b.IsBlacklisted(pod1))
	assert.False(t, b.IsBlacklisted(pod2))
}

// The sweep has nothing current to say about a quarantined pod, so it neither
// sets nor clears its load exclusion, and does not report a recovery for it.
func TestSweepLoadOutliers_QuarantinedPodGetsNoVerdict(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	dead := blacklistTestPod("dead")
	b.SetLoadExcluded(dead, true)
	failDead(&b, dead, podProbeFailureIterations)
	require.True(t, b.IsQuarantined(dead))
	src := fakePods{"test-model": {pod1, pod2, pod3, dead}}
	// Its frozen load would read as perfectly ordinary, which would lift the exclusion.
	loads := loadTable{"test-model/1": 20, "test-model/2": 20, "test-model/3": 20, "test-model/dead": 20}
	var recovered []string

	b.sweepLoadOutliers(src, loads.depth, func(pod *v1.Pod) { recovered = append(recovered, pod.Name) })

	state := failureStateFor(t, &b, dead)
	state.mu.Lock()
	defer state.mu.Unlock()
	assert.True(t, state.loadExcluded, "a verdict from frozen metrics must not lift the load exclusion")
	assert.Empty(t, recovered)
}

// Only quarantine is left out. A load-excluded pod is still scraped and has to stay
// scored, or nothing would ever lift its exclusion.
func TestSweepLoadOutliers_LoadExcludedPodStaysScored(t *testing.T) {
	var b PodBlacklist
	pod1, pod2, pod3 := blacklistTestPod("1"), blacklistTestPod("2"), blacklistTestPod("3")
	src := fakePods{"test-model": {pod1, pod2, pod3}}
	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 20, "test-model/3": 100}.depth, nil)
	require.True(t, b.IsBlacklisted(pod3))
	require.False(t, b.IsQuarantined(pod3))

	b.sweepLoadOutliers(src, loadTable{"test-model/1": 20, "test-model/2": 20, "test-model/3": 20}.depth, nil)

	assert.False(t, b.IsBlacklisted(pod3), "a load-excluded pod must still be scored so it can recover")
}

// TestSweepLoadOutliers_ExclusionIsORAcrossModels covers a pod that belongs
// to more than one model's replica set (e.g. a base model and a LoRA adapter
// sharing the same pods). It is a clear outlier under model-a but reports a
// perfectly ordinary, tied load under model-b. Because SetLoadExcluded has
// no model of its own, the final verdict must be the OR of both -- applying
// each model's verdict as it's processed (last-write-wins) would let
// model-b's later "not an outlier" call silently clear the exclusion
// model-a's outlier verdict just set.
func TestSweepLoadOutliers_ExclusionIsORAcrossModels(t *testing.T) {
	shared := blacklistTestPod("shared")
	a2, a3 := blacklistTestPod("a2"), blacklistTestPod("a3")
	b2, b3 := blacklistTestPod("b2"), blacklistTestPod("b3")
	src := fakePods{
		"model-a": {shared, a2, a3},
		"model-b": {shared, b2, b3},
	}
	loads := loadTable{
		// Under model-a, shared is a clear outlier against a2/a3.
		"model-a/shared": 100, "model-a/a2": 20, "model-a/a3": 25,
		// Under model-b, shared is perfectly ordinary -- tied with its peers.
		"model-b/shared": 20, "model-b/b2": 20, "model-b/b3": 20,
	}

	var bl PodBlacklist
	bl.sweepLoadOutliers(src, loads.depth, nil)

	assert.True(t, bl.IsBlacklisted(shared), "shared pod is overloaded for model-a and must stay excluded regardless of model-b's ordinary verdict")
	assert.False(t, bl.IsBlacklisted(a2))
	assert.False(t, bl.IsBlacklisted(a3))
	assert.False(t, bl.IsBlacklisted(b2))
	assert.False(t, bl.IsBlacklisted(b3))
}
