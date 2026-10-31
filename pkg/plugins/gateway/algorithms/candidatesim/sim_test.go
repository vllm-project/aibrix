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

// Package candidatesim compares candidate-selection policies for gateway routing (RFC #3188)
// against full scoring in a discrete-time simulation. It holds only tests: run it with
//
//	CANDIDATE_SIM=1 go test -run TestCandidateSelectionQuality -v ./pkg/plugins/gateway/algorithms/candidatesim/
//
// The model, deliberately simple:
//   - Pods have heterogeneous capacity (output tokens/s) and serve their running requests by
//     processor sharing.
//   - Several gateways route round-robin arrivals. Each sees its own picks live and the other
//     gateways' load only as of the last refresh, every SignalDelay (0 = always exact).
//   - The load score mirrors load-balance: running / capacity, lower is better.
//   - Before any scoring, the gateway's load-imbalance gate runs on the counts the gateway
//     observes: when max-min >= ImbalanceMinGap and max > ImbalanceFactor*(mean of the others+1),
//     only the pods at the minimum count stay eligible (ApplyLoadImbalanceGate). Every policy
//     picks its candidates from the pods the gate leaves, except with GateSample, which draws
//     the candidates first and gates only them, as the gateway's candidate stage does.
//   - With prefix caching on, each request belongs to a Zipf-distributed prefix group. A pod
//     holding the group's prefix serves the request with less work, and the blend adds an
//     equal-weight prefix term after min-max normalizing load over the scored pods, as
//     multiStrategyRouter does.
//
// For every request the simulation also computes what full scoring would have chosen on the
// same state, so "agreement" isolates the candidate stage's effect on each decision.
package candidatesim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type simConfig struct {
	Pods        int
	Gateways    int
	Utilization float64
	MeanWork    float64 // output tokens per request
	// HeavyTail draws 10% of requests with a mean of 5.5x MeanWork and the rest with 0.5x, keeping
	// the overall mean; otherwise request work is exponential.
	HeavyTail     bool
	PrefixOn      bool
	PrefixGroups  int
	PrefixPerPod  int     // prefix groups a pod keeps cached (LRU)
	PrefixSaving  float64 // fraction of work saved on a prefix hit
	SignalDelay   time.Duration
	Step          time.Duration
	Warmup        time.Duration
	Duration      time.Duration
	PrefixSeedMax int // M: prefix holders added to the candidate set
	// SeedRandom picks which prefix holders fill the M seed slots at random, as SeedCandidates
	// does; otherwise the least loaded holders, as the gateway observes them, come first.
	SeedRandom bool
	// Load-imbalance gate, as AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP and _FACTOR. MinGap 0 disables it.
	ImbalanceMinGap int
	ImbalanceFactor float64
	Seed            uint64
}

type policyKind int

const (
	policyFull policyKind = iota
	policyRandom
	policyRandomSeeded
	policySnapshot
	policyCountIndex
	policyCountIndexMixed
	// policyBlendRanked is the K-only candidate stage: a scout of 4K random pods plus up to K
	// prefix holders is ranked on the blended score of the gateway's observed counts (the
	// in-memory estimate); the best K/2 are kept and the other K/2 are drawn at random.
	policyBlendRanked
	// policyScoutRecommend approximates #3353: scout 4K random pods, recommend the best
	// R = min(K/4, 4) of them on the observed blend, reserve max(min(K/2, R), K/2-4R) prefix
	// holders, and fill the rest at random.
	policyScoutRecommend
)

type policy struct {
	Kind            policyKind
	K               int
	SnapshotRefresh time.Duration
	// LiveScoring scores the candidates with exact running counts, as a gateway would after
	// reading only the candidates' counts from Redis. Candidate selection, and the imbalance
	// gate unless GateSample, still use the delayed counts.
	LiveScoring bool
	// GateSample runs the imbalance gate on the candidates instead of on the whole fleet, as the
	// gateway's candidate stage does: candidates first, then the gate and scoring on them only.
	// With LiveScoring the gate then sees the candidates' exact counts.
	GateSample bool
	// Ranked is how many of the K slots policyBlendRanked fills by rank; 0 means K/2.
	Ranked int
}

func (p policy) String() string {
	if p.GateSample {
		q := p
		q.GateSample = false
		return q.String() + ", gate on sample"
	}
	if p.LiveScoring {
		q := p
		q.LiveScoring = false
		return q.String() + ", live stage 2"
	}
	switch p.Kind {
	case policyFull:
		return "full"
	case policyRandom:
		return fmt.Sprintf("A random K=%d", p.K)
	case policyRandomSeeded:
		return fmt.Sprintf("A random+prefix K=%d", p.K)
	case policySnapshot:
		return fmt.Sprintf("D snapshot K=%d T=%v", p.K, p.SnapshotRefresh)
	case policyCountIndex:
		return fmt.Sprintf("C count index K=%d", p.K)
	case policyCountIndexMixed:
		return fmt.Sprintf("C+A index K/2 + random K/2, K=%d", p.K)
	case policyBlendRanked:
		if p.Ranked > 0 {
			return fmt.Sprintf("K-only: scout 4K+holders, rank %d + random, K=%d", p.Ranked, p.K)
		}
		return fmt.Sprintf("K-only: scout 4K+holders, rank K/2 + random K/2, K=%d", p.K)
	case policyScoutRecommend:
		return fmt.Sprintf("scout 4K, R=min(K/4,4) + prefix reserve + random, K=%d", p.K)
	}
	return "unknown"
}

