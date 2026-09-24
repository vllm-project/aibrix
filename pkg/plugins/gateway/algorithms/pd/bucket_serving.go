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
// it splits the overlap into one band per roleset and adapts the cut points to
// the observed traffic, so each roleset receives a length-homogeneous slice of
// the load. The ranges the pods declare stay authoritative: a band only ever
// covers lengths that every roleset assigned to it declares, and lengths
// outside a split keep today's behavior, where every covering roleset stays a
// candidate. No engine or autoscaling configuration is involved.
//
// The plan is advisory. The routing path treats a band assignment as a
// preference and falls back to the full covering set when the assigned roleset
// is loaded, so a stale or wrong plan costs balance, not correctness.
//
// Concurrency: one mutex guards every model state. Observe and Plan run on the
// request path; both do O(bins) work under the lock and a plan is recomputed at
// most once per RefreshInterval or when the roleset set changes.

// BucketMode selects what the adaptive cut points balance.
type BucketMode string

const (
	// BucketModeRPS balances request counts: the cuts sit at request-count
	// quantiles, so the roleset of every band receives the same number of requests.
	// It is the mode for latency-sensitive, request-bound deployments.
	BucketModeRPS BucketMode = "rps"

	// BucketModeThroughput balances token mass: the cuts sit at token-mass
	// quantiles, so every band's roleset receives the same prompt work. It is
	// the mode for throughput-bound deployments, where padding cost and GPU
	// time track tokens rather than request counts.
	BucketModeThroughput BucketMode = "throughput"
)

// ValidBucketModes lists the mode names a config profile or environment
// variable may select, for validation and diagnostics.
func ValidBucketModes() []string {
	return []string{string(BucketModeRPS), string(BucketModeThroughput)}
}

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
	// DefaultBucketServeHalfLife is the half-life of the observation EWMA. It
	// spans several request bursts while still letting a traffic shift move
	// the cut points within a minute.
	DefaultBucketServeHalfLife = 30 * time.Second

	// DefaultBucketServeRefreshInterval bounds how often a model's plan is
	// recomputed. Requests between refreshes reuse the cached plan, so the
	// adaptive machinery runs at most once per interval per model.
	DefaultBucketServeRefreshInterval = 5 * time.Second

	// DefaultBucketServeMinSplitShare is the share of a model's observed
	// traffic an interval shared by several rolesets needs before it is split.
	// Thinner intervals keep a single band: splitting them would fragment the
	// load and starve a roleset for no homogeneity gain.
	DefaultBucketServeMinSplitShare = 0.05

	// DefaultBucketServeMaxBands caps the number of affinity bands (the bands
	// that carry a roleset assignment) in one model's plan. The cap bounds
	// both the metric label space and how fragmented the load may get.
	DefaultBucketServeMaxBands = 16

	// bucketServeBinsPerOctave is the resolution of the length histogram: 8
	// bins per power of two, about 9% per bin. Adaptive cut points land on bin
	// boundaries, so this is also their resolution.
	bucketServeBinsPerOctave = 8

	// bucketServeBins covers prompt lengths up to 2^21 tokens, far beyond the
	// context windows in use; longer prompts land in the last bin.
	bucketServeBins = 21 * bucketServeBinsPerOctave
)

// BucketServeConfig holds the tunables of a BucketServeTracker. The zero value
// is usable: normalize fills every unset value from the defaults.
type BucketServeConfig struct {
	// Enabled turns the adaptive plan on. It requires prompt-length bucketing
	// to be on as well, because the plan only re-orders the rolesets that
	// bucketing already filtered to.
	Enabled bool

	// Mode selects the objective of the cut points; see BucketMode.
	Mode BucketMode

	// HalfLife is the half-life of the observation EWMA.
	HalfLife time.Duration

	// RefreshInterval is the shortest interval between two plan recomputations
	// for one model.
	RefreshInterval time.Duration

	// MinSplitShare is the traffic share a shared interval needs before it is
	// split into bands.
	MinSplitShare float64

	// MaxBands caps the affinity bands in one model's plan.
	MaxBands int
}

