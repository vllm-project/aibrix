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

package trafficdrain

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

type podOpt func(*v1.Pod)

// testPod returns a scheduled, running, routable pod owned by ReplicaSet rs-1 that became
// ready at readyAt and was created a minute before.
func testPod(name string, readyAt time.Time, opts ...podOpt) *v1.Pod {
	isController := true
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			UID:               types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(readyAt.Add(-time.Minute)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "llama-7d9c", UID: "rs-1", Controller: &isController,
			}},
		},
		Spec: v1.PodSpec{NodeName: "node-a"},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []v1.PodCondition{{
				Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: metav1.NewTime(readyAt),
			}},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func notReady(p *v1.Pod) { p.Status.Conditions = nil }
func noOwner(p *v1.Pod)  { p.OwnerReferences = nil }
func draining(p *v1.Pod) {
	p.Annotations = map[string]string{constants.PodDrainingAnnotationKey: "true"}
}
func noPodIP(p *v1.Pod) { p.Status.PodIP = "" }
func terminating(p *v1.Pod) {
	ts := metav1.NewTime(testNow.Add(-time.Second))
	p.DeletionTimestamp = &ts
}
func onNode(n string) podOpt    { return func(p *v1.Pod) { p.Spec.NodeName = n } }
func withUID(u string) podOpt   { return func(p *v1.Pod) { p.UID = types.UID(u) } }
func ownedBy(uid string) podOpt { return func(p *v1.Pod) { p.OwnerReferences[0].UID = types.UID(uid) } }
func deletionCostOf(v string) podOpt {
	return func(p *v1.Pod) { p.Annotations = map[string]string{podDeletionCostAnnotation: v} }
}