type request struct {
	arrival   float64
	remaining float64
	gateway   int
	pod       int
}

type pod struct {
	capacity float64
	running  []*request
	groups   []int // cached prefix groups, most recent last
}

func (p *pod) holds(group int) bool {
	for _, g := range p.groups {
		if g == group {
			return true
		}
	}
	return false
}

func (p *pod) touch(group, limit int) {
	for i, g := range p.groups {
		if g == group {
			copy(p.groups[i:], p.groups[i+1:])
			p.groups[len(p.groups)-1] = group
			return
		}
	}
	if len(p.groups) == limit {
		copy(p.groups, p.groups[1:])
		p.groups = p.groups[:limit-1]
	}
	p.groups = append(p.groups, group)
}

type result struct {
	Policy        string
	Requests      int
	Agreement     float64 // share of decisions as good as full scoring's
	Gated         float64 // share of decisions the imbalance gate restricted
	MeanLatency   float64 // seconds
	P99Latency    float64 // seconds
	PrefixHit     float64
	ScoredPerReq  float64
	MaxToMeanLoad float64 // time-averaged max/mean running across pods
}

type sim struct {
	cfg    simConfig
	pol    policy
	rng    *rand.Rand
	pods   []*pod
	now    float64
	zipf   *rand.Zipf
	holder map[int][]int // prefix group -> pods holding it

	// Per gateway: running requests it routed per pod (live), and the total running per pod
	// and its own share as of the last refresh.
	local       [][]int
	refTotal    [][]int
	refLocal    [][]int
	nextRefresh float64

	// Per gateway ranking snapshot for policySnapshot.
	snapshot     [][]int
	nextSnapshot float64

	latencies      []float64
	agree, decided int
	gated          int
	exact          bool // observed returns exact counts (stage-2 live scoring)
	hits           int
	scored         int
	loadRatioSum   float64
	loadRatioN     int
}

func newSim(cfg simConfig, pol policy) *sim {
	s := &sim{cfg: cfg, pol: pol, rng: rand.New(rand.NewPCG(cfg.Seed, 7))}
	caps := []float64{3000, 4000, 6000}
	for i := 0; i < cfg.Pods; i++ {
		s.pods = append(s.pods, &pod{capacity: caps[i%len(caps)]})
	}
	s.zipf = rand.NewZipf(s.rng, 1.1, 1, uint64(cfg.PrefixGroups-1))
	s.holder = map[int][]int{}
	for g := 0; g < cfg.Gateways; g++ {
		s.local = append(s.local, make([]int, cfg.Pods))
		s.refTotal = append(s.refTotal, make([]int, cfg.Pods))
		s.refLocal = append(s.refLocal, make([]int, cfg.Pods))
		s.snapshot = append(s.snapshot, nil)
	}
	return s
}

// observed is the running count gateway g believes pod i has.
func (s *sim) observed(g, i int) int {
	if s.cfg.SignalDelay == 0 || s.exact {
		return len(s.pods[i].running)
	}
	return s.refTotal[g][i] - s.refLocal[g][i] + s.local[g][i]
}

func (s *sim) loadScore(g, i int) float64 {
	return float64(s.observed(g, i)+1) / s.pods[i].capacity
}

func (s *sim) refreshSignals() {
	for g := range s.local {
		for i, p := range s.pods {
			s.refTotal[g][i] = len(p.running)
			s.refLocal[g][i] = s.local[g][i]
		}
	}
}

func (s *sim) refreshSnapshots() {
	for g := range s.snapshot {
		order := make([]int, len(s.pods))
		for i := range order {
			order[i] = i
		}
		s.rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		sort.SliceStable(order, func(a, b int) bool { return s.loadScore(g, order[a]) < s.loadScore(g, order[b]) })
		s.snapshot[g] = order
	}
}

// finalScores returns the blended score (higher is better) of each pod in set, normalizing load
// over the set itself as the router does.
func (s *sim) finalScores(g, group int, set []int) []float64 {
	scores := make([]float64, len(set))
	lo, hi := math.Inf(1), math.Inf(-1)
	for j, i := range set {
		l := s.loadScore(g, i)
		scores[j] = l
		lo, hi = math.Min(lo, l), math.Max(hi, l)
	}
	for j, i := range set {
		norm := 1.0
		if hi > lo {
			norm = 1 - (scores[j]-lo)/(hi-lo)
		}
		if s.cfg.PrefixOn {
			prefix := 0.0
			if s.pods[i].holds(group) {
				prefix = 1
			}
			scores[j] = 0.5*norm + 0.5*prefix
		} else {
			scores[j] = norm
		}
	}
	return scores
}

func (s *sim) argmax(set []int, scores []float64) int {
	best, bestScore, ties := -1, math.Inf(-1), 0
	for j, i := range set {
		switch {
		case scores[j] > bestScore+1e-12:
			best, bestScore, ties = i, scores[j], 1
		case math.Abs(scores[j]-bestScore) <= 1e-12:
			ties++
			if s.rng.IntN(ties) == 0 {
				best = i
			}
		}
	}
	return best
}