// DefaultBucketServeConfig returns the shipped tunables, disabled.
func DefaultBucketServeConfig() BucketServeConfig {
	return BucketServeConfig{
		Enabled:         false,
		Mode:            BucketModeThroughput,
		HalfLife:        DefaultBucketServeHalfLife,
		RefreshInterval: DefaultBucketServeRefreshInterval,
		MinSplitShare:   DefaultBucketServeMinSplitShare,
		MaxBands:        DefaultBucketServeMaxBands,
	}
}

// normalized replaces unset or unusable tunables with their defaults, so a
// partial configuration cannot make the tracker misbehave.
func (c BucketServeConfig) normalized() BucketServeConfig {
	def := DefaultBucketServeConfig()
	if mode, ok := ParseBucketMode(string(c.Mode)); ok {
		c.Mode = mode
	} else {
		c.Mode = def.Mode
	}
	if c.HalfLife <= 0 {
		c.HalfLife = def.HalfLife
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = def.RefreshInterval
	}
	if c.MinSplitShare <= 0 || c.MinSplitShare >= 1 {
		c.MinSplitShare = def.MinSplitShare
	}
	if c.MaxBands <= 0 {
		c.MaxBands = def.MaxBands
	}
	return c
}

// BucketGroup is one roleset that can serve a prompt-length range, in
// inclusive token bounds.
type BucketGroup struct {
	Name     string
	Min, Max int
}

// BucketBand is one routing unit of a plan: prompts in [Min, Max] belong to
// this band. An empty Group marks a band without an assignment, where routing
// keeps every covering roleset as a candidate.
type BucketBand struct {
	Min, Max int
	Group    string
}

// BucketPlanEvents records what changed when a plan was recomputed.
type BucketPlanEvents struct {
	// Refreshed reports whether this call recomputed the plan. It is false for
	// calls that returned the cached plan.
	Refreshed bool

	// Splits and Merges count the affinity cut points this refresh added and
	// removed, where a cut point is a boundary between two touching bands
	// that belong to different rolesets.
	Splits, Merges int

	// Bands is the number of bands (assigned and unassigned) in the plan.
	Bands int
}

// BucketPlan is one model's current plan. The Bands slice is shared with the
// tracker and must not be modified by the caller.
type BucketPlan struct {
	Bands  []BucketBand
	Events BucketPlanEvents
}

// BandIndexFor returns the index of the band covering length, or -1 when the
// plan has no band for it.
func (p BucketPlan) BandIndexFor(length int) int {
	for i := range p.Bands {
		if length >= p.Bands[i].Min && length <= p.Bands[i].Max {
			return i
		}
	}
	return -1
}

// GroupFor returns the roleset the plan assigns to length. ok is false when no
// band covers the length or the covering band carries no assignment.
func (p BucketPlan) GroupFor(length int) (string, bool) {
	i := p.BandIndexFor(length)
	if i < 0 || p.Bands[i].Group == "" {
		return "", false
	}
	return p.Bands[i].Group, true
}

// BucketServeTracker is the per-process bucket-serving state of every model
// this gateway has routed. The zero value is not usable; build one with
// NewBucketServeTracker.
type BucketServeTracker struct {
	cfg BucketServeConfig

	mu     sync.Mutex
	models map[string]*bucketServeModel
}

// NewBucketServeTracker builds a tracker on cfg, with unset tunables filled
// from the defaults.
func NewBucketServeTracker(cfg BucketServeConfig) *BucketServeTracker {
	return &BucketServeTracker{
		cfg:    cfg.normalized(),
		models: make(map[string]*bucketServeModel),
	}
}

// Config returns the tracker's normalized configuration.
func (t *BucketServeTracker) Config() BucketServeConfig {
	if t == nil {
		return DefaultBucketServeConfig()
	}
	return t.cfg
}

// Configure installs cfg as the configuration this model's plans are computed
// with. A model starts on the tracker's configuration; a request whose resolved
// knobs select another mode, or switch the plan on, calls Configure, which
// drops the cached plan when something changed so the next Plan recomputes it.
// Other models are untouched. It is a no-op on a nil tracker, an empty model
// and an unchanged configuration.
func (t *BucketServeTracker) Configure(model string, cfg BucketServeConfig) {
	if t == nil || model == "" {
		return
	}
	cfg = cfg.normalized()
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.modelLocked(model)
	if m.cfg == cfg {
		return
	}
	m.cfg = cfg
	m.planAt = time.Time{}
}