func names(pods []*v1.Pod) []string {
	out := make([]string, 0, len(pods))
	for _, p := range pods {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

func candidates(t *testing.T, pods []*v1.Pod, k int, horizon time.Duration) Result {
	t.Helper()
	p, ok := Get(ControllerOrderPolicy)
	require.True(t, ok)
	return p.Candidates(Request{Pods: pods, K: k, Now: testNow, Horizon: horizon})
}

// The cases of TestSortingActivePodsWithRanks in Kubernetes' pkg/controller/controller_utils_test.go.
func TestCompareFollowsKubernetesSortCases(t *testing.T) {
	now := testNow
	then1Month := now.AddDate(0, -1, 0)
	then2Hours := now.Add(-2 * time.Hour)
	then5Hours := now.Add(-5 * time.Hour)
	then8Hours := now.Add(-8 * time.Hour)
	var zeroTime time.Time
	restartAlways := v1.ContainerRestartPolicyAlways
	pod := func(name, node string, phase v1.PodPhase, ready bool, restarts, sideRestarts int32,
		readySince, created time.Time, annotations map[string]string) *v1.Pod {
		var conditions []v1.PodCondition
		var containerStatuses, initContainerStatuses []v1.ContainerStatus
		if ready {
			conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue, LastTransitionTime: metav1.NewTime(readySince)}}
			containerStatuses = []v1.ContainerStatus{{RestartCount: restarts}}
			initContainerStatuses = []v1.ContainerStatus{{Name: "sidecar", RestartCount: sideRestarts}}
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created), Name: name, Annotations: annotations},
			Spec:       v1.PodSpec{NodeName: node, InitContainers: []v1.Container{{Name: "sidecar", RestartPolicy: &restartAlways}}},
			Status: v1.PodStatus{
				Conditions: conditions, ContainerStatuses: containerStatuses,
				InitContainerStatuses: initContainerStatuses, Phase: phase,
			},
		}
	}
	var (
		unscheduledPod                      = pod("unscheduled", "", v1.PodPending, false, 0, 0, zeroTime, zeroTime, nil)
		scheduledPendingPod                 = pod("pending", "node", v1.PodPending, false, 0, 0, zeroTime, zeroTime, nil)
		unknownPhasePod                     = pod("unknown-phase", "node", v1.PodUnknown, false, 0, 0, zeroTime, zeroTime, nil)
		runningNotReadyPod                  = pod("not-ready", "node", v1.PodRunning, false, 0, 0, zeroTime, zeroTime, nil)
		runningReadyNoLastTransitionTimePod = pod("ready-no-ltt", "node", v1.PodRunning, true, 0, 0, zeroTime, zeroTime, nil)
		runningReadyNow                     = pod("ready-now", "node", v1.PodRunning, true, 0, 0, now, now, nil)
		runningReadyThen                    = pod("ready-then", "node", v1.PodRunning, true, 0, 0, then1Month, then1Month, nil)
		runningReadyNowHighRestarts         = pod("ready-high-restarts", "node", v1.PodRunning, true, 9001, 0, now, now, nil)
		runningReadyNowHighSideRestarts     = pod("ready-high-side-restarts", "node", v1.PodRunning, true, 9001, 9001, now, now, nil)
		runningReadyNowCreatedThen          = pod("ready-now-created-then", "node", v1.PodRunning, true, 0, 0, now, then1Month, nil)
		lowPodDeletionCost                  = pod("low-deletion-cost", "node", v1.PodRunning, true, 0, 0, now, then1Month,
			map[string]string{podDeletionCostAnnotation: "10"})
		highPodDeletionCost = pod("high-deletion-cost", "node", v1.PodRunning, true, 0, 0, now, then1Month,
			map[string]string{podDeletionCostAnnotation: "100"})
		unscheduled5Hours = pod("unscheduled-5-hours", "", v1.PodPending, false, 0, 0, then5Hours, then5Hours, nil)
		unscheduled8Hours = pod("unscheduled-8-hours", "", v1.PodPending, false, 0, 0, then8Hours, then8Hours, nil)
		ready2Hours       = pod("ready-2-hours", "", v1.PodRunning, true, 0, 0, then2Hours, then1Month, nil)
		ready5Hours       = pod("ready-5-hours", "", v1.PodRunning, true, 0, 0, then5Hours, then1Month, nil)
		ready8Hours       = pod("ready-8-hours", "", v1.PodRunning, true, 0, 0, then8Hours, then1Month, nil)
	)

	equal := []struct{ p1, p2 *v1.Pod }{
		{p1: unscheduledPod},
		{p1: scheduledPendingPod},
		{p1: unknownPhasePod},
		{p1: runningNotReadyPod},
		{p1: runningReadyNowCreatedThen},
		{p1: runningReadyNow},
		{p1: runningReadyThen},
		{p1: runningReadyNowHighRestarts},
		{p1: unscheduled5Hours, p2: unscheduled8Hours},
		{p1: ready5Hours, p2: ready8Hours},
		// Kubernetes 1.32+ puts the pod with more sidecar restarts first and 1.31 does not
		// compare them, so the policy treats the pair as unpredictable.
		{p1: runningReadyNowHighSideRestarts, p2: runningReadyNowHighRestarts},
	}
	for i, tc := range equal {
		p2 := tc.p2
		if p2 == nil {
			p2 = tc.p1
		}
		k1, k2 := newPodKey(tc.p1, 1), newPodKey(p2, 1)
		assert.Zero(t, compare(k1, k2, now), "equality case %d: %s vs %s", i, tc.p1.Name, p2.Name)
		assert.Zero(t, compare(k2, k1, now), "equality case %d: %s vs %s", i, p2.Name, tc.p1.Name)
	}

	type withRank struct {
		pod  *v1.Pod
		rank int
	}
	ordered := []struct{ first, second withRank }{
		{first: withRank{unscheduledPod, 1}, second: withRank{scheduledPendingPod, 2}},
		{first: withRank{unscheduledPod, 2}, second: withRank{scheduledPendingPod, 1}},
		{first: withRank{scheduledPendingPod, 1}, second: withRank{unknownPhasePod, 2}},
		{first: withRank{unknownPhasePod, 1}, second: withRank{runningNotReadyPod, 2}},
		{first: withRank{runningNotReadyPod, 1}, second: withRank{runningReadyNoLastTransitionTimePod, 1}},
		{first: withRank{runningReadyNoLastTransitionTimePod, 1}, second: withRank{runningReadyNow, 1}},
		{first: withRank{runningReadyNow, 2}, second: withRank{runningReadyNoLastTransitionTimePod, 1}},
		{first: withRank{runningReadyNow, 1}, second: withRank{runningReadyThen, 1}},
		{first: withRank{runningReadyNow, 2}, second: withRank{runningReadyThen, 1}},
		{first: withRank{runningReadyNowHighRestarts, 1}, second: withRank{runningReadyNow, 1}},
		{first: withRank{runningReadyNow, 2}, second: withRank{runningReadyNowHighRestarts, 1}},
		{first: withRank{runningReadyNow, 1}, second: withRank{runningReadyNowCreatedThen, 1}},
		{first: withRank{runningReadyNowCreatedThen, 2}, second: withRank{runningReadyNow, 1}},
		{first: withRank{lowPodDeletionCost, 2}, second: withRank{highPodDeletionCost, 1}},
		{first: withRank{ready2Hours, 1}, second: withRank{ready5Hours, 1}},
	}
	for i, tc := range ordered {
		first, second := newPodKey(tc.first.pod, tc.first.rank), newPodKey(tc.second.pod, tc.second.rank)
		assert.Equal(t, -1, compare(first, second, now),
			"case %d: %s (rank %d) should go before %s (rank %d)", i, tc.first.pod.Name, tc.first.rank, tc.second.pod.Name, tc.second.rank)
		assert.Equal(t, 1, compare(second, first, now),
			"case %d: %s (rank %d) should go after %s (rank %d)", i, tc.second.pod.Name, tc.second.rank, tc.first.pod.Name, tc.first.rank)
	}
}

