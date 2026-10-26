/*
Copyright 2024 The Aibrix Team.

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

package router

import (
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/metrics"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/encode"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/engine"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/prefill"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/pd/selector"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/prefixcache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/configprofiles"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterPD                     types.RoutingAlgorithm = "pd"
	VLLMEngine                   string                 = "vllm"
	SGLangEngine                 string                 = "sglang"
	TensorRTLLM                  string                 = "trtllm"
	LLMEngineIdentifier          string                 = constants.ModelLabelEngine
	PDRoleSetIdentifier          string                 = pd.RoleSetLabel
	PDRoleIdentifier             string                 = pd.RoleLabel
	PDRolePrefill                string                 = pd.RolePrefill
	PDRoleDecode                 string                 = pd.RoleDecode
	PDRoleEncode                 string                 = encode.Role
	RoleReplicaIndex             string                 = "stormservice.orchestration.aibrix.ai/role-replica-index"
	PodGroupIndex                string                 = pd.PodGroupIndexLabel
	PromptLenBucketMinLength     string                 = "prompt-len-bucket-min-length"
	PromptLenBucketMaxLength     string                 = "prompt-len-bucket-max-length"
	defaultPrefillRequestTimeout int                    = 30

	defaultPrefillLoadImbalanceMinSpread      int32   = 16
	defaultDecodeLoadImbalanceMinSpread       float64 = 16
	defaultDecodeThroughputImbalanceMinSpread float64 = 2048
	defaultRequestRateHighLoadThreshold               = 1.0
	defaultRequestRateLowLoadThreshold                = 0.25
	defaultDecodeScoreRatioThreshold          float64 = 1.5 // min queue-drain time ratio to trigger drain-rate routing
	defaultPrefillLoadCapFactor               float64 = 1.5 // c in the c×(mean+1) prefill candidacy cap
	defaultDrainRateEpsilon                   float64 = 0.1 // floor for drain rate to avoid division by zero

	pdRouteValidateLLMEngineFail       = "pd-validate-llm-engine-fail"
	pdRouteFilterPrefillDecodePodsFail = "pd-filter-prefill-decode-pods-fail"
	pdRoutePrefillRequestError         = "pd-do-prefill-request-error"
	pdRoutePrefillRequestSuccess       = "pd-prefill-request-success"
)

const (
	// KVConnectorTypeIdentifier specifies the KV transfer backend for a PD pod. Supported values include "shfs", "nixl", and "mooncake".
	KVConnectorTypeIdentifier = "model.aibrix.ai/kv-connector-type"
	// KV connector types for different backends
	KVConnectorTypeSHFS     = "shfs"     // Default - AIBrix SHFS/KVCacheManager (GPU)
	KVConnectorTypeNIXL     = "nixl"     // NIXL for Neuron (uses disagg_prefill_resp wrapper)
	KVConnectorTypeMooncake = "mooncake" // Mooncake KV transfer backend

	HeaderPrefillTargetPodIP = "prefill-target-pod-ip"
	HeaderPrefillTargetPod   = "prefill-target-pod"
)

var (
	// seconds before a prefill HTTP request times out
	prefillRequestTimeout int = utils.LoadEnvInt("AIBRIX_PREFILL_REQUEST_TIMEOUT", defaultPrefillRequestTimeout)
	// min (max-min) request-count spread to trigger prefill load-imbalance routing
	aibrixPrefillLoadImbalanceMinSpread int32 = int32(utils.LoadEnvInt("AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD", int(defaultPrefillLoadImbalanceMinSpread)))
	// c in the bounded-load cap: a prefill pod with more than c×(mean+1) in-flight
	// prefills is skipped. Unlike the stddev filter it can fire at any pool size.
	aibrixPrefillLoadCapFactor float64 = utils.LoadEnvFloat("AIBRIX_PREFILL_LOAD_CAP_FACTOR", defaultPrefillLoadCapFactor)
	// min (max-min) request-count spread to trigger decode load-imbalance routing
	aibrixDecodeLoadImbalanceMinSpread float64 = utils.LoadEnvFloat("AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD", defaultDecodeLoadImbalanceMinSpread)
	// min (max-min) token-throughput spread (tok/s) to trigger decode throughput-imbalance routing
	aibrixDecodeThroughputImbalanceMinSpread float64 = utils.LoadEnvFloat("AIBRIX_DECODE_THROUGHPUT_IMBALANCE_MIN_SPREAD", defaultDecodeThroughputImbalanceMinSpread)
	// max/min drain-rate score ratio above which the slowest decode pod is avoided
	aibrixDecodeScoreRatioThreshold float64 = utils.LoadEnvFloat("AIBRIX_DECODE_SCORE_RATIO_THRESHOLD", defaultDecodeScoreRatioThreshold)
	// route to pods whose prompt-length bucket matches the request
	aibrixPromptLengthBucketing bool = utils.LoadEnvBool("AIBRIX_PROMPT_LENGTH_BUCKETING", false)
	// add an estimate of the output generated so far to the token_load decode score
	aibrixDecodeTokenLoadOutputGrowth bool = utils.LoadEnvBool("AIBRIX_DECODE_TOKEN_LOAD_OUTPUT_GROWTH", true)
	// aibrixTokenLoadSharedLedger shares the token_load decode ledger with the
	// other gateway replicas through Redis when Redis is configured; false keeps
	// it local to this process.
	aibrixTokenLoadSharedLedger bool = utils.LoadEnvBool("AIBRIX_TOKEN_LOAD_SHARED_LEDGER", true)
	// aibrixDecodeTokenLoadCapacityAware scales each decode pod's token_load
	// score by its KV capacity, so pods of different sizes are compared by how
	// full they are rather than by token counts.
	aibrixDecodeTokenLoadCapacityAware bool = utils.LoadEnvBool("AIBRIX_DECODE_TOKEN_LOAD_CAPACITY_AWARE", true)
	// KV transfer backend: "shfs" (GPU/SHFS) or "nixl" (Neuron)
	aibrixKVConnectorType string = utils.LoadEnv("AIBRIX_KV_CONNECTOR_TYPE", KVConnectorTypeSHFS)
	// prefill pod scoring strategy: "prefix_cache", "least_request", "conductor" or "token_load"
	aibrixPrefillScorePolicy string = utils.LoadEnv("AIBRIX_PREFILL_SCORE_POLICY", pd.PrefillScorePolicyPrefixCache)
	// decode pod scoring strategy: "load_balancing" or "least_request"
	aibrixDecodeScorePolicy string = utils.LoadEnv("AIBRIX_DECODE_SCORE_POLICY", pd.ScorePolicyLoadBalancing)
)

// loadBalancingDecodePolicy is shared for nil-policy fallback and invalid-score fallback (stateless type).
var loadBalancingDecodePolicy = pd.LoadBalancingDecodePolicy{}

// validPrefillScorePolicies lists the accepted AIBRIX_PREFILL_SCORE_POLICY /
// routingConfig.prefillScorePolicy values, for log lines.
var validPrefillScorePolicies = []string{
	pd.PrefillScorePolicyPrefixCache,
	pd.PrefillScorePolicyLeastRequest,
	pd.PrefillScorePolicyConductor,
	pd.PrefillScorePolicyTokenLoad,
	pd.PrefillScorePolicyHybridCacheLoad,
}

func init() {
	// Point the vLLM engine handler at the live connector-type var so that tests
	// (and runtime config changes via env) are reflected without a second copy.
	engine.SetConnectorTypeFunc(func() string { return aibrixKVConnectorType })
}

// effectivePromptLengthBucketing reports whether prompt-length bucketing is on
// for this request, honouring the profile override when one is set.
func effectivePromptLengthBucketing(routingCtx *types.RoutingContext) bool {
	return routingCtx.PDOverrides().PromptLengthBucketing
}

// bucketServeMode returns the bucket-serve mode this request routes with, and
// whether the adaptive plan is on for it at all. Both come from the request's
// resolved knobs, so a profile can switch the plan on, or pick another mode,
// for the models it routes.
func bucketServeMode(routingCtx *types.RoutingContext) (pd.BucketMode, bool) {
	ov := routingCtx.PDOverrides()
	if !ov.BucketServe {
		return "", false
	}
	if mode, ok := pd.ParseBucketMode(ov.BucketServeMode); ok {
		return mode, true
	}
	return pd.BucketModeThroughput, true
}

// bucketServeGroups returns one group per eligible roleset, with the
// prompt-length range its pods declare and its prefill replica count, which is
// how much of the model's traffic the roleset should carry. It runs a profile
// lookup per roleset, not per pod: the pods of a roleset share one model config
// profile, and the planner only needs the range once. The order does not
// matter: the planner sorts a copy, and the plan cache key sorts its parts.
func bucketServeGroups(routingCtx *types.RoutingContext, readyPods []*v1.Pod) []pd.BucketGroup {
	eligible := eligiblePDRolesets(readyPods)
	groups := make([]pd.BucketGroup, 0, len(eligible))
	for id, b := range eligible {
		minLength, maxLength := 0, math.MaxInt32
		if len(b.prefills) > 0 {
			minLength, maxLength = podPromptLenBucketBounds(routingCtx, b.prefills[0])
		}
		groups = append(groups, pd.BucketGroup{Name: id, Min: minLength, Max: maxLength, Replicas: len(b.prefills)})
	}
	return groups
}

// podPromptLenBucketBounds returns the prompt-length range a pod's model
// config profile declares, with the whole range as the default: a pod that
// declares none is not bucket-scoped.
func podPromptLenBucketBounds(routingCtx *types.RoutingContext, pod *v1.Pod) (int, int) {
	profile := configprofiles.ResolveProfileFrom(routingCtx.ModelConfig, pod, routingCtx.ReqConfigProfile)
	if profile == nil {
		// Without a model config (model-level or model.aibrix.ai/config on the
		// pod) a pod is not bucket-scoped; treat as any length.
		return 0, math.MaxInt32
	}
	return promptLenBucketBounds(configprofiles.ParseRoutingConfig(profile.RoutingConfig))
}

// effectiveScorePolicies returns prefill/decode scoring policies for this request.
// When routingCtx.ConfigProfile.RoutingConfig sets prefillScorePolicy and/or decodeScorePolicy,
// those override the gateway defaults from AIBRIX_PREFILL_SCORE_POLICY / AIBRIX_DECODE_SCORE_POLICY
// (stored on the router at startup). Empty or missing fields keep the env-based policies.
// A non-empty decodeScorePolicy that is not recognized returns an error so routing does not
// silently run with a different policy than the user configured.
func (r *pdRouter) effectiveScorePolicies(routingCtx *types.RoutingContext) (pd.PrefillScorePolicy, pd.DecodeScorePolicy, error) {
	prefill := r.prefillPolicy
	decode := r.decodePolicy
	if routingCtx.ConfigProfile == nil || routingCtx.ConfigProfile.Routing == nil {
		return prefill, decode, nil
	}
	cfg := routingCtx.ConfigProfile.Routing
	if s := strings.TrimSpace(cfg.PrefillScorePolicy); s != "" {
		switch s {
		case pd.PrefillScorePolicyLeastRequest:
			prefill = pd.NewLeastRequestPrefillPolicy()
		case pd.PrefillScorePolicyPrefixCache:
			prefill = newPrefixCachePrefillPolicy(r.prefixCacheIndexer)
		case pd.PrefillScorePolicyConductor:
			prefill = newConductorPrefillPolicy(r.prefixCacheIndexer, r.cache)
		case pd.PrefillScorePolicyTokenLoad:
			prefill = pd.NewTokenLoadPrefillPolicy(r.tokenLoadTracker)
		case pd.PrefillScorePolicyHybridCacheLoad:
			prefill = newHybridCacheLoadPrefillPolicy(r.prefixCacheIndexer, r.tokenLoadTracker)
		default:
			klog.InfoS("unknown prefillScorePolicy in routingConfig, keeping env-based policy",
				"request_id", routingCtx.RequestID, "value", s,
				"valid", validPrefillScorePolicies)
			prefill = r.prefillPolicy
		}
	}
	if s := strings.TrimSpace(cfg.DecodeScorePolicy); s != "" {
		d, _, unknown := pd.ResolveDecodePolicy(s)
		if unknown {
			valid := pd.ValidDecodePolicyNames()
			klog.Warningf("unknown decodeScorePolicy in routingConfig (request_id=%s value=%q valid=%v)",
				routingCtx.RequestID, s, valid)
			return nil, nil, fmt.Errorf("unknown decodeScorePolicy %q in routingConfig (valid: %v)", s, valid)
		}
		decode = d
	}
	if strings.TrimSpace(cfg.PrefillScorePolicy) != "" || strings.TrimSpace(cfg.DecodeScorePolicy) != "" {
		klog.V(4).InfoS("pd score policies from model config profile routingConfig",
			"request_id", routingCtx.RequestID, "prefill_policy", prefill.Name(),
			"decode_policy", decode.Name())
	}
	return prefill, decode, nil
}

type pdRouter struct {
	// sharedDecodeLedger is set when the decode ledger is shared with the other
	// gateway replicas (see AIBRIX_TOKEN_LOAD_SHARED_LEDGER).
	sharedDecodeLedger    sharedDecodeLedgerCache
	cache                 cache.Cache
	prefillPolicy         pd.PrefillScorePolicy
	decodePolicy          pd.DecodeScorePolicy
	prefixCacheIndexer    *prefixcacheindexer.PrefixHashTable
	prefillRequestTracker *pd.PrefillRequestTracker
	pendingDecodeTracker  *pd.PendingDecodeTracker
	httpClient            *http.Client
	prefixUpdateCh        chan prefixUpdateJob
	countersMu            sync.RWMutex
	selectionCounts       map[string]int64
	podSelector           selector.PodSelector
	prefillExecutor       prefill.PrefillExecutor
	encodeExecutor        *encode.Executor
	trtHandler            *engine.TRTLLMHandler

	// pendingPrefixCredits holds the prefix-cache credit selection chose for a
	// request's prefill pod, keyed by request ID. Selection runs under the
	// model's selection lock (selectStripes) and the decode pod is admitted only
	// after it, so Route takes the credit right after selection and enqueues it
	// once the decode pod admitted the request: a rejected request credits no
	// pod. This relies on request IDs being unique and on Route being the only
	// caller of Select; another caller would leave its entries here.
	pendingPrefixCredits sync.Map

	// tokenLoadTracker is the token-weighted prefill ledger read by the
	// token_load prefill policy, and the decode ledger read by the token_load
	// decode policy. It is charged in filterPrefillDecodePods for requests
	// scored by those policies, released by the prefill executor when the
	// prefill call returns, and released fully on request completion through
	// the cache.RequestTracker callbacks (see DoneRequestCount). nil in routers
	// built without one (tests); every access is nil-guarded.
	tokenLoadTracker *pd.TokenLoadTracker

	// bucketServe is the adaptive bucket-serve planner: it bands prompt
	// lengths across the rolesets that share them, so the prefill scoring picks
	// inside one band instead of across every covering roleset. Like
	// tokenLoadTracker it is built unconditionally, so a profile can switch it
	// on without a gateway restart; every access is nil-guarded.
	bucketServe *pd.BucketServeTracker

	// bucketServeMetricObserver is set only by tests that need to inspect
	// bucket-serve emissions without replacing process-wide metric hooks.
	bucketServeMetricObserver *bucketServeMetricObserver

	// selectStripes makes "read every candidate's tracked load, pick the best,
	// register the pick" one atomic step per model in filterPrefillDecodePods.
	// Without it, concurrent requests in a burst all read the same pre-burst
	// tracker snapshot and pile onto whichever pods looked idle at read time,
	// because the registration that would have made them visible to each other
	// only happened after selection returned.
	//
	// Striping by base model (routingCtx.BaseModel when set for LoRA requests,
	// else routingCtx.Model) across a fixed power-of-two array avoids
	// cross-model serialization and map growth under dynamic model churn while
	// keeping each model's bursts — including bursts across LoRA adapters
	// sharing the same base PD pods — strictly serialized. Two unrelated base
	// models that hash-collide onto the same stripe simply serialize with each
	// other, which is always safe and matches pre-striping behavior. Each
	// selection acquires at most one stripe, so stripe-to-stripe deadlocks
	// cannot occur; post-selection bookkeeping (pendingPrefixCredits,
	// countersMu/selectionCounts, and selection/bucket-serve Prometheus counters)
	// runs after the stripe is released (recordPDSelection) so countersMu is no
	// longer nested under selectStripes. All underlying trackers
	// (prefillRequestTracker, pendingDecodeTracker, tokenLoadTracker,
	// pendingPrefixCredits) also maintain their own internal synchronization.
	//
	// Only in-memory tracker-dependent work runs under the stripe lock: the
	// imbalance fast paths, the per-pod scoring, selectFinalPDPair, and the
	// tracker registrations (selectAndRegisterPDPairUnderLock). Tokenization,
	// prefix matching and conductor histogram prefetch (PrefillScorePolicy.Prepare),
	// the bucket-serve plan, combined-pod spillover checks, the cross-gateway
	// running-request / shared decode-ledger Redis read, read-only scraped decode
	// metrics, pod-key indexing, and prompt-token estimation run before the lock
	// is taken so neither network I/O nor read-only cache/body work serializes
	// concurrent selections.
	selectStripes [pdSelectLockStripes]paddedMutex
}

// pdSelectLockStripes is the fixed power-of-two stripe count for per-model PD
// selection serialization.
const pdSelectLockStripes = 64

// Compile-time check: pdSelectLockStripes must be a power of two so
// selectStripeIndex can mask with (pdSelectLockStripes - 1).
var _ [0]struct{} = [pdSelectLockStripes & (pdSelectLockStripes - 1)]struct{}{}

type paddedMutex struct {
	sync.Mutex
	_ [56]byte // keep adjacent stripes on separate 64-byte cache lines
}

// selectLockKey returns the model name used to pick a selection lock stripe:
// BaseModel when set (so LoRA adapters sharing base PD pods serialize on the
// same stripe), otherwise Model.
func selectLockKey(routingCtx *types.RoutingContext) string {
	if routingCtx.BaseModel != "" {
		return routingCtx.BaseModel
	}
	return routingCtx.Model
}

// selectStripeIndex hashes model with 32-bit FNV-1a into [0, pdSelectLockStripes)
// without allocating.
func selectStripeIndex(model string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(model); i++ {
		h ^= uint32(model[i])
		h *= prime32
	}
	return h & (pdSelectLockStripes - 1)
}

func (r *pdRouter) selectLock(model string) *sync.Mutex {
	return &r.selectStripes[selectStripeIndex(model)].Mutex
}

type bucketServeMetricObserver struct {
	counter func(string, map[string]string)
	gauge   func(string, map[string]string)
}

func newPrefixCachePrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable) pd.PrefillScorePolicy {
	return pd.NewPrefixCachePrefillPolicy(prefixcache.NewTokenizer(), sharedPrefixTable)
}

func newConductorPrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable, metricCache cache.MetricCache) pd.PrefillScorePolicy {
	return pd.NewConductorPrefillPolicy(prefixcache.NewTokenizer(), sharedPrefixTable, metricCache, pd.NewConductorPrefillPolicyConfig())
}

func newHybridCacheLoadPrefillPolicy(sharedPrefixTable *prefixcacheindexer.PrefixHashTable, tracker *pd.TokenLoadTracker) pd.PrefillScorePolicy {
	return pd.NewHybridCacheLoadPrefillPolicy(prefixcache.NewTokenizer(), sharedPrefixTable, tracker)
}

func NewPDRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		klog.Error("fail to get cache store in prefix cache router")
		return nil, err
	}
	return NewPDRouterWithCacheAndPrefixIndexer(c, prefixcacheindexer.GetSharedPrefixHashTable())
}

// NewPDRouterWithCacheAndPrefixIndexer builds a PD router on an explicit cache
// and prefix table instead of the process-global ones, so an isolated
// RouterManager (see NewRouterManagerWithCacheAndPrefixIndexer) can route
// "pd" without initialising the global cache. A nil sharedPrefixTable gets a
// fresh table.
func NewPDRouterWithCacheAndPrefixIndexer(c cache.Cache, sharedPrefixTable *prefixcacheindexer.PrefixHashTable) (types.Router, error) {
	if sharedPrefixTable == nil {
		sharedPrefixTable = prefixcacheindexer.NewPrefixHashTable()
	}
	// One tracker per router, created unconditionally so that a routingConfig
	// can switch a model to token_load or hybrid_cache_load without a gateway
	// restart.
	tokenLoadTracker := pd.NewTokenLoadTracker()
	// The bucket-serve planner is unconditional for the same reason: its
	// configuration comes from each request's resolved knobs.
	bucketServeTracker := pd.NewBucketServeTracker()

	var policy pd.PrefillScorePolicy
	switch aibrixPrefillScorePolicy {
	case pd.PrefillScorePolicyLeastRequest:
		policy = pd.NewLeastRequestPrefillPolicy()
	case pd.PrefillScorePolicyPrefixCache:
		policy = newPrefixCachePrefillPolicy(sharedPrefixTable)
	case pd.PrefillScorePolicyConductor:
		policy = newConductorPrefillPolicy(sharedPrefixTable, c)
	case pd.PrefillScorePolicyTokenLoad:
		policy = pd.NewTokenLoadPrefillPolicy(tokenLoadTracker)
	case pd.PrefillScorePolicyHybridCacheLoad:
		policy = newHybridCacheLoadPrefillPolicy(sharedPrefixTable, tokenLoadTracker)
	default:
		klog.InfoS("pd_router unknown AIBRIX_PREFILL_SCORE_POLICY, using prefix_cache",
			"value", aibrixPrefillScorePolicy, "valid", validPrefillScorePolicies)
		policy = newPrefixCachePrefillPolicy(sharedPrefixTable)
	}
	klog.InfoS("pd_router prefill score policy", "policy", policy.Name())

	decodePol, _, unknownDecode := pd.ResolveDecodePolicy(aibrixDecodeScorePolicy)
	if unknownDecode {
		klog.InfoS("pd_router unknown AIBRIX_DECODE_SCORE_POLICY, using load_balancing",
			"value", aibrixDecodeScorePolicy, "valid", pd.ValidDecodePolicyNames())
	}
	klog.InfoS("pd_router decode score policy", "policy", decodePol.Name(), "describe", decodePol.Describe())

	// Create a shared HTTP client with connection pooling. otelhttp creates the
	// prefill client span and injects its trace context into the outbound request.
	transport := pd.NewHTTPTransport()
	// Deliberately no client-level Timeout: every call carries its own
	// deadline, which is AIBRIX_PREFILL_REQUEST_TIMEOUT unless the request's
	// profile overrides it (see DefaultExecutor.effectiveRequestTimeout), and a
	// timeout fixed here would silently cap a larger profile value. The decode
	// abort sets its own deadline the same way.
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(transport),
	}

	trtScheduleStyle := utils.LoadEnv("AIBRIX_TRT_SCHEDULE_STYLE", engine.TRTContextFirst)
	switch trtScheduleStyle {
	case engine.TRTContextFirst, engine.TRTGenerationFirst:
	default:
		// Same policy as the env-driven knobs above: an unrecognized value is
		// reported and the safe default wins, instead of failing router
		// construction (the router manager would then register a nil provider
		// for "pd", which panics, recovered, on every pd request).
		klog.InfoS("pd_router unknown AIBRIX_TRT_SCHEDULE_STYLE, using context_first",
			"value", trtScheduleStyle,
			"valid", []string{engine.TRTContextFirst, engine.TRTGenerationFirst})
		trtScheduleStyle = engine.TRTContextFirst
	}
	// NewTRTLLMHandler now fails only for generation_first without a provider,
	// which is a programming error and must still fail construction.
	trtHandler, err := engine.NewTRTLLMHandler(trtScheduleStyle, engine.NewTRTServerInfoCache(httpClient))
	if err != nil {
		return nil, err
	}

	r := &pdRouter{
		trtHandler:            trtHandler,
		cache:                 c,
		prefillPolicy:         policy,
		decodePolicy:          decodePol,
		prefixCacheIndexer:    sharedPrefixTable,
		prefillRequestTracker: pd.NewPrefillRequestTracker(),
		pendingDecodeTracker:  pd.NewPendingDecodeTracker(),
		tokenLoadTracker:      tokenLoadTracker,
		bucketServe:           bucketServeTracker,
		httpClient:            httpClient,
		prefixUpdateCh:        make(chan prefixUpdateJob, 1024),
		selectionCounts:       make(map[string]int64),
	}
	r.podSelector = selector.NewDefaultSelector(r.filterPrefillDecodePods)
	r.prefillExecutor = prefill.NewDefaultExecutor(httpClient, r.prefillRequestTracker,
		prefill.WithTokenLoadTracker(tokenLoadTracker))
	r.encodeExecutor = encode.NewExecutor(httpClient)
	// Request completion is only observable through the cache's request
	// tracker callbacks; that is where the resident-KV charge is released.
	c.RegisterRequestTracker(r)
	r.shareDecodeLedger()

	r.startPrefixUpdater()
	return r, nil
}

// AddRequestCount implements cache.RequestTracker. The pd router charges its
// ledgers at selection time (filterPrefillDecodePods), so there is nothing to
// do here.
func (r *pdRouter) AddRequestCount(_ *types.RoutingContext, _ string, _ string) int64 { return 0 }

// DoneRequestCount implements cache.RequestTracker: the request is finished,
// so nothing of it can still be resident on its prefill pod. The callbacks
// fire for every gateway request, not only pd-routed ones; unknown request
// IDs are a cheap no-op. ctx may be nil (request cancelled before routing).
func (r *pdRouter) DoneRequestCount(ctx *types.RoutingContext, requestID string, _ string, _ int64) {
	r.releaseTokenLoad(trackerKeyFor(ctx, requestID))
}

// DoneRequestTrace implements cache.RequestTracker; see DoneRequestCount.
func (r *pdRouter) DoneRequestTrace(ctx *types.RoutingContext, requestID string, _ string, _, _, _ int64) {
	r.releaseTokenLoad(trackerKeyFor(ctx, requestID))
}

// trackerKeyFor returns the request's gateway-internal load-tracker key. The completion callbacks
// above receive the routing context, which is nil only when the request was cancelled before
// routing -- nothing was charged in that case, so the fallback requestID is a harmless no-op. When
// present, charges are keyed by ctx.TrackerKey() (never the client-controllable RequestID) so a
// shared traceparent cannot release another request's charge.
//
// RoutingContext is pooled. In the current flow the completion callback runs on the request's own
// goroutine before the context is returned to the pool (Process calls finishRequestCount before
// RoutingContext.Delete), so ctx is always this request's. The passed requestID equals
// ctx.RequestID because HandleRequestHeaders syncs them (st.requestID = st.routerCtx.RequestID in
// gateway.go, the "sync requestID if it was overridden by traceparent header" line), so the guard
// is a no-op on today's path. It is a cheap safeguard against a future reordering or async path
// handing us a context already reused by another request: on a mismatch, fall back to requestID
// rather than release the wrong request's charge -- the tracker then no-ops and the charge is
// reclaimed by its TTL. If the two ids are ever intentionally decoupled, revisit this.
func trackerKeyFor(ctx *types.RoutingContext, requestID string) string {
	if ctx != nil && ctx.RequestID == requestID {
		return ctx.TrackerKey()
	}
	return requestID
}

// chargeTokenLoad charges the request's estimated prefill cost to pod when
// policy scores from the token-load tracker. Called under the model's selection
// lock right after the prefill registration so the next selection sees this
// charge.
//
// The charge covers only the tokens pod has to compute: the growth of the
// conversation since its previous turn when the request carries the
// caller-owned x-aibrix-session-key header, otherwise the part of the prompt
// the pod's prefix cache does not hold when scorer looked it up, otherwise
// the whole prompt. See TokenLoadTracker.NewTokens. The gateway-issued
// x-session-id of session-affinity routing is deliberately not consulted:
// it names a backend, not a conversation.
func (r *pdRouter) chargeTokenLoad(routingCtx *types.RoutingContext, pod *v1.Pod, policy pd.PrefillScorePolicy, scorer pd.PrefillScorer) {
	if r.tokenLoadTracker == nil || !pd.UsesTokenLoad(policy) {
		return
	}
	r.chargeTokenLoadWithKeyAndTokens(routingCtx, pod, utils.GeneratePodKey(pod.Namespace, pod.Name), policy, scorer, pd.EstimatePromptTokens(routingCtx.ReqBody))
}

func (r *pdRouter) chargeTokenLoadWithKeyAndTokens(routingCtx *types.RoutingContext, pod *v1.Pod, podKey string, policy pd.PrefillScorePolicy, scorer pd.PrefillScorer, promptTokens int) {
	if r.tokenLoadTracker == nil || !pd.UsesTokenLoad(policy) {
		return
	}
	overrides := routingCtx.PDOverrides()
	// MaxSessions caps the tracker's shared session table, so it is not a
	// per-request knob and stays environment-only (see types.PDOverrides).
	maxSessions := r.tokenLoadTracker.Config().MaxSessions

	sessionID := routingCtx.ReqHeaders[constants.HeaderSessionKey]
	matchPct := pd.PrefixMatchPercent(scorer, podKey)
	newTokens, source := r.tokenLoadTracker.NewTokensWithSessionLimits(routingCtx.Model, sessionID, promptTokens, matchPct,
		overrides.TokenLoad.SessionTTL, maxSessions)
	cost := r.tokenLoadTracker.PrefillCostWithRequestCost(newTokens, overrides.TokenLoad.RequestCost)
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("pd_router token_load charge",
			"request_id", routingCtx.RequestID, "pod_name", pod.Name, "policy", policy.Name(),
			"prompt_tokens", promptTokens, "new_tokens", newTokens, "source", source,
			"prefix_match_percent", matchPct, "cost", cost)
	}
	r.tokenLoadTracker.AcquirePrefillWithTTL(routingCtx.TrackerKey(), podKey, cost, overrides.TokenLoad.TTL)
}

// chargeDecodeTokenLoad charges the request's prompt to the decode pod when the
// request is scored by the token_load decode policy. The decode pod receives
// the whole prompt's KV from the prefill pod, so the charge is the full prompt,
// not the prefix-matched remainder the prefill charge uses. It carries no fixed
// per-request cost: AIBRIX_TOKEN_LOAD_REQUEST_COST models prefill setup work,
// not KV on the decoder. It is released on request completion or prefill
// failure (releaseTokenLoad, or the executor for an async prefill leg).
func (r *pdRouter) chargeDecodeTokenLoad(routingCtx *types.RoutingContext, pod *v1.Pod, policy pd.DecodeScorePolicy) {
	if r.tokenLoadTracker == nil || !pd.UsesDecodeTokenLoad(policy) {
		return
	}
	r.chargeDecodeTokenLoadWithKeyAndTokens(routingCtx, pod, utils.GeneratePodKey(pod.Namespace, pod.Name), policy, pd.EstimatePromptTokens(routingCtx.ReqBody))
}

func (r *pdRouter) chargeDecodeTokenLoadWithKeyAndTokens(routingCtx *types.RoutingContext, pod *v1.Pod, podKey string, policy pd.DecodeScorePolicy, promptTokens int) {
	if r.tokenLoadTracker == nil || !pd.UsesDecodeTokenLoad(policy) {
		return
	}
	overrides := routingCtx.PDOverrides()
	cost := float64(promptTokens)
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("pd_router token_load decode charge",
			"request_id", routingCtx.RequestID, "pod_name", pod.Name, "prompt_tokens", cost)
	}
	r.tokenLoadTracker.AcquireDecodeWithTTL(routingCtx.TrackerKey(), podKey, cost, overrides.TokenLoad.TTL)
}

// releaseTokenLoad drops whatever the request still holds on the token-load
// tracker: the terminal paths (prefill failure, request completion).
func (r *pdRouter) releaseTokenLoad(trackerKey string) {
	if r.tokenLoadTracker == nil {
		return
	}
	r.tokenLoadTracker.ReleaseAll(trackerKey)
}

func (r *pdRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	// Park the request's resolved PD overrides on its leg before anything can
	// read them: selection reads them through the routing context, and the
	// async prefill leg, including the decode abort it may start, reads them
	// off the leg after this request's routing context has been recycled. The
	// values were resolved once for the request (ResolveRoutingOverrides).
	pdOverrides := ctx.RoutingOverrides().PD
	ctx.SetPDOverrides(&pdOverrides)

	readyPods := readyPodList.All()

	// Validate the request body before any pod selection or prefix-index
	// mutation, for every engine: it must be a JSON object and must not
	// repeat a gateway-controlled top-level key (sjson only edits the first
	// occurrence, so a duplicate would let the client's value override the
	// gateway's). A malformed request must not pollute selection counters or
	// the prefix cache. ctx.Engine is already set by selectTargetPod.
	// The TRT handler is router-scoped (its schedule style comes from the
	// environment); every other engine uses the shared registry. This is the
	// only handler resolution in the request path: the result is passed to the
	// executor, so validation and dispatch cannot drift apart.
	handler := engine.Resolve(ctx.Engine)
	if ctx.Engine == TensorRTLLM && r.trtHandler != nil {
		handler = r.trtHandler
	}
	if err := engine.ValidateRequest(ctx.ReqBody, handler); err != nil {
		return "", err
	}

	// Select registers the chosen pods with pendingDecodeTracker and
	// prefillRequestTracker atomically with the decision (see selectStripes);
	// Route owns the matching removals.
	prefillPod, decodePod, err := r.podSelector.Select(ctx, readyPods)
	// Take the prefix credit selection recorded before any return below.
	credit, hasCredit := r.pendingPrefixCredits.LoadAndDelete(ctx.TrackerKey())
	if err != nil {
		metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
			map[string]string{"status": pdRouteFilterPrefillDecodePodsFail, "status_code": "400"})
		return "", fmt.Errorf("failed to filter prefill/decode pods for request %s: %w", ctx.RequestID, err)
	}

	defer r.pendingDecodeTracker.RemovePendingDecode(ctx.TrackerKey())

	// Admit the decode pod against the gateway's per-replica in-flight cap before the
	// prefill leg is sent. Once the prefill has run, a rejection would only throw away its
	// work and leave its KV waiting for a decode that never comes.
	if err := ctx.AdmitTarget(decodePod); err != nil {
		r.prefillRequestTracker.RemovePrefillRequest(ctx.TrackerKey())
		r.releaseTokenLoad(ctx.TrackerKey())
		return "", fmt.Errorf("decode pod %s rejected request %s: %w", decodePod.Name, ctx.RequestID, err)
	}
	// Credit the prefill pod only now that the request was admitted, but still before its
	// prefill is sent, so requests with the same prefix that arrive during it follow it.
	if hasCredit {
		job := credit.(prefixUpdateJob)
		r.enqueuePrefixUpdate(job.prefixHashes, job.model, job.pod)
	}

	if prefillPod != nil {

		klog.InfoS("selected prefill/decode pods", "request_id", ctx.RequestID, "prefill_pod", prefillPod.Name, "decode_pod", decodePod.Name)
		if ctx.RespHeaders == nil {
			ctx.RespHeaders = make(map[string]string)
		}
		ctx.RespHeaders[HeaderPrefillTargetPod] = prefillPod.Name
		ctx.RespHeaders[HeaderPrefillTargetPodIP] = prefillPod.Status.PodIP
		// Record where the decode leg is going before the prefill leg is
		// fired. For an async engine that leg runs in its own goroutine and
		// can fail before SetTargetPod below has even run, and it must be able
		// to aim an /abort_request at the decode pod that is waiting for a KV
		// transfer that will never arrive. PodAddress yields exactly the
		// address Route returns to Envoy, so the abort lands on the HTTP
		// server actually serving the decode leg.
		ctx.SetDecodeTarget(ctx.PodAddress(decodePod), decodePod.Name)
		// The async dispatch contract of the handler also decides how the
		// gateway treats a failure that arrives after the decode leg has started
		// responding. Record it before dispatching so the stream goroutine never
		// has to infer the mode from the engine name (TRT context-first and
		// generation-first share one).
		ctx.SetResetAfterHeaders(engine.AsyncDispatchPolicyFor(handler).ResetAfterHeaders)
		r.encodeMedia(ctx, readyPods, prefillPod)
		// The prefill registration was made by Select; the executor's
		// RemovePrefillRequest (sync/async) is the matching decrement.
		err = r.doPrefillRequest(ctx, prefillPod, handler)

		if err != nil {
			// Remove is a no-op if the executor already cleaned up (e.g. sync HTTP failure).
			r.prefillRequestTracker.RemovePrefillRequest(ctx.TrackerKey())
			r.releaseTokenLoad(ctx.TrackerKey())
			metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestFailTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": pdRoutePrefillRequestError, "status_code": "500"})
			klog.ErrorS(err, pdRoutePrefillRequestError, "request_id", ctx.RequestID)
			return "", fmt.Errorf("prefill request failed for request %s: %w", ctx.RequestID, err)
		}
		if !handler.IsAsync() {
			metrics.EmitMetricToPrometheus(ctx, nil, metrics.GatewayPrefillRequestSuccessTotal, &metrics.SimpleMetricValue{Value: 1.0},
				map[string]string{"status": pdRoutePrefillRequestSuccess, "status_code": "200"})
		}
	}

	if prefillPod == nil {
		// A combined pod serves the whole request, so it is the one that reads
		// the encoder output.
		r.encodeMedia(ctx, readyPods, decodePod)
	}
	ctx.SetTargetPod(decodePod)
	return ctx.TargetAddress(), nil
}

type Scores struct {
	Pod   *v1.Pod
	Score float64
}

// bucketServeBand returns the roleset the adaptive bucket-serve plan prefers
// for promptLength, together with the upper bound of the band that covers it.
// It records the request in the plan's traffic picture first, so the cut points
// follow what this gateway routes, and it republishes the plan's gauge series
// whenever the plan is recomputed. An empty roleset means the plan has no
// opinion: the feature is off for the request, the prompt length is unknown, or
// no band covers that length.
func (r *pdRouter) bucketServeBand(routingCtx *types.RoutingContext, readyPods []*v1.Pod, promptLength int) (string, int) {
	tracker := r.bucketServe
	if tracker == nil || promptLength <= 0 {
		return "", 0
	}
	mode, on := bucketServeMode(routingCtx)
	if !on {
		return "", 0
	}
	res := tracker.Band(routingCtx.Model, mode, promptLength, time.Now(), bucketServeGroups(routingCtx, readyPods))
	if res.Refreshed {
		r.publishBucketServePlan(routingCtx, res)
	}
	return res.Roleset, res.Max
}

// publishBucketServePlan republishes the gauge series of a recomputed plan:
// the highest upper bound among the bands every roleset holds, and a delete
// for every roleset no live plan holds anymore, so no series outlives the
// plans that produced it.
func (r *pdRouter) publishBucketServePlan(routingCtx *types.RoutingContext, res pd.BandResult) {
	bounds := make(map[string]int, len(res.Plan))
	for _, band := range res.Plan {
		if band.Max > bounds[band.Group] {
			bounds[band.Group] = band.Max
		}
	}
	for roleset, bound := range bounds {
		r.emitBucketServeMetric(routingCtx, metrics.PDBucketServeBandMax, float64(bound),
			map[string]string{"roleset": roleset}, false)
	}
	for _, roleset := range res.Dropped {
		if _, held := bounds[roleset]; held {
			continue
		}
		metrics.DeleteGaugeMetricForPod(metrics.PDBucketServeBandMax, routingCtx, nil, map[string]string{"roleset": roleset})
	}
}

func (r *pdRouter) emitBucketServeMetric(routingCtx *types.RoutingContext, name string, value float64, labels map[string]string, counter bool) {
	if observer := r.bucketServeMetricObserver; observer != nil {
		if counter {
			observer.counter(name, labels)
		} else {
			observer.gauge(name, labels)
		}
		return
	}
	metrics.EmitMetricToPrometheus(routingCtx, nil, name, &metrics.SimpleMetricValue{Value: value}, labels)
}

// filterPrefillDecodePods selects one prefill pod and one decode pod for the request.
// For multi-node tensor parallelism, only pods with PodGroupIndex="0" run the HTTP
// server and are routable; pods without that label are included for backward compatibility.
//
// Selection sequence:
//
//  1. collectAndBucketPods — partitions readyPods from eligible rolesets into prefill,
//     decode, and (when bucketing is on) prompt-length-filtered and combined-role slices.
//
//  2. Pod availability check — errors immediately when no prefill or decode pods exist
//     and no combined pod is available to cover the gap.
//
//  3. Bucketing path (prompt-length bucketing only) —
//     a. No bucket match: prompt length falls outside every declared range.
//     - Combined pods available → pick a random combined pod (no prefill HTTP call).
//     - No combined pods → error; do not route to the wrong storm.
//     b. Bucket match + load imbalance: prefill or decode pods are hot while a
//     combined pod is idle → score and pick the best combined pod.
//
//  4. Prefill load-imbalance fast path — if outstanding prefill counts are skewed
//     beyond aibrixPrefillLoadImbalanceMinSpread, narrow to the single least-loaded
//     prefill pod and align decodePods to its roleset. Continues to step 5.
//
//  5. Decode load-imbalance fast path — three ordered checks (request-count spread,
//     throughput spread, drain-rate ratio); the first that fires narrows decodePods
//     to a single pod and aligns prefillPods to its roleset. Steps 4 and 5 are
//     independent: both can fire on the same request if both prefill and decode are
//     imbalanced, with each narrowing its own side of the pair. Under the token_load
//     decode policy the decode result is ignored and every decode pod is scored.
//
//  6. Bucket-serve band preference (AIBRIX_BUCKET_SERVE only) — the adaptive plan
//     bands the request's prompt length to one roleset, so narrow the candidates to
//     that roleset, keeping both sides aligned. The band is a preference, not a
//     gate: when the load fast paths of steps 4 and 5 already reached past the
//     banded roleset, or it has no candidate left on either side, their choice
//     stands. The plan is computed before the lock is taken; only this narrowing
//     runs under it.
//
//  7. Score remaining candidates — scorePreparedPrefillPods and scoreDecodePods
//     evaluate every roleset; finalPDScore picks the roleset with the lowest
//     combined score.
//
//  8. Register — the chosen decode pod is recorded in pendingDecodeTracker and the
//     chosen prefill pod in prefillRequestTracker (and, under the token_load
//     prefill or decode policy, charged to tokenLoadTracker) before returning, so
//     the next selection sees this one. Steps 4-8 read in-memory tracker state and
//     run under the model's selection lock (selectStripes); steps 1-3, the policy
//     Prepare step (tokenization, prefix matching), the bucket-serve plan (which
//     takes only the tracker lock), and the cross-gateway running-request / shared
//     decode-ledger read run before the lock is taken so Redis I/O does not
//     serialize concurrent selections. Route owns the matching removals.
func (r *pdRouter) filterPrefillDecodePods(routingCtx *types.RoutingContext, readyPods []*v1.Pod) (*v1.Pod, *v1.Pod, error) {
	pdOverrides := routingCtx.PDOverrides()
	bucketing := pdOverrides.PromptLengthBucketing

	var promptLength int
	if bucketing {
		promptLength, _ = routingCtx.PromptLength()
		klog.V(4).InfoS("prompt length based filtering enabled", "request_id", routingCtx.RequestID, "prompt_length", promptLength)
	}

	prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods := r.collectAndBucketPods(routingCtx, readyPods, promptLength)
	combinedAvailable := bucketing && len(combinedPods) > 0
	if len(prefillPods) == 0 && !combinedAvailable {
		return nil, nil, fmt.Errorf("prefill pods are not ready: prefill=%d, decode=%d", len(prefillPods), len(decodePods))
	}
	if len(decodePods) == 0 && !combinedAvailable {
		return nil, nil, fmt.Errorf("decode pods are not ready: prefill=%d, decode=%d", len(prefillPods), len(decodePods))
	}

	if bucketing {
		if len(promptLengthBucketingPrefillPods) == 0 || len(promptLengthBucketingDecodePods) == 0 {
			// No bucket matches the request's prompt length.
			if combinedAvailable {
				// Combined pods cover any prompt length; pick one at random.
				klog.InfoS("no bucket matches prompt length, routing to combined pod",
					"requestId", routingCtx.RequestID, "promptLength", promptLength, "combinedPods", len(combinedPods),
					"bucketPrefillPods", len(promptLengthBucketingPrefillPods), "bucketDecodePods", len(promptLengthBucketingDecodePods))
				combinedPod := combinedPods[rand.IntN(len(combinedPods))]
				r.pendingDecodeTracker.AddPendingDecode(routingCtx.TrackerKey(), utils.GeneratePodKey(combinedPod.Namespace, combinedPod.Name))
				return nil, combinedPod, nil
			}
			// Do not fall back to unfiltered PD pods: each pod group (storm) is sized for a
			// specific prompt-length range and cross-bucket routing produces incorrect results.
			return nil, nil, fmt.Errorf("no prompt-length bucket matches prompt length %d and no combined pods available", promptLength)
		}
	}

	prefillPol, decodePol, err := r.effectiveScorePolicies(routingCtx)
	if err != nil {
		return nil, nil, err
	}
	if decodePol == nil {
		decodePol = loadBalancingDecodePolicy
	}

	// Precompute prefill pod keys once before taking selectStripe.Lock() so
	// Prepare, tracker lookups, imbalance resolution, scoring, and registration
	// do not repeatedly format namespace/name keys.
	prefillPodKeys, prefillPodsByKey, prefillReadyMap, prefillKeySlice := buildPrefillPodKeyIndex(prefillPods)

	prefillScorer, err := r.preparePrefillScorerWithMap(routingCtx, prefillPods, prefillReadyMap, prefillPol)
	if err != nil {
		return nil, nil, fmt.Errorf("prefill scorer preparation failed: %w", err)
	}

	// The adaptive bucket-serve plan, computed before the lock like the policy
	// Prepare step: it reads nothing the selection lock protects and takes the
	// tracker lock instead. Step 6 below applies it.
	bandRoleset, bandMax := "", 0
	if bucketing {
		bandRoleset, bandMax = r.bucketServeBand(routingCtx, readyPods, promptLength)
	}

	if bucketing {
		// Bucket match exists; check if load imbalance favours a combined pod instead.
		// shouldPickCombined and scoreCombinedPods read only cached metrics (not tracker
		// state), and combined pod keys are disjoint from decodePods (matching step 3a),
		// so running this check and AddPendingDecode before the Redis read and selection
		// lock avoids both an unused Redis round trip and lock contention on combined spillover.
		if r.shouldPickCombined(routingCtx, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods) {
			combinedPod := r.scoreCombinedPods(routingCtx, combinedPods)
			if combinedPod != nil {
				klog.InfoS("load imbalance detected, selecting combined pod", "requestId", routingCtx.RequestID, "selectedCombinedPod", combinedPod.Name)
				r.pendingDecodeTracker.AddPendingDecode(routingCtx.TrackerKey(), utils.GeneratePodKey(combinedPod.Namespace, combinedPod.Name))
				return nil, combinedPod, nil
			}
		}
	}

	// Prefetch cross-gateway running-request counts (and shared decode ledger
	// when token_load uses it), read-only scraped decode metrics, and prompt
	// token estimates before acquiring the model's selection lock so neither
	// Redis I/O nor read-only cache/body work serializes concurrent selections.
	usesDecodeTokenLoad := pd.UsesDecodeTokenLoad(decodePol)
	var remoteDecodeLedger map[string]cache.RemoteDecodeLoad
	var remoteOut *map[string]cache.RemoteDecodeLoad
	if r.sharedDecodeLedger != nil && usesDecodeTokenLoad {
		remoteOut = &remoteDecodeLedger
	}
	runningReqCounts, runningErr := r.decodeRunningRequests(routingCtx, decodePods, remoteOut)
	decodeSnapshots := r.prefetchDecodeMetrics(routingCtx, decodePods, usesDecodeTokenLoad, true)

	promptTokens := 0
	if r.tokenLoadTracker != nil && (pd.UsesTokenLoad(prefillPol) || usesDecodeTokenLoad) {
		promptTokens = pd.EstimatePromptTokens(routingCtx.ReqBody)
	}

	selectedPrefill, selectedDecode, prefixHashes, selectedPrefillKey, selectedDecodeKey, bandApplied, err := r.selectAndRegisterPDPairUnderLock(
		routingCtx, pdOverrides,
		prefillPods, decodePods,
		prefillPodKeys, prefillPodsByKey, prefillKeySlice, decodeSnapshots,
		runningReqCounts, runningErr, remoteDecodeLedger,
		prefillPol, decodePol, prefillScorer,
		bandRoleset, bandMax, promptTokens,
	)
	if bandApplied {
		bandLabels := map[string]string{"roleset": bandRoleset}
		r.emitBucketServeMetric(routingCtx, metrics.PDBucketServeBandTotal, 1.0, bandLabels, true)
		r.emitBucketServeMetric(routingCtx, metrics.PDBucketServePromptTokensTotal, float64(promptLength), bandLabels, true)
	}
	if err != nil {
		return nil, nil, err
	}

	// Post-selection bookkeeping (prefix credit map, selection counters, Prometheus)
	// does not depend on selectStripe.Lock(); running it after Unlock keeps the
	// per-model critical section minimal.
	r.recordPDSelection(routingCtx, prefixHashes, selectedPrefill, selectedPrefillKey, selectedDecode, selectedDecodeKey)
	return selectedPrefill, selectedDecode, nil
}

func buildPrefillPodKeyIndex(prefillPods []*v1.Pod) (map[*v1.Pod]string, map[string]*v1.Pod, map[string]struct{}, []string) {
	podKeys := make(map[*v1.Pod]string, len(prefillPods))
	podsByKey := make(map[string]*v1.Pod, len(prefillPods))
	readyMap := make(map[string]struct{}, len(prefillPods))
	keySlice := make([]string, len(prefillPods))
	for i, pod := range prefillPods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		podKeys[pod] = podKey
		podsByKey[podKey] = pod
		readyMap[podKey] = struct{}{}
		keySlice[i] = podKey
	}
	return podKeys, podsByKey, readyMap, keySlice
}

// selectAndRegisterPDPairUnderLock executes the per-model tracker-dependent
// critical section under selectStripe.Lock(): imbalance fast paths, bucket-serve
// alignment, prefill/decode scoring, final pairing, and atomic tracker charges.
// No r.cache lookups, pod-key formatting, body scans, or Prometheus selection
// emissions run inside this lock.
func (r *pdRouter) selectAndRegisterPDPairUnderLock(
	routingCtx *types.RoutingContext,
	pdOverrides *types.PDOverrides,
	prefillPods, decodePods []*v1.Pod,
	prefillPodKeys map[*v1.Pod]string,
	prefillPodsByKey map[string]*v1.Pod,
	prefillKeySlice []string,
	decodeSnapshots map[*v1.Pod]decodePodSnapshot,
	runningReqCounts map[string]int64,
	runningErr error,
	remoteDecodeLedger map[string]cache.RemoteDecodeLoad,
	prefillPol pd.PrefillScorePolicy,
	decodePol pd.DecodeScorePolicy,
	prefillScorer pd.PrefillScorer,
	bandRoleset string,
	bandMax, promptTokens int,
) (*v1.Pod, *v1.Pod, []uint64, string, string, bool, error) {
	selectMu := r.selectLock(selectLockKey(routingCtx))
	selectMu.Lock()
	defer selectMu.Unlock()

	prefillCounts := r.prefillRequestTracker.GetPrefillRequestCountsForKeys(prefillKeySlice)

	// check for prefill and decode imbalance
	targetPod, isImbalanced := r.loadImbalanceSelectPrefillPodWithKeys(prefillPods, prefillPodsByKey,
		prefillCounts,
		pdOverrides.Spreads.PrefillLoadImbalanceMinSpread)
	if isImbalanced && targetPod != nil {
		prefillPods = []*v1.Pod{targetPod}
		decodePods = utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier])
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("load imbalance detected, selecting least-loaded prefill pod",
				"request_id", routingCtx.RequestID, "selected_prefill_pod", targetPod.Name,
				"roleset", targetPod.Labels[PDRoleSetIdentifier], "decode_pods_after_align", pdPodNames(decodePods),
			)
		}
	}

	targetPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage := r.loadImbalanceSelectDecodePodFromSnapshots(routingCtx, decodePods, decodeSnapshots, runningReqCounts, runningErr)
	targetPod = decodeFastPathPick(routingCtx, targetPod, decodePol)
	if targetPod != nil {
		decodePods = []*v1.Pod{targetPod}
		if aligned := utils.FilterPodsByLabel(prefillPods, PDRoleSetIdentifier, targetPod.Labels[PDRoleSetIdentifier]); len(aligned) > 0 {
			prefillPods = aligned
		}
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("load imbalance detected in decode pods",
				"request_id", routingCtx.RequestID, "selected_decode_pod", targetPod.Name,
				"roleset", targetPod.Labels[PDRoleSetIdentifier], "prefill_pods_after_align", pdPodNames(prefillPods),
			)
		}
	}

	bandApplied := false
	if bandRoleset != "" {
		alignedPrefill := utils.FilterPodsByLabel(prefillPods, PDRoleSetIdentifier, bandRoleset)
		alignedDecode := utils.FilterPodsByLabel(decodePods, PDRoleSetIdentifier, bandRoleset)
		if len(alignedPrefill) > 0 && len(alignedDecode) > 0 {
			prefillPods, decodePods = alignedPrefill, alignedDecode
			bandApplied = true
			if klog.V(4).Enabled() {
				klog.V(4).InfoS("bucket-serve band picked the roleset",
					"request_id", routingCtx.RequestID, "roleset", bandRoleset, "band_max", bandMax,
					"prefill_pods_after_band", pdPodNames(prefillPods))
			}
		}
	}

	prefillScores, maxPrefillScore, prefixHashes := r.scorePreparedPrefillPodsWithKeys(routingCtx, prefillPods, prefillPodKeys, prefillCounts, prefillScorer)
	decodeRun := r.scoreDecodePodsFromSnapshots(routingCtx, decodePods, decodeSnapshots, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage, decodePol, remoteDecodeLedger)
	selectedPrefill, selectedDecode, err := r.selectFinalPDPair(routingCtx, prefillScores, maxPrefillScore, decodeRun)
	if err != nil {
		return nil, nil, nil, "", "", bandApplied, err
	}

	// Register while still holding the model's selection lock so the next selection counts this one.
	selectedPrefillKey := prefillPodKeys[selectedPrefill]
	if selectedPrefillKey == "" {
		selectedPrefillKey = utils.GeneratePodKey(selectedPrefill.Namespace, selectedPrefill.Name)
	}
	selectedDecodeKey := decodeSnapshots[selectedDecode].podKey
	if selectedDecodeKey == "" {
		selectedDecodeKey = utils.GeneratePodKey(selectedDecode.Namespace, selectedDecode.Name)
	}
	r.pendingDecodeTracker.AddPendingDecode(routingCtx.TrackerKey(), selectedDecodeKey)
	r.prefillRequestTracker.AddPrefillRequest(routingCtx.TrackerKey(), selectedPrefillKey)
	r.chargeTokenLoadWithKeyAndTokens(routingCtx, selectedPrefill, selectedPrefillKey, prefillPol, prefillScorer, promptTokens)
	r.chargeDecodeTokenLoadWithKeyAndTokens(routingCtx, selectedDecode, selectedDecodeKey, decodePol, promptTokens)
	return selectedPrefill, selectedDecode, prefixHashes, selectedPrefillKey, selectedDecodeKey, bandApplied, nil
}

// decodeFastPathPick returns the decode load-imbalance fast path's pick, or nil
// when the decode policy must score the whole set instead. The fast path picks
// by request count, throughput or drain rate. Those are the signals token_load
// replaces: a pod holding one long prompt has the fewest requests and the most
// KV. The fast path still runs under token_load, to fill the metric maps.
func decodeFastPathPick(routingCtx *types.RoutingContext, targetPod *v1.Pod, decodePol pd.DecodeScorePolicy) *v1.Pod {
	if targetPod == nil || !pd.UsesDecodeTokenLoad(decodePol) {
		return targetPod
	}
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("decode load imbalance fast path skipped under token_load",
			"request_id", routingCtx.RequestID, "fast_path_decode_pod", targetPod.Name)
	}
	return nil
}

// loadImbalanceSelectPrefillPod is a fast path that runs before scorePrefillPods when
// outstanding prefill load is visibly skewed across readyPods.
//
// podRequestCount holds per-pod active prefill counts from PrefillRequestTracker for
// in-flight prefill HTTP calls from other concurrent requests, keyed by pod key. The current request is
// not counted yet — filterPrefillDecodePods registers it after selection completes,
// under the same selection lock hold.
// Ties among equally-loaded pods are broken by drawing uniformly at random from the tied
// pod keys; readyPods is only used to resolve that key back to a pod, so its order does
// not influence the result.
//
// Returns one pod tied for the minimum count and imbalance=true when
// max(count) − min(count) > minSpread (strictly greater than). Otherwise returns
// nil, false. minSpread is AIBRIX_PREFILL_LOAD_IMBALANCE_MIN_SPREAD unless the
// request's profile overrides it. An empty podRequestCount map always returns
// nil, false.
//
// The caller (filterPrefillDecodePods) narrows decodePods to the selected pod's roleset
// so that prefill and decode remain aligned to the same roleset pair after this step.
func (r *pdRouter) loadImbalanceSelectPrefillPod(readyPods []*v1.Pod, podRequestCount map[string]int32, minSpread int32) (*v1.Pod, bool) {
	return r.loadImbalanceSelectPrefillPodWithKeys(readyPods, nil, podRequestCount, minSpread)
}

func (r *pdRouter) loadImbalanceSelectPrefillPodWithKeys(readyPods []*v1.Pod, podsByKey map[string]*v1.Pod, podRequestCount map[string]int32, minSpread int32) (*v1.Pod, bool) {
	var imbalance bool
	var targetPod *v1.Pod
	var targetPods []string
	minValue := int32(math.MaxInt32)
	maxValue := int32(math.MinInt32)

	if len(podRequestCount) == 0 {
		return targetPod, imbalance
	}

	for _, value := range podRequestCount {
		if value < minValue {
			minValue = value
		}
		if value > maxValue {
			maxValue = value
		}
	}
	if maxValue-minValue <= minSpread {
		return nil, false
	}
	for podKey, value := range podRequestCount {
		if minValue == value {
			targetPods = append(targetPods, podKey)
		}
	}

	if len(targetPods) > 0 {
		chosenKey := targetPods[rand.IntN(len(targetPods))]
		if podsByKey != nil {
			targetPod = podsByKey[chosenKey]
		} else {
			targetPod = routeutil.PodsByKey(readyPods)[chosenKey]
		}
		imbalance = true
		if targetPod != nil && klog.V(4).Enabled() {
			klog.V(4).InfoS("prefill request imbalance detected, selecting least-loaded pod",
				"min_request_count", minValue, "max_request_count", maxValue,
				"selected_prefill_pod", targetPod.Name, "roleset", targetPod.Labels[PDRoleSetIdentifier],
				"candidate_pods", strings.Join(targetPods, ","),
			)
		}
	}

	return targetPod, imbalance
}

// loadImbalanceSelectDecodePod is a fast path that runs before scoreDecodePods when decode
// load is visibly skewed. It walks filteredDecodePods once, filling podRequestCounts,
// podThroughputs, and podFreeGpuUsage for the scoring pass, then applies three ordered checks
// (each runs only if the previous did not return):
//
//  1. Request imbalance: among pods that report RealtimeNumRequestsRunning, compute
//     effective count = running + PendingDecodeTracker pending count for that pod.
//     Pending counts come from concurrent Route calls that have registered
//     AddPendingDecode but not yet returned. If max − min effective count is at least
//     loadMinSpread (AIBRIX_DECODE_LOAD_IMBALANCE_MIN_SPREAD unless the profile
//     overrides it), return the least-loaded metric-bearing pod.
//     Pods without a running-request metric are excluded from the spread (their pending
//     count is still stored in podRequestCounts for scoring) so freshly restarted pods
//     are not treated as idle and given a thundering herd.
//
//  2. Throughput spread: among pods that report AvgGenerationThroughputToksPerS (per model),
//     if max − min throughput exceeds throughputMinSpread, return the
//     lowest-throughput pod. Pods missing the metric are excluded from this check.
//
//  3. Drain-rate scoring: if every pod has a positive RealtimeRunningRequestsDrainRate1m,
//     score each pod as effectiveRequestCount / drainRate. If maxScore/minScore exceeds
//     scoreRatioThreshold, return the pod with the lowest score. Skipped when
//     any drain rate is missing or non-positive.
//
// Returns nil when none of the checks fire; the caller falls through to scoreDecodePods with
// the collected maps. A non-nil pod return also carries maxRequestCount, maxThroughput, and
// maxFreeGPUUsage from the same pass (maxima include all pods, including those without metrics).
// maxRequestCount and maxThroughput are floored at 1 and maxFreeGPUUsage is floored at 1 so
// that scoreDecodePods normalization denominators are always positive. The caller narrows
// prefillPods to the selected pod's roleset. KV cache headroom uses KVCacheUsagePerc
// (missing metric is treated as 0% usage = 100% free).
func (r *pdRouter) loadImbalanceSelectDecodePod(ctx *types.RoutingContext, filteredDecodePods []*v1.Pod) (*v1.Pod, float64, float64, float64, map[string]float64, map[string]float64, map[string]float64) {
	runningReqCounts, runningErr := r.decodeRunningRequests(ctx, filteredDecodePods, nil)
	return r.loadImbalanceSelectDecodePodFromCounts(ctx, filteredDecodePods, runningReqCounts, runningErr)
}

// decodePodSnapshot holds read-only per-decode-pod keys and scraped metrics
// prefetched before acquiring selectStripe.Lock(), so imbalance checks and
// decode scoring inside the per-model critical section never touch r.cache or
// recompute utils.GeneratePodKey.
type decodePodSnapshot struct {
	podKey        string
	throughput    float64
	hasThroughput bool
	freeGPU       float64
	drainRate     float64
	hasDrainRate  bool
	metricsReady  bool
	kvCapacity    float64
	hasKVCapacity bool
}

// prefetchDecodeMetrics collects read-only scraped metrics and pod keys for
// decodePods outside selectStripe.Lock(). Drain-rate lookups short-circuit per
// roleset as soon as one pod in that roleset lacks a positive drain rate: since
// imbalance alignment only ever narrows decodePods by whole roleset, any pod
// missing a positive drain rate disqualifies its entire roleset (and the full
// fleet) from the drain-rate fast path.
func (r *pdRouter) prefetchDecodeMetrics(ctx *types.RoutingContext, decodePods []*v1.Pod, usesDecodeTokenLoad, fetchReadiness bool) map[*v1.Pod]decodePodSnapshot {
	snaps := make(map[*v1.Pod]decodePodSnapshot, len(decodePods))
	var rolesetMissingDrain map[string]struct{}
	fetchCapacity := usesDecodeTokenLoad && aibrixDecodeTokenLoadCapacityAware && r.cache != nil
	for _, pod := range decodePods {
		if pod == nil {
			continue
		}
		snap := decodePodSnapshot{
			podKey:       utils.GeneratePodKey(pod.Namespace, pod.Name),
			freeGPU:      100,
			metricsReady: usesDecodeTokenLoad,
		}
		if r.cache != nil {
			// A NaN or infinite gauge is treated like a missing one: either would make
			// the shared maxima in decode scoring non-finite and every decode score NaN.
			if tokenThroughput, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.AvgGenerationThroughputToksPerS); err == nil && tokenThroughput != nil && isFinite(tokenThroughput.GetSimpleValue()) {
				snap.throughput = tokenThroughput.GetSimpleValue()
				snap.hasThroughput = true
			}
			kvUsageVal := float64(0)
			if kvUsage, err := r.cache.GetMetricValueByPodModel(pod.Name, pod.Namespace, ctx.Model, metrics.KVCacheUsagePerc); err == nil && kvUsage != nil && isFinite(kvUsage.GetSimpleValue()) {
				kvUsageVal = kvUsage.GetSimpleValue()
			}
			freeGPU := math.Round(100 - kvUsageVal*100)
			if freeGPU <= 0 {
				freeGPU = 0.1
			}
			snap.freeGPU = freeGPU

			roleset := pod.Labels[PDRoleSetIdentifier]
			if _, missing := rolesetMissingDrain[roleset]; !missing {
				if drainRate, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeRunningRequestsDrainRate1m); err == nil && drainRate != nil && drainRate.GetSimpleValue() > 0 {
					snap.drainRate = drainRate.GetSimpleValue()
					snap.hasDrainRate = true
				} else {
					if rolesetMissingDrain == nil {
						rolesetMissingDrain = make(map[string]struct{})
					}
					rolesetMissingDrain[roleset] = struct{}{}
				}
			}

			if fetchReadiness && !usesDecodeTokenLoad {
				snap.metricsReady = r.decodePodMetricsReady(ctx, pod)
			}
			if fetchCapacity {
				snap.kvCapacity, snap.hasKVCapacity = r.podKVCapacity(pod)
			}
		}
		snaps[pod] = snap
	}
	return snaps
}

// loadImbalanceSelectDecodePodFromCounts runs loadImbalanceSelectDecodePod using
// pre-fetched running-request counts (which may cover a superset of
// filteredDecodePods, e.g. from RoutingContext.RunningRequests or before
// prefill imbalance narrowed decodePods; entries are only looked up by pod key
// and never mutated).
func (r *pdRouter) loadImbalanceSelectDecodePodFromCounts(ctx *types.RoutingContext, filteredDecodePods []*v1.Pod, runningReqCounts map[string]int64, runningErr error) (*v1.Pod, float64, float64, float64, map[string]float64, map[string]float64, map[string]float64) {
	snaps := r.prefetchDecodeMetrics(ctx, filteredDecodePods, false, false)
	return r.loadImbalanceSelectDecodePodFromSnapshots(ctx, filteredDecodePods, snaps, runningReqCounts, runningErr)
}

func (r *pdRouter) loadImbalanceSelectDecodePodFromSnapshots(ctx *types.RoutingContext, filteredDecodePods []*v1.Pod, snaps map[*v1.Pod]decodePodSnapshot, runningReqCounts map[string]int64, runningErr error) (*v1.Pod, float64, float64, float64, map[string]float64, map[string]float64, map[string]float64) {
	spreads := ctx.PDOverrides().Spreads
	loadMinSpread := spreads.DecodeLoadImbalanceMinSpread
	throughputMinSpread := spreads.DecodeThroughputImbalanceMinSpread
	scoreRatioThreshold := spreads.DecodeScoreRatioThreshold

	podRequestCounts := make(map[string]float64, len(filteredDecodePods))
	podThroughputs := make(map[string]float64, len(filteredDecodePods))
	podFreeGpuUsage := make(map[string]float64, len(filteredDecodePods))

	var minRequestPod *v1.Pod
	var minRequestPodKey string
	maxRequestCount := float64(1)
	var minThroughputPod *v1.Pod
	var minThroughputPodKey string
	maxThroughput := float64(1)
	maxFreeGPUUsage := float64(1)
	maxObservedRequestCount := float64(0)
	minObservedRequestCount := math.MaxFloat64
	maxObservedThroughput := float64(0)
	minObservedThroughput := math.MaxFloat64
	utils.Shuffle(filteredDecodePods)

	for _, pod := range filteredDecodePods {
		snap := snaps[pod]
		podKey := snap.podKey
		requestCount := r.pendingDecodeTracker.GetPendingDecodeCount(podKey)
		if runningErr != nil {
			podRequestCounts[podKey] = requestCount
		} else {
			requestCount += float64(runningReqCounts[podKey])
			podRequestCounts[podKey] = requestCount
			if requestCount < minObservedRequestCount {
				minObservedRequestCount = requestCount
				minRequestPod = pod
				minRequestPodKey = podKey
			}
			maxObservedRequestCount = math.Max(maxObservedRequestCount, requestCount)
		}
		maxRequestCount = math.Max(maxRequestCount, podRequestCounts[podKey])

		if !snap.hasThroughput {
			podThroughputs[podKey] = 0
		} else {
			throughput := snap.throughput
			podThroughputs[podKey] = throughput
			if throughput < minObservedThroughput {
				minObservedThroughput = throughput
				minThroughputPod = pod
				minThroughputPodKey = podKey
			}
			maxObservedThroughput = math.Max(maxObservedThroughput, throughput)
			maxThroughput = math.Max(maxThroughput, throughput)
		}

		podFreeGpuUsage[podKey] = snap.freeGPU
		maxFreeGPUUsage = math.Max(maxFreeGPUUsage, snap.freeGPU)
	}

	if minRequestPod != nil && maxObservedRequestCount-minObservedRequestCount >= loadMinSpread {
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("request imbalance at decode pods", "request_id", ctx.RequestID,
				"min_request_count", minObservedRequestCount, "max_request_count", maxObservedRequestCount,
				"free_gpu_percent", podFreeGpuUsage[minRequestPodKey], "decode_pod", minRequestPod.Name)
		}
		return minRequestPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	if minThroughputPod != nil && maxObservedThroughput-minObservedThroughput > throughputMinSpread {
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("throughput imbalance at decode pods", "request_id", ctx.RequestID,
				"min_request_count", minObservedRequestCount, "max_request_count", maxObservedRequestCount,
				"min_throughput", minObservedThroughput, "max_throughput", maxObservedThroughput,
				"free_gpu_percent", podFreeGpuUsage[minThroughputPodKey], "decode_pod", minThroughputPod.Name)
		}
		return minThroughputPod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	var minScorePod *v1.Pod
	minScore := math.MaxFloat64
	maxScore := float64(0)
	drainRatesAvailable := true

	for _, pod := range filteredDecodePods {
		snap := snaps[pod]
		if !snap.hasDrainRate {
			drainRatesAvailable = false
			break
		}
		score := podRequestCounts[snap.podKey] / math.Max(snap.drainRate, defaultDrainRateEpsilon)
		if score < minScore {
			minScore = score
			minScorePod = pod
		}
		maxScore = math.Max(maxScore, score)
	}

	if drainRatesAvailable && minScore > 0 && maxScore/minScore > scoreRatioThreshold {
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("drain rate imbalance at decode pods", "request_id", ctx.RequestID,
				"min_score", minScore, "max_score", maxScore,
				"ratio", maxScore/minScore, "decode_pod", minScorePod.Name)
		}
		return minScorePod, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
	}

	return nil, maxRequestCount, maxThroughput, maxFreeGPUUsage, podRequestCounts, podThroughputs, podFreeGpuUsage
}

// scorePrefillPods scores candidate prefill pods with prefillPolicy after any imbalance
// fast path has narrowed the list. prefillPods are shuffled before scoring so ties
// within a roleset are broken randomly.
//
// Active prefill counts come from PrefillRequestTracker (in-flight prefill HTTP from
// other concurrent requests; the current request is not counted yet). Pods whose count
// exceeds mean + standardDeviationFactor×stddev
// (AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR) or the bounded-load cap
// (see routeutil.LoadCap) are skipped as overloaded. When
// every pod in a roleset is filtered this way, that roleset has no entry in the
// returned map and finalPDScore will not be able to select it.
//
// Returns a per-roleset score map, maxPrefillScore (the maximum score among all pods
// scored in this pass, used by finalPDScore for normalization), and prefix hashes
// from the policy's Prepare step for asynchronous prefix-cache updates.
// Returns nil, 0, nil when Prepare fails (distinct from an empty map, which means
// all pods were filtered by the stddev check).
//
// Policy resolution: the policy argument, then r.prefillPolicy, then prefix_cache.
//
// scorePrefillPods is preparePrefillScorer followed by scorePreparedPrefillPods;
// filterPrefillDecodePods calls the two halves separately so that Prepare runs
// before the selection lock is taken and only the tracker-dependent half runs under it.
func (r *pdRouter) scorePrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, prefillPolicy pd.PrefillScorePolicy) (map[string]*Scores, float64, []uint64) {
	scorer, err := r.preparePrefillScorer(routingCtx, prefillPods, prefillPolicy)
	if err != nil {
		return nil, 0, nil
	}
	return r.scorePreparedPrefillPods(routingCtx, prefillPods, scorer)
}

// preparePrefillScorer resolves the prefill policy (the argument, then
// r.prefillPolicy, then prefix_cache) and runs its Prepare step: tokenization
// and prefix-cache matching over prefillPods. Prepare depends only on the
// request and the prefix index, not on tracker state, so callers may run it
// before taking the selection lock. prefillPods may later be narrowed by the imbalance
// fast paths; a scorer prepared over the wider set scores any subset of it.
func (r *pdRouter) preparePrefillScorer(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, prefillPolicy pd.PrefillScorePolicy) (pd.PrefillScorer, error) {
	readyPodsMap := make(map[string]struct{}, len(prefillPods))
	for _, pod := range prefillPods {
		readyPodsMap[utils.GeneratePodKey(pod.Namespace, pod.Name)] = struct{}{}
	}
	return r.preparePrefillScorerWithMap(routingCtx, prefillPods, readyPodsMap, prefillPolicy)
}

func (r *pdRouter) preparePrefillScorerWithMap(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, readyPodsMap map[string]struct{}, prefillPolicy pd.PrefillScorePolicy) (pd.PrefillScorer, error) {
	policy := prefillPolicy
	if policy == nil {
		policy = r.prefillPolicy
	}
	if policy == nil {
		tbl := r.prefixCacheIndexer
		if tbl == nil {
			tbl = prefixcacheindexer.GetSharedPrefixHashTable()
		}
		policy = newPrefixCachePrefillPolicy(tbl)
	}
	scorer, err := policy.Prepare(routingCtx, prefillPods, readyPodsMap)
	if err != nil {
		klog.ErrorS(err, "prefill scorer preparation failed",
			"request_id", routingCtx.RequestID, "policy", policy.Name(), "model", routingCtx.Model)
		return nil, err
	}
	return scorer, nil
}

// scorePreparedPrefillPods is the tracker-dependent half of scorePrefillPods:
// it reads the current prefill request counts and scores prefillPods with an
// already-prepared scorer. filterPrefillDecodePods calls scorePreparedPrefillPodsWithKeys
// under the selection lock.
func (r *pdRouter) scorePreparedPrefillPods(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, scorer pd.PrefillScorer) (map[string]*Scores, float64, []uint64) {
	return r.scorePreparedPrefillPodsWithKeys(routingCtx, prefillPods, nil, nil, scorer)
}

func (r *pdRouter) scorePreparedPrefillPodsWithKeys(routingCtx *types.RoutingContext, prefillPods []*v1.Pod, podKeys map[*v1.Pod]string, precomputedCounts map[string]int32, scorer pd.PrefillScorer) (map[string]*Scores, float64, []uint64) {
	utils.Shuffle(prefillPods)
	podRequestCount := precomputedCounts
	if podRequestCount == nil {
		podRequestCount = r.prefillRequestTracker.GetPrefillRequestCountsForPods(prefillPods)
	}

	var maxRequestCount float64 = 1
	requestCounts := make([]float64, 0, len(prefillPods))
	for _, pod := range prefillPods {
		podKey := podKeys[pod]
		if podKey == "" {
			podKey = utils.GeneratePodKey(pod.Namespace, pod.Name)
		}
		cf := float64(podRequestCount[podKey])
		requestCounts = append(requestCounts, cf)
		if cf > maxRequestCount {
			maxRequestCount = cf
		}
	}
	meanRequestCount := routeutil.Mean(requestCounts)
	stdDevRequestCount := routeutil.StandardDeviation(requestCounts)
	// The prefill candidacy filter shares AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR with
	// the prefix-cache strategies; the request's resolved overrides carry it.
	sigma := routingCtx.RoutingOverrides().PrefixCache.StandardDeviationFactor
	loadCap := routeutil.LoadCap(meanRequestCount, aibrixPrefillLoadCapFactor)

	keyedScorer, hasKeyedScorer := scorer.(pd.KeyedPrefillScorer)
	prefillScores := map[string]*Scores{}
	maxPrefillScore := float64(1)
	for i, pod := range prefillPods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		reqCnt := requestCounts[i]
		if reqCnt > meanRequestCount+float64(sigma)*stdDevRequestCount {
			if klog.V(4).Enabled() {
				klog.V(4).InfoS("prefill pod request count is higher than mean request count, skipping",
					"request_id", routingCtx.RequestID, "pod_name", pod.Name,
					"req_cnt", reqCnt, "mean_req_cnt", meanRequestCount, "std_dev_req_cnt", stdDevRequestCount)
			}
			continue
		}
		if reqCnt > loadCap {
			if klog.V(4).Enabled() {
				klog.V(4).InfoS("prefill pod request count is above the bounded-load cap, skipping",
					"request_id", routingCtx.RequestID, "pod_name", pod.Name,
					"req_cnt", reqCnt, "mean_req_cnt", meanRequestCount, "load_cap", loadCap)
			}
			continue
		}

		var score float64
		if hasKeyedScorer {
			podKey := podKeys[pod]
			if podKey == "" {
				podKey = utils.GeneratePodKey(pod.Namespace, pod.Name)
			}
			score = keyedScorer.ScorePodByKey(pod, podKey, reqCnt, maxRequestCount)
		} else {
			score = scorer.ScorePod(pod, reqCnt, maxRequestCount)
		}
		if existing, exists := prefillScores[rolesetName]; !exists || score < existing.Score {
			prefillScores[rolesetName] = &Scores{Pod: pod, Score: score}
		}
		if score > maxPrefillScore {
			maxPrefillScore = score
		}
	}

	return prefillScores, maxPrefillScore, scorer.PrefixHashes()
}

// scoreDecodePods scores candidate decode pods with policy after any imbalance fast path
// has narrowed the list. filteredDecodePods are shuffled before scoring.
//
// Metric inputs (podRequestCounts, podThroughputs, podFreeGpuUsage and their batch
// maxima) are normally populated by loadImbalanceSelectDecodePod; when that returns
// nil, the caller still passes the maps built during that pass.
//
// Policy resolution: the policy argument, then r.decodePolicy, then load_balancing.
// When at least one pod reports RealtimeNumRequestsRunning, pods missing that metric
// receive a cold-start score (1.0 + PendingDecodeTracker pending count) instead of
// full policy scoring, except under token_load, which scores every pod from the
// gateway's decode ledger. podRequestCounts include pending decode from concurrent Route
// calls that have registered AddPendingDecode. If no pod has the running-request metric,
// all pods are scored normally.
//
// Lower score is better. The lowest-scoring pod per roleset is kept in DecodeScoreRun.
// PerRoleset. Invalid (NaN) scores from the primary policy are retried once with
// load_balancing; FallbackUsed is set when that happens. Pods still invalid after
// fallback are skipped. MaxScore is the maximum score among pods scored in this pass
// (minimum 0.01), used by finalPDScore for normalization.
func (r *pdRouter) scoreDecodePods(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64,
	policy pd.DecodeScorePolicy) pd.DecodeScoreRun {
	return r.scoreDecodePodsWithLedger(routingCtx, filteredDecodePods, maxRequestCount, maxThroughput, maxFreeGPUUsage,
		podRequestCounts, podThroughputs, podFreeGpuUsage, policy, nil)
}

// scoreDecodePodsWithLedger is scoreDecodePods with the decode ledger the other
// gateway replicas hold on each pod (nil when it is not shared): token_load adds
// it to this replica's own.
func (r *pdRouter) scoreDecodePodsWithLedger(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64,
	policy pd.DecodeScorePolicy, remoteLedger map[string]cache.RemoteDecodeLoad) pd.DecodeScoreRun {
	return r.scoreDecodePodsFromSnapshots(routingCtx, filteredDecodePods, nil,
		maxRequestCount, maxThroughput, maxFreeGPUUsage,
		podRequestCounts, podThroughputs, podFreeGpuUsage, policy, remoteLedger)
}

func (r *pdRouter) scoreDecodePodsFromSnapshots(routingCtx *types.RoutingContext, filteredDecodePods []*v1.Pod,
	snaps map[*v1.Pod]decodePodSnapshot,
	maxRequestCount float64, maxThroughput float64, maxFreeGPUUsage float64,
	podRequestCounts map[string]float64, podThroughputs map[string]float64, podFreeGpuUsage map[string]float64,
	policy pd.DecodeScorePolicy, remoteLedger map[string]cache.RemoteDecodeLoad) pd.DecodeScoreRun {
	if policy == nil {
		policy = r.decodePolicy
	}
	if policy == nil {
		policy = loadBalancingDecodePolicy
	}
	policyName := policy.Name()

	out := pd.DecodeScoreRun{
		PerRoleset: make(map[string]pd.RolesetDecodePick),
		MaxScore:   0.01,
		Policy:     policyName,
	}
	if len(filteredDecodePods) == 0 {
		return out
	}

	utils.Shuffle(filteredDecodePods)

	// token_load scores from the gateway's own ledger, which is exact for every
	// pod from its first request, so the cold-start score below (built for
	// policies that read scraped engine metrics, and in their units) does not
	// apply to it.
	usesDecodeTokenLoad := pd.UsesDecodeTokenLoad(policy)
	decodeRates := decodeTokenLoadRates(usesDecodeTokenLoad, podThroughputs, podRequestCounts)
	var decodeCapacities map[string]float64
	if snaps != nil {
		decodeCapacities = decodeKVCapacitiesFromSnapshots(usesDecodeTokenLoad, filteredDecodePods, snaps)
	} else {
		decodeCapacities = r.decodeKVCapacities(usesDecodeTokenLoad, filteredDecodePods)
	}
	anyMetricsReady := false
	var metricsReadyByPod map[string]bool
	if snaps != nil {
		for _, pod := range filteredDecodePods {
			if snaps[pod].metricsReady {
				anyMetricsReady = true
				break
			}
		}
	} else {
		metricsReadyByPod = make(map[string]bool, len(filteredDecodePods))
		for _, pod := range filteredDecodePods {
			ready := usesDecodeTokenLoad || r.decodePodMetricsReady(routingCtx, pod)
			metricsReadyByPod[utils.GeneratePodKey(pod.Namespace, pod.Name)] = ready
			if ready {
				anyMetricsReady = true
			}
		}
	}

	var conductorMedianTP float64
	if policyName == pd.DecodePolicyConductor {
		if snaps != nil {
			conductorMedianTP = conductorFleetMedianThroughputFromSnapshots(
				filteredDecodePods, snaps, anyMetricsReady, podThroughputs)
		} else {
			conductorMedianTP = conductorFleetMedianThroughput(
				filteredDecodePods, metricsReadyByPod, anyMetricsReady, podThroughputs)
		}
	}

	// The per-pod score strings exist only for the decode_score_summary log,
	// so build them only when that log is going to be written; this runs
	// under the selection lock for every request.
	verbose := klog.V(4).Enabled()
	var scoredPods, skippedPods []string
	if verbose {
		scoredPods = make([]string, 0, len(filteredDecodePods))
		skippedPods = make([]string, 0, len(filteredDecodePods))
	}

	for _, pod := range filteredDecodePods {
		rolesetName := pod.Labels[PDRoleSetIdentifier]
		var podKey string
		var podReady bool
		if snaps != nil {
			snap := snaps[pod]
			podKey = snap.podKey
			podReady = snap.metricsReady
		} else {
			podKey = utils.GeneratePodKey(pod.Namespace, pod.Name)
			podReady = metricsReadyByPod[podKey]
		}
		if anyMetricsReady && !podReady {
			// Cold-start: assign a neutral score (1.0 = idle warm pod) plus gateway-tracked
			// pending requests so the roleset competes fairly instead of being excluded entirely.
			// Once the pod's first request completes and metrics arrive, it transitions to full scoring.
			pending := float64(r.pendingDecodeTracker.GetPendingDecodeCount(podKey))
			coldScore := 1.0 + pending
			if verbose {
				scoredPods = append(scoredPods, fmt.Sprintf("%s:score=%.4f,roleset=%s(cold)", pod.Name, coldScore, rolesetName))
				klog.V(4).InfoS("decode pod metrics not ready, using cold-start score",
					"request_id", routingCtx.RequestID, "pod", pod.Name, "roleset", rolesetName,
					"cold_score", coldScore, "pending", pending)
			}
			recordDecodePick(&out, rolesetName, pod, coldScore)
			continue
		}

		decodeScore, ok := r.scoreWarmDecodePod(
			routingCtx, pod, podKey, policy, policyName, conductorMedianTP,
			usesDecodeTokenLoad, decodeRates[podKey], decodeCapacities, remoteLedger,
			podRequestCounts[podKey], podThroughputs[podKey], podFreeGpuUsage[podKey],
			maxRequestCount, maxThroughput, maxFreeGPUUsage, &out.FallbackUsed, verbose,
		)
		if !ok {
			if verbose {
				skippedPods = append(skippedPods, fmt.Sprintf("%s:invalid_score", pod.Name))
			}
			continue
		}

		if verbose {
			scoredPods = append(scoredPods, fmt.Sprintf("%s:score=%.4f,roleset=%s", pod.Name, decodeScore, rolesetName))
		}
		recordDecodePick(&out, rolesetName, pod, decodeScore)
	}

	if verbose {
		logDecodeScoreSummary(routingCtx.RequestID, policyName, out, anyMetricsReady, scoredPods, skippedPods)
	}

	return out
}

// recordDecodePick updates the lowest-score decode candidate for rolesetName and
// tracks the pass-wide maximum score for final score normalisation.
func recordDecodePick(out *pd.DecodeScoreRun, rolesetName string, pod *v1.Pod, score float64) {
	if existing, exists := out.PerRoleset[rolesetName]; !exists || score < existing.Score {
		out.PerRoleset[rolesetName] = pd.RolesetDecodePick{Pod: pod, Score: score}
	}
	if score > out.MaxScore {
		out.MaxScore = score
	}
}

// scoreWarmDecodePod scores a single ready decode pod, applying conductor's
// fleet-median throughput fallback and falling back to load_balancing if the
// primary policy produces an invalid score.
func (r *pdRouter) scoreWarmDecodePod(
	routingCtx *types.RoutingContext,
	pod *v1.Pod,
	podKey string,
	policy pd.DecodeScorePolicy,
	policyName pd.DecodePolicyName,
	conductorMedianTP float64,
	usesDecodeTokenLoad bool,
	decodeRate float64,
	decodeCapacities map[string]float64,
	remoteLedger map[string]cache.RemoteDecodeLoad,
	runningReqs float64,
	rawThroughput float64,
	freeGPU float64,
	maxRequestCount float64,
	maxThroughput float64,
	maxFreeGPUUsage float64,
	fallbackUsed *bool,
	verbose bool,
) (float64, bool) {
	in := pd.DecodePodInput{
		RunningReqs:     runningReqs,
		Throughput:      rawThroughput,
		FreeGPUPercent:  freeGPU,
		MaxRequestCount: maxRequestCount,
		MaxThroughput:   maxThroughput,
		MaxFreeGPUUsage: maxFreeGPUUsage,
	}
	if policyName == pd.DecodePolicyConductor && in.Throughput <= 0 && conductorMedianTP > 0 {
		in.Throughput = conductorMedianTP
		in.ThroughputFallback = true
	}
	if usesDecodeTokenLoad && r.tokenLoadTracker != nil {
		in.DecodeTokens = scaleByCapacity(r.decodeTokenLoad(podKey, decodeRate, remoteLedger), decodeCapacities, podKey)
	}

	decodeScore := policy.ScoreDecodePod(routingCtx, pod, in)
	if !pd.InvalidDecodeScore(decodeScore) {
		return decodeScore, true
	}

	if policyName != pd.DecodePolicyLoadBalancing {
		in.Throughput = rawThroughput
		in.ThroughputFallback = false
		decodeScore = loadBalancingDecodePolicy.ScoreDecodePod(routingCtx, pod, in)
		*fallbackUsed = true
	}
	if !pd.InvalidDecodeScore(decodeScore) {
		return decodeScore, true
	}

	if verbose {
		klog.V(4).InfoS("decode score invalid after policy and load_balancing fallback, skipping pod",
			"request_id", routingCtx.RequestID, "pod", pod.Name, "policy", policyName,
			"roleset", pod.Labels[PDRoleSetIdentifier])
	}
	return 0, false
}

// logDecodeScoreSummary emits the V(4) decode_score_summary diagnostic log line.
func logDecodeScoreSummary(
	requestID string,
	policyName pd.DecodePolicyName,
	out pd.DecodeScoreRun,
	anyMetricsReady bool,
	scoredPods []string,
	skippedPods []string,
) {
	perRolesetBest := make([]string, 0, len(out.PerRoleset))
	for roleset, pick := range out.PerRoleset {
		perRolesetBest = append(perRolesetBest, fmt.Sprintf("%s:%s=%.4f", roleset, pick.Pod.Name, pick.Score))
	}
	sort.Strings(perRolesetBest)
	klog.V(4).InfoS("decode_score_summary",
		"request_id", requestID, "policy", policyName,
		"fallback_used", out.FallbackUsed, "any_metrics_ready", anyMetricsReady,
		"scored", strings.Join(scoredPods, " "), "skipped", strings.Join(skippedPods, " "),
		"per_roleset_best", strings.Join(perRolesetBest, " "),
	)
}

// decodeTokenLoadRates returns, per decode pod key, the per-request decode rate
// the token_load score uses to estimate the output its outstanding requests have
// generated: the pod's scraped generation throughput divided by its running
// requests (pending included, as in podRequestCounts). A pod without both uses
// the mean rate of the pods that have them; with none, no pod gets a rate and
// the score is prompt tokens only. Returns nil unless token_load scores the pass
// with AIBRIX_DECODE_TOKEN_LOAD_OUTPUT_GROWTH on.
func decodeTokenLoadRates(usesDecodeTokenLoad bool, throughputs, requestCounts map[string]float64) map[string]float64 {
	if !usesDecodeTokenLoad || !aibrixDecodeTokenLoadOutputGrowth {
		return nil
	}
	rates := make(map[string]float64, len(requestCounts))
	sum, measured := 0.0, 0
	for podKey, n := range requestCounts {
		if tp := throughputs[podKey]; tp > 0 && n > 0 {
			rates[podKey] = tp / n
			sum += tp / n
			measured++
		}
	}
	if measured == 0 {
		return nil
	}
	mean := sum / float64(measured)
	for podKey := range requestCounts {
		if _, ok := rates[podKey]; !ok {
			rates[podKey] = mean
		}
	}
	return rates
}

// conductorFleetMedianThroughput returns the median positive generation throughput
// across warm candidate decode pods so conductor scores unmeasured (e.g. idle or
// newly started) pods in the same ms scale as measured peers instead of falling
// back directly to DefaultDecodeTBTFallbackMs. Returns 0 when no candidate has a
// positive measured rate.
func conductorFleetMedianThroughput(
	pods []*v1.Pod,
	metricsReadyByPod map[string]bool,
	anyMetricsReady bool,
	throughputs map[string]float64,
) float64 {
	measured := make([]float64, 0, len(pods))
	for _, pod := range pods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		if anyMetricsReady && !metricsReadyByPod[podKey] {
			continue
		}
		if tp := throughputs[podKey]; tp > 0 {
			measured = append(measured, tp)
		}
	}
	n := len(measured)
	if n == 0 {
		return 0
	}
	sort.Float64s(measured)
	if n%2 == 1 {
		return measured[n/2]
	}
	return (measured[n/2-1] + measured[n/2]) / 2.0
}

// finalPDScore picks the winning prefill/decode pod pair after scorePrefillPods and
// scoreDecodePods. Only rolesets with an entry in both prefillScores and
// decodeRun.PerRoleset are considered, so prefill and decode always come from the
// same roleset.
//
// For each eligible roleset, combined score is:
//
//	normalizedPrefill + normalizedDecode
//	  = prefillScore.Score / maxPrefillScore + decodePick.Score / decodeRun.MaxScore
//
// Lower combined score wins. Ties retain whichever roleset was last seen with that
// score (map iteration order). Emits per-roleset final_score V(4) logs.
//
// Error cases:
//   - decodeRun.Err is set: decode scoring failed upstream.
//   - prefillScores is nil: Prepare failed in scorePrefillPods.
//   - prefillScores is non-nil but empty: all prefill pods were filtered by the stddev
//     check, leaving no eligible prefill candidate; targetPrefillPod stays nil.
//   - prefillScores is non-empty but no roleset has a matching decode entry: the
//     roleset sets are disjoint after load-imbalance alignment; targetDecodePod stays nil.
//
// On success, records the prefill pod's prefix-cache credit when prefixHashes is non-empty
// (Route enqueues it once the decode pod admitted the request, see pendingPrefixCredits), increments
// internal selection counters, and emits PDSelectedPrefillPodTotal / PDSelectedDecodePodTotal.
// Does not update request trackers; Route registers them after pod selection returns.
func conductorFleetMedianThroughputFromSnapshots(
	pods []*v1.Pod,
	snaps map[*v1.Pod]decodePodSnapshot,
	anyMetricsReady bool,
	throughputs map[string]float64,
) float64 {
	measured := make([]float64, 0, len(pods))
	for _, pod := range pods {
		snap := snaps[pod]
		if anyMetricsReady && !snap.metricsReady {
			continue
		}
		if tp := throughputs[snap.podKey]; tp > 0 {
			measured = append(measured, tp)
		}
	}
	n := len(measured)
	if n == 0 {
		return 0
	}
	sort.Float64s(measured)
	if n%2 == 1 {
		return measured[n/2]
	}
	return (measured[n/2-1] + measured[n/2]) / 2.0
}

func (r *pdRouter) finalPDScore(routingCtx *types.RoutingContext,
	prefixHashes []uint64,
	prefillScores map[string]*Scores, maxPrefillScore float64,
	decodeRun pd.DecodeScoreRun,
) (*v1.Pod, *v1.Pod, error) {
	targetPrefillPod, targetDecodePod, err := r.selectFinalPDPair(routingCtx, prefillScores, maxPrefillScore, decodeRun)
	if err != nil {
		return nil, nil, err
	}
	r.recordPDSelection(
		routingCtx,
		prefixHashes,
		targetPrefillPod, utils.GeneratePodKey(targetPrefillPod.Namespace, targetPrefillPod.Name),
		targetDecodePod, utils.GeneratePodKey(targetDecodePod.Namespace, targetDecodePod.Name),
	)
	return targetPrefillPod, targetDecodePod, nil
}

// selectFinalPDPair evaluates per-roleset combined normalized scores and picks
// the winning prefill/decode pair without performing non-tracker side effects
// (prefix credit storage, selection counter locks, or Prometheus emissions).
func (r *pdRouter) selectFinalPDPair(routingCtx *types.RoutingContext,
	prefillScores map[string]*Scores, maxPrefillScore float64,
	decodeRun pd.DecodeScoreRun,
) (*v1.Pod, *v1.Pod, error) {
	if decodeRun.Err != nil {
		return nil, nil, fmt.Errorf("decode scoring failed: %w", decodeRun.Err)
	}

	var targetPrefillPod, targetDecodePod *v1.Pod
	minScore := math.MaxFloat64

	for roleset, prefillScore := range prefillScores {
		decodePick, ok := decodeRun.PerRoleset[roleset]
		if !ok {
			if klog.V(4).Enabled() {
				klog.V(4).InfoS("final_score_skip_roleset",
					"request_id", routingCtx.RequestID, "roleset", roleset,
					"prefill_pod", prefillScore.Pod.Name, "reason", "no_decode_score_for_roleset")
			}
			continue
		}

		normalizedPrefillScore := prefillScore.Score / maxPrefillScore
		normalizedDecodeScore := decodePick.Score / decodeRun.MaxScore
		final := normalizedPrefillScore + normalizedDecodeScore

		if final < minScore {
			minScore = final
			targetPrefillPod = prefillScore.Pod
			targetDecodePod = decodePick.Pod
		}

		if klog.V(4).Enabled() {
			klog.V(4).InfoS("final_score",
				"request_id", routingCtx.RequestID, "roleset", roleset,
				"final_score", final, "prefill_pod", prefillScore.Pod.Name,
				"decode_pod", decodePick.Pod.Name,
				"prefill_score", prefillScore.Score, "normalized_prefill_score", normalizedPrefillScore,
				"decode_score", decodePick.Score, "normalized_decode_score", normalizedDecodeScore,
				"decode_policy", decodeRun.Policy, "decode_fallback_used", decodeRun.FallbackUsed,
			)
		}
	}

	if targetPrefillPod == nil {
		return nil, nil, fmt.Errorf("target prefill pod is nil")
	}
	if targetDecodePod == nil {
		return nil, nil, fmt.Errorf("target decode pod is nil")
	}

	return targetPrefillPod, targetDecodePod, nil
}

// recordPDSelection records the post-selection prefix credit, selection counts,
// and Prometheus metrics for a chosen PD pair. Safe to run after releasing
// selectStripe.Lock().
func (r *pdRouter) recordPDSelection(routingCtx *types.RoutingContext, prefixHashes []uint64,
	targetPrefillPod *v1.Pod, targetPrefillKey string,
	targetDecodePod *v1.Pod, targetDecodeKey string,
) {
	if len(prefixHashes) > 0 {
		r.pendingPrefixCredits.Store(routingCtx.TrackerKey(), prefixUpdateJob{
			prefixHashes: append([]uint64(nil), prefixHashes...),
			model:        routingCtx.Model,
			pod:          targetPrefillKey,
		})
	}

	r.countersMu.Lock()
	r.selectionCounts[targetPrefillKey]++
	r.selectionCounts[targetDecodeKey]++
	r.countersMu.Unlock()

	metrics.EmitMetricToPrometheus(routingCtx, targetPrefillPod, metrics.PDSelectedPrefillPodTotal, &metrics.SimpleMetricValue{Value: 1.0}, nil)
	metrics.EmitMetricToPrometheus(routingCtx, targetDecodePod, metrics.PDSelectedDecodePodTotal, &metrics.SimpleMetricValue{Value: 1.0}, nil)
}

func (r *pdRouter) SubscribedMetrics() []string {
	return []string{}
}

type prefixUpdateJob struct {
	prefixHashes []uint64
	model        string
	pod          string
}

func (r *pdRouter) startPrefixUpdater() {
	// single worker to serialize updates, minimizing lock contention in the indexer
	go func() {
		for job := range r.prefixUpdateCh {
			r.prefixCacheIndexer.AddPrefix(job.prefixHashes, job.model, job.pod)
		}
	}()
}

func (r *pdRouter) enqueuePrefixUpdate(prefixHashes []uint64, model, pod string) {
	// copy slice to avoid data races if caller reuses the backing array
	copyHashes := append([]uint64(nil), prefixHashes...)
	select {
	case r.prefixUpdateCh <- prefixUpdateJob{
		prefixHashes: copyHashes,
		model:        model,
		pod:          pod,
	}:
		// enqueued
	default:
		// channel full; drop to keep routing path non-blocking
		klog.Warningf("Prefix update channel full, dropping update for model %s on pod %s", model, pod)
	}
}

func (r *pdRouter) isPodSuitableForPromptLength(routingCtx *types.RoutingContext, pod *v1.Pod, promptLength int) bool {
	minLength, maxLength := podPromptLenBucketBounds(routingCtx, pod)

	if minLength > maxLength {
		return false
	}
	// If no prompt length range is configured, the pod is assumed to be suitable for handling any length.
	if minLength == 0 && maxLength == math.MaxInt32 {
		return true
	}

	return promptLength >= minLength && promptLength <= maxLength
}

// collectAndBucketPods partitions readyPods into prefill, decode, and combined
// pod slices for PD-disaggregated routing. It operates in two phases:
//
// Phase 1 groups pods by their roleset (PDRoleSetIdentifier label), separating
// prefill and decode pods, while collecting combined-role pods that are eligible
// for the given promptLength. Pods missing required PD labels or an HTTP server
// are skipped.
//
// Phase 2 builds the output slices from replica-complete rolesets (all expected
// prefill and decode replicas ready). When prompt-length bucketing is enabled, it
// also produces filtered prefill/decode slices restricted to pods whose capacity
// bucket covers promptLength; bucket-filtered prefill and decode slices are applied
// together only when both sides have suitable pods.
//
// Returns (prefillPods, decodePods, promptLengthBucketingPrefillPods,
// promptLengthBucketingDecodePods, combinedPods).
func (r *pdRouter) collectAndBucketPods(routingCtx *types.RoutingContext, readyPods []*v1.Pod, promptLength int) ([]*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod, []*v1.Pod) {
	bucketingEnabled := effectivePromptLengthBucketing(routingCtx)

	var combinedPods []*v1.Pod
	if bucketingEnabled {
		for _, pod := range readyPods {
			_, hasRoleset := pod.Labels[PDRoleSetIdentifier]
			if !hasRoleset {
				continue
			}
			roleID, hasRole := pod.Labels[PDRoleIdentifier]
			if !hasRole || roleID == PDRolePrefill || roleID == PDRoleDecode || roleID == PDRoleEncode {
				continue
			}
			if !pd.IsPodWithHTTPServer(pod) {
				continue
			}
			if isCombinedPod(routingCtx, pod) && r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
				combinedPods = append(combinedPods, pod)
			}
		}
	}

	eligible := eligiblePDRolesets(readyPods)
	var prefillPods, decodePods []*v1.Pod
	var promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods []*v1.Pod

	for _, b := range eligible {
		prefillPods = append(prefillPods, b.prefills...)
		decodePods = append(decodePods, b.decodes...)

		if bucketingEnabled {
			var bucketPrefills, bucketDecodes []*v1.Pod
			for _, pod := range b.prefills {
				if r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
					bucketPrefills = append(bucketPrefills, pod)
				}
			}
			for _, pod := range b.decodes {
				if r.isPodSuitableForPromptLength(routingCtx, pod, promptLength) {
					bucketDecodes = append(bucketDecodes, pod)
				}
			}
			if len(bucketPrefills) > 0 && len(bucketDecodes) > 0 {
				promptLengthBucketingPrefillPods = append(promptLengthBucketingPrefillPods, bucketPrefills...)
				promptLengthBucketingDecodePods = append(promptLengthBucketingDecodePods, bucketDecodes...)
			}
		}
	}

	// Apply bucket-filtered pods atomically so prefill/decode stay roleset-aligned.
	if bucketingEnabled && len(promptLengthBucketingPrefillPods) > 0 && len(promptLengthBucketingDecodePods) > 0 {
		prefillPods = promptLengthBucketingPrefillPods
		decodePods = promptLengthBucketingDecodePods
	}

	return prefillPods, decodePods, promptLengthBucketingPrefillPods, promptLengthBucketingDecodePods, combinedPods
}

// shouldPickCombined returns true when at least one combined pod is under low
// load (request rate < defaultRequestRateLowLoadThreshold) and at least one
// prefill or decode pod is over high load (> defaultRequestRateHighLoadThreshold).
// The decode check is skipped when a prefill pod already qualifies as high-load.
func (r *pdRouter) shouldPickCombined(routingCtx *types.RoutingContext, prefillPods, decodePods, combinedPods []*v1.Pod) bool {
	combinedLowLoad := false
	for _, combinePod := range combinedPods {
		if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, combinePod) < defaultRequestRateLowLoadThreshold {
			combinedLowLoad = true
			break
		}
	}
	if !combinedLowLoad {
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("combined_load", "requestId", routingCtx.RequestID, "prefillHighLoad", false, "decodeHighLoad", false, "combinedLowLoad", combinedLowLoad)
		}
		return false
	}

	prefillHighLoad := false
	for _, prefillPod := range prefillPods {
		if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, prefillPod) > defaultRequestRateHighLoadThreshold {
			prefillHighLoad = true
			break
		}
	}

	decodeHighLoad := false
	if !prefillHighLoad {
		for _, decodePod := range decodePods {
			if calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, decodePod) > defaultRequestRateHighLoadThreshold {
				decodeHighLoad = true
				break
			}
		}
	}

	if klog.V(4).Enabled() {
		klog.V(4).InfoS("loads", "requestId", routingCtx.RequestID, "prefillHighLoad", prefillHighLoad, "decodeHighLoad", decodeHighLoad, "combinedLowLoad", combinedLowLoad)
	}
	return (prefillHighLoad || decodeHighLoad) && combinedLowLoad
}

// scoreCombinedPods returns the combined pod with the lowest request-rate score.
// Pods are shuffled before scoring so ties are broken randomly.
func (r *pdRouter) scoreCombinedPods(routingCtx *types.RoutingContext, combinedPods []*v1.Pod) *v1.Pod {
	utils.Shuffle(combinedPods)
	var bestPod *v1.Pod
	minScore := math.MaxFloat64
	for _, pod := range combinedPods {
		score := calculatePodScoreBasedOffRequestRate(routingCtx, r.cache, pod)
		if klog.V(4).Enabled() {
			klog.V(4).InfoS("combined_pod_score", "requestId", routingCtx.RequestID, "pod_name", pod.Name, "score", score)
		}
		if score < minScore {
			minScore = score
			bestPod = pod
		}
	}
	return bestPod
}

func isCombinedPod(routingCtx *types.RoutingContext, pod *v1.Pod) bool {
	profile := configprofiles.ResolveProfileFrom(routingCtx.ModelConfig, pod, routingCtx.ReqConfigProfile)
	if profile == nil {
		return false
	}
	cfg := configprofiles.ParseRoutingConfig(profile.RoutingConfig)
	return cfg != nil && cfg.Combined != nil && *cfg.Combined
}

// promptLenBucketBounds returns the prompt-length range a pod's profile
// declares, with the whole range as the default: a pod that declares none is
// not bucket-scoped. A negative minimum or a non-positive maximum falls back to
// its default, mirroring the pod-side tolerance of a partially filled range.
func promptLenBucketBounds(cfg *types.RoutingConfig) (int, int) {
	minLength, maxLength := 0, math.MaxInt32
	if cfg == nil {
		return minLength, maxLength
	}
	if v := cfg.PromptLenBucketMinLength; v != nil && *v > 0 {
		minLength = *v
	}
	if v := cfg.PromptLenBucketMaxLength; v != nil && *v > 0 {
		maxLength = *v
	}
	return minLength, maxLength
}

// decodePodMetricsReady reports whether RealtimeNumRequestsRunning is available for pod.
// Used by scoreDecodePods to gate cold-start scoring vs full policy scoring.
func (r *pdRouter) decodePodMetricsReady(ctx *types.RoutingContext, pod *v1.Pod) bool {
	if r == nil || r.cache == nil || ctx == nil || pod == nil {
		return false
	}
	_, runningErr := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.RealtimeNumRequestsRunning)
	return runningErr == nil
}

func getLLMEngine(pod *v1.Pod, labelName string, defaultValue string) string {
	labelTarget, ok := pod.Labels[labelName]
	if !ok {
		return defaultValue
	}
	return labelTarget
}

// ValidateAndGetLLMEngine validates that all pods use the same LLM engine label and returns it.
func ValidateAndGetLLMEngine(pods []*v1.Pod) (string, error) {
	if len(pods) == 0 {
		return "", fmt.Errorf("no pods provided")
	}

	firstEngine := getLLMEngine(pods[0], LLMEngineIdentifier, VLLMEngine)

	for i := 1; i < len(pods); i++ {
		engine := getLLMEngine(pods[i], LLMEngineIdentifier, VLLMEngine)
		if engine != firstEngine {
			return "", fmt.Errorf("inconsistent LLM engines detected: pod %s has %s, pod %s has %s",
				pods[0].Name, firstEngine, pods[i].Name, engine)
		}
	}

	return firstEngine, nil
}

// sharedDecodeLedgerCache is implemented by caches that can share the
// token_load decode ledger across gateway replicas (cache.Store with Redis).
type sharedDecodeLedgerCache interface {
	SharedDecodeLedgerAvailable() bool
	PublishDecodeLedger(provider cache.DecodeLedgerProvider) func(podKey string)
	GetPodsRunningRequestsAndDecodeLedger(pods []*v1.Pod) (map[string]int64, map[string]cache.RemoteDecodeLoad, error)
	GetPodsDecodeLedger(pods []*v1.Pod) map[string]cache.RemoteDecodeLoad
}

// shareDecodeLedger publishes the decode ledger to the other gateway replicas
// and reads theirs, when AIBRIX_TOKEN_LOAD_SHARED_LEDGER is on and the cache
// can share it. It does not depend on the default decode policy: a model's
// config profile can select token_load per request (routingConfig), which is
// also why the tracker always exists. Until something charges the decode
// ledger, the publisher has nothing to write.
func (r *pdRouter) shareDecodeLedger() {
	if !aibrixTokenLoadSharedLedger || r.tokenLoadTracker == nil {
		return
	}
	shared, ok := r.cache.(sharedDecodeLedgerCache)
	if !ok || !shared.SharedDecodeLedgerAvailable() {
		return
	}
	tracker := r.tokenLoadTracker
	notify := shared.PublishDecodeLedger(func(podKey string) cache.DecodeLedgerState {
		tokens, charges, sumChargedAt := tracker.DecodeLedgerState(podKey)
		return cache.DecodeLedgerState{Tokens: tokens, Charges: charges, SumChargedAt: sumChargedAt}
	})
	tracker.SetDecodeLedgerListener(notify)
	r.sharedDecodeLedger = shared
	klog.InfoS("pd_router token_load decode ledger is shared across gateway replicas")
}

// decodeRunningRequests reads the live running-request counts of pods and, when
// remoteLedger is not nil, the other replicas' decode ledgers. When remoteLedger
// is not needed, it reads through routingCtx.RunningRequests so one routing
// decision shares a single Redis read (e.g. reusing the snapshot already
// populated by narrowToInflightCapacity). When remoteLedger is needed and the
// decision snapshot already covers pods, it reuses those cached counts and
// reads only the decode ledger via GetPodsDecodeLedger; otherwise it reads both
// in one round trip via GetPodsRunningRequestsAndDecodeLedger.
func (r *pdRouter) decodeRunningRequests(routingCtx *types.RoutingContext, pods []*v1.Pod, remoteLedger *map[string]cache.RemoteDecodeLoad) (map[string]int64, error) {
	if remoteLedger == nil || r.sharedDecodeLedger == nil {
		return routingCtx.RunningRequests(pods, r.cache.GetPodsRunningRequests)
	}
	if counts, ok := routingCtx.CachedRunningRequests(pods); ok {
		*remoteLedger = r.sharedDecodeLedger.GetPodsDecodeLedger(pods)
		return counts, nil
	}
	counts, ledger, err := r.sharedDecodeLedger.GetPodsRunningRequestsAndDecodeLedger(pods)
	*remoteLedger = ledger
	return counts, err
}

// decodeTokenLoad is the token_load decode load of the pod identified by
// podKey: this replica's charged prompt tokens and their estimated output, plus
// the same for the other replicas' charges when the ledger is shared. rate is
// the per-request decode rate (see decodeTokenLoadRates).
func (r *pdRouter) decodeTokenLoad(podKey string, rate float64, remoteLedger map[string]cache.RemoteDecodeLoad) float64 {
	load := r.tokenLoadTracker.GetDecodeLoad(podKey) + r.tokenLoadTracker.DecodeGrowth(podKey, rate)
	if remote, ok := remoteLedger[podKey]; ok {
		load += remote.Tokens
		if rate > 0 {
			load += rate * remote.Elapsed
		}
	}
	return load
}

// decodeKVCapacities returns, per decode pod key, the factor that scales the
// pod's token_load score to the mean capacity of the candidates: mean
// capacity / pod capacity, with capacities in KV tokens. Scores then rank pods
// by how full they are, and stay in tokens: with equal capacities every factor
// is 1. A pod whose capacity is unknown uses the mean (factor 1). Returns nil,
// which leaves the scores in tokens, unless token_load scores the pass with
// AIBRIX_DECODE_TOKEN_LOAD_CAPACITY_AWARE on and some candidate reports a
// capacity.
func decodeKVCapacitiesFromSnapshots(usesDecodeTokenLoad bool, pods []*v1.Pod, snaps map[*v1.Pod]decodePodSnapshot) map[string]float64 {
	if !usesDecodeTokenLoad || !aibrixDecodeTokenLoadCapacityAware {
		return nil
	}
	sum := 0.0
	measured := 0
	for _, pod := range pods {
		if snap := snaps[pod]; snap.hasKVCapacity {
			sum += snap.kvCapacity
			measured++
		}
	}
	if measured == 0 {
		return nil
	}
	mean := sum / float64(measured)
	factors := make(map[string]float64, len(pods))
	for _, pod := range pods {
		snap := snaps[pod]
		if snap.hasKVCapacity {
			factors[snap.podKey] = mean / snap.kvCapacity
		} else {
			factors[snap.podKey] = 1
			if klog.V(4).Enabled() {
				klog.V(4).InfoS("pd_router token_load decode capacity unknown, scoring at the mean",
					"pod_name", pod.Name, "mean_capacity_tokens", mean)
			}
		}
	}
	return factors
}

func (r *pdRouter) decodeKVCapacities(usesDecodeTokenLoad bool, pods []*v1.Pod) map[string]float64 {
	if !usesDecodeTokenLoad || !aibrixDecodeTokenLoadCapacityAware {
		return nil
	}
	capacities := make(map[string]float64, len(pods))
	sum := 0.0
	for _, pod := range pods {
		if c, ok := r.podKVCapacity(pod); ok {
			capacities[utils.GeneratePodKey(pod.Namespace, pod.Name)] = c
			sum += c
		}
	}
	if len(capacities) == 0 {
		return nil
	}
	mean := sum / float64(len(capacities))
	factors := make(map[string]float64, len(pods))
	for _, pod := range pods {
		key := utils.GeneratePodKey(pod.Namespace, pod.Name)
		factors[key] = 1
		if c, ok := capacities[key]; ok {
			factors[key] = mean / c
		} else {
			klog.V(4).InfoS("pd_router token_load decode capacity unknown, scoring at the mean",
				"pod_name", pod.Name, "mean_capacity_tokens", mean)
		}
	}
	return factors
}

// podKVCapacity reads a pod's KV cache capacity in tokens from its scraped
// metrics: num_gpu_blocks × block_size from vLLM's cache config info, or the
// capacity gauge SGLang reports. With data parallelism, vLLM's front end sums
// num_gpu_blocks over the engines it manages and puts that sum on every
// engine's cache_config_info series, so the first series is the pod's total.
func (r *pdRouter) podKVCapacity(pod *v1.Pod) (float64, bool) {
	if v, err := r.cache.GetMetricValueByPod(pod.Name, pod.Namespace, metrics.KVCacheCapacityTokens); err == nil && v != nil {
		if c := v.GetSimpleValue(); validKVCapacity(c) {
			return c, true
		}
	}
	blocks, ok := podLabelMetricFloat(r.cache, pod, metrics.KVCacheNumGPUBlocks)
	if !ok {
		return 0, false
	}
	blockSize, ok := podLabelMetricFloat(r.cache, pod, metrics.KVCacheBlockSize)
	if !ok {
		return 0, false
	}
	return blocks * blockSize, true
}

// podLabelMetricFloat parses a positive number from a label-valued pod metric.
func podLabelMetricFloat(c cache.Cache, pod *v1.Pod, metricName string) (float64, bool) {
	v, err := c.GetMetricValueByPod(pod.Name, pod.Namespace, metricName)
	if err != nil {
		return 0, false
	}
	label, ok := v.(*metrics.LabelValueMetricValue)
	if !ok || label == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(label.Value, 64)
	if err != nil || !validKVCapacity(f) {
		return 0, false
	}
	return f, true
}

// validKVCapacity reports whether a scraped capacity value is usable: positive
// and finite. The Prometheus text format accepts NaN and +Inf as values.
func validKVCapacity(f float64) bool {
	return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// isFinite reports whether a scraped gauge value is neither NaN nor infinite.
func isFinite(f float64) bool {
	return !math.IsInf(f, 0) && !math.IsNaN(f)
}

// scaleByCapacity applies podKey's capacity factor (see decodeKVCapacities)
// to a token_load score; nil factors leave it unchanged.
func scaleByCapacity(load float64, factors map[string]float64, podKey string) float64 {
	if f, ok := factors[podKey]; ok {
		return load * f
	}
	return load
}
