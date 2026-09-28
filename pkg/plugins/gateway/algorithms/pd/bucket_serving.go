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
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BucketServeTracker keeps, per model, an EWMA picture of the prompt lengths
// this gateway routes and turns it into a plan that assigns prompt-length
// bands to rolesets. It is the gateway half of a BucketServe-style bucketer
// (BucketServe: Bucket-Based Dynamic Batching for Smart and Efficient LLM
// Inference Serving, arXiv 2507.17120).
//
// The gateway cannot form engine batches, so the tracker shapes the traffic
// instead: wherever several rolesets declare overlapping prompt-length ranges,
// it splits the overlap into bands and adapts the cut points to the observed
// traffic, so each roleset receives a length-homogeneous slice of the load.
// The ranges the pods declare stay authoritative: a band only ever covers
// lengths that every roleset assigned to it declares, and lengths outside a
// split keep today's behavior, where every covering roleset stays a candidate.
// No engine or autoscaling configuration is involved.
//
// The plan is advisory. The routing path treats a band assignment as a
// preference and falls back to the full covering set when the assigned roleset
// is loaded, so a stale or wrong plan costs balance, not correctness.
//
// Concurrency: one mutex guards every model state. Band runs on the request
// path; it does O(bins) work under the lock and recomputes a model's plan at
// most once per refresh interval for a given mode and roleset set.

// BucketMode selects what the adaptive cut points balance.
type BucketMode string

const (
	// BucketModeRPS splits shared ranges at request-count quantiles, so the
	// traffic a roleset is banded tracks the replica share it still needs,
	// counted in requests. It is the mode for latency-sensitive,
	// request-bound deployments.
	BucketModeRPS BucketMode = "rps"

	// BucketModeThroughput splits shared ranges at prompt-token quantiles,
	// so the traffic a roleset is banded tracks the replica share it still
	// needs, counted in prompt tokens. It is the mode for throughput-bound
	// deployments, where padding cost and GPU time track tokens rather than
	// request counts.
	BucketModeThroughput BucketMode = "throughput"
)

// ParseBucketMode parses a bucket-serve mode name. An unknown name returns
// ok=false and the caller keeps its current value.
func ParseBucketMode(name string) (BucketMode, bool) {
	switch BucketMode(strings.ToLower(strings.TrimSpace(name))) {
	case BucketModeRPS:
		return BucketModeRPS, true
	case BucketModeThroughput:
		return BucketModeThroughput, true
	default:
		return "", false
	}
}

const (
	// bucketServeHalfLife is the half-life of the observation EWMA. It spans
	// several request bursts while still letting a traffic shift move the cut
	// points within a minute.
	bucketServeHalfLife = 30 * time.Second

	// bucketServeRefreshInterval bounds how often a model's plan is recomputed
	// for one mode and roleset set. Requests between refreshes reuse the cached
	// plan, so the adaptive machinery runs at most once per interval.
	bucketServeRefreshInterval = 5 * time.Second

	// bucketServeMinSplitShare is the share of a model's observed traffic an
	// interval shared by several rolesets needs before it is split. Thinner
	// intervals keep every covering roleset a candidate: splitting them would
	// fragment the load and starve a roleset for no homogeneity gain.
	bucketServeMinSplitShare = 0.05

	// bucketServeMaxBands caps the number of affinity bands in one model's
	// plan. The cap bounds both the metric label space and how fragmented the
	// load may get; a shared interval that cannot afford a band per roleset
	// still splits into as many bands as the budget left allows.
	bucketServeMaxBands = 16

	// bucketServePlanCacheSize bounds the number of cached plans per model.
	// The key space is the modes times the roleset sets this gateway routes, so
	// two profiles asking for different plans do not evict each other.
	bucketServePlanCacheSize = 8

	// bucketServeBinsPerOctave is the resolution of the length histogram: 8
	// bins per power of two, about 9% per bin. Adaptive cut points land on bin
	// boundaries, so this is also their resolution.
	bucketServeBinsPerOctave = 8

	// bucketServeBins covers prompt lengths up to 2^21 tokens, far beyond the
	// context windows in use; longer prompts land in the last bin.
	bucketServeBins = 21 * bucketServeBinsPerOctave
)

// BucketGroup is one roleset that can serve a prompt-length range, in
// inclusive token bounds. Replicas is the roleset's prefill replica count,
// which is how much of the model's traffic the roleset should carry.
type BucketGroup struct {
	Name     string
	Min, Max int
	Replicas int
}