// eligible applies the load-imbalance gate to the counts gateway g observes and returns the
// pods left, or nil when the gate does not fire (every pod stays eligible).
func (s *sim) eligible(g int) []int {
	if s.cfg.ImbalanceMinGap == 0 {
		return nil
	}
	lo, hi, sum := math.MaxInt, 0, 0
	for i := range s.pods {
		v := s.observed(g, i)
		lo, hi, sum = min(lo, v), max(hi, v), sum+v
	}
	if hi-lo < s.cfg.ImbalanceMinGap {
		return nil
	}
	if n := len(s.pods); n > 2 && float64(hi) <= s.cfg.ImbalanceFactor*(float64(sum-hi)/float64(n-1)+1) {
		return nil
	}
	var out []int
	for i := range s.pods {
		if s.observed(g, i) == lo {
			out = append(out, i)
		}
	}
	return out
}

// eligibleAmong is the imbalance gate over pods only: nil when it does not fire, otherwise the
// pods at the minimum count.
func (s *sim) eligibleAmong(g int, pods []int) []int {
	if s.cfg.ImbalanceMinGap == 0 || len(pods) == 0 {
		return nil
	}
	lo, hi, sum := math.MaxInt, 0, 0
	for _, i := range pods {
		v := s.observed(g, i)
		lo, hi, sum = min(lo, v), max(hi, v), sum+v
	}
	if hi-lo < s.cfg.ImbalanceMinGap {
		return nil
	}
	if n := len(pods); n > 2 && float64(hi) <= s.cfg.ImbalanceFactor*(float64(sum-hi)/float64(n-1)+1) {
		return nil
	}
	var out []int
	for _, i := range pods {
		if s.observed(g, i) == lo {
			out = append(out, i)
		}
	}
	return out
}

func (s *sim) prefixSeeds(g, group int, allowed map[int]bool) []int {
	if !s.cfg.PrefixOn || s.cfg.PrefixSeedMax == 0 {
		return nil
	}
	var holders []int
	for _, h := range s.holder[group] {
		if allowed == nil || allowed[h] {
			holders = append(holders, h)
		}
	}
	if s.cfg.SeedRandom {
		// SeedCandidates ranks holders by match length only and breaks ties at random.
		s.rng.Shuffle(len(holders), func(a, b int) { holders[a], holders[b] = holders[b], holders[a] })
	} else {
		sort.Slice(holders, func(a, b int) bool { return s.loadScore(g, holders[a]) < s.loadScore(g, holders[b]) })
	}
	if len(holders) > s.cfg.PrefixSeedMax {
		holders = holders[:s.cfg.PrefixSeedMax]
	}
	return holders
}

func union(a, b []int) []int {
	seen := make(map[int]struct{}, len(a)+len(b))
	out := make([]int, 0, len(a)+len(b))
	for _, list := range [][]int{a, b} {
		for _, i := range list {
			if _, ok := seen[i]; !ok {
				seen[i] = struct{}{}
				out = append(out, i)
			}
		}
	}
	return out
}

// candidates returns the pods the policy scores, drawn from universe (the pods the gate left).
func (s *sim) candidates(g, group int, universe []int, allowed map[int]bool) []int {
	n := len(universe)
	k := min(s.pol.K, n)
	switch s.pol.Kind {
	case policyRandom, policyRandomSeeded:
		perm := s.rng.Perm(n)[:k]
		for j, idx := range perm {
			perm[j] = universe[idx]
		}
		if s.pol.Kind == policyRandomSeeded {
			return union(s.prefixSeeds(g, group, allowed), perm)
		}
		return perm
	case policySnapshot:
		picked := make([]int, 0, k)
		for _, i := range s.snapshot[g] {
			if len(picked) == k {
				break
			}
			if allowed == nil || allowed[i] {
				picked = append(picked, i)
			}
		}
		return union(s.prefixSeeds(g, group, allowed), picked)
	case policyCountIndex, policyCountIndexMixed:
		order := append([]int(nil), universe...)
		s.rng.Shuffle(n, func(a, b int) { order[a], order[b] = order[b], order[a] })
		sort.SliceStable(order, func(a, b int) bool { return s.observed(g, order[a]) < s.observed(g, order[b]) })
		if s.pol.Kind == policyCountIndexMixed {
			// Half the slots from the index, half drawn uniformly from the rest, so stale counts
			// cannot steer every candidate to the same few pods.
			rest := order[min(s.pol.K/2, n):]
			s.rng.Shuffle(len(rest), func(a, b int) { rest[a], rest[b] = rest[b], rest[a] })
		}
		return union(s.prefixSeeds(g, group, allowed), order[:k])
	case policyBlendRanked:
		if n <= k {
			return universe
		}
		scout := universe
		if 8*k < n {
			var holders []int
			if s.cfg.PrefixOn {
				for _, h := range s.holder[group] {
					if allowed == nil || allowed[h] {
						holders = append(holders, h)
					}
				}
				s.rng.Shuffle(len(holders), func(a, b int) { holders[a], holders[b] = holders[b], holders[a] })
				holders = holders[:min(k, len(holders))]
			}
			scout = s.fillRandom(universe, holders, len(holders)+4*k)
		}
		r := k / 2
		if s.pol.Ranked > 0 {
			r = min(s.pol.Ranked, k)
		}
		ranked := s.bestByEstimate(g, group, scout, r)
		return s.fillRandom(universe, ranked, k)
	case policyScoutRecommend:
		if n <= k {
			return universe
		}
		scout := make([]int, 0, 4*k)
		for _, idx := range s.rng.Perm(n)[:min(4*k, n)] {
			scout = append(scout, universe[idx])
		}
		r := min(k/4, 4)
		picked := s.bestByEstimate(g, group, scout, r)
		reserve := max(min(k/2, r), k/2-4*r)
		if s.cfg.PrefixOn {
			inUniverse := allowed
			holders := make([]int, 0, len(s.holder[group]))
			for _, h := range s.holder[group] {
				if inUniverse == nil || inUniverse[h] {
					holders = append(holders, h)
				}
			}
			s.rng.Shuffle(len(holders), func(a, b int) { holders[a], holders[b] = holders[b], holders[a] })
			before := len(picked)
			for _, h := range union(picked, holders)[before:] {
				if len(picked)-before >= reserve {
					break
				}
				picked = append(picked, h)
			}
		}
		return s.fillRandom(universe, picked, k)
	}
	return universe
}

