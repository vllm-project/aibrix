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

package candidatesim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// defaultEvalConfig extends simConfig with per-replica inflight cap and pod
// blacklist settings to evaluate default-on candidate selection (#3188).
type defaultEvalConfig struct {
	simConfig
	InflightCap int // 0 disables per-pod inflight cap
	// BlacklistMask is a policy-independent pod exclusion mask (len == Pods).
	// When BlacklistAtWarmup is true, exclusions activate at t == Warmup and the
	// post-warmup arrival rate is resized to the exact remaining healthy capacity.
	BlacklistMask      []bool
	BlacklistAtWarmup  bool
	RandomOnlyScoutCap bool // evaluate the 50% scout cap on the random 4K slice only
	// ScoutRandomCapMode overrides the 8K >= N whole-fleet shortcut in selectKOnly:
	// 0 = shipped (whole fleet when 8K >= N),
	// 1 = always draw random 4K scout (capped at N),
	// 2 = always draw random min(4K, N/2) scout.
	ScoutRandomCapMode int
}

func newEvalConfig(pods int, util float64, prefixOn bool, delay, dur time.Duration, seed uint64) defaultEvalConfig {
	return defaultEvalConfig{
		simConfig: simConfig{
			Pods: pods, Gateways: 4, Utilization: util, MeanWork: 200,
			PrefixOn: prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
			SignalDelay: delay, Step: 5 * time.Millisecond,
			Warmup: 3 * time.Second, Duration: dur,
			SeedRandom: true, Seed: seed,
			ImbalanceMinGap: 8, ImbalanceFactor: 2,
		},
	}
}

type evalResult struct {
	result
	Rejected429    int
	False429       int
	Reselects      int
	FallbacksAllBL int
}

type evalSim struct {
	*sim
	ecfg        defaultEvalConfig
	blacklisted []bool
	blCount     int
	rejected429 int
	false429    int
	reselects   int
	fallbacksBL int
}

// buildBlacklist constructs a single policy-independent blacklist mask for
// (pods, frac, correlated, seed) so that full (K=0) and every K-only policy
// face the exact same blacklisted pods and remaining healthy capacity.
//
// When correlated is true, it runs one shared reference full (K=0) warmup to
// identify the pods holding the hottest Zipf prefix groups, then blacklists
// those holders up to round(frac*pods) (filling any remainder uniformly).
func buildBlacklist(base simConfig, frac float64, correlated bool) []bool {
	mask := make([]bool, base.Pods)
	target := int(math.Round(frac * float64(base.Pods)))
	if target <= 0 {
		return mask
	}
	rng := rand.New(rand.NewPCG(base.Seed^0x5bf03635, uint64(base.Pods)*31+uint64(target)))
	count := 0
	if correlated {
		refCfg := base
		refCfg.SignalDelay = 0
		refCfg.Warmup = 0
		refCfg.Duration = base.Warmup
		ref := newSim(refCfg, policy{Kind: policyFull, LiveScoring: true})
		ref.run()
		for g := 0; g < base.PrefixGroups && count < target; g++ {
			for _, h := range ref.holder[g] {
				if !mask[h] {
					mask[h] = true
					count++
					if count >= target {
						break
					}
				}
			}
		}
	}
	for _, idx := range rng.Perm(base.Pods) {
		if count >= target {
			break
		}
		if !mask[idx] {
			mask[idx] = true
			count++
		}
	}
	return mask
}

func newEvalSim(ecfg defaultEvalConfig, pol policy) *evalSim {
	s := newSim(ecfg.simConfig, pol)
	es := &evalSim{
		sim:         s,
		ecfg:        ecfg,
		blacklisted: make([]bool, ecfg.Pods),
	}
	if len(ecfg.BlacklistMask) == ecfg.Pods && !ecfg.BlacklistAtWarmup {
		es.activateBlacklist(ecfg.BlacklistMask)
	}
	return es
}

func (es *evalSim) activateBlacklist(mask []bool) {
	if len(mask) != len(es.blacklisted) {
		return
	}
	es.blCount = 0
	for i, b := range mask {
		es.blacklisted[i] = b
		if b {
			es.blCount++
		}
	}
}

func (es *evalSim) healthyCapacity() float64 {
	capSum := 0.0
	for i, p := range es.pods {
		if !es.blacklisted[i] {
			capSum += p.capacity
		}
	}
	return capSum
}

