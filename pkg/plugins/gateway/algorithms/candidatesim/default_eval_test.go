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
	InflightCap         int     // 0 disables per-pod inflight cap
	BlacklistFrac       float64 // fraction of pods excluded (soft cap 50%)
	CorrelatedBlacklist bool    // blacklist hottest prefix holders first
	RandomOnlyScoutCap  bool    // evaluate the 50% scout cap on the random 4K slice only
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

func newEvalSim(ecfg defaultEvalConfig, pol policy) *evalSim {
	s := newSim(ecfg.simConfig, pol)
	es := &evalSim{
		sim:         s,
		ecfg:        ecfg,
		blacklisted: make([]bool, ecfg.Pods),
	}
	if ecfg.BlacklistFrac > 0 && !ecfg.CorrelatedBlacklist {
		nBL := int(math.Round(ecfg.BlacklistFrac * float64(ecfg.Pods)))
		for _, idx := range s.rng.Perm(ecfg.Pods)[:nBL] {
			es.blacklisted[idx] = true
			es.blCount++
		}
	}
	return es
}

// applyCorrelatedBlacklist runs once right after warmup: it blacklists pods
// holding the hottest Zipf prefix groups first (up to BlacklistFrac*Pods) so
// FindCandidates returns blacklisted holders.
func (es *evalSim) applyCorrelatedBlacklist() {
	if !es.ecfg.CorrelatedBlacklist || es.ecfg.BlacklistFrac <= 0 {
		return
	}
	target := int(math.Round(es.ecfg.BlacklistFrac * float64(es.ecfg.Pods)))
	for g := 0; g < es.cfg.PrefixGroups && es.blCount < target; g++ {
		for _, h := range es.holder[g] {
			if !es.blacklisted[h] {
				es.blacklisted[h] = true
				es.blCount++
				if es.blCount >= target {
					break
				}
			}
		}
	}
	for _, idx := range es.rng.Perm(es.ecfg.Pods) {
		if es.blCount >= target {
			break
		}
		if !es.blacklisted[idx] {
			es.blacklisted[idx] = true
			es.blCount++
		}
	}
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
	// Mirrors AIBRIX_POD_BLACKLIST_MAX_EXCLUDED_PERCENT default = 50%.
	return excluded*100 > 50*total
}