// bestByEstimate returns the m pods of set with the best blended score on the counts gateway g
// observes, ties broken at random.
func (s *sim) bestByEstimate(g, group int, set []int, m int) []int {
	order := append([]int(nil), set...)
	s.rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
	scores := s.finalScores(g, group, order)
	idx := make([]int, len(order))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	out := make([]int, 0, m)
	for _, i := range idx[:min(m, len(idx))] {
		out = append(out, order[i])
	}
	return out
}

// fillRandom adds pods of universe not yet in picked, uniformly at random, until it holds k.
func (s *sim) fillRandom(universe, picked []int, k int) []int {
	seen := make(map[int]struct{}, k)
	for _, i := range picked {
		seen[i] = struct{}{}
	}
	for _, idx := range s.rng.Perm(len(universe)) {
		if len(picked) >= k {
			break
		}
		if _, dup := seen[universe[idx]]; !dup {
			seen[universe[idx]] = struct{}{}
			picked = append(picked, universe[idx])
		}
	}
	return picked
}

func (s *sim) route(r *request, group int) {
	if s.pol.GateSample {
		s.routeGateSample(r, group)
		return
	}
	g := r.gateway
	universe := s.eligible(g)
	var allowed map[int]bool
	if universe != nil {
		s.gated++
		allowed = make(map[int]bool, len(universe))
		for _, i := range universe {
			allowed[i] = true
		}
	} else {
		universe = make([]int, len(s.pods))
		for i := range universe {
			universe[i] = i
		}
	}
	cand := s.candidates(g, group, universe, allowed)
	s.exact = s.pol.LiveScoring
	defer func() { s.exact = false }()
	target := s.argmax(cand, s.finalScores(g, group, cand))
	if s.pol.Kind == policyFull {
		s.agree++
	} else {
		// Full scoring normalizes over every eligible pod, so compare on that scale.
		fullScores := s.finalScores(g, group, universe)
		pos := make(map[int]int, len(universe))
		for j, i := range universe {
			pos[i] = j
		}
		best := s.argmax(universe, fullScores)
		if fullScores[pos[target]] >= fullScores[pos[best]]-1e-9 {
			s.agree++
		}
	}
	s.decided++
	s.scored += len(cand)
	s.place(r, target, group)
}

// routeGateSample routes as the gateway's candidate stage does: draw the candidates from the
// whole fleet, then run the gate and scoring on them only.
func (s *sim) routeGateSample(r *request, group int) {
	g := r.gateway
	fleet := make([]int, len(s.pods))
	for i := range fleet {
		fleet[i] = i
	}
	cand := s.candidates(g, group, fleet, nil)
	s.exact = s.pol.LiveScoring
	defer func() { s.exact = false }()
	if gated := s.eligibleAmong(g, cand); gated != nil {
		s.gated++
		cand = gated
	}
	target := s.argmax(cand, s.finalScores(g, group, cand))
	fullScores := s.finalScores(g, group, fleet)
	if fullScores[target] >= fullScores[s.argmax(fleet, fullScores)]-1e-9 {
		s.agree++
	}
	s.decided++
	s.scored += len(cand)
	s.place(r, target, group)
}

func (s *sim) place(r *request, target, group int) {
	p := s.pods[target]
	r.pod = target
	if s.cfg.PrefixOn {
		if p.holds(group) {
			r.remaining *= 1 - s.cfg.PrefixSaving
			if s.now >= s.cfg.Warmup.Seconds() {
				s.hits++
			}
		} else {
			if len(p.groups) == s.cfg.PrefixPerPod {
				evicted := p.groups[0]
				hs := s.holder[evicted]
				for k, h := range hs {
					if h == target {
						s.holder[evicted] = append(hs[:k], hs[k+1:]...)
						break
					}
				}
			}
			s.holder[group] = append(s.holder[group], target)
		}
		p.touch(group, s.cfg.PrefixPerPod)
	}
	p.running = append(p.running, r)
	s.local[r.gateway][target]++
}

