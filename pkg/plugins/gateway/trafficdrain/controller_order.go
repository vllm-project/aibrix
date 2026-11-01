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
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/vllm-project/aibrix/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ControllerOrderPolicy is the name of the policy that drains the pods the workload
// controller will delete next.
const ControllerOrderPolicy = "controller-order"

const (
	// podDeletionCostAnnotation is Kubernetes' pod-deletion-cost annotation (beta and on by
	// default since Kubernetes 1.22).
	podDeletionCostAnnotation = "controller.kubernetes.io/pod-deletion-cost"

	// clockSkew starts the checked window a little before Now, so that a controller whose
	// clock runs slightly behind the gateway's still sees the predicted order.
	clockSkew = 2 * time.Second
)

func init() {
	register(controllerOrder{})
}

// controllerOrder predicts the pods the ReplicaSet controller deletes when a Deployment
// scales in by K. The controller sorts the ReplicaSet's active pods with
// ActivePodsWithRanks.Less (k8s.io/kubernetes pkg/controller/controller_utils.go) and
// deletes the first K (getPodsToDelete in pkg/controller/replicaset/replica_set.go).
// compare below mirrors that sort.
//
// The controller sorts with its own clock when it scales in, possibly minutes after the
// gateway decided, and it compares ready and creation times in power-of-two age buckets, so
// the order changes as pods age. The policy sorts the pods at Now-clockSkew, then checks at
// every bucket edge up to Now+Horizon that the same K pods are still each strictly ahead of
// every other pod. Otherwise it skips the round.
//
// Only Deployments are covered: every active pod of the model must be owned by one
// ReplicaSet. StormService roles are deleted by the RoleSet controller in its own order.
type controllerOrder struct{}

func (controllerOrder) Name() string { return ControllerOrderPolicy }

func (controllerOrder) Candidates(req Request) Result {
	if req.K <= 0 {
		return Result{}
	}
	active := make([]*v1.Pod, 0, len(req.Pods))
	routable := 0
	for _, p := range req.Pods {
		if !utils.IsPodActive(p) {
			continue
		}
		active = append(active, p)
		if utils.FilterReadyPod(p) {
			routable++
		}
	}
	if len(active) == 0 {
		return Result{Skip: SkipTooFewPods}
	}
	if !ownedByOneReplicaSet(active) {
		return Result{Skip: SkipOwners}
	}
	if routable < 2 || req.K >= len(active) {
		return Result{Skip: SkipTooFewPods}
	}

	// The controller ranks a pod by the number of the Deployment's active pods on its node
	// (getPodsRankedByRelatedPodsOnSameNode). With one ReplicaSet, those are the active pods.
	onNode := make(map[string]int, len(active))
	for _, p := range active {
		onNode[p.Spec.NodeName]++
	}
	keys := make([]*podKey, 0, len(active))
	for _, p := range active {
		keys = append(keys, newPodKey(p, onNode[p.Spec.NodeName]))
	}

	instants := orderChanges(keys, req.Now.Add(-clockSkew), req.Now.Add(req.Horizon))
	next, ok := firstK(keys, req.K, instants[0])
	if !ok {
		return Result{Skip: SkipTie}
	}
	for _, at := range instants[1:] {
		if aheadOfTheRest(next, keys, at) {
			continue
		}
		if _, ok := firstK(keys, req.K, at); !ok {
			return Result{Skip: SkipTie}
		}
		return Result{Skip: SkipOrderChanges}
	}

	res := Result{Next: make([]*v1.Pod, 0, len(next))}
	for _, k := range next {
		if utils.IsPodDraining(k.pod) {
			return Result{Skip: SkipDraining}
		}
		res.Next = append(res.Next, k.pod)
		if utils.FilterReadyPod(k.pod) {
			res.Drain = append(res.Drain, k.pod)
		}
	}
	if len(res.Drain) >= routable {
		return Result{Skip: SkipTooFewPods}
	}
	return res
}