func (es *evalSim) selectKOnly(g, group int, ready []int, allowScoutBLFilter bool) []int {
	n := len(ready)
	k := es.pol.K
	if k <= 0 || n <= k {
		return ready
	}
	var scout []int
	var randStart int
	if 8*k >= n {
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
		scout = es.fillRandom(ready, holders, len(holders)+4*k)
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

func (es *evalSim) runEval() evalResult {
	cfg := es.cfg
	totalCap := 0.0
	for _, p := range es.pods {
		totalCap += p.capacity
	}
	effectiveCap := totalCap
	if es.ecfg.BlacklistFrac > 0 {
		effectiveCap *= (1 - es.ecfg.BlacklistFrac)
	}
	work := cfg.MeanWork
	if cfg.PrefixOn {
		work *= 1 - cfg.PrefixSaving*0.5
	}
	rate := cfg.Utilization * effectiveCap / work
	dt := cfg.Step.Seconds()
	end := cfg.Duration.Seconds()
	nextArrival := es.rng.ExpFloat64() / rate
	gw := 0
	appliedCorrBL := false

	for es.now = 0; es.now < end; es.now += dt {
		if cfg.SignalDelay > 0 && es.now >= es.nextRefresh {
			es.refreshSignals()
			es.nextRefresh += cfg.SignalDelay.Seconds()
		}
		if !appliedCorrBL && es.now >= cfg.Warmup.Seconds() {
			es.applyCorrelatedBlacklist()
			appliedCorrBL = true
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

// TestCandidateSelectionDefaultK compares shipped K-only (K=16, 32, 64) at
// delay=1s (the #3532 remote running reader default) against exact-count full
// scoring (K=0, SignalDelay=0) across N={200, 300, 500, 2000} and
// util={0.85, 0.92}. Run with CANDIDATE_SIM=defaultk.
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
							cfg := defaultEvalConfig{
								simConfig: simConfig{
									Pods: n, Gateways: 4, Utilization: u, MeanWork: 200,
									PrefixOn: prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
									SignalDelay: delay, Step: 5 * time.Millisecond,
									Warmup: 3 * time.Second, Duration: dur,
									SeedRandom: true, Seed: seed,
									ImbalanceMinGap: 8, ImbalanceFactor: 2,
								},
							}
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
	sb.WriteString("\n### Default-K Sweep (K-only delay=1s vs exact full K=0, 4 gateways, 3 seeds)\n")
	sb.WriteString("Format: mean_ratio (min..max) / p99_ratio [| prefix_hit%]\n\n")
	for _, u := range utils {
		fmt.Fprintf(&sb, "#### Utilization = %.2f\n\n", u)
		sb.WriteString("| N | Policy | prefix=false (mean / P99) | prefix=true (mean / P99 / hit%) |\n|---:|---|---|---|\n")
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
// delay=1s. Run with CANDIDATE_SIM=defaultk.
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
							cfg := defaultEvalConfig{
								simConfig: simConfig{
									Pods: n, Gateways: 4, Utilization: tc.util, MeanWork: 200,
									PrefixOn: prefixOn, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
									SignalDelay: delay, Step: 5 * time.Millisecond,
									Warmup: 3 * time.Second, Duration: 15 * time.Second,
									SeedRandom: true, Seed: seed,
									ImbalanceMinGap: 8, ImbalanceFactor: 2,
								},
								InflightCap: tc.cap,
							}
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
	sb.WriteString("\n### requestsInflight Cap Evaluation (4 gateways, delay=1s, 3 seeds)\n\n")
	for _, n := range sizes {
		fmt.Fprintf(&sb, "#### N=%d\n\n", n)
		sb.WriteString("| Scenario | Policy | 429% | False-429% | Mean Lat Ratio | P99 Ratio |\n|---|---|---:|---:|---:|---:|\n")
		for _, tc := range cases {
			for _, prefixOn := range []bool{false, true} {
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
					label := "full (K=0)"
					if k > 0 {
						label = fmt.Sprintf("K-only K=%d", k)
					}
					numSeeds := float64(len(seeds))
					fmt.Fprintf(&sb, "| util=%.2f cap=%d prefix=%v | %s | %.3f%% | %.3f%% | %.2fx | %.2fx |\n",
						tc.util, tc.cap, prefixOn, label,
						100*rejSum/reqSum, 100*falseSum/reqSum, meanR/numSeeds, p99R/numSeeds)
				}
			}
		}
		sb.WriteString("\n")
	}
	t.Log(sb.String())
}

// TestCandidateSelectionBlacklist evaluates K={0, 16, 32, 64} under uniform and
// correlated (hot prefix holder) blacklists at N={300, 500, 2000}, including a
// comparison of shipped whole-scout cap vs random-only 4K scout cap (#3543).
// Run with CANDIDATE_SIM=defaultk.
func TestCandidateSelectionBlacklist(t *testing.T) {
	if os.Getenv("CANDIDATE_SIM") != "defaultk" {
		t.Skip("set CANDIDATE_SIM=defaultk to run the blacklist evaluation")
	}
	type key struct {
		n          int
		frac       float64
		correlated bool
		randOnly   bool
		k          int
		seed       uint64
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
		{300, 0.40, true, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.15, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.40, false, []uint64{1, 2, 3}, 15 * time.Second, false},
		{500, 0.40, true, []uint64{1, 2, 3}, 15 * time.Second, true},
		{2000, 0.40, true, []uint64{1, 2}, 10 * time.Second, true},
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

	results := map[key]evalResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for _, sc := range scenarios {
		vs := append([]variant(nil), baseVariants...)
		if sc.checkRand {
			vs = append(vs, randCapVariants...)
		}
		for _, v := range vs {
			for _, seed := range sc.seeds {
				ck := key{sc.n, sc.frac, sc.correlated, v.randOnly, v.k, seed}
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
					cfg := defaultEvalConfig{
						simConfig: simConfig{
							Pods: sc.n, Gateways: 4, Utilization: 0.85, MeanWork: 200,
							PrefixOn: true, PrefixGroups: 2000, PrefixPerPod: 32, PrefixSaving: 0.3,
							SignalDelay: delay, Step: 5 * time.Millisecond,
							Warmup: 3 * time.Second, Duration: sc.dur,
							SeedRandom: true, Seed: seed,
							ImbalanceMinGap: 8, ImbalanceFactor: 2,
						},
						BlacklistFrac:       sc.frac,
						CorrelatedBlacklist: sc.correlated,
						RandomOnlyScoutCap:  v.randOnly,
					}
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
	sb.WriteString("\n### Blacklist x K-only Evaluation (prefix=true, util=0.85, delay=1s)\n\n")
	sb.WriteString("| Scenario | Policy | Mean Lat Ratio | P99 Ratio | Prefix Hit% | O(N) Reselect% |\n|---|---|---:|---:|---:|---:|\n")
	for _, sc := range scenarios {
		mode := "uniform"
		if sc.correlated {
			mode = "correlated-hot-holders"
		}
		vs := append([]variant(nil), baseVariants...)
		if sc.checkRand {
			vs = append(vs, randCapVariants...)
		}
		ns := float64(len(sc.seeds))
		for _, v := range vs {
			var meanR, p99R, hit, resel, req float64
			for _, seed := range sc.seeds {
				r := results[key{sc.n, sc.frac, sc.correlated, v.randOnly, v.k, seed}]
				f := results[key{sc.n, sc.frac, sc.correlated, false, 0, seed}]
				meanR += r.MeanLatency / f.MeanLatency
				p99R += r.P99Latency / f.P99Latency
				hit += r.PrefixHit
				resel += float64(r.Reselects)
				req += float64(r.Requests)
			}
			fmt.Fprintf(&sb, "| N=%d BL=%.0f%% (%s) | %s | %.2fx | %.2fx | %.1f%% | %.2f%% |\n",
				sc.n, sc.frac*100, mode, v.label, meanR/ns, p99R/ns, 100*hit/ns, 100*resel/req)
		}
	}
	t.Log(sb.String())
}