func TestDeletionCostParsing(t *testing.T) {
	for v, want := range map[string]int32{
		"10": 10, "-3": -3, "0": 0, "": 0, "+10": 0, "008": 0, "abc": 0, "99999999999": 0,
	} {
		p := testPod("p", testNow, deletionCostOf(v))
		assert.Equal(t, want, deletionCost(p), "annotation %q", v)
	}
	assert.Equal(t, int32(0), deletionCost(testPod("p", testNow)))
}

func TestControllerOrderPicksThePodTheReplicaSetDeletes(t *testing.T) {
	twoDays := testNow.Add(-48 * time.Hour)
	tests := []struct {
		name      string
		pods      []*v1.Pod
		k         int
		wantNext  []string
		wantDrain []string
	}{
		{
			// The example on #3187: A and B ready for two days, C added by a scale-out 20 minutes ago.
			name:      "newest ready pod",
			pods:      []*v1.Pod{testPod("a", twoDays), testPod("b", twoDays.Add(time.Hour)), testPod("c", testNow.Add(-20*time.Minute))},
			k:         1,
			wantNext:  []string{"c"},
			wantDrain: []string{"c"},
		},
		{
			name: "terminating and finished pods are not counted",
			pods: []*v1.Pod{
				testPod("a", twoDays.Add(-72*time.Hour)), testPod("b", twoDays),
				testPod("c", testNow.Add(-20*time.Minute), terminating),
				testPod("d", testNow.Add(-10*time.Minute), func(p *v1.Pod) { p.Status.Phase = v1.PodSucceeded }),
				testPod("e", twoDays.Add(-24*time.Hour)),
			},
			k:         1,
			wantNext:  []string{"b"},
			wantDrain: []string{"b"},
		},
		{
			name: "a not-ready pod goes first and needs no drain",
			pods: []*v1.Pod{
				testPod("a", twoDays), testPod("b", twoDays.Add(-72*time.Hour)),
				testPod("c", testNow.Add(-20*time.Minute)), testPod("d", testNow.Add(-5*time.Minute), notReady),
			},
			k:        1,
			wantNext: []string{"d"},
		},
		{
			name: "two removals: the not-ready pod, then the newest ready pod",
			pods: []*v1.Pod{
				testPod("a", twoDays), testPod("b", twoDays.Add(-72*time.Hour)),
				testPod("c", testNow.Add(-20*time.Minute)), testPod("d", testNow.Add(-5*time.Minute), notReady),
			},
			k:         2,
			wantNext:  []string{"c", "d"},
			wantDrain: []string{"c"},
		},
		{
			name: "pods sharing a node go before a newer pod alone on its node",
			pods: []*v1.Pod{
				testPod("a", testNow.Add(-96*time.Hour), onNode("node-a")),
				testPod("b", testNow.Add(-24*time.Hour), onNode("node-a")),
				testPod("c", testNow.Add(-20*time.Minute), onNode("node-b")),
			},
			k:         1,
			wantNext:  []string{"b"},
			wantDrain: []string{"b"},
		},
		{
			name: "a lower pod-deletion-cost goes first",
			pods: []*v1.Pod{
				testPod("a", testNow.Add(-120*time.Hour), deletionCostOf("-1")),
				testPod("b", twoDays), testPod("c", testNow.Add(-20*time.Minute)),
			},
			k:         1,
			wantNext:  []string{"a"},
			wantDrain: []string{"a"},
		},
		{
			name: "a tie among pods the controller keeps does not matter",
			pods: []*v1.Pod{
				testPod("a", twoDays), testPod("b", twoDays),
				testPod("c", testNow.Add(-20*time.Minute)),
			},
			k:         1,
			wantNext:  []string{"c"},
			wantDrain: []string{"c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := candidates(t, tc.pods, tc.k, 15*time.Minute)
			require.Empty(t, res.Skip)
			assert.Equal(t, tc.wantNext, names(res.Next))
			if tc.wantDrain == nil {
				tc.wantDrain = []string{}
			}
			assert.Equal(t, tc.wantDrain, names(res.Drain))
		})
	}
}