func (es *evalSim) filterBlacklisted(pods []int) []int {
	if es.blCount == 0 {
		return pods
	}
	kept := make([]int, 0, len(pods))
	for _, i := range pods {
		if !es.blacklisted[i] {
			kept = append(kept, i)
		}
	}
	return kept
}

func overBlacklistCap(kept, total int) bool {
	if total == 0 || kept == 0 {
		return true
	}
	excluded := total - kept
	// Mirrors AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT default = 50% (ExclusionOverCap).
	return excluded*100 > 50*total
}

// selectKOnly mirrors policyBlendRanked in sim_test.go (keep in sync), adding
// the scout-stage blacklist (#3543) and inflight (#3467) filters in between.
func (es *evalSim) selectKOnly(g, group int, ready []int, allowScoutBLFilter bool) []int {
	n := len(ready)
	k := es.pol.K
	if k <= 0 || n <= k {
		return ready
	}
	var scout []int
	var randStart int
	// Mirrors 2*candidateScoutFactor*k >= len(readyPods) in algorithms/candidates.go (candidateScout).
	if es.ecfg.ScoutRandomCapMode == 0 && 8*k >= n {
		scout = ready
		randStart = 0
	} else {
		var holders []int
		if es.cfg.PrefixOn {
			inReady := make(map[int]struct{}, len(ready))
			for _, i := range ready {
				inReady[i] = struct{}{}
			}
			for _, h := range es.holder[group] {
				if _, ok := inReady[h]; ok {
					holders = append(holders, h)
				}
			}
			es.rng.Shuffle(len(holders), func(a, b int) { holders[a], holders[b] = holders[b], holders[a] })
			holders = holders[:min(k, len(holders))]
		}
		randStart = len(holders)
		randSize := 4 * k
		if es.ecfg.ScoutRandomCapMode == 2 {
			randSize = min(randSize, n/2)
		}
		scout = es.fillRandom(ready, holders, min(len(holders)+randSize, n))
	}

	// 1. Cap-aware blacklist scout filter (#3543) before inflight filter.
	if allowScoutBLFilter && es.blCount > 0 {
		var overCap bool
		if es.ecfg.RandomOnlyScoutCap && randStart > 0 && randStart < len(scout) {
			randPart := scout[randStart:]
			keptRand := es.filterBlacklisted(randPart)
			overCap = overBlacklistCap(len(keptRand), len(randPart))
		} else {
			kept := es.filterBlacklisted(scout)
			overCap = overBlacklistCap(len(kept), len(scout))
		}
		if !overCap {
			if kept := es.filterBlacklisted(scout); len(kept) > 0 {
				scout = kept
			}
		}
	}

	// 2. Soft in-memory inflight saturation filter on scout (#3467).
	if es.ecfg.InflightCap > 0 {
		kept := make([]int, 0, len(scout))
		for _, i := range scout {
			if es.observed(g, i) < es.ecfg.InflightCap {
				kept = append(kept, i)
			}
		}
		if len(kept) > 0 {
			scout = kept
		}
	}

	r := k / 2
	if es.pol.Ranked > 0 {
		r = min(es.pol.Ranked, k)
	}
	ranked := es.bestByEstimate(g, group, scout, r)
	return es.fillRandom(ready, ranked, k)
}