// ownedByOneReplicaSet reports whether every pod's controller is the same ReplicaSet.
func ownedByOneReplicaSet(pods []*v1.Pod) bool {
	var owner *metav1.OwnerReference
	for _, p := range pods {
		ref := metav1.GetControllerOf(p)
		if ref == nil || ref.Kind != "ReplicaSet" || ref.APIVersion != "apps/v1" {
			return false
		}
		if owner == nil {
			owner = ref
		} else if ref.UID != owner.UID {
			return false
		}
	}
	return true
}

// podKey holds the pod fields the controller's order reads, read once per decision.
type podKey struct {
	pod      *v1.Pod
	node     string
	phase    int
	ready    bool
	readyAt  time.Time
	cost     int32
	rank     int
	restarts int32
	sidecars int32
	created  time.Time
	uid      types.UID
}

func newPodKey(p *v1.Pod, rank int) *podKey {
	k := &podKey{
		pod:     p,
		node:    p.Spec.NodeName,
		phase:   phaseOrder[p.Status.Phase],
		ready:   utils.IsPodReady(p),
		readyAt: readyTime(p),
		cost:    deletionCost(p),
		rank:    rank,
		created: p.CreationTimestamp.Time,
		uid:     p.UID,
	}
	k.restarts, k.sidecars = restarts(p)
	return k
}

// firstK sorts the pods as the controller would at the given time and returns the first k.
// It reports false if one of them is not strictly ahead of every pod after it, since the
// controller's sort.Sort would then be free to put that pod on either side of the cut.
func firstK(keys []*podKey, k int, at time.Time) ([]*podKey, bool) {
	sorted := append([]*podKey(nil), keys...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return compare(sorted[i], sorted[j], at) < 0
	})
	for _, in := range sorted[:k] {
		for _, out := range sorted[k:] {
			if compare(in, out, at) >= 0 {
				return nil, false
			}
		}
	}
	return sorted[:k], true
}

// aheadOfTheRest reports whether every pod in first is strictly ahead of every other pod at
// the given time. Then any sort puts exactly those pods first, so no new sort is needed.
func aheadOfTheRest(first, keys []*podKey, at time.Time) bool {
	in := make(map[*podKey]bool, len(first))
	for _, k := range first {
		in[k] = true
	}
	for _, a := range first {
		for _, b := range keys {
			if !in[b] && compare(a, b, at) >= 0 {
				return false
			}
		}
	}
	return true
}

// orderChanges returns from, followed by every instant in (from, to] at which a pod's ready
// time or creation time enters a new age bucket. Between two of these instants the
// controller's order does not change.
func orderChanges(keys []*podKey, from, to time.Time) []time.Time {
	at := []time.Time{from}
	for _, k := range keys {
		at = bucketEdges(at, k.created, from, to)
		at = bucketEdges(at, k.readyAt, from, to)
	}
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })
	out := at[:1]
	for _, t := range at[1:] {
		if !t.Equal(out[len(out)-1]) {
			out = append(out, t)
		}
	}
	return out
}

// bucketEdges appends the instants in (from, to] at which the age of t reaches a power of
// two nanoseconds, where logarithmicRankDiff moves it to the next bucket.
func bucketEdges(at []time.Time, t, from, to time.Time) []time.Time {
	if t.IsZero() {
		return at
	}
	for b := 0; b < 63; b++ {
		edge := t.Add(time.Duration(int64(1) << b))
		if edge.After(to) {
			break
		}
		if edge.After(from) {
			at = append(at, edge)
		}
	}
	return at
}

// phaseOrder is podPhaseToOrdinal in Kubernetes: Pending, then Unknown, then Running.
var phaseOrder = map[v1.PodPhase]int{v1.PodPending: 0, v1.PodUnknown: 1, v1.PodRunning: 2}