func TestControllerOrderSkips(t *testing.T) {
	twoDays := testNow.Add(-48 * time.Hour)
	recent := testNow.Add(-20 * time.Minute)
	// x became ready 100 s ago and y 150 s ago, so x is in the newer age bucket now. 37 s from
	// now x reaches y's bucket, and y's lower UID puts y first from then on.
	x := testPod("x", testNow.Add(-100*time.Second), withUID("uid-2"))
	y := testPod("y", testNow.Add(-150*time.Second), withUID("uid-1"))
	tests := []struct {
		name    string
		pods    []*v1.Pod
		k       int
		horizon time.Duration
		want    SkipReason
	}{
		{
			name: "pods of two ReplicaSets (a rollout)",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", twoDays), testPod("c", recent, ownedBy("rs-2"))},
			k:    1, want: SkipOwners,
		},
		{
			name: "a pod with no controller",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", twoDays), testPod("c", recent, noOwner)},
			k:    1, want: SkipOwners,
		},
		{
			name: "not a Deployment",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", recent, func(p *v1.Pod) { p.OwnerReferences[0].Kind = "RoleSet" })},
			k:    1, want: SkipOwners,
		},
		{
			name: "one routable pod",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", recent, notReady)},
			k:    1, want: SkipTooFewPods,
		},
		{
			name: "the scale-in removes every active pod",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", recent)},
			k:    2, want: SkipTooFewPods,
		},
		{
			name: "the scale-in removes every routable pod",
			pods: []*v1.Pod{testPod("a", twoDays.Add(-48*time.Hour), noPodIP), testPod("b", twoDays), testPod("c", recent)},
			k:    2, want: SkipTooFewPods,
		},
		{
			name: "no pods",
			k:    1, want: SkipTooFewPods,
		},
		{
			name: "two newest pods ready and created in the same second",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", recent), testPod("c", recent)},
			k:    1, want: SkipTie,
		},
		{
			name: "the order changes within the horizon",
			pods: []*v1.Pod{testPod("a", twoDays), x, y},
			k:    1, horizon: 15 * time.Minute, want: SkipOrderChanges,
		},
		{
			// x moved into y's bucket 0.56 s ago (2^37 ns = 137.44 s), so a controller whose
			// clock is a second behind still deletes x, and one on time deletes y.
			name: "the order changed within the clock-skew allowance",
			pods: []*v1.Pod{
				testPod("a", twoDays),
				testPod("x", testNow.Add(-138*time.Second), withUID("uid-2")),
				testPod("y", testNow.Add(-150*time.Second), withUID("uid-1")),
			},
			k: 1, horizon: 10 * time.Second, want: SkipOrderChanges,
		},
		{
			name: "the predicted pod is already draining",
			pods: []*v1.Pod{testPod("a", twoDays), testPod("b", twoDays.Add(-time.Hour)), testPod("c", recent, draining)},
			k:    1, want: SkipDraining,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.horizon == 0 {
				tc.horizon = 15 * time.Minute
			}
			res := candidates(t, tc.pods, tc.k, tc.horizon)
			assert.Equal(t, tc.want, res.Skip)
			assert.Empty(t, res.Next)
			assert.Empty(t, res.Drain)
		})
	}

	// With a horizon that ends before x reaches y's bucket, x is a safe prediction.
	res := candidates(t, []*v1.Pod{testPod("a", twoDays), x, y}, 1, 10*time.Second)
	require.Empty(t, res.Skip)
	assert.Equal(t, []string{"x"}, names(res.Next))

	// K of zero asks for nothing.
	res = candidates(t, []*v1.Pod{testPod("a", twoDays), testPod("b", recent)}, 0, time.Minute)
	assert.Empty(t, res.Skip)
	assert.Empty(t, res.Next)
}

// controllerSort is the controller's sort.Sort over ActivePodsWithRanks, built on compare.
type controllerSort struct {
	keys []*podKey
	now  time.Time
}

func (s controllerSort) Len() int      { return len(s.keys) }
func (s controllerSort) Swap(i, j int) { s.keys[i], s.keys[j] = s.keys[j], s.keys[i] }
func (s controllerSort) Less(i, j int) bool {
	return compare(s.keys[i], s.keys[j], s.now) < 0
}