func (s *sim) work() float64 {
	if s.cfg.HeavyTail && s.rng.Float64() < 0.1 {
		return s.rng.ExpFloat64() * 5.5 * s.cfg.MeanWork
	}
	if s.cfg.HeavyTail {
		return s.rng.ExpFloat64() * 0.5 * s.cfg.MeanWork
	}
	return s.rng.ExpFloat64() * s.cfg.MeanWork
}

func (s *sim) run() result {
	cfg := s.cfg
	totalCap := 0.0
	for _, p := range s.pods {
		totalCap += p.capacity
	}
	work := cfg.MeanWork
	if cfg.PrefixOn {
		work *= 1 - cfg.PrefixSaving*0.5 // roughly half the requests hit
	}
	rate := cfg.Utilization * totalCap / work
	dt := cfg.Step.Seconds()
	end := cfg.Duration.Seconds()
	nextArrival := s.rng.ExpFloat64() / rate
	gw := 0
	if s.pol.Kind == policySnapshot {
		s.refreshSnapshots()
		s.nextSnapshot = s.pol.SnapshotRefresh.Seconds()
	}
	for s.now = 0; s.now < end; s.now += dt {
		if cfg.SignalDelay > 0 && s.now >= s.nextRefresh {
			s.refreshSignals()
			s.nextRefresh += cfg.SignalDelay.Seconds()
		}
		if s.pol.Kind == policySnapshot && s.now >= s.nextSnapshot {
			s.refreshSnapshots()
			s.nextSnapshot += s.pol.SnapshotRefresh.Seconds()
		}
		for nextArrival < s.now+dt {
			r := &request{arrival: nextArrival, remaining: s.work(), gateway: gw}
			gw = (gw + 1) % cfg.Gateways
			group := int(s.zipf.Uint64())
			if s.now < cfg.Warmup.Seconds() {
				// Warm-up decisions shape the state but are not counted.
				agree, decided, scored, gated := s.agree, s.decided, s.scored, s.gated
				s.route(r, group)
				s.agree, s.decided, s.scored, s.gated = agree, decided, scored, gated
			} else {
				s.route(r, group)
			}
			nextArrival += s.rng.ExpFloat64() / rate
		}
		maxRun, sumRun := 0, 0
		for i, p := range s.pods {
			if len(p.running) == 0 {
				continue
			}
			share := p.capacity * dt / float64(len(p.running))
			kept := p.running[:0]
			for _, r := range p.running {
				r.remaining -= share
				if r.remaining > 0 {
					kept = append(kept, r)
					continue
				}
				s.local[r.gateway][i]--
				if r.arrival >= cfg.Warmup.Seconds() {
					s.latencies = append(s.latencies, s.now+dt-r.arrival)
				}
			}
			p.running = kept
			maxRun = max(maxRun, len(kept))
			sumRun += len(kept)
		}
		if s.now >= cfg.Warmup.Seconds() && sumRun > 0 {
			s.loadRatioSum += float64(maxRun) / (float64(sumRun) / float64(len(s.pods)))
			s.loadRatioN++
		}
	}
	// Every ratio below needs completed requests after the warmup. A configuration without any
	// would print zeros or NaN into a results table, so fail loudly instead.
	if len(s.latencies) == 0 || s.decided == 0 || s.loadRatioN == 0 {
		panic(fmt.Sprintf("candidatesim: no requests completed between warmup %v and duration %v", cfg.Warmup, cfg.Duration))
	}
	sort.Float64s(s.latencies)
	mean := 0.0
	for _, l := range s.latencies {
		mean += l
	}
	res := result{
		Policy:        s.pol.String(),
		Requests:      s.decided,
		Agreement:     float64(s.agree) / float64(s.decided),
		Gated:         float64(s.gated) / float64(s.decided),
		MeanLatency:   mean / float64(len(s.latencies)),
		P99Latency:    s.latencies[len(s.latencies)*99/100],
		ScoredPerReq:  float64(s.scored) / float64(s.decided),
		MaxToMeanLoad: s.loadRatioSum / float64(s.loadRatioN),
	}
	if cfg.PrefixOn {
		res.PrefixHit = float64(s.hits) / float64(s.decided)
	}
	return res
}

