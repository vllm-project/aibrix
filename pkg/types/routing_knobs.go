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

package types

import (
	"sync"
	"sync/atomic"
)

// RoutingOverrides carries one request's resolved routing knobs: the process
// defaults with the model config profile's overrides applied. Every field is a
// concrete value, so a read site uses it directly:
//
//	factor := routingCtx.RoutingOverrides().LoadBalance.ImbalanceFactor
//
// The values are resolved once per request at the profile boundary
// (routingalgorithms.ResolveRoutingOverrides): a profile value the matching
// AIBRIX_* variable would reject is dropped there, with a log, and the
// environment default is kept. A profile can therefore only narrow or sharpen
// routing behaviour, never silently drop a threshold. A request whose profile
// sets nothing reads the process defaults registered by the routing algorithm
// package through SetDefaultRoutingOverrides.
//
// Knobs that configure process-wide state instead of a routing decision have no
// field here on purpose, because all models of a gateway process share that
// state and a per-request value could not be applied without corrupting it:
// the Preble eviction loop and histogram window, the VTC token tracker's
// window, time unit, token weights and min/max floors, the session-affinity
// local cache capacity and the token-load session table cap. Those keep their
// environment-only semantics.
type RoutingOverrides struct {
	LoadBalance LoadBalanceOverrides
	PrefixCache PrefixCacheOverrides
	Preble      PrebleOverrides
	VTC         VTCOverrides
	AutoBlend   AutoBlendOverrides
	PD          PDOverrides
}

// LoadBalanceOverrides mirrors the AIBRIX_LOAD_BALANCE_* knobs of the
// load-balance score and its imbalance gate.
type LoadBalanceOverrides struct {
	// ImbalanceFactor overrides AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR: the factor
	// of the mean over which the gate flags the busiest replica as a hotspot.
	// Used for pools of three or more replicas.
	ImbalanceFactor float64
	// ImbalanceMinGap overrides AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP: the
	// minimum absolute gap between the busiest and the least busy replica
	// required to trigger the gate.
	ImbalanceMinGap int
	// QueuedWeight overrides AIBRIX_LOAD_BALANCE_QUEUED_WEIGHT: the weight of
	// queued requests in the score. 0 is the V1 formula, running requests only.
	QueuedWeight float64
	// KVPressureAlpha overrides AIBRIX_LOAD_BALANCE_KV_PRESSURE_ALPHA: the
	// strength of the quadratic KV-pressure penalty. 0 drops the penalty.
	KVPressureAlpha float64
	// KVCriticalFree overrides AIBRIX_LOAD_BALANCE_KV_CRITICAL_FREE: the
	// free-KV fraction below which a replica scores +Inf. 0 disables the
	// guardrail.
	KVCriticalFree float64
}

// PrefixCacheOverrides mirrors AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR.
type PrefixCacheOverrides struct {
	// StandardDeviationFactor is how many standard deviations above the mean
	// replica request count a prefix-match candidate may sit before it is
	// skipped. Read by prefix-cache and by the PD prefill scorer.
	StandardDeviationFactor int
}

// PrebleOverrides mirrors the prefix-cache-preble cost-model knobs that are
// read per request.
type PrebleOverrides struct {
	// TargetGPU overrides AIBRIX_ROUTER_PREBLE_TARGET_GPU: the GPU the cost
	// model assumes for its replicas. "A6000" and "V100" are the known values.
	TargetGPU string
	// DecodingLength overrides AIBRIX_ROUTER_PREBLE_DECODING_LENGTH: the assumed
	// number of decoding tokens per request of the cost model.
	DecodingLength int
}

// VTCOverrides mirrors the vtc-basic score knobs read per request.
type VTCOverrides struct {
	// MaxPodLoad overrides AIBRIX_ROUTER_VTC_BASIC_MAX_POD_LOAD: the
	// running-request count at which the utilization score saturates.
	MaxPodLoad float64
	// FairnessWeight overrides AIBRIX_ROUTER_VTC_BASIC_FAIRNESS_WEIGHT: the
	// weight of the fairness term. 0 drops the term.
	FairnessWeight float64
	// UtilizationWeight overrides AIBRIX_ROUTER_VTC_BASIC_UTILIZATION_WEIGHT:
	// the weight of the utilization term. 0 drops the term.
	UtilizationWeight float64
}