// replicas returns the replica weight of the group. A group without a replica
// count counts as one, so it stays a candidate.
func (g BucketGroup) replicas() int {
	if g.Replicas < 1 {
		return 1
	}
	return g.Replicas
}

// BucketBand is one routing unit of a plan: prompts in [Min, Max] prefer the
// band's roleset. A plan carries no band for a length interval no roleset
// needs, and there the routing path keeps every covering roleset a candidate.
type BucketBand struct {
	Min, Max int
	Group    string
}

// BandResult reports what one Band call observed: the roleset the plan prefers
// for the request's prompt length, the upper bound of that band, and the plan
// itself when the call recomputed it.
type BandResult struct {
	// Roleset is the roleset the plan prefers for the request. Empty when the
	// plan has no opinion: no band covers the length, or no roleset needs
	// traffic there.
	Roleset string

	// Max is the upper prompt-length bound of the band that covers the
	// request. It is meaningful only when Roleset is non-empty.
	Max int

	// Plan is the recomputed plan, sorted by Min, shared with the tracker and
	// must not be modified. It is set only when Refreshed is true.
	Plan []BucketBand

	// Dropped lists the rolesets that left every live plan on this refresh,
	// in name order: they were named before, and neither the new plan nor a
	// cached plan still inside its refresh interval holds a band for them now,
	// so a caller can delete their gauge series. It is set only when
	// Refreshed is true.
	Dropped []string

	// Refreshed reports whether this call recomputed the plan rather than
	// serving the cached one.
	Refreshed bool
}

// BucketServeTracker is the per-process bucket-serving state of every model
// this gateway has routed. The zero value is not usable; build one with
// NewBucketServeTracker.
type BucketServeTracker struct {
	mu     sync.Mutex
	models map[string]*bucketServeModel
}

// NewBucketServeTracker builds an empty tracker.
func NewBucketServeTracker() *BucketServeTracker {
	return &BucketServeTracker{models: make(map[string]*bucketServeModel)}
}