func TestCandidateSelectionQuality(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") == "" {
		t.Skip("set CANDIDATE_SIM=1 to run the candidate-selection simulation")
	}
	policies := []policy{
		{Kind: policyFull},
		{Kind: policyRandom, K: 8},
		{Kind: policyRandom, K: 16},
		{Kind: policyRandom, K: 32},
		{Kind: policyRandomSeeded, K: 16},
		{Kind: policyCountIndex, K: 16},
		{Kind: policyCountIndexMixed, K: 16},
		{Kind: policySnapshot, K: 16, SnapshotRefresh: 5 * time.Millisecond},
		{Kind: policySnapshot, K: 8, SnapshotRefresh: 50 * time.Millisecond},
		{Kind: policySnapshot, K: 16, SnapshotRefresh: 50 * time.Millisecond},
		{Kind: policySnapshot, K: 32, SnapshotRefresh: 50 * time.Millisecond},
	}
	var out strings.Builder
	for _, prefixOn := range []bool{false, true} {
		for _, delay := range []time.Duration{0, 100 * time.Millisecond, time.Second} {
			cfg := simConfig{
				Pods: 500, Gateways: 4, Utilization: 0.85, MeanWork: 200,
				PrefixOn: prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
				SignalDelay: delay, Step: 5 * time.Millisecond,
				Warmup: 5 * time.Second, Duration: 25 * time.Second,
				PrefixSeedMax: 4, Seed: 42,
				ImbalanceMinGap: 8, ImbalanceFactor: 2,
			}
			fmt.Fprintf(&out, "\n### prefix=%v signal delay=%v (N=%d, %d gateways, utilization %.2f)\n\n",
				prefixOn, delay, cfg.Pods, cfg.Gateways, cfg.Utilization)
			out.WriteString("| Policy | Agreement | Mean latency (s) | P99 latency (s) | Max/mean running | Prefix hit | Gate fired | Pods scored/req |\n")
			out.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")
			// Each policy's simulation has its own state and random source, so they run in parallel.
			results := make([]result, len(policies))
			var wg sync.WaitGroup
			for j, pol := range policies {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[j] = newSim(cfg, pol).run()
				}()
			}
			wg.Wait()
			for _, res := range results {
				hit := "-"
				if prefixOn {
					hit = fmt.Sprintf("%.1f%%", 100*res.PrefixHit)
				}
				fmt.Fprintf(&out, "| %s | %.1f%% | %.3f | %.3f | %.2f | %s | %.1f%% | %.0f |\n",
					res.Policy, 100*res.Agreement, res.MeanLatency, res.P99Latency, res.MaxToMeanLoad, hit, 100*res.Gated, res.ScoredPerReq)
			}
		}
	}
	t.Log(out.String())
	if path := os.Getenv("CANDIDATE_SIM_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCandidateSelectionRobustness reruns the main policies across seeds, a larger fleet and
// heavy-tailed request sizes, and reports each policy's mean latency relative to full scoring on
// the same seed. Run it with CANDIDATE_SIM=robust; it takes tens of minutes.
func TestCandidateSelectionRobustness(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "robust" {
		t.Skip("set CANDIDATE_SIM=robust to run the robustness sweep")
	}
	policies := []policy{
		{Kind: policyFull},
		{Kind: policyRandomSeeded, K: 16},
		{Kind: policyCountIndex, K: 16},
		{Kind: policyCountIndexMixed, K: 16},
		{Kind: policySnapshot, K: 16, SnapshotRefresh: 50 * time.Millisecond},
	}
	type variant struct {
		name      string
		pods      int
		heavyTail bool
		seeds     []uint64
		duration  time.Duration
	}
	variants := []variant{
		{"N=500, exponential sizes, 5 seeds", 500, false, []uint64{1, 2, 3, 4, 5}, 25 * time.Second},
		{"N=500, heavy-tailed sizes, 3 seeds", 500, true, []uint64{1, 2, 3}, 25 * time.Second},
		{"N=2000, exponential sizes, 1 seed", 2000, false, []uint64{1}, 13 * time.Second},
	}
	type scenario struct {
		prefixOn bool
		delay    time.Duration
	}
	var scenarios []scenario
	for _, prefixOn := range []bool{false, true} {
		for _, delay := range []time.Duration{0, 100 * time.Millisecond, time.Second} {
			scenarios = append(scenarios, scenario{prefixOn, delay})
		}
	}

	var out strings.Builder
	for _, v := range variants {
		// results[scenario][seed][policy]
		results := make([][][]result, len(scenarios))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for si, sc := range scenarios {
			results[si] = make([][]result, len(v.seeds))
			for ki, seed := range v.seeds {
				results[si][ki] = make([]result, len(policies))
				for pi, pol := range policies {
					cfg := simConfig{
						Pods: v.pods, Gateways: 4, Utilization: 0.85, MeanWork: 200, HeavyTail: v.heavyTail,
						PrefixOn: sc.prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
						SignalDelay: sc.delay, Step: 5 * time.Millisecond,
						Warmup: 3 * time.Second, Duration: v.duration,
						PrefixSeedMax: 4, Seed: seed,
						ImbalanceMinGap: 8, ImbalanceFactor: 2,
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						results[si][ki][pi] = newSim(cfg, pol).run()
					}()
				}
			}
		}
		wg.Wait()

		fmt.Fprintf(&out, "\n### %s\n\n", v.name)
		out.WriteString("Mean latency relative to full scoring on the same seed: mean (min to max). Full scoring's own mean latency in seconds in the first row; prefix hit ratio after the slash when prefix caching is on.\n\n")
		out.WriteString("| Policy |")
		for _, sc := range scenarios {
			fmt.Fprintf(&out, " prefix=%v delay=%v |", sc.prefixOn, sc.delay)
		}
		out.WriteString("\n| --- |" + strings.Repeat(" --- |", len(scenarios)) + "\n")
		for pi, pol := range policies {
			fmt.Fprintf(&out, "| %s |", pol)
			for si, sc := range scenarios {
				var sum, lo, hi, hit float64
				lo = math.Inf(1)
				for ki := range v.seeds {
					r := results[si][ki][pi]
					x := r.MeanLatency
					if pi > 0 {
						x /= results[si][ki][0].MeanLatency
					}
					sum += x
					lo, hi = math.Min(lo, x), math.Max(hi, x)
					hit += r.PrefixHit
				}
				n := float64(len(v.seeds))
				cell := fmt.Sprintf("%.3f s", sum/n)
				if pi > 0 {
					cell = fmt.Sprintf("%.2f", sum/n)
					if len(v.seeds) > 1 {
						cell += fmt.Sprintf(" (%.2f to %.2f)", lo, hi)
					}
				}
				if sc.prefixOn {
					cell += fmt.Sprintf(" / %.1f%%", 100*hit/n)
				}
				fmt.Fprintf(&out, " %s |", cell)
			}
			out.WriteString("\n")
		}
	}
	t.Log(out.String())
	if path := os.Getenv("CANDIDATE_SIM_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCandidateSelectionKOnly compares the K-only candidate designs for Varun's second TODO on
// #3292 with the stage on main (K random plus M prefix seeds) and full scoring, all with live
// stage 2 and the gate on the candidates, as the gateway runs them. Run it with
// CANDIDATE_SIM=konly; CANDIDATE_SIM_PODS=2000 runs the larger fleet.
func TestCandidateSelectionKOnly(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "konly" {
		t.Skip("set CANDIDATE_SIM=konly to run the K-only comparison")
	}
	pods := 500
	if v, err := strconv.Atoi(os.Getenv("CANDIDATE_SIM_PODS")); err == nil && v > 0 {
		pods = v
	}
	live := func(p policy) policy { p.LiveScoring, p.GateSample = true, true; return p }
	policies := []policy{
		{Kind: policyFull},
		live(policy{Kind: policyRandomSeeded, K: 16}),
		live(policy{Kind: policyBlendRanked, K: 16}),
		live(policy{Kind: policyScoutRecommend, K: 16}),
		live(policy{Kind: policyRandomSeeded, K: 32}),
		live(policy{Kind: policyBlendRanked, K: 32}),
		live(policy{Kind: policyScoutRecommend, K: 32}),
		// Ranking three quarters of the slots: the sample stops resembling the fleet, the gate
		// on the sample fires against hot prefix holders, and prefix-on runs can destabilize.
		live(policy{Kind: policyBlendRanked, K: 32, Ranked: 24}),
	}
	seeds := []uint64{1, 2, 3}
	type scenario struct {
		prefixOn bool
		delay    time.Duration
	}
	var scenarios []scenario
	for _, prefixOn := range []bool{false, true} {
		for _, delay := range []time.Duration{100 * time.Millisecond, time.Second} {
			scenarios = append(scenarios, scenario{prefixOn, delay})
		}
	}
	results := make([][][]result, len(scenarios))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for si, sc := range scenarios {
		results[si] = make([][]result, len(seeds))
		for ki, seed := range seeds {
			results[si][ki] = make([]result, len(policies))
			for pi, pol := range policies {
				cfg := simConfig{
					Pods: pods, Gateways: 4, Utilization: 0.85, MeanWork: 200,
					PrefixOn: sc.prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
					SignalDelay: sc.delay, Step: 5 * time.Millisecond,
					Warmup: 3 * time.Second, Duration: 15 * time.Second,
					PrefixSeedMax: 16, SeedRandom: true, Seed: seed,
					ImbalanceMinGap: 8, ImbalanceFactor: 2,
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					results[si][ki][pi] = newSim(cfg, pol).run()
				}()
			}
		}
	}
	wg.Wait()

	var out strings.Builder
	fmt.Fprintf(&out, "\n### N=%d, 4 gateways, utilization 0.85, %d seeds\n\n", pods, len(seeds))
	out.WriteString("Mean latency relative to full scoring on the same seed: mean (min to max); P99 ratio after the semicolon; prefix hit after the slash. Full scoring's mean latency in seconds in the first row.\n\n")
	out.WriteString("| Policy |")
	for _, sc := range scenarios {
		fmt.Fprintf(&out, " prefix=%v delay=%v |", sc.prefixOn, sc.delay)
	}
	out.WriteString("\n| --- |" + strings.Repeat(" --- |", len(scenarios)) + "\n")
	for pi, pol := range policies {
		fmt.Fprintf(&out, "| %s |", pol)
		for si, sc := range scenarios {
			var sum, p99, hit float64
			lo, hi := math.Inf(1), math.Inf(-1)
			for ki := range seeds {
				r, full := results[si][ki][pi], results[si][ki][0]
				x := r.MeanLatency
				if pi > 0 {
					x /= full.MeanLatency
					p99 += r.P99Latency / full.P99Latency
				}
				sum += x
				lo, hi = math.Min(lo, x), math.Max(hi, x)
				hit += r.PrefixHit
			}
			n := float64(len(seeds))
			cell := fmt.Sprintf("%.3f s", sum/n)
			if pi > 0 {
				cell = fmt.Sprintf("%.2f (%.2f to %.2f); %.2f", sum/n, lo, hi, p99/n)
			}
			if sc.prefixOn {
				cell += fmt.Sprintf(" / %.1f%%", 100*hit/n)
			}
			fmt.Fprintf(&out, " %s |", cell)
		}
		out.WriteString("\n")
	}
	t.Log(out.String())
	if path := os.Getenv("CANDIDATE_SIM_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCandidateSelectionLiveStage2 compares today's design, full scoring on exact counts read
// from Redis for every pod on every request, with a candidate stage whose index is refreshed
// from Redis only every SignalDelay and which then reads exact counts for its candidates. Run it
// with CANDIDATE_SIM=live.
func TestCandidateSelectionLiveStage2(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "live" {
		t.Skip("set CANDIDATE_SIM=live to run the live stage-2 comparison")
	}
	policies := []policy{
		{Kind: policyRandomSeeded, K: 16, LiveScoring: true, GateSample: true},
		{Kind: policyCountIndex, K: 16, LiveScoring: true},
		{Kind: policyCountIndexMixed, K: 16, LiveScoring: true},
		{Kind: policyCountIndex, K: 16},
	}
	switch os.Getenv("CANDIDATE_SIM_GATE") {
	case "fleet":
		policies[0].GateSample = false
	case "sample":
		for i := range policies[:3] {
			policies[i].GateSample = true
		}
	}
	seeds := []uint64{1, 2, 3}
	delays := []time.Duration{100 * time.Millisecond, time.Second}
	if os.Getenv("CANDIDATE_SIM_QUICK") != "" {
		seeds, delays = seeds[:1], delays[:1]
		policies = []policy{policies[0], policies[2]}
	}
	var out strings.Builder
	out.WriteString("Mean latency relative to full scoring on exact counts (today with Redis), same seed: mean (min to max). Prefix hit ratio after the slash.\n\n")
	out.WriteString("| Policy | Index refresh | Prefix off | Prefix on |\n| --- | --- | --- | --- |\n")
	base := func(prefixOn bool, delay time.Duration, seed uint64) simConfig {
		return simConfig{
			Pods: livePods(), Gateways: 4, Utilization: 0.85, MeanWork: 200,
			PrefixOn: prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
			SignalDelay: delay, Step: 5 * time.Millisecond,
			Warmup: 3 * time.Second, Duration: 25 * time.Second,
			PrefixSeedMax: liveM(), SeedRandom: os.Getenv("CANDIDATE_SIM_SEED") != "load", Seed: seed,
			ImbalanceMinGap: 8, ImbalanceFactor: 2,
		}
	}
	full := map[[2]int]result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	run := func(cfg simConfig, pol policy, store func(result)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := newSim(cfg, pol).run()
			mu.Lock()
			store(r)
			mu.Unlock()
		}()
	}
	for pi, prefixOn := range []bool{false, true} {
		for ki, seed := range seeds {
			run(base(prefixOn, 0, seed), policy{Kind: policyFull}, func(r result) { full[[2]int{pi, ki}] = r })
		}
	}
	got := map[[4]int]result{}
	for di, delay := range delays {
		for poi, pol := range policies {
			for pi, prefixOn := range []bool{false, true} {
				for ki, seed := range seeds {
					run(base(prefixOn, delay, seed), pol, func(r result) { got[[4]int{di, poi, pi, ki}] = r })
				}
			}
		}
	}
	wg.Wait()
	fmt.Fprintf(&out, "| full, exact counts | every request | %.3f s | %.3f s / %.1f%% |\n",
		full[[2]int{0, 0}].MeanLatency, full[[2]int{1, 0}].MeanLatency, 100*full[[2]int{1, 0}].PrefixHit)
	for di, delay := range delays {
		for poi, pol := range policies {
			fmt.Fprintf(&out, "| %s | %v |", pol, delay)
			for pi := range []bool{false, true} {
				var sum, hit float64
				lo, hi := math.Inf(1), math.Inf(-1)
				for ki := range seeds {
					r := got[[4]int{di, poi, pi, ki}]
					x := r.MeanLatency / full[[2]int{pi, ki}].MeanLatency
					sum += x
					lo, hi = math.Min(lo, x), math.Max(hi, x)
					hit += r.PrefixHit
				}
				n := float64(len(seeds))
				cell := fmt.Sprintf("%.2f (%.2f to %.2f)", sum/n, lo, hi)
				if pi == 1 {
					cell += fmt.Sprintf(" / %.1f%%", 100*hit/n)
				}
				fmt.Fprintf(&out, " %s |", cell)
			}
			out.WriteString("\n")
		}
	}
	t.Log(out.String())
	if path := os.Getenv("CANDIDATE_SIM_OUT"); path != "" {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The live stage-2 comparison runs the shipped defaults (500 pods, M=16, holders seeded at
// random, gate on the sample) unless CANDIDATE_SIM_PODS, CANDIDATE_SIM_M or CANDIDATE_SIM_SEED=load
// say otherwise; CANDIDATE_SIM_QUICK=1 runs one seed at the 100 ms refresh only. The index-backed
// policies (C, C+A) gate the whole fleet on the index; CANDIDATE_SIM_GATE=sample gates their
// sample instead, and CANDIDATE_SIM_GATE=fleet gates the whole fleet for policy A too.
func livePods() int {
	if n, err := strconv.Atoi(os.Getenv("CANDIDATE_SIM_PODS")); err == nil && n > 0 {
		return n
	}
	return 500
}

func liveM() int {
	if n, err := strconv.Atoi(os.Getenv("CANDIDATE_SIM_M")); err == nil && n >= 0 {
		return n
	}
	return 16
}