// compare returns -1 if the controller deletes a before b, 1 if it deletes b before a, and 0
// if the order of the two cannot be predicted. It follows ActivePodsWithRanks.Less with
// Kubernetes' default feature gates; the step numbers are Kubernetes'.
func compare(a, b *podKey, now time.Time) int {
	// 1. Unscheduled before scheduled.
	if a.node != b.node && (a.node == "" || b.node == "") {
		return before(a.node == "")
	}
	// 2. Pending, then Unknown, then Running.
	if a.phase != b.phase {
		return before(a.phase < b.phase)
	}
	// 3. Not ready before ready.
	if a.ready != b.ready {
		return before(!a.ready)
	}
	// 4. Lower pod-deletion-cost first.
	if a.cost != b.cost {
		return before(a.cost < b.cost)
	}
	// 5. More of the Deployment's active pods on the same node first.
	if a.rank != b.rank {
		return before(a.rank > b.rank)
	}
	// 6. Of two ready pods, the one that became ready more recently first.
	if a.ready && b.ready && !a.readyAt.Equal(b.readyAt) {
		return compareTimes(a, b, a.readyAt, b.readyAt, now)
	}
	// 7. More container restarts first. Kubernetes 1.32 and later then compare the restarts
	// of sidecar (restartable init) containers, and 1.31 does not, so a difference there
	// cannot be predicted without knowing the cluster's version.
	if a.restarts != b.restarts {
		return before(a.restarts > b.restarts)
	}
	if a.sidecars != b.sidecars {
		return 0
	}
	// 8. Created more recently first.
	if !a.created.Equal(b.created) {
		return compareTimes(a, b, a.created, b.created, now)
	}
	return 0
}

func before(aFirst bool) int {
	if aFirst {
		return -1
	}
	return 1
}

// compareTimes orders two different times the way Less does: a zero time first; otherwise
// the newer power-of-two age bucket at now first (logarithmicRankDiff), and within one
// bucket the lower UID first.
func compareTimes(a, b *podKey, ta, tb, now time.Time) int {
	if ta.IsZero() || tb.IsZero() {
		return before(ta.IsZero())
	}
	if d := logarithmicRankDiff(ta, tb, now); d != 0 {
		return before(d < 0)
	}
	switch {
	case a.uid < b.uid:
		return -1
	case a.uid > b.uid:
		return 1
	}
	return 0
}

// logarithmicRankDiff is Kubernetes' function of the same name.
func logarithmicRankDiff(t1, t2, now time.Time) int64 {
	d1 := now.Sub(t1)
	d2 := now.Sub(t2)
	r1 := int64(-1)
	r2 := int64(-1)
	if d1 > 0 {
		r1 = int64(math.Log2(float64(d1)))
	}
	if d2 > 0 {
		r2 = int64(math.Log2(float64(d2)))
	}
	return r1 - r2
}

// readyTime is the time the pod became ready, or the zero time if it is not ready.
func readyTime(p *v1.Pod) time.Time {
	if !utils.IsPodReady(p) {
		return time.Time{}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == v1.PodReady && c.Status == v1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// restarts returns the highest restart count among the pod's containers, and among its
// sidecar (restartable init) containers.
func restarts(p *v1.Pod) (containers, sidecars int32) {
	for _, c := range p.Status.ContainerStatuses {
		containers = max(containers, c.RestartCount)
	}
	sidecar := map[string]bool{}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			sidecar[c.Name] = true
		}
	}
	for _, c := range p.Status.InitContainerStatuses {
		if sidecar[c.Name] {
			sidecars = max(sidecars, c.RestartCount)
		}
	}
	return containers, sidecars
}

// deletionCost parses the pod-deletion-cost annotation as Kubernetes does: a missing or
// invalid value counts as 0.
func deletionCost(p *v1.Pod) int32 {
	s, ok := p.Annotations[podDeletionCostAnnotation]
	if !ok || s == "" {
		return 0
	}
	// Kubernetes rejects a leading plus sign and leading zeros.
	if s != "0" && s[0] != '-' && (s[0] < '1' || s[0] > '9') {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}