// randomGroup builds the pods of one Deployment with the collisions real fleets have: pods
// from one scale-out that were created or became ready in the same second, pods sharing a
// node, unscheduled and not-ready pods, restarts and deletion costs.
func randomGroup(r *rand.Rand) []*v1.Pod {
	n := 2 + r.IntN(10)
	pods := make([]*v1.Pod, 0, n)
	readyTimes := make([]time.Time, 0, n)
	for i := 0; i < n; i++ {
		var readyAt time.Time
		switch {
		case i > 0 && r.IntN(4) == 0: // same scale-out as an earlier pod
			readyAt = readyTimes[r.IntN(i)]
		case r.IntN(2) == 0: // added recently
			readyAt = testNow.Add(-time.Duration(r.IntN(3600)) * time.Second)
		default:
			readyAt = testNow.Add(-time.Duration(r.IntN(14*24*3600)) * time.Second)
		}
		p := testPod(fmt.Sprintf("p%d", i), readyAt, withUID(fmt.Sprintf("%08x", r.Uint32())),
			onNode(fmt.Sprintf("node-%d", r.IntN(3))))
		if r.IntN(3) == 0 {
			p.CreationTimestamp = metav1.NewTime(readyAt.Add(-time.Duration(30+r.IntN(90)) * time.Second))
		}
		switch r.IntN(12) {
		case 0:
			p.Spec.NodeName = ""
			p.Status.Phase = v1.PodPending
			notReady(p)
		case 1:
			notReady(p)
		case 2:
			p.Status.ContainerStatuses = []v1.ContainerStatus{{RestartCount: int32(1 + r.IntN(2))}}
		case 3:
			p.Annotations = map[string]string{podDeletionCostAnnotation: fmt.Sprint(r.IntN(3) - 1)}
		}
		pods = append(pods, p)
		readyTimes = append(readyTimes, readyAt)
	}
	return pods
}

// Whenever the policy picks pods, the controller's sort must put exactly those pods first at
// every instant of the horizon, whatever order the pods come in. The test checks every bucket
// edge, the nanosecond before it, and random instants.
func TestControllerOrderPredictionHoldsOverTheHorizon(t *testing.T) {
	r := rand.New(rand.NewPCG(3187, 1))
	horizons := []time.Duration{0, time.Minute, 5 * time.Minute, 15 * time.Minute}
	picked, skipped := 0, map[SkipReason]int{}
	for g := 0; g < 3000; g++ {
		pods := randomGroup(r)
		k := 1 + r.IntN(len(pods))
		horizon := horizons[r.IntN(len(horizons))]
		res := candidates(t, pods, k, horizon)
		if res.Skip != "" {
			skipped[res.Skip]++
			continue
		}
		if k >= len(pods) {
			t.Fatalf("group %d: picked pods although K=%d removes all %d pods", g, k, len(pods))
		}
		picked++
		want := names(res.Next)
		require.Len(t, want, k)

		onNode := map[string]int{}
		for _, p := range pods {
			onNode[p.Spec.NodeName]++
		}
		keys := make([]*podKey, 0, len(pods))
		for _, p := range pods {
			keys = append(keys, newPodKey(p, onNode[p.Spec.NodeName]))
		}
		from, to := testNow.Add(-clockSkew), testNow.Add(horizon)
		var instants []time.Time
		for _, e := range orderChanges(keys, from, to) {
			instants = append(instants, e, e.Add(-time.Nanosecond))
		}
		for i := 0; i < 10; i++ {
			instants = append(instants, from.Add(time.Duration(r.Int64N(int64(to.Sub(from))+1))))
		}
		for _, at := range instants {
			if at.Before(from) {
				continue
			}
			shuffled := append([]*podKey(nil), keys...)
			r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			sort.Sort(controllerSort{keys: shuffled, now: at})
			deleted := make([]*v1.Pod, 0, k)
			for _, key := range shuffled[:k] {
				deleted = append(deleted, key.pod)
			}
			require.Equal(t, want, names(deleted), "group %d at %s", g, at.Sub(testNow))
		}
	}
	t.Logf("3000 random groups: picked %d, skipped %v", picked, skipped)
	assert.Greater(t, picked, 600, "too few groups picked for the check to mean much")
}

func TestPolicyRegistry(t *testing.T) {
	assert.Equal(t, []string{ControllerOrderPolicy}, Names())
	p, ok := Get(ControllerOrderPolicy)
	require.True(t, ok)
	assert.Equal(t, ControllerOrderPolicy, p.Name())
	_, ok = Get("deletion-cost")
	assert.False(t, ok)
}