func (es *evalSim) routeEval(r *request, group int, countStats bool) {
	g := r.gateway
	fleet := make([]int, len(es.pods))
	for i := range fleet {
		fleet[i] = i
	}

	var cand []int
	if es.pol.Kind == policyFull {
		cand = fleet
		if es.blCount > 0 {
			kept := es.filterBlacklisted(cand)
			if !overBlacklistCap(len(kept), len(cand)) {
				cand = kept
			} else if countStats {
				es.fallbacksBL++
			}
		}
	} else {
		cand = es.selectKOnly(g, group, fleet, true)
		// Authoritative blacklist check on K candidates + O(N) ReselectCandidates (#3543).
		if es.blCount > 0 {
			kept := es.filterBlacklisted(cand)
			if !overBlacklistCap(len(kept), len(cand)) {
				cand = kept
			} else if len(cand) < len(fleet) {
				allowed := es.filterBlacklisted(fleet)
				if !overBlacklistCap(len(allowed), len(fleet)) {
					if countStats {
						es.reselects++
					}
					cand = es.selectKOnly(g, group, allowed, false)
				} else if countStats {
					es.fallbacksBL++
				}
			} else if countStats {
				es.fallbacksBL++
			}
		}
	}

	// Authoritative exact-count inflight filter on candidates (or full fleet).
	es.exact = es.pol.LiveScoring || es.pol.Kind == policyFull
	defer func() { es.exact = false }()

	if es.ecfg.InflightCap > 0 {
		kept := make([]int, 0, len(cand))
		for _, i := range cand {
			if len(es.pods[i].running) < es.ecfg.InflightCap {
				kept = append(kept, i)
			}
		}
		if len(kept) == 0 {
			if countStats {
				es.decided++
				es.rejected429++
				for _, i := range fleet {
					if es.blCount > 0 && es.blacklisted[i] {
						continue
					}
					if len(es.pods[i].running) < es.ecfg.InflightCap {
						es.false429++
						break
					}
				}
			}
			return
		}
		cand = kept
	}

	if gated := es.eligibleAmong(g, cand); gated != nil {
		if countStats {
			es.gated++
		}
		cand = gated
	}
	target := es.argmax(cand, es.finalScores(g, group, cand))
	if countStats {
		es.decided++
		es.scored += len(cand)
	}
	es.place(r, target, group)
}

// runEval mirrors sim.run in sim_test.go (keep in sync), adding policy-independent
// post-warmup blacklist activation sized from actual remaining healthy capacity
// and inflight-cap / reselect / fallback accounting.
func (es *evalSim) runEval() evalResult {
	cfg := es.cfg
	work := cfg.MeanWork
	if cfg.PrefixOn {
		work *= 1 - cfg.PrefixSaving*0.5
	}
	// Size arrival rate from actual healthy capacity rather than assuming
	// (1 - BlacklistFrac) * totalCap, because pod capacities are heterogeneous.
	rate := cfg.Utilization * es.healthyCapacity() / work
	dt := cfg.Step.Seconds()
	end := cfg.Duration.Seconds()
	nextArrival := es.rng.ExpFloat64() / rate
	gw := 0
	activatedAtWarmup := !es.ecfg.BlacklistAtWarmup

	for es.now = 0; es.now < end; es.now += dt {
		if cfg.SignalDelay > 0 && es.now >= es.nextRefresh {
			es.refreshSignals()
			es.nextRefresh += cfg.SignalDelay.Seconds()
		}
		if !activatedAtWarmup && es.now >= cfg.Warmup.Seconds() {
			es.activateBlacklist(es.ecfg.BlacklistMask)
			if hc := es.healthyCapacity(); hc > 0 {
				rate = cfg.Utilization * hc / work
			}
			activatedAtWarmup = true
		}
		for nextArrival < es.now+dt {
			r := &request{arrival: nextArrival, remaining: es.work(), gateway: gw}
			gw = (gw + 1) % cfg.Gateways
			group := int(es.zipf.Uint64())
			es.routeEval(r, group, es.now >= cfg.Warmup.Seconds())
			nextArrival += es.rng.ExpFloat64() / rate
		}
		maxRun, sumRun := 0, 0
		for i, p := range es.pods {
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
				es.local[r.gateway][i]--
				if r.arrival >= cfg.Warmup.Seconds() {
					es.latencies = append(es.latencies, es.now+dt-r.arrival)
				}
			}
			p.running = kept
			maxRun = max(maxRun, len(kept))
			sumRun += len(kept)
		}
		if es.now >= cfg.Warmup.Seconds() && sumRun > 0 {
			es.loadRatioSum += float64(maxRun) / (float64(sumRun) / float64(len(es.pods)))
			es.loadRatioN++
		}
	}

	sort.Float64s(es.latencies)
	mean := 0.0
	for _, l := range es.latencies {
		mean += l
	}
	var p99 float64
	if len(es.latencies) > 0 {
		p99 = es.latencies[len(es.latencies)*99/100]
	}
	res := evalResult{
		result: result{
			Policy:        es.pol.String(),
			Requests:      es.decided,
			Gated:         float64(es.gated) / float64(max(es.decided, 1)),
			MeanLatency:   mean / float64(max(len(es.latencies), 1)),
			P99Latency:    p99,
			ScoredPerReq:  float64(es.scored) / float64(max(es.decided, 1)),
			MaxToMeanLoad: es.loadRatioSum / float64(max(es.loadRatioN, 1)),
		},
		Rejected429:    es.rejected429,
		False429:       es.false429,
		Reselects:      es.reselects,
		FallbacksAllBL: es.fallbacksBL,
	}
	if cfg.PrefixOn && es.decided > es.rejected429 {
		res.PrefixHit = float64(es.hits) / float64(es.decided-es.rejected429)
	}
	return res
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	m := sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		d := x - m
		sq += d * d
	}
	return m, math.Sqrt(sq / float64(len(xs)))
}