// AutoBlendOverrides mirrors the AIBRIX_ROUTING_AUTO_BLEND_* weights of the
// load-balance scorer the gateway silently blends behind every non-exclusive
// strategy.
type AutoBlendOverrides struct {
	// LoadBalanceWeight is the weight of the auto-blended load-balance scorer.
	// 0 disables the auto-blend for the request.
	LoadBalanceWeight int
	// LeastRequestWeight is the weight of the least-request scorer the
	// auto-blend adds for multi-port pods. 0 disables that addition.
	LeastRequestWeight int
	// PrefixCacheWeight is the prefix-cache weight of the dedicated
	// prefix-cache/load-balance ratio a bare "prefix-cache" request gets.
	PrefixCacheWeight int
	// PrefixCacheLoadBalanceWeight is the load-balance weight of that ratio.
	// 0 leaves those requests with prefix-cache scoring alone.
	PrefixCacheLoadBalanceWeight int
}

var (
	defaultRoutingOverrides atomic.Pointer[RoutingOverrides]
	// defaultOverridesMu serializes the two installers, which run at package
	// startup: the routing algorithm package installs the whole table, and
	// SetDefaultPDOverrides fills only the PD part for callers that have no
	// opinion on the rest.
	defaultOverridesMu sync.Mutex
)

// SetDefaultRoutingOverrides installs the process-wide defaults of the routing
// knobs. The routing algorithm package assembles them from the AIBRIX_*
// variables at startup, so a read site on a request without a model config
// profile gets the environment values without a per-request copy.
func SetDefaultRoutingOverrides(o *RoutingOverrides) {
	if o == nil {
		o = &RoutingOverrides{}
	}
	defaultOverridesMu.Lock()
	defer defaultOverridesMu.Unlock()
	defaultRoutingOverrides.Store(o)
}

// SetDefaultPDOverrides installs the process-wide PD defaults, keeping the
// routing defaults that are already installed. It exists so a caller that only
// owns PD knobs (the pd package and its tests) can fill its half of the table
// without copying the rest.
func SetDefaultPDOverrides(o *PDOverrides) {
	defaultOverridesMu.Lock()
	defer defaultOverridesMu.Unlock()
	next := &RoutingOverrides{}
	if cur := defaultRoutingOverrides.Load(); cur != nil {
		*next = *cur
	}
	if o != nil {
		next.PD = *o
	}
	defaultRoutingOverrides.Store(next)
}

// DefaultRoutingOverrides returns the process defaults installed by
// SetDefaultRoutingOverrides. It never returns nil.
func DefaultRoutingOverrides() *RoutingOverrides {
	if o := defaultRoutingOverrides.Load(); o != nil {
		return o
	}
	return &RoutingOverrides{}
}

// DefaultPDOverrides returns the PD half of the process defaults, which is what
// a PD read site falls back to when the request carries no resolved overrides.
// The returned struct is read-only and never nil.
func DefaultPDOverrides() *PDOverrides {
	return &DefaultRoutingOverrides().PD
}

// SetRoutingOverrides records the request's resolved routing overrides. A nil
// value means the request carries no profile overrides, and reads then fall
// back to the process defaults.
func (r *RoutingContext) SetRoutingOverrides(o *RoutingOverrides) {
	if r == nil {
		return
	}
	r.routingOverrides = o
}

// RoutingOverrides returns the request's resolved routing overrides, or the
// process defaults when the request carries none. The returned struct is
// read-only and never nil.
func (r *RoutingContext) RoutingOverrides() *RoutingOverrides {
	if r == nil || r.routingOverrides == nil {
		return DefaultRoutingOverrides()
	}
	return r.routingOverrides
}

// ClearRoutingOverrides drops the request's resolved overrides, restoring the
// process defaults. Reset calls it so a pooled context cannot steer the next
// request with the previous one's profile.
func (r *RoutingContext) ClearRoutingOverrides() {
	if r == nil {
		return
	}
	r.routingOverrides = nil
}