// Band records one routed request and returns the roleset the adaptive plan
// prefers for its prompt length. It is the request-path entry point: the call
// decays the model's traffic picture, records the request, recomputes the plan
// when the cached one is stale or the mode or roleset set changed, and reports
// the band that covers the length. Calls asking for different modes or fleet
// shapes keep their own cached plans. It is a no-op on a nil tracker, an empty
// model and a non-positive length.
func (t *BucketServeTracker) Band(model string, mode BucketMode, length int, now time.Time, groups []BucketGroup) BandResult {
	if t == nil || model == "" || length <= 0 {
		return BandResult{}
	}
	// The canonical name keys the plan cache, so two spellings of one mode
	// share the plan instead of planning twice.
	if parsed, ok := ParseBucketMode(string(mode)); ok {
		mode = parsed
	} else {
		mode = BucketModeThroughput
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.modelLocked(model)
	m.decay(bucketServeHalfLife, now)
	bin := bucketServeBin(length)
	m.weights[bin]++
	m.masses[bin] += float64(length)

	bands, dropped, refreshed := m.planFor(mode, now, groups)
	res := BandResult{Refreshed: refreshed, Dropped: dropped}
	if refreshed {
		res.Plan = bands
	}
	for i := range bands {
		if length >= bands[i].Min && length <= bands[i].Max {
			res.Roleset = bands[i].Group
			res.Max = bands[i].Max
			break
		}
	}
	return res
}

// bucketServeModel is one model's observation state and cached plans.
type bucketServeModel struct {
	// weights and masses are EWMA counts over the length histogram: routed
	// requests and routed prompt tokens, one entry per bin.
	weights []float64
	masses  []float64

	// last is the time the EWMA picture was last decayed to.
	last time.Time

	// plans caches one plan per mode and roleset-set fingerprint, so a request
	// that resolves another profile serves its own cached plan instead of
	// evicting the plan of the other profile.
	plans map[string]bucketServePlan

	// published is the union of rolesets the model's plans held after the
	// last refresh, so a recomputation can report the rolesets that left every
	// live plan.
	published map[string]struct{}
}

// bucketServePlan is one cached plan: its bands, the rolesets those bands
// hold, and the time it was computed.
type bucketServePlan struct {
	bands []BucketBand
	held  map[string]struct{}
	at    time.Time
}

// modelLocked returns the model's state, creating it on first use.
func (t *BucketServeTracker) modelLocked(model string) *bucketServeModel {
	m := t.models[model]
	if m == nil {
		m = &bucketServeModel{
			weights: make([]float64, bucketServeBins),
			masses:  make([]float64, bucketServeBins),
		}
		t.models[model] = m
	}
	return m
}

// planFor returns the model's plan for one mode and roleset set, recomputing
// it when the cached plan for that pair is stale. refreshed reports whether
// this call recomputed the plan, and dropped the rolesets that left every live
// plan: they were held before and no plan that can still serve a request holds
// them now.
func (m *bucketServeModel) planFor(mode BucketMode, now time.Time, groups []BucketGroup) ([]BucketBand, []string, bool) {
	key := string(mode) + "|" + bucketServeGroupsKey(groups)
	if p, ok := m.plans[key]; ok && now.Sub(p.at) < bucketServeRefreshInterval {
		return p.bands, nil, false
	}
	bands := m.computeBands(mode, groups)
	held := make(map[string]struct{}, len(bands))
	for _, band := range bands {
		held[band.Group] = struct{}{}
	}
	// The new plan holds its bands, and so does every other cached plan still
	// inside its refresh interval: the next request that matches one is served
	// from that cache. A roleset no live plan holds is reported dropped, so a
	// refresh of one mode never clears the gauge series a plan of another mode
	// still serves.
	live := make(map[string]struct{}, len(held))
	for name := range held {
		live[name] = struct{}{}
	}
	for k, p := range m.plans {
		if k == key || now.Sub(p.at) >= bucketServeRefreshInterval {
			continue
		}
		for name := range p.held {
			live[name] = struct{}{}
		}
	}
	var dropped []string
	for name := range m.published {
		if _, ok := live[name]; !ok {
			dropped = append(dropped, name)
		}
	}
	sort.Strings(dropped)
	m.published = live
	if m.plans == nil {
		m.plans = make(map[string]bucketServePlan, 2)
	}
	m.plans[key] = bucketServePlan{bands: bands, held: held, at: now}
	if len(m.plans) > bucketServePlanCacheSize {
		oldestKey, oldestAt := "", time.Time{}
		for k, p := range m.plans {
			if k == key {
				continue
			}
			if oldestKey == "" || p.at.Before(oldestAt) {
				oldestKey, oldestAt = k, p.at
			}
		}
		if oldestKey != "" {
			delete(m.plans, oldestKey)
		}
	}
	return bands, dropped, true
}

// decay applies the EWMA decay from the last observation to now, so the
// histogram tracks recent traffic. When the tracker was idle for many
// half-lives the picture fades to empty, and an empty picture means "no plan",
// which keeps the routing behavior unchanged until traffic returns.
func (m *bucketServeModel) decay(halfLife time.Duration, now time.Time) {
	if now.Before(m.last) {
		// A caller went back in time (tests observe with fixed clocks); keep
		// the newer state instead of growing the weights.
		return
	}
	delta := now.Sub(m.last)
	if m.last.IsZero() || delta <= 0 {
		m.last = now
		return
	}
	factor := math.Pow(0.5, delta.Seconds()/halfLife.Seconds())
	for i := range m.weights {
		m.weights[i] *= factor
		m.masses[i] *= factor
	}
	m.last = now
}

// bucketServeSegment is a maximal prompt-length interval whose set of covering
// rolesets does not change, the unit the cut points are computed in.
type bucketServeSegment struct {
	min, max int
	covering []BucketGroup
	weight   float64
}

// computeBands builds one plan: the declared roleset ranges are cut into
// segments, every segment shared by several rolesets is split into one band
// per roleset that still needs traffic, and each band is debited against the
// traffic its roleset just took, so a roleset carrying several overlaps draws
// against one replica share of the model's traffic instead of one share per
// segment. A segment that is not split carries no band: there every covering
// roleset stays a candidate, which is the same preference as no plan at all.
func (m *bucketServeModel) computeBands(mode BucketMode, groups []BucketGroup) []BucketBand {
	segs := bucketServeSegments(groups)
	if len(segs) == 0 {
		return nil
	}

	vec := m.weights
	if mode == BucketModeThroughput {
		vec = m.masses
	}
	total := 0.0
	for i := range segs {
		segs[i].weight = bucketServeRangeWeight(vec, segs[i].min, segs[i].max)
		total += segs[i].weight
	}
	if total <= 0 {
		return nil
	}

	// Charge every roleset the traffic it already owns, the segments only it
	// covers: no cut can move that traffic, so the shared segments only have to
	// cover what each roleset still needs to reach its replica share. That
	// need is what keeps a wide overlap from handing a roleset more band than
	// its pods can drain, which the prefill fast path would undo on the next
	// request anyway.
	exclusive := make(map[string]float64, len(groups))
	replicas := make(map[string]int, len(groups))
	for i := range segs {
		if len(segs[i].covering) == 1 {
			exclusive[segs[i].covering[0].Name] += segs[i].weight
		}
		for _, g := range segs[i].covering {
			if _, ok := replicas[g.Name]; !ok {
				replicas[g.Name] = g.replicas()
			}
		}
	}
	replicaTotal := 0
	for _, r := range replicas {
		replicaTotal += r
	}
	need := make(map[string]float64, len(replicas))
	for name, r := range replicas {
		target := total * float64(r) / float64(replicaTotal)
		if d := target - exclusive[name]; d > 0 {
			need[name] = d
		}
	}

	// Split the shared segments worth splitting, busiest first, and spend the
	// band budget as bands are emitted: a segment that emits nothing costs
	// nothing, so it cannot starve a later segment. Every emitted band is
	// debited against the traffic its roleset just took, so a roleset that
	// overlaps several segments draws against one replica share of the model's
	// traffic instead of one share per segment.
	candidates := make([]bucketServeSegment, 0, len(segs))
	for i := range segs {
		if len(segs[i].covering) < 2 || segs[i].weight/total < bucketServeMinSplitShare {
			continue
		}
		candidates = append(candidates, segs[i])
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		return candidates[a].weight > candidates[b].weight
	})

	bands := make([]BucketBand, 0, bucketServeMaxBands)
	budget := bucketServeMaxBands
	for i := range candidates {
		if budget <= 0 {
			break
		}
		seg := candidates[i]
		// Only the rolesets that still need traffic take part, ordered by how
		// much they need after the bands already assigned to them.
		active := make([]BucketGroup, 0, len(seg.covering))
		for _, g := range seg.covering {
			if need[g.Name] > 0 {
				active = append(active, g)
			}
		}
		if len(active) == 0 {
			continue
		}
		sort.SliceStable(active, func(a, b int) bool {
			na, nb := need[active[a].Name], need[active[b].Name]
			if na != nb {
				return na > nb
			}
			return active[a].Name < active[b].Name
		})
		parts := len(active)
		if parts > budget {
			// A segment that cannot afford one band per active roleset still
			// splits into as many bands as the budget left allows, so a wide
			// overlap is thinned rather than dropped on the floor.
			parts = budget
		}
		if parts == 1 {
			if len(active) == 1 {
				// One roleset still needs traffic here and nothing else does,
				// so the whole interval prefers it.
				bands = append(bands, BucketBand{Min: seg.min, Max: seg.max, Group: active[0].Name})
				budget--
				need[active[0].Name] -= seg.weight
			}
			// The budget truncated several active rolesets to one band:
			// leave the interval to the load fast paths rather than pin it to
			// one of several rolesets that still need traffic.
			continue
		}
		shares := make([]float64, 0, parts)
		for _, g := range active[:parts] {
			shares = append(shares, need[g.Name])
		}
		cuts := bucketServeCuts(vec, seg.min, seg.max, shares)
		if len(cuts) != parts-1 {
			// The interval cannot hold that many distinct cuts; keep it whole
			// rather than emit overlapping bands.
			continue
		}
		lower := seg.min
		for j := 0; j < parts; j++ {
			upper := seg.max
			if j < len(cuts) {
				upper = cuts[j] - 1
			}
			bands = append(bands, BucketBand{Min: lower, Max: upper, Group: active[j].Name})
			need[active[j].Name] -= bucketServeRangeWeight(vec, lower, upper)
			lower = upper + 1
		}
		budget -= parts
	}
	sort.Slice(bands, func(a, b int) bool { return bands[a].Min < bands[b].Min })
	return bands
}