func medianVal(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

// TestCandidateSelectionDefaultK compares shipped K-only (K=16, 32, 64) at
// delay=1s (the #3532 remote running reader default) against exact-count full
// scoring (K=0, SignalDelay=0) across N={200, 300, 500, 2000} and
// util={0.85, 0.92} (15s runs at N<=500, 10s runs at N=2000).
// Run with CANDIDATE_SIM=defaultk (~5-6 min on 8 cores).
func TestCandidateSelectionDefaultK(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the default-K evaluation")
	}
	type cellKey struct {
		n        int
		util     float64
		prefixOn bool
		k        int
		seed     uint64
	}
	sizes := []int{200, 300, 500, 2000}
	utils := []float64{0.85, 0.92}
	ks := []int{0, 16, 32, 64}
	seeds := []uint64{1, 2, 3}

	results := map[cellKey]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, n := range sizes {
		for _, u := range utils {
			for _, prefixOn := range []bool{false, true} {
				for _, k := range ks {
					for _, seed := range seeds {
						key := cellKey{n, u, prefixOn, k, seed}
						wg.Add(1)
						go func() {
							defer wg.Done()
							sem <- struct{}{}
							defer func() { <-sem }()
							dur := 15 * time.Second
							if n == 2000 {
								dur = 10 * time.Second
							}
							delay := time.Second
							pol := policy{Kind: policyBlendRanked, K: k, LiveScoring: true, GateSample: true}
							if k == 0 {
								delay = 0
								pol = policy{Kind: policyFull, LiveScoring: true}
							}
							cfg := newEvalConfig(n, u, prefixOn, delay, dur, seed)
							r := newEvalSim(cfg, pol).runEval()
							mu.Lock()
							results[key] = r
							mu.Unlock()
						}()
					}
				}
			}
		}
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n### Default-K sweep across fleet size and utilization (delay=1s vs exact full K=0, 4 gateways, 3 seeds; 15s at N<=500, 10s at N=2000)\n\n")
	for _, u := range utils {
		fmt.Fprintf(&sb, "#### Utilization = %.2f\n\n", u)
		sb.WriteString("| N | Policy | prefix=false (mean / P99) | prefix=true (mean / P99 / hit%) |\n| --- | --- | --- | --- |\n")
		for _, n := range sizes {
			for _, k := range ks {
				label := "full (K=0, exact)"
				if k > 0 {
					label = fmt.Sprintf("K-only K=%d (1s RRQ)", k)
				}
				fmt.Fprintf(&sb, "| %d | %s |", n, label)
				for _, prefixOn := range []bool{false, true} {
					numSeeds := float64(len(seeds))
					if k == 0 {
						var meanSum, p99Sum, hitSum float64
						for _, seed := range seeds {
							r := results[cellKey{n, u, prefixOn, 0, seed}]
							meanSum += r.MeanLatency
							p99Sum += r.P99Latency
							hitSum += r.PrefixHit
						}
						if prefixOn {
							fmt.Fprintf(&sb, " %.3fs / %.3fs / %.1f%% |", meanSum/numSeeds, p99Sum/numSeeds, 100*hitSum/numSeeds)
						} else {
							fmt.Fprintf(&sb, " %.3fs / %.3fs |", meanSum/numSeeds, p99Sum/numSeeds)
						}
					} else {
						var meanR, p99R, hitSum float64
						lo, hi := math.Inf(1), math.Inf(-1)
						for _, seed := range seeds {
							r := results[cellKey{n, u, prefixOn, k, seed}]
							f := results[cellKey{n, u, prefixOn, 0, seed}]
							mr := r.MeanLatency / f.MeanLatency
							meanR += mr
							lo = math.Min(lo, mr)
							hi = math.Max(hi, mr)
							p99R += r.P99Latency / f.P99Latency
							hitSum += r.PrefixHit
						}
						if prefixOn {
							fmt.Fprintf(&sb, " %.2fx (%.2f..%.2f) / %.2fx / %.1f%% |", meanR/numSeeds, lo, hi, p99R/numSeeds, 100*hitSum/numSeeds)
						} else {
							fmt.Fprintf(&sb, " %.2fx (%.2f..%.2f) / %.2fx |", meanR/numSeeds, lo, hi, p99R/numSeeds)
						}
					}
				}
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
	}
	t.Log(sb.String())
}

// TestCandidateSelectionInflightCap measures 429 and false-429 rates under
// per-replica requestsInflight caps for K={0, 16, 32, 64} at N={300, 500},
// delay=1s. Emits the complete markdown table (including full baseline and
// prefix=true overload rows). Run with CANDIDATE_SIM=defaultk (~3 min on 8 cores).
func TestCandidateSelectionInflightCap(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the requestsInflight evaluation")
	}
	type key struct {
		n        int
		util     float64
		cap      int
		prefixOn bool
		k        int
		seed     uint64
	}
	sizes := []int{300, 500}
	cases := []struct {
		util float64
		cap  int
	}{
		{0.85, 2},
		{0.92, 4},
		{0.92, 12},
		{1.05, 4},
	}
	ks := []int{0, 16, 32, 64}
	seeds := []uint64{1, 2, 3}

	results := map[key]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, n := range sizes {
		for _, tc := range cases {
			for _, prefixOn := range []bool{false, true} {
				for _, k := range ks {
					for _, seed := range seeds {
						ck := key{n, tc.util, tc.cap, prefixOn, k, seed}
						wg.Add(1)
						go func() {
							defer wg.Done()
							sem <- struct{}{}
							defer func() { <-sem }()
							delay := time.Second
							pol := policy{Kind: policyBlendRanked, K: k, LiveScoring: true, GateSample: true}
							if k == 0 {
								delay = 0
								pol = policy{Kind: policyFull, LiveScoring: true}
							}
							cfg := newEvalConfig(n, tc.util, prefixOn, delay, 15*time.Second, seed)
							cfg.InflightCap = tc.cap
							r := newEvalSim(cfg, pol).runEval()
							mu.Lock()
							results[ck] = r
							mu.Unlock()
						}()
					}
				}
			}
		}
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n### Per-replica `requestsInflight` cap and false-429 evaluation (4 gateways, delay=1s, 3 seeds)\n\n")
	sb.WriteString("| Fleet | Scenario | 429% (full / K=16 / K=32 / K=64) | False-429% (full / K=16 / K=32 / K=64) | Mean Lat Ratio (K=16 / K=32 / K=64) | P99 Ratio (K=16 / K=32 / K=64) |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	numSeeds := float64(len(seeds))
	for _, n := range sizes {
		for _, tc := range cases {
			for _, prefixOn := range []bool{false, true} {
				var rejCols, falseCols, meanCols, p99Cols []string
				for _, k := range ks {
					var rejSum, falseSum, reqSum, meanR, p99R float64
					for _, seed := range seeds {
						r := results[key{n, tc.util, tc.cap, prefixOn, k, seed}]
						f := results[key{n, tc.util, tc.cap, prefixOn, 0, seed}]
						rejSum += float64(r.Rejected429)
						falseSum += float64(r.False429)
						reqSum += float64(r.Requests)
						if f.MeanLatency > 0 {
							meanR += r.MeanLatency / f.MeanLatency
						}
						if f.P99Latency > 0 {
							p99R += r.P99Latency / f.P99Latency
						}
					}
					rejCols = append(rejCols, fmt.Sprintf("%.3f%%", 100*rejSum/reqSum))
					falseCols = append(falseCols, fmt.Sprintf("%.3f%%", 100*falseSum/reqSum))
					if k > 0 {
						meanCols = append(meanCols, fmt.Sprintf("%.2fx", meanR/numSeeds))
						p99Cols = append(p99Cols, fmt.Sprintf("%.2fx", p99R/numSeeds))
					}
				}
				fmt.Fprintf(&sb, "| N=%d | util=%.2f cap=%d prefix=%v | %s | %s | %s | %s |\n",
					n, tc.util, tc.cap, prefixOn,
					strings.Join(rejCols, " / "),
					strings.Join(falseCols, " / "),
					strings.Join(meanCols, " / "),
					strings.Join(p99Cols, " / "))
			}
		}
	}
	t.Log(sb.String())
}

// TestCandidateSelectionBlacklist evaluates K={0, 16, 32, 64} under policy-independent
// uniform and correlated (hot prefix holder) blacklists at N={300, 500, 2000},
// including BL=45% and BL=48% near the 50% exclusion cap knee and a comparison of
// shipped whole-scout cap vs random-only 4K scout cap (#3543).
// Run with CANDIDATE_SIM=defaultk (~4-5 min on 8 cores).
func TestCandidateSelectionBlacklist(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the blacklist evaluation")
	}
	type scenario struct {
		n          int
		frac       float64
		correlated bool
		seeds      []uint64
		dur        time.Duration
		checkRand  bool
	}
	scenarios := []scenario{
		{300, 0.15, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{300, 0.40, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{300, 0.48, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{300, 0.40, true, []uint64{1, 2, 3}, 15 * time.Second, false},
		{300, 0.48, true, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.15, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.40, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.48, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.40, true, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.48, true, []uint64{1, 2, 3}, 15 * time.Second, false},
		{2000, 0.40, true, []uint64{1, 2, 3}, 10 * time.Second, true},
		{2000, 0.48, true, []uint64{1, 2, 3}, 10 * time.Second, true},
	}
	type variant struct {
		k        int
		randOnly bool
		label    string
	}
	baseVariants := []variant{
		{0, false, "full (K=0)"},
		{16, false, "K=16"},
		{32, false, "K=32"},
		{64, false, "K=64"},
	}
	randCapVariants := []variant{
		{32, true, "K=32 (random-only 4K cap)"},
		{64, true, "K=64 (random-only 4K cap)"},
	}

	// Build each policy-independent blacklist mask once per (scenario, seed).
	type maskKey struct {
		si   int
		seed uint64
	}
	masks := map[maskKey][]bool{}
	for si, sc := range scenarios {
		for _, seed := range sc.seeds {
			base := newEvalConfig(sc.n, 0.85, true, 0, sc.dur, seed).simConfig
			masks[maskKey{si, seed}] = buildBlacklist(base, sc.frac, sc.correlated)
		}
	}

	type key struct {
		si       int
		k        int
		randOnly bool
		seed     uint64
	}
	results := map[key]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for si, sc := range scenarios {
		vs := append([]variant(nil), baseVariants...)
		if sc.checkRand {
			vs = append(vs, randCapVariants...)
		}
		for _, v := range vs {
			for _, seed := range sc.seeds {
				ck := key{si, v.k, v.randOnly, seed}
				mask := masks[maskKey{si, seed}]
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					delay := time.Second
					pol := policy{Kind: policyBlendRanked, K: v.k, LiveScoring: true, GateSample: true}
					if v.k == 0 {
						delay = 0
						pol = policy{Kind: policyFull, LiveScoring: true}
					}
					cfg := newEvalConfig(sc.n, 0.85, true, delay, sc.dur, seed)
					cfg.BlacklistMask = mask
					cfg.BlacklistAtWarmup = sc.correlated
					cfg.RandomOnlyScoutCap = v.randOnly
					r := newEvalSim(cfg, pol).runEval()
					mu.Lock()
					results[ck] = r
					mu.Unlock()
				}()
			}
		}
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n### Pod blacklist x K-only evaluation (policy-independent blacklist, true remaining-capacity sizing, prefix=true, util=0.85, delay=1s, 3 seeds; 15s at N<=500, 10s at N=2000)\n\n")
	sb.WriteString("| Scenario | Policy | Mean Lat (med / mean) | P99 Lat (med / mean) | Prefix Hit% | O(N) Reselect% | Fallback% |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for si, sc := range scenarios {
		mode := "uniform"
		if sc.correlated {
			mode = "correlated hot holders"
		}
		vs := append([]variant(nil), baseVariants...)
		if sc.checkRand {
			vs = append(vs, randCapVariants...)
		}
		ns := float64(len(sc.seeds))
		for _, v := range vs {
			means := make([]float64, 0, len(sc.seeds))
			p99s := make([]float64, 0, len(sc.seeds))
			var hit, resel, fb, req float64
			for _, seed := range sc.seeds {
				r := results[key{si, v.k, v.randOnly, seed}]
				f := results[key{si, 0, false, seed}]
				means = append(means, r.MeanLatency/f.MeanLatency)
				p99s = append(p99s, r.P99Latency/f.P99Latency)
				hit += r.PrefixHit
				resel += float64(r.Reselects)
				fb += float64(r.FallbacksAllBL)
				req += float64(r.Requests)
			}
			mm, _ := meanStd(means)
			pm, _ := meanStd(p99s)
			fmt.Fprintf(&sb, "| N=%d BL=%.0f%% (%s) | %s | %.2fx / %.2fx | %.2fx / %.2fx | %.1f%% | %.2f%% | %.2f%% |\n",
				sc.n, sc.frac*100, mode, v.label, medianVal(means), mm, medianVal(p99s), pm,
				100*hit/ns, 100*resel/req, 100*fb/req)
		}
	}
	t.Log(sb.String())
}

// TestCandidateSelectionMidFleet10Seed runs 10 seeds (1..10) on the mid-size
// N={300, 500} prefix=true delay=1s cells (with policy-independent blacklists
// and true remaining-capacity sizing) and reports medians alongside mean ± std.
// Run with CANDIDATE_SIM=defaultk (~3 min on 8 cores).
func TestCandidateSelectionMidFleet10Seed(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the 10-seed mid-fleet evaluation")
	}
	type scenario struct {
		n    int
		util float64
		bl   float64
		corr bool
		name string
	}
	scenarios := []scenario{
		{300, 0.85, 0, false, "N=300 util=0.85 no BL"},
		{300, 0.92, 0, false, "N=300 util=0.92 no BL"},
		{300, 0.85, 0.40, true, "N=300 util=0.85 BL=40% corr"},
		{500, 0.85, 0, false, "N=500 util=0.85 no BL"},
		{500, 0.92, 0, false, "N=500 util=0.92 no BL"},
		{500, 0.85, 0.40, true, "N=500 util=0.85 BL=40% corr"},
	}
	type variant struct {
		k         int
		scoutMode int
		label     string
	}
	variants := []variant{
		{0, 0, "full (K=0)"},
		{32, 0, "K=32 (shipped)"},
		{64, 0, "K=64 (shipped, 8K>=N whole-fleet)"},
		{64, 1, "K=64 (random 4K scout)"},
		{64, 2, "K=64 (random min(4K,N/2) scout)"},
	}
	seeds := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	type maskKey struct {
		si   int
		seed uint64
	}
	masks := map[maskKey][]bool{}
	for si, sc := range scenarios {
		if sc.bl <= 0 {
			continue
		}
		for _, seed := range seeds {
			base := newEvalConfig(sc.n, sc.util, true, 0, 15*time.Second, seed).simConfig
			masks[maskKey{si, seed}] = buildBlacklist(base, sc.bl, sc.corr)
		}
	}

	type key struct {
		si   int
		vi   int
		seed uint64
	}
	results := map[key]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for si, sc := range scenarios {
		for vi, v := range variants {
			for _, seed := range seeds {
				ck := key{si, vi, seed}
				mask := masks[maskKey{si, seed}]
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					delay := time.Second
					pol := policy{Kind: policyBlendRanked, K: v.k, LiveScoring: true, GateSample: true}
					if v.k == 0 {
						delay = 0
						pol = policy{Kind: policyFull, LiveScoring: true}
					}
					cfg := newEvalConfig(sc.n, sc.util, true, delay, 15*time.Second, seed)
					cfg.BlacklistMask = mask
					cfg.BlacklistAtWarmup = sc.corr
					cfg.ScoutRandomCapMode = v.scoutMode
					r := newEvalSim(cfg, pol).runEval()
					mu.Lock()
					results[ck] = r
					mu.Unlock()
				}()
			}
		}
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n### Mid-fleet 10-seed check (prefix=true, delay=1s, seeds 1..10; policy-independent BL & true remaining-capacity sizing)\n\n")
	sb.WriteString("| Scenario | Policy | Mean Lat (med / mean ± std) | P99 Lat (med / mean ± std) | Prefix Hit% | O(N) Reselect% |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for si, sc := range scenarios {
		for vi, v := range variants {
			if vi == 0 {
				continue
			}
			means := make([]float64, 0, len(seeds))
			p99s := make([]float64, 0, len(seeds))
			var hitSum, reselSum, reqSum float64
			for _, seed := range seeds {
				r := results[key{si, vi, seed}]
				f := results[key{si, 0, seed}]
				means = append(means, r.MeanLatency/f.MeanLatency)
				p99s = append(p99s, r.P99Latency/f.P99Latency)
				hitSum += r.PrefixHit
				reselSum += float64(r.Reselects)
				reqSum += float64(r.Requests)
			}
			mm, ms := meanStd(means)
			pm, ps := meanStd(p99s)
			fmt.Fprintf(&sb, "| %s | %s | %.3fx / %.3fx ± %.3f | %.3fx / %.3fx ± %.3f | %.1f%% | %.2f%% |\n",
				sc.name, v.label, medianVal(means), mm, ms, medianVal(p99s), pm, ps,
				100*hitSum/float64(len(seeds)), 100*reselSum/reqSum)
		}
	}
	t.Log(sb.String())
}

// TestCandidateSelectionHeavyTail checks K={16, 32, 64} vs exact full (K=0)
// under heavy-tailed request work (HeavyTail: true) at N={300, 500}, util=0.85,
// delay=1s over 5 seeds. Run with CANDIDATE_SIM=defaultk (~1 min on 8 cores).
func TestCandidateSelectionHeavyTail(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the heavy-tail evaluation")
	}
	type key struct {
		n        int
		prefixOn bool
		k        int
		seed     uint64
	}
	sizes := []int{300, 500}
	ks := []int{0, 16, 32, 64}
	seeds := []uint64{1, 2, 3, 4, 5}

	results := map[key]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, n := range sizes {
		for _, prefixOn := range []bool{false, true} {
			for _, k := range ks {
				for _, seed := range seeds {
					ck := key{n, prefixOn, k, seed}
					wg.Add(1)
					go func() {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						delay := time.Second
						pol := policy{Kind: policyBlendRanked, K: k, LiveScoring: true, GateSample: true}
						if k == 0 {
							delay = 0
							pol = policy{Kind: policyFull, LiveScoring: true}
						}
						cfg := newEvalConfig(n, 0.85, prefixOn, delay, 15*time.Second, seed)
						cfg.HeavyTail = true
						r := newEvalSim(cfg, pol).runEval()
						mu.Lock()
						results[ck] = r
						mu.Unlock()
					}()
				}
			}
		}
	}
	wg.Wait()

	var sb strings.Builder
	sb.WriteString("\n### Heavy-tailed request sizes (HeavyTail=true, util=0.85, delay=1s, 4 gateways, 5 seeds)\n\n")
	sb.WriteString("| N | Policy | prefix=false (med / mean ± std; P99 med) | prefix=true (med / mean ± std; P99 med / hit%) |\n")
	sb.WriteString("| --- | --- | --- | --- |\n")
	for _, n := range sizes {
		for _, k := range ks {
			label := "full (K=0, exact)"
			if k > 0 {
				label = fmt.Sprintf("K-only K=%d (1s RRQ)", k)
			}
			fmt.Fprintf(&sb, "| %d | %s |", n, label)
			for _, prefixOn := range []bool{false, true} {
				if k == 0 {
					var ms, ps, hs []float64
					for _, seed := range seeds {
						r := results[key{n, prefixOn, 0, seed}]
						ms = append(ms, r.MeanLatency)
						ps = append(ps, r.P99Latency)
						hs = append(hs, r.PrefixHit)
					}
					mm, _ := meanStd(ms)
					pm, _ := meanStd(ps)
					hm, _ := meanStd(hs)
					if prefixOn {
						fmt.Fprintf(&sb, " %.3fs / %.3fs / %.1f%% |", mm, pm, 100*hm)
					} else {
						fmt.Fprintf(&sb, " %.3fs / %.3fs |", mm, pm)
					}
				} else {
					var mr, pr []float64
					var hitSum float64
					for _, seed := range seeds {
						r := results[key{n, prefixOn, k, seed}]
						f := results[key{n, prefixOn, 0, seed}]
						mr = append(mr, r.MeanLatency/f.MeanLatency)
						pr = append(pr, r.P99Latency/f.P99Latency)
						hitSum += r.PrefixHit
					}
					mm, mstd := meanStd(mr)
					if prefixOn {
						fmt.Fprintf(&sb, " %.2fx / %.2fx ± %.2f; P99 %.2fx / %.1f%% |",
							medianVal(mr), mm, mstd, medianVal(pr), 100*hitSum/float64(len(seeds)))
					} else {
						fmt.Fprintf(&sb, " %.2fx / %.2fx ± %.2f; P99 %.2fx |",
							medianVal(mr), mm, mstd, medianVal(pr))
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	t.Log(sb.String())
}