// bucketServeModel is one model's observation state and cached plan.
type bucketServeModel struct {
	// cfg is the configuration this model's plans are computed with. It
	// starts as the tracker's configuration and Configure replaces it when a
	// request resolves different bucket-serve knobs, so one model can run
	// another mode without touching the other models.
	cfg BucketServeConfig

	// weights and masses are EWMA counts over the length histogram: routed
	// requests and routed prompt tokens, one entry per bin.
	weights []float64
	masses  []float64

	// last is the time the EWMA picture was last decayed to.
	last time.Time

	// plan, planKey and planAt cache the last computed plan. planKey
	// fingerprints the roleset ranges the plan was computed for, so a change
	// in the fleet invalidates the plan without waiting for the refresh
	// interval.
	plan    []BucketBand
	planKey string
	planAt  time.Time
}

// Observe records one routed request's prompt length. It is a no-op on a nil
// tracker, an empty model or a non-positive length.
func (t *BucketServeTracker) Observe(model string, promptLength int, now time.Time) {
	if t == nil || model == "" || promptLength <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.modelLocked(model)
	m.decay(m.cfg.HalfLife, now)
	bin := bucketServeBin(promptLength)
	m.weights[bin]++
	m.masses[bin] += float64(promptLength)
}

// Plan returns the plan for model under the given roleset groups, recomputing
// it when the cached one is stale or the groups changed. A nil tracker, an
// empty model or no groups returns an empty plan and no events.
func (t *BucketServeTracker) Plan(model string, now time.Time, groups []BucketGroup) BucketPlan {
	if t == nil || model == "" {
		return BucketPlan{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.modelLocked(model)
	m.decay(m.cfg.HalfLife, now)

	key := bucketServeGroupsKey(groups)
	if !m.planAt.IsZero() && key == m.planKey && now.Sub(m.planAt) < m.cfg.RefreshInterval {
		return BucketPlan{Bands: m.plan}
	}

	bands := t.computeBands(m, groups)
	splits, merges := bucketServePlanDiff(m.plan, bands)
	m.plan, m.planKey, m.planAt = bands, key, now
	return BucketPlan{
		Bands: bands,
		Events: BucketPlanEvents{
			Refreshed: true,
			Splits:    splits,
			Merges:    merges,
			Bands:     len(bands),
		},
	}
}

// modelLocked returns the model's state, creating it on first use.
func (t *BucketServeTracker) modelLocked(model string) *bucketServeModel {
	m := t.models[model]
	if m == nil {
		m = &bucketServeModel{
			cfg:     t.cfg,
			weights: make([]float64, bucketServeBins),
			masses:  make([]float64, bucketServeBins),
		}
		t.models[model] = m
	}
	return m
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

// segment is a maximal prompt-length interval whose set of covering rolesets
// does not change, the unit the cut points are computed in.
type bucketServeSegment struct {
	min, max int
	covering []BucketGroup
	weight   float64
}

// computeBands builds the plan: the declared roleset ranges are cut into
// segments, every segment shared by two or more rolesets is split into one band
// per roleset at mode-dependent quantiles of the observed traffic, and the
// bands are capped at MaxBands. Segments that are not split (single roleset,
// too little traffic, or dropped by the cap) become one band without an
// assignment.
func (t *BucketServeTracker) computeBands(m *bucketServeModel, groups []BucketGroup) []BucketBand {
	segs := bucketServeSegments(m, groups)
	if len(segs) == 0 {
		return nil
	}

	// The objective vector of the mode: what the cuts are supposed to balance.
	vec := m.weights
	if m.cfg.Mode == BucketModeThroughput {
		vec = m.masses
	}
	total := 0.0
	for i := range segs {
		segs[i].weight = bucketServeRangeWeight(vec, segs[i].min, segs[i].max)
		total += segs[i].weight
	}

	// Decide which segments are split. A shared segment needs enough of the
	// model's traffic to be worth splitting.
	split := make([]bool, len(segs))
	affinityBands := 0
	for i := range segs {
		if len(segs[i].covering) < 2 || total <= 0 {
			continue
		}
		if segs[i].weight/total < m.cfg.MinSplitShare {
			continue
		}
		split[i] = true
		affinityBands += len(segs[i].covering)
	}

	// Enforce the cap by merging the lightest split segments first.
	if affinityBands > m.cfg.MaxBands {
		order := make([]int, 0, len(segs))
		for i := range segs {
			if split[i] {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(a, b int) bool { return segs[order[a]].weight < segs[order[b]].weight })
		for _, i := range order {
			if affinityBands <= m.cfg.MaxBands {
				break
			}
			split[i] = false
			affinityBands -= len(segs[i].covering)
		}
	}

	bands := make([]BucketBand, 0, len(segs)+affinityBands)
	for i := range segs {
		seg := segs[i]
		if !split[i] {
			bands = append(bands, BucketBand{Min: seg.min, Max: seg.max})
			continue
		}
		cuts := bucketServeCuts(vec, seg.min, seg.max, len(seg.covering))
		if len(cuts) != len(seg.covering)-1 {
			// The segment is too narrow for the bin resolution to place
			// distinct cuts; keep it whole rather than emit overlapping bands.
			bands = append(bands, BucketBand{Min: seg.min, Max: seg.max})
			continue
		}
		lower := seg.min
		for j, g := range seg.covering {
			upper := seg.max
			if j < len(cuts) {
				upper = cuts[j] - 1
			}
			bands = append(bands, BucketBand{Min: lower, Max: upper, Group: g.Name})
			lower = upper + 1
		}
	}
	return bands
}

// bucketServeSegments cuts the declared roleset ranges into maximal intervals
// with a constant covering set.
func bucketServeSegments(m *bucketServeModel, groups []BucketGroup) []bucketServeSegment {
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

// bucketServeCuts places k-1 cut points over [min, max] at mode quantiles of
// vec. It returns nil when fewer than k-1 distinct cuts fit, which the caller
// reads as "keep the segment whole".
func bucketServeCuts(vec []float64, min, max, k int) []int {
	if k < 2 || max <= min {
		return nil
	}
	total := bucketServeRangeWeight(vec, min, max)
	if total <= 0 {
		return nil
	}
	width := max - min + 1
	cuts := make([]int, 0, k-1)
	for i := 1; i < k; i++ {
		target := total * float64(i) / float64(k)
		acc := 0.0
		cut := 0
		for bin := 0; bin < bucketServeBins; bin++ {
			center := bucketServeBinCenter(bin)
			if center < min || center > max {
				continue
			}
			acc += vec[bin]
			if acc >= target {
				cut = center
				break
			}
		}
		if cut < min+1 || cut > max {
			// The distribution is too thin to place this cut from the bins;
			// fall back to an even split of the interval.
			cut = min + width*i/k
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

// bucketServeGroupsKey fingerprints the roleset ranges so a change in the fleet
// invalidates the cached plan.
func bucketServeGroupsKey(groups []BucketGroup) string {
	if len(groups) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groups))
	for _, g := range groups {
		parts = append(parts, g.Name+"="+strconv.Itoa(g.Min)+"-"+strconv.Itoa(g.Max))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// bucketServePlanDiff counts the affinity cut points added (splits) and removed
// (merges) between two plans.
func bucketServePlanDiff(oldBands, newBands []BucketBand) (splits, merges int) {
	oldCuts := bucketServeCutsOf(oldBands)
	newCuts := bucketServeCutsOf(newBands)
	for cut := range newCuts {
		if !oldCuts[cut] {
			splits++
		}
	}
	for cut := range oldCuts {
		if !newCuts[cut] {
			merges++
		}
	}
	return splits, merges
}

// bucketServeCutsOf returns the cut points where two touching bands carry
// different rolesets. A boundary against an unassigned band is geometry
// rather than affinity, so it does not count.
func bucketServeCutsOf(bands []BucketBand) map[int]bool {
	cuts := make(map[int]bool)
	for i := 0; i+1 < len(bands); i++ {
		cur, next := bands[i], bands[i+1]
		if cur.Group == "" || next.Group == "" || cur.Group == next.Group {
			continue
		}
		if cur.Max+1 != next.Min {
			continue
		}
		cuts[cur.Max] = true
	}
	return cuts
}