// bucketServeSegments cuts the declared roleset ranges into maximal intervals
// with a constant covering set.
func bucketServeSegments(groups []BucketGroup) []bucketServeSegment {
	valid := make([]BucketGroup, 0, len(groups))
	for _, g := range groups {
		if g.Name == "" || g.Min < 0 || g.Max < g.Min {
			continue
		}
		// The bounds below hold g.Max+1, so an upper bound at the top of int
		// must move down one: on a 32-bit build math.MaxInt32 is both the
		// open-range sentinel an unconfigured pod passes and the top of int,
		// and adding one to it would wrap to the bottom and scramble the
		// bounds. Shortening an open range by one token is not observable.
		if g.Max == math.MaxInt {
			g.Max = math.MaxInt - 1
		}
		valid = append(valid, g)
	}
	if len(valid) == 0 {
		return nil
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].Min != valid[j].Min {
			return valid[i].Min < valid[j].Min
		}
		if valid[i].Max != valid[j].Max {
			return valid[i].Max < valid[j].Max
		}
		return valid[i].Name < valid[j].Name
	})

	bounds := make([]int, 0, 2*len(valid))
	for _, g := range valid {
		bounds = append(bounds, g.Min, g.Max+1)
	}
	sort.Ints(bounds)

	var segs []bucketServeSegment
	for i := 0; i+1 < len(bounds); i++ {
		lo, hi := bounds[i], bounds[i+1]
		if hi <= lo {
			continue
		}
		var covering []BucketGroup
		for _, g := range valid {
			if g.Min <= lo && hi <= g.Max+1 {
				covering = append(covering, g)
			}
		}
		if len(covering) == 0 {
			continue
		}
		segs = append(segs, bucketServeSegment{min: lo, max: hi - 1, covering: covering})
	}
	return segs
}

// bucketServeCuts places one cut per share over [min, max] so the bands between
// the cuts carry prompt traffic in proportion to shares, using the histogram
// vector. It returns nil when the interval cannot hold that many distinct
// cuts, which the caller reads as "keep the segment whole".
func bucketServeCuts(vec []float64, min, max int, shares []float64) []int {
	k := len(shares)
	if k < 2 || max <= min {
		return nil
	}
	sum := 0.0
	for _, s := range shares {
		if s > 0 {
			sum += s
		}
	}
	total := bucketServeRangeWeight(vec, min, max)
	if sum <= 0 || total <= 0 {
		return nil
	}
	cuts := make([]int, 0, k-1)
	acc := 0.0
	for i := 0; i < k-1; i++ {
		acc += shares[i]
		target := total * acc / sum
		cut := 0
		running := 0.0
		for bin := 0; bin < bucketServeBins; bin++ {
			center := bucketServeBinCenter(bin)
			if center < min || center > max {
				continue
			}
			running += vec[bin]
			if running >= target {
				cut = center
				break
			}
		}
		if cut < min+1 || cut > max {
			// The bins cannot place this cut; keep the segment whole instead
			// of publishing a split the histogram did not ask for.
			return nil
		}
		cuts = append(cuts, cut)
	}
	sort.Ints(cuts)
	for i := range cuts {
		if cuts[i] < min+1 {
			cuts[i] = min + 1
		}
		if cuts[i] > max {
			cuts[i] = max
		}
		if i > 0 && cuts[i] <= cuts[i-1] {
			cuts[i] = cuts[i-1] + 1
		}
	}
	if cuts[len(cuts)-1] > max {
		return nil
	}
	return cuts
}

// bucketServeRangeWeight sums the histogram over the bins whose center falls
// inside [min, max].
func bucketServeRangeWeight(vec []float64, min, max int) float64 {
	total := 0.0
	for bin := 0; bin < bucketServeBins; bin++ {
		center := bucketServeBinCenter(bin)
		if center < min || center > max {
			continue
		}
		total += vec[bin]
	}
	return total
}

// bucketServeBin maps a prompt length to its histogram bin.
func bucketServeBin(length int) int {
	if length <= 1 {
		return 0
	}
	bin := int(math.Floor(math.Log2(float64(length)) * bucketServeBinsPerOctave))
	if bin < 0 {
		return 0
	}
	if bin >= bucketServeBins {
		return bucketServeBins - 1
	}
	return bin
}

// bucketServeBinCenter returns the length a bin represents, used for ranges
// and cut placement.
func bucketServeBinCenter(bin int) int {
	return int(math.Round(math.Pow(2, (float64(bin)+0.5)/bucketServeBinsPerOctave)))
}

// bucketServeGroupsKey fingerprints the roleset ranges and replica counts so a
// change in the fleet invalidates the cached plans.
func bucketServeGroupsKey(groups []BucketGroup) string {
	if len(groups) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, g.Name+"="+strconv.Itoa(g.Min)+"-"+strconv.Itoa(g.Max)+"x"+strconv.Itoa(g.replicas()))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
