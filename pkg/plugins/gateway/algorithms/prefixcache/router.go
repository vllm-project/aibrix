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

package prefixcache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms/routeutil"
	"github.com/vllm-project/aibrix/pkg/types"
	"github.com/vllm-project/aibrix/pkg/utils"
	"github.com/vllm-project/aibrix/pkg/utils/prefixcacheindexer"
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
)

const (
	defaultStandardDeviationFactor = 1
	defaultLoadCapFactor           = 1.5 // c in the c×(mean+1) matched-pod cap

	// tokenizerTypeTiktoken is the tiktoken tokenizer type
	tokenizerTypeTiktoken = "tiktoken"

	selectionPrefixMatch          = "prefix_match"
	selectionLeastRequestFallback = "least_request_fallback"
)

const RouterPrefixCache types.RoutingAlgorithm = "prefix-cache"

var (
	tokenizerType               = utils.LoadEnv(constants.EnvPrefixCacheTokenizerType, "character")
	standardDeviationFactor int = utils.LoadEnvInt("AIBRIX_PREFIX_CACHE_STANDARD_DEVIATION_FACTOR", defaultStandardDeviationFactor)
	// c in the bounded-load cap: a matched pod with more than c×(mean+1) running
	// requests is skipped. Unlike the stddev check it can fire at any pool size.
	loadCapFactor float64 = utils.LoadEnvFloat("AIBRIX_PREFIX_CACHE_LOAD_CAP_FACTOR", defaultLoadCapFactor)
)

// PrefixCacheMetrics holds all prefix cache metrics
type PrefixCacheMetrics struct {
	prefixCacheRoutingDecisions *prometheus.CounterVec
	prefixCacheIndexerStatus    *prometheus.GaugeVec
	prefixCacheRoutingLatency   *prometheus.HistogramVec
	prefixCacheRoutingSelection *prometheus.CounterVec
	prefixCacheRoutingErrors    *prometheus.CounterVec
	prefixCacheMatchOperations  *prometheus.CounterVec
}

// Global metrics instance
var (
	prefixCacheMetrics     *PrefixCacheMetrics
	prefixCacheMetricsOnce sync.Once
	prefixCacheMetricsMu   sync.RWMutex
)

// createPrefixCacheMetrics creates all prefix cache metrics (but doesn't register them)
func createPrefixCacheMetrics() *PrefixCacheMetrics {
	return &PrefixCacheMetrics{
		prefixCacheRoutingDecisions: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_routing_decisions_total",
				Help:      "Total number of routing decisions by match percentage",
			},
			[]string{"model", "match_percent_bucket", "using_kv_sync"},
		),
		prefixCacheIndexerStatus: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_indexer_status",
				Help:      "Status of prefix cache indexer (1=available, 0=unavailable)",
			},
			[]string{"model", "indexer_type"},
		),
		prefixCacheRoutingLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_routing_latency_seconds",
				Help:      "Latency of prefix cache routing decisions",
				Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 15), // 10us to ~160ms
			},
			[]string{"model", "using_kv_sync"},
		),
		prefixCacheRoutingSelection: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_routing_selection_total",
				Help:      "Total number of prefix cache routing pod selections by method",
			},
			[]string{"model", "selection", "using_kv_sync"},
		),
		prefixCacheRoutingErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_routing_errors_total",
				Help:      "Total number of prefix cache routing failures by reason",
			},
			[]string{"model", "reason", "using_kv_sync"},
		),
		prefixCacheMatchOperations: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Subsystem: constants.AibrixSubsystemName,
				Name:      "prefix_cache_match_operations_total",
				Help:      "Prefix matching operations by seeded reuse or index lookup outcome",
			},
			[]string{"model", "result"},
		),
	}
}

// registerPrefixCacheMetrics registers all metrics with Prometheus
func (m *PrefixCacheMetrics) register() error {
	collectors := []prometheus.Collector{
		m.prefixCacheRoutingDecisions,
		m.prefixCacheIndexerStatus,
		m.prefixCacheRoutingLatency,
		m.prefixCacheRoutingSelection,
		m.prefixCacheRoutingErrors,
		m.prefixCacheMatchOperations,
	}

	for _, collector := range collectors {
		if err := prometheus.Register(collector); err != nil {
			// If already registered, it's ok (might happen in tests)
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return fmt.Errorf("failed to register metric: %w", err)
			}
		}
	}

	return nil
}

// initializePrefixCacheMetrics initializes and registers prefix cache metrics
func initializePrefixCacheMetrics() error {
	var err error
	prefixCacheMetricsOnce.Do(func() {
		prefixCacheMetricsMu.Lock()
		defer prefixCacheMetricsMu.Unlock()

		metrics := createPrefixCacheMetrics()
		if registerErr := metrics.register(); registerErr != nil {
			err = registerErr
			klog.Errorf("Failed to register prefix cache metrics: %v", registerErr)
			return
		}
		prefixCacheMetrics = metrics
		klog.Info("Prefix cache metrics registered successfully")
	})
	return err
}

// getPrefixCacheMetrics returns the global metrics instance if available
func getPrefixCacheMetrics() *PrefixCacheMetrics {
	prefixCacheMetricsMu.RLock()
	defer prefixCacheMetricsMu.RUnlock()
	return prefixCacheMetrics
}

func init() {
	// This prefix-cache-specific gate (and its tuning knob) was replaced by the centralized
	// load-imbalance gate the gateway now applies ahead of every non-exclusive strategy (see
	// ApplyLoadImbalanceGate in load_balance.go). Warn rather than silently ignoring the old
	// var, since an operator who tuned it would otherwise have no signal their setting stopped
	// taking effect.
	if _, ok := os.LookupEnv("AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT"); ok {
		klog.Warning("AIBRIX_PREFIX_CACHE_POD_RUNNING_REQUEST_IMBALANCE_ABS_COUNT is no longer used: " +
			"the load-imbalance gate is now centralized in the gateway and configured via " +
			"AIBRIX_LOAD_BALANCE_IMBALANCE_MIN_GAP / AIBRIX_LOAD_BALANCE_IMBALANCE_FACTOR instead.")
	}
}

// kvSyncPrefixCacheRouter handles routing when KV sync is enabled
type kvSyncPrefixCacheRouter struct {
	cache         cache.Cache
	tokenizerPool TokenizerPoolInterface
	syncIndexer   *syncindexer.SyncPrefixHashTable
}

type prefixCacheRouter struct {
	cache              cache.Cache
	tokenizer          tokenizer.Tokenizer
	prefixCacheIndexer *prefixcacheindexer.PrefixHashTable

	// Add TokenizerPool field
	tokenizerPool TokenizerPoolInterface // nil when not using remote tokenizer

	// KV sync router - only created when needed
	kvSyncRouter *kvSyncPrefixCacheRouter
}

// TokenizerPoolInterface defines the interface for tokenizer pools
type TokenizerPoolInterface interface {
	GetTokenizer(model string, pods []*v1.Pod) tokenizer.Tokenizer
	Close() error
}

// panicTokenizer is a sentinel implementation that panics if used directly.
// It serves as a safety guard to ensure all tokenization goes through the
// model-aware helper method when TokenizerPool is enabled.
type panicTokenizer struct{}

func (p *panicTokenizer) TokenizeInputText(text string) ([]byte, error) {
	panic("tokenizer.TokenizeInputText was called directly. " +
		"Use the model-aware getTokenizerForRequest(ctx) helper method instead.")
}

// NewTokenizer returns the local tokenizer selected by AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE.
func NewTokenizer() tokenizer.Tokenizer {
	if tokenizerType == tokenizerTypeTiktoken {
		return tokenizer.NewTiktokenTokenizer()
	}
	return tokenizer.NewCharacterTokenizer()
}

// loadTokenizerPoolConfigFromEnv builds the remote tokenizer pool configuration
// from the environment.
//
// The duration defaults are time.Duration values on purpose: utils.LoadEnvDuration
// parses the environment value with time.ParseDuration and already returns a
// time.Duration, so scaling its result by time.Second again would multiply any
// user-supplied value by 1e9. Only KV Event Sync constants are defined in
// pkg/constants, the rest are read by name here.
func loadTokenizerPoolConfigFromEnv() TokenizerPoolConfig {
	return TokenizerPoolConfig{
		EnableVLLMRemote:     true, // We're using it, so enable it
		EndpointTemplate:     utils.LoadEnv("AIBRIX_VLLM_TOKENIZER_ENDPOINT_TEMPLATE", "http://%s:8000"),
		HealthCheckPeriod:    utils.LoadEnvDuration("AIBRIX_TOKENIZER_HEALTH_CHECK_PERIOD", 30*time.Second),
		TokenizerTTL:         utils.LoadEnvDuration("AIBRIX_TOKENIZER_TTL", 300*time.Second),
		MaxTokenizersPerPool: utils.LoadEnvInt("AIBRIX_MAX_TOKENIZERS_PER_POOL", 100),
		DefaultTokenizer:     nil, // Set by the caller
		Timeout:              utils.LoadEnvDuration("AIBRIX_TOKENIZER_REQUEST_TIMEOUT", 5*time.Second),
		ModelServiceMap:      make(map[string]string),
		// Fixed remote tokenizer endpoint shared by all models, e.g. a Service
		// that load-balances tokenize calls across replicas. It is optional, so
		// read it with os.Getenv: LoadEnv would log a warning when unset.
		FixedEndpoint: strings.TrimSpace(os.Getenv(constants.EnvPrefixCacheRemoteTokenizerEndpoint)),
	}
}

func NewPrefixCacheRouter() (types.Router, error) {
	c, err := cache.Get()
	if err != nil {
		return nil, err
	}
	return NewPrefixCacheRouterWithOptions(c, prefixcacheindexer.GetSharedPrefixHashTable())
}

// NewPrefixCacheRouterWithCache constructs the prefix-cache router with an
// explicit cache while preserving tokenizer and indexer behavior.
func NewPrefixCacheRouterWithCache(c cache.Cache) (types.Router, error) {
	return NewPrefixCacheRouterWithOptions(c, nil)
}

// NewPrefixCacheRouterWithOptions constructs a prefix-cache router with an
// explicit cache and optional per-instance prefix table. A nil indexer creates
// a new private table; the production constructor passes the shared table
// explicitly.
func NewPrefixCacheRouterWithOptions(c cache.Cache, indexer *prefixcacheindexer.PrefixHashTable) (types.Router, error) {
	// Initialize prefix cache metrics if enabled
	if err := initializePrefixCacheMetrics(); err != nil {
		klog.Errorf("Failed to initialize prefix cache metrics: %v", err)
		// Continue without metrics rather than failing
	}

	var tokenizerObj tokenizer.Tokenizer
	var tokenizerPool *TokenizerPool

	// Note: tokenizerType is a package variable defined in this file
	// tokenizerType = utils.LoadEnv(constants.EnvPrefixCacheTokenizerType, "character")

	// Check configuration dependencies
	// Only KV Event Sync constants are defined in pkg/constants
	var useRemoteTokenizer = utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)
	// Using constant from pkg/constants/kv_event_sync.go
	var kvSyncEnabled = utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)

	// Log configuration state
	klog.InfoS("prefix cache router configuration",
		"remote_tokenizer_requested", useRemoteTokenizer,
		"kv_sync_requested", kvSyncEnabled)

	// Preserve existing dependency logic: KV sync requires remote tokenizer
	if kvSyncEnabled && !useRemoteTokenizer {
		klog.Warning("KV event sync requires remote tokenizer. " +
			"Remote tokenizer will be automatically enabled.")
		useRemoteTokenizer = true
	}

	// Configure TokenizerPool if remote tokenizer is needed
	if useRemoteTokenizer {
		// Load pool configuration from environment
		poolConfig := loadTokenizerPoolConfigFromEnv()

		// Create default tokenizer based on configured type
		var defaultTokenizer = NewTokenizer()
		poolConfig.DefaultTokenizer = defaultTokenizer

		// Create the pool
		pool := NewTokenizerPool(poolConfig, c)
		tokenizerPool = pool

		// Use panic tokenizer to catch any direct usage
		// All tokenization should go through pool in route methods
		tokenizerObj = &panicTokenizer{}

		klog.Info("TokenizerPool initialized with remote tokenizer support")
	} else {
		// Fallback to local tokenizer (existing behavior when disabled)
		tokenizerObj = NewTokenizer()
	}

	// Log final configuration
	klog.InfoS("prefix_cache_configurations",
		"tokenizer_type", tokenizerType,
		"remote_tokenizer_enabled", tokenizerPool != nil,
		"kv_sync_enabled", kvSyncEnabled,
		"matched_pods_running_requests_standard_deviation_factor", standardDeviationFactor)

	// Create main router with local indexer
	if indexer == nil {
		indexer = prefixcacheindexer.NewPrefixHashTable()
	}
	router := prefixCacheRouter{
		cache:              c,
		tokenizer:          tokenizerObj,
		prefixCacheIndexer: indexer,
		// Only assign tokenizerPool if it's not nil to avoid interface nil issues
	}

	// Only set tokenizerPool if it was actually created
	if tokenizerPool != nil {
		router.tokenizerPool = tokenizerPool
	}

	// Only create KV sync router if enabled
	if kvSyncEnabled && useRemoteTokenizer && tokenizerPool != nil {
		kvSyncRouter := &kvSyncPrefixCacheRouter{
			cache:         c,
			tokenizerPool: tokenizerPool,
			syncIndexer:   syncindexer.GetSharedSyncPrefixHashTable(),
		}

		router.kvSyncRouter = kvSyncRouter

		if metrics := getPrefixCacheMetrics(); metrics != nil {
			metrics.prefixCacheIndexerStatus.WithLabelValues("_init_", "sync").Set(1)
			metrics.prefixCacheIndexerStatus.WithLabelValues("_init_", "local").Set(0)
		}
	} else if metrics := getPrefixCacheMetrics(); metrics != nil {
		metrics.prefixCacheIndexerStatus.WithLabelValues("_init_", "local").Set(1)
		metrics.prefixCacheIndexerStatus.WithLabelValues("_init_", "sync").Set(0)
	}

	return router, nil
}

// getTokenizerForRequest returns the appropriate tokenizer for the current request.
// This method encapsulates the conditional logic for choosing between the pool
// and the local tokenizer, ensuring model-aware tokenization when available.
func (p *prefixCacheRouter) getTokenizerForRequest(ctx *types.RoutingContext, readyPodList types.PodList) tokenizer.Tokenizer {
	// If pool exists, use model-specific tokenizer
	if p.tokenizerPool != nil {
		return p.tokenizerPool.GetTokenizer(ctx.Model, readyPodList.All())
	}

	// When pool is nil, return p.tokenizer
	// This is safe because:
	// 1. If useRemoteTokenizer=true, p.tokenizer is panicTokenizer, but p.tokenizerPool!=nil, so we won't reach here
	// 2. If useRemoteTokenizer=false, p.tokenizer is local tokenizer, which is what we want
	return p.tokenizer
}

func (k *kvSyncPrefixCacheRouter) getTokenizerForRequest(ctx *types.RoutingContext, readyPodList types.PodList) tokenizer.Tokenizer {
	if k.tokenizerPool != nil {
		return k.tokenizerPool.GetTokenizer(ctx.Model, readyPodList.All())
	}
	// This shouldn't happen as kvSyncRouter requires TokenizerPool
	return nil
}

// tokenizeInputText preserves local and legacy tokenizers while allowing remote
// tokenization to follow the request lifetime. The parent check also prevents a
// canceled chat-template attempt from starting the ordinary text fallback.
func tokenizeInputText(ctx *types.RoutingContext, tok tokenizer.Tokenizer) ([]byte, error) {
	if ctx.Context != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if contextual, ok := tok.(tokenizer.ContextTokenizer); ok {
			return contextual.TokenizeInputTextWithContext(ctx.Context, ctx.PrefixText())
		}
	}
	return tok.TokenizeInputText(ctx.PrefixText())
}

// cachedPrefixTokens tokenizes a request at most once. A blended route calls
// ScoreAll and then PostRouteUpdate, and a remote tokenizer would otherwise be
// asked for the same prompt twice.
func cachedPrefixTokens(ctx *types.RoutingContext, tokenize func() ([]byte, error)) ([]byte, error) {
	if tokens := ctx.PrefixTokens(); tokens != nil {
		return tokens, nil
	}
	tokens, err := tokenize()
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		tokens = []byte{} // so an empty result is cached too
	}
	ctx.SetPrefixTokens(tokens)
	return tokens, nil
}

func (p *prefixCacheRouter) tokenize(ctx *types.RoutingContext, readyPodList types.PodList) ([]byte, error) {
	return cachedPrefixTokens(ctx, func() ([]byte, error) {
		return tokenizeInputText(ctx, p.getTokenizerForRequest(ctx, readyPodList))
	})
}

func (p prefixCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	if p.kvSyncRouter != nil {
		return p.kvSyncRouter.Route(ctx, readyPodList)
	}
	// Original implementation unchanged
	return p.routeOriginal(ctx, readyPodList)
}

// routeOriginal preserves the exact original implementation for backward compatibility
func (p prefixCacheRouter) routeOriginal(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	startTime := time.Now()
	defer func() {
		if metrics := getPrefixCacheMetrics(); metrics != nil {
			metrics.prefixCacheRoutingLatency.WithLabelValues(ctx.Model, "false").Observe(time.Since(startTime).Seconds())
		}
	}()

	var prefixHashes []uint64
	var matchedPods map[string]int
	var targetPod *v1.Pod
	var selection string

	tokens, err := p.tokenize(ctx, readyPodList)
	if err != nil {
		recordRoutingError(ctx.Model, "tokenize_failed", false)
		return "", err
	}

	readyPods := readyPodList.All()

	// Compute request counts once; reused by pod selection and log calls.
	podRequestCount := routeutil.RequestCounts(ctx, p.cache, readyPods)

	matchedPods, prefixHashes = p.matchPrefix(ctx, readyPods, tokens, false)
	if klog.V(4).Enabled() {
		klog.V(4).InfoS("prefix_hashes", "request_id", ctx.RequestID, "prefix_hashes", prefixHashes, "cached_blocks", p.prefixCacheIndexer.Len())
	}

	if len(matchedPods) > 0 {
		// The request's resolved overrides carry the profile's value on top of
		// the process default (see ResolveRoutingOverrides).
		sigma := ctx.RoutingOverrides().PrefixCache.StandardDeviationFactor
		targetPod = getTargetPodFromMatchedPodsFromCounts(podRequestCount, readyPods, matchedPods, sigma)
		if targetPod != nil {
			selection = selectionPrefixMatch
		}
	}

	// no pod with prefix match, as a fallback select pod with least request count
	if len(matchedPods) == 0 || targetPod == nil {
		fallbackPod := routeutil.SelectTargetPodWithLeastRequestCountFromCounts(podRequestCount, readyPods)
		if fallbackPod != nil {
			targetPod = fallbackPod
			if len(matchedPods) > 0 {
				selection = "prefix_match_skipped"
			} else {
				selection = selectionLeastRequestFallback
			}
		}
	}
	if targetPod == nil {
		klog.V(4).InfoS("prefix_cache_no_pods_available", "request_id", ctx.RequestID)
		recordRoutingError(ctx.Model, "no_target_pod", false)
		return "", errors.New("no target pod found")
	}
	// Index the prompt only on a pod that admitted the request.
	if err := ctx.AdmitTarget(targetPod); err != nil {
		return "", err
	}

	targetPodKey := utils.GeneratePodKey(targetPod.Namespace, targetPod.Name)
	if len(prefixHashes) > 0 {
		p.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPodKey)
	}

	matchPercent := 0
	if percent, exists := matchedPods[targetPodKey]; exists {
		matchPercent = percent
	}

	klog.InfoS("prefix_cache_route",
		"request_id", ctx.RequestID,
		"target_pod", targetPod.Name,
		"selection", selection,
		"cache_hit", selection == selectionPrefixMatch,
		"prefix_overlap_pct", matchPercent,
		"request_count", podRequestCount[targetPodKey])
	recordRoutingDecision(ctx.Model, matchPercent, false)
	if selection != "" {
		recordRoutingSelection(ctx.Model, selection, false)
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

func (p prefixCacheRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	if p.kvSyncRouter != nil {
		return p.kvSyncRouter.PostRouteUpdate(ctx, readyPodList, targetPod)
	}

	tokens, err := p.tokenize(ctx, readyPodList)
	if err != nil {
		return err
	}

	targetPodKey := utils.GeneratePodKey(targetPod.Namespace, targetPod.Name)
	matchedPods, prefixHashes := p.matchPrefix(ctx, []*v1.Pod{targetPod}, tokens, false)

	matchPercent := matchedPods[targetPodKey]
	if matchPercent > 0 {
		klog.InfoS("prefix_cache_route",
			"request_id", ctx.RequestID,
			"target_pod", targetPod.Name,
			"prefix_overlap_pct", matchPercent)
	}

	if len(prefixHashes) > 0 {
		p.prefixCacheIndexer.AddPrefix(prefixHashes, ctx.Model, targetPodKey)
	}

	return nil
}

// indexContext returns the sync index context a request's blocks are kept
// under. KV events index an adapter's blocks under its base model, keyed by
// the adapter name, which is also the model name the request uses. Events take
// the base model from the pod's model label, so the adapter's baseModel must
// match that label or its requests won't find any blocks.
func (k *kvSyncPrefixCacheRouter) indexContext(model string) (string, int64) {
	if k.cache != nil {
		if base, ok := k.cache.ModelBaseModel(model); ok {
			return base, syncindexer.LoraIDFromName(model)
		}
	}
	return model, -1
}

func (k *kvSyncPrefixCacheRouter) PostRouteUpdate(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) error {
	pods := readyPodList.All()
	modelName := ctx.Model
	if modelName == "" && len(pods) > 0 {
		modelName, _ = constants.ModelNameFromMetadata(pods[0].Labels, pods[0].Annotations)
	}

	tokens, err := k.tokenize(ctx, readyPodList)
	if err != nil {
		return err
	}

	if k.syncIndexer == nil {
		return fmt.Errorf("sync indexer not available for KV sync routing")
	}
	indexModel, loraID := k.indexContext(modelName)
	_, prefixHashes := k.matchPrefix(ctx, pods, tokens, indexModel, loraID, false)
	if len(prefixHashes) == 0 {
		return nil
	}
	selectedPodKey := fmt.Sprintf("%s/%s", targetPod.Namespace, targetPod.Name)
	return k.syncIndexer.AddPrefix(indexModel, loraID, selectedPodKey, prefixHashes)
}

// ScoreAll traverses the Radix Tree to calculate the prefix match ratio (matched tokens / total tokens) for all ready pods.
// Unlike simple metric fetching, this dynamically calculates the score based on the specific request's input tokens.
func (p prefixCacheRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	return p.scoreAll(ctx, readyPodList, false)
}

func (p prefixCacheRouter) scoreAll(ctx *types.RoutingContext, readyPodList types.PodList, remember bool) ([]float64, []bool, error) {
	if p.kvSyncRouter != nil {
		return p.kvSyncRouter.scoreAll(ctx, readyPodList, remember)
	}
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	tokens, err := p.tokenize(ctx, readyPodList)
	if err != nil {
		return nil, nil, err
	}

	matchedPods, _ := p.matchPrefix(ctx, pods, tokens, remember)

	for i, pod := range pods {
		matchPercent := matchedPods[utils.GeneratePodKey(pod.Namespace, pod.Name)]
		scores[i] = float64(matchPercent)
		scored[i] = true
	}

	return scores, scored, nil
}

// CandidateScores implements types.CandidateScorer. Prefix matching reads only the in-memory
// prefix index, so these are the scores ScoreAll returns. The tokens and raw matches are kept on
// ctx for the rest of the decision (see types.SeededPrefixMatch), so scoring the candidates that
// survive the stage reuses this lookup instead of matching again.
func (p prefixCacheRouter) CandidateScores(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	return p.scoreAll(ctx, readyPodList, true)
}

// Polarity returns whether higher or lower score is better.
func (p prefixCacheRouter) Polarity() types.Polarity {
	return types.PolarityMost
}

// ScoreAll computes the scores for all ready pods in a single batch operation for KV sync router.
func (k *kvSyncPrefixCacheRouter) ScoreAll(ctx *types.RoutingContext, readyPodList types.PodList) ([]float64, []bool, error) {
	return k.scoreAll(ctx, readyPodList, false)
}

func (k *kvSyncPrefixCacheRouter) scoreAll(ctx *types.RoutingContext, readyPodList types.PodList, remember bool) ([]float64, []bool, error) {
	pods := readyPodList.All()
	scores := make([]float64, len(pods))
	scored := make([]bool, len(pods))

	modelName := ctx.Model
	if modelName == "" && len(pods) > 0 {
		modelName, _ = constants.ModelNameFromMetadata(pods[0].Labels, pods[0].Annotations)
	}

	indexModel, loraID := k.indexContext(modelName)

	tokens, err := k.tokenize(ctx, readyPodList)
	if err != nil {
		return nil, nil, err
	}

	if k.syncIndexer == nil {
		return nil, nil, fmt.Errorf("sync indexer not available for KV sync routing")
	}
	matchedPods, _ := k.matchPrefix(ctx, pods, tokens, indexModel, loraID, remember)

	for i, pod := range pods {
		podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
		matchPercent := matchedPods[podKey]
		scores[i] = float64(matchPercent)
		scored[i] = true
	}

	return scores, scored, nil
}

// Cleanup gracefully shuts down the TokenizerPool if it exists
// A value receiver supports the router value returned by the constructors.
func (p prefixCacheRouter) Cleanup() error {
	if p.tokenizerPool != nil {
		klog.Info("Shutting down TokenizerPool...")
		if err := p.tokenizerPool.Close(); err != nil {
			klog.Errorf("Error closing TokenizerPool: %v", err)
			return err
		}
	}
	return nil
}

// buildTokenizeInputFromChatRequest converts ChatCompletionRequest to TokenizeInput
// preserving multimodal content and vLLM-specific parameters. tools is the raw "tools"
// array of the request, forwarded so the chat template renders the tool definitions.
func buildTokenizeInputFromChatRequest(chatReq *types.ChatCompletionRequest, tools json.RawMessage) (*tokenizer.TokenizeInput, error) {
	if len(chatReq.Messages) == 0 {
		return nil, fmt.Errorf("no messages in chat completion request")
	}

	// Convert OpenAI messages to tokenizer messages, preserving content structure
	messages := make([]tokenizer.ChatMessage, len(chatReq.Messages))
	for i, msg := range chatReq.Messages {
		role := msg.GetRole()
		if role == nil {
			return nil, fmt.Errorf("message at index %d has no role", i)
		}

		// Directly marshal the content to preserve its structure
		content := msg.GetContent()
		contentAny := content.AsAny()
		if contentAny == nil {
			return nil, fmt.Errorf("message at index %d has nil content", i)
		}
		contentJSON, err := json.Marshal(contentAny)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal message content at index %d: %w", i, err)
		}

		messages[i] = tokenizer.ChatMessage{
			Role:    *role,
			Content: contentJSON,
		}
	}

	// Extract vLLM-specific parameters with defaults matching vLLM behavior
	addSpecialTokens := false
	if chatReq.AddSpecialTokens != nil {
		addSpecialTokens = *chatReq.AddSpecialTokens
	}

	addGenerationPrompt := true
	if chatReq.AddGenerationPrompt != nil {
		addGenerationPrompt = *chatReq.AddGenerationPrompt
	}

	returnTokenStrings := false
	if chatReq.ReturnTokenStrings != nil {
		returnTokenStrings = *chatReq.ReturnTokenStrings
	}

	return &tokenizer.TokenizeInput{
		Type:                tokenizer.ChatInput,
		Messages:            messages,
		AddSpecialTokens:    addSpecialTokens,
		AddGenerationPrompt: addGenerationPrompt,
		ReturnTokenStrings:  returnTokenStrings,
		Tools:               tools,
	}, nil
}

// rawChatTools returns the "tools" array of a chat request body exactly as sent, or nil
// when the field is absent, null or not an array. The raw bytes are forwarded instead of
// re-encoding the parsed request, which would drop fields the OpenAI types do not model.
func rawChatTools(body []byte) json.RawMessage {
	if !bytes.Contains(body, []byte(`"tools"`)) {
		return nil
	}
	var req struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	tools := bytes.TrimSpace(req.Tools)
	if len(tools) == 0 || tools[0] != '[' {
		return nil
	}
	return tools
}

// tokenizeChatRequest attempts to tokenize a chat completion request using chat template.
// Returns the tokenized bytes on success, or nil if tokenization should fall back to text mode.
// All errors are logged internally at V(4) level.
func (k *kvSyncPrefixCacheRouter) tokenizeChatRequest(ctx *types.RoutingContext, tokenizerToUse tokenizer.Tokenizer) []byte {
	// Check if tokenizer supports extended features
	extTokenizer, ok := tokenizerToUse.(tokenizer.ExtendedTokenizer)
	if !ok {
		klog.V(4).InfoS("tokenizer does not support ExtendedTokenizer, using text tokenization",
			"request_id", ctx.RequestID)
		return nil
	}

	// Parse request body as ChatCompletionRequest
	var chatReq types.ChatCompletionRequest
	if err := json.Unmarshal(ctx.ReqBody, &chatReq); err != nil {
		klog.V(4).InfoS("failed to parse chat request, falling back to text",
			"request_id", ctx.RequestID,
			"error", err)
		return nil
	}

	if len(chatReq.Messages) == 0 {
		klog.V(4).InfoS("no messages in chat request, falling back to text",
			"request_id", ctx.RequestID)
		return nil
	}

	// Build TokenizeInput from request
	input, err := buildTokenizeInputFromChatRequest(&chatReq, rawChatTools(ctx.ReqBody))
	if err != nil {
		klog.V(4).InfoS("failed to build tokenize input, falling back to text",
			"request_id", ctx.RequestID,
			"error", err)
		return nil
	}

	// Perform tokenization with chat template
	result, err := extTokenizer.TokenizeWithOptions(ctx.Context, *input)
	if err != nil {
		klog.V(4).InfoS("chat tokenization failed, falling back to text",
			"request_id", ctx.RequestID,
			"error", err)
		return nil
	}

	// Success - log and return tokens
	tokens := tokenizer.IntToByteArray(result.Tokens)
	klog.V(4).InfoS("tokenized using chat template",
		"request_id", ctx.RequestID,
		"message_count", len(input.Messages),
		"tools_included", len(input.Tools) > 0,
		"token_count", len(result.Tokens),
		"add_generation_prompt", input.AddGenerationPrompt,
		"add_special_tokens", input.AddSpecialTokens)
	return tokens
}

// tokenize renders chat requests through the chat template, as the engine does,
// so the hashes line up with the blocks it reports in KV events. Route, ScoreAll
// and PostRouteUpdate must all tokenize this way.
func (k *kvSyncPrefixCacheRouter) tokenize(ctx *types.RoutingContext, readyPodList types.PodList) ([]byte, error) {
	return cachedPrefixTokens(ctx, func() ([]byte, error) {
		tok := k.getTokenizerForRequest(ctx, readyPodList)
		if tok == nil {
			return nil, fmt.Errorf("TokenizerPool not initialized for KV sync router")
		}
		if utils.PathWithoutQuery(ctx.ReqPath) == "/v1/chat/completions" {
			if tokens := k.tokenizeChatRequest(ctx, tok); tokens != nil {
				return tokens, nil
			}
		}
		return tokenizeInputText(ctx, tok)
	})
}

// Route handles KV sync routing with clean implementation
func (k *kvSyncPrefixCacheRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	var prefixHashes []uint64
	var matchedPods map[string]int
	var targetPod *v1.Pod
	var selection string

	// Get model information from context
	modelName := ctx.Model
	allPods := readyPodList.All()
	if modelName == "" && len(allPods) > 0 {
		modelName, _ = constants.ModelNameFromMetadata(allPods[0].Labels, allPods[0].Annotations)
	}

	startTime := time.Now()
	defer func() {
		if metrics := getPrefixCacheMetrics(); metrics != nil {
			metrics.prefixCacheRoutingLatency.WithLabelValues(modelName, "true").Observe(time.Since(startTime).Seconds())
		}
	}()

	indexModel, loraID := k.indexContext(modelName)

	tokens, err := k.tokenize(ctx, readyPodList)
	if err != nil {
		recordRoutingError(modelName, "tokenize_failed", true)
		return "", err
	}

	readyPods := readyPodList.All()

	// Match prefixes using sync indexer
	if k.syncIndexer == nil {
		recordRoutingError(modelName, "sync_indexer_unavailable", true)
		return "", fmt.Errorf("sync indexer not available for KV sync routing")
	}
	matchedPods, prefixHashes = k.matchPrefix(ctx, readyPods, tokens, indexModel, loraID, false)

	podRequestCount := routeutil.RequestCounts(ctx, k.cache, readyPods)

	if klog.V(4).Enabled() {
		klog.V(4).InfoS("prefix cache matching completed",
			"model", modelName,
			"lora_id", loraID,
			"matched_pods", len(matchedPods),
			"prefix_hashes", len(prefixHashes),
			"ready_pods", readyPodList.Len())
	}

	if len(matchedPods) > 0 {
		// The request's resolved overrides carry the profile's value on top of
		// the process default (see ResolveRoutingOverrides).
		sigma := ctx.RoutingOverrides().PrefixCache.StandardDeviationFactor
		targetPod = getTargetPodFromMatchedPodsFromCounts(podRequestCount, readyPods, matchedPods, sigma)
		if targetPod != nil {
			selection = selectionPrefixMatch
			klog.V(4).InfoS("prefix_cache_matched_pods",
				"request_id", ctx.RequestID,
				"target_pod", targetPod.Name,
				"target_pod_ip", targetPod.Status.PodIP,
				"matched_pods", matchedPods,
				"pod_request_count", podRequestCount)
		} else {
			klog.V(4).InfoS("prefix_cache_skip_matched_pods",
				"request_id", ctx.RequestID,
				"matched_pods", matchedPods,
				"pod_request_count", podRequestCount)
		}
	}

	// Fallback to least request count selection
	if len(matchedPods) == 0 || targetPod == nil {
		targetPod = routeutil.SelectTargetPodWithLeastRequestCountFromCounts(podRequestCount, readyPods)
		if targetPod != nil {
			if len(matchedPods) > 0 {
				selection = "prefix_match_skipped"
			} else {
				selection = selectionLeastRequestFallback
			}
			klog.V(4).InfoS("prefix_cache_fallback_least_request_count",
				"request_id", ctx.RequestID,
				"target_pod", targetPod.Name,
				"target_pod_ip", targetPod.Status.PodIP,
				"matched_pods", matchedPods,
				"pod_request_count", podRequestCount)
		}
	}

	// Handle case where no pods are available
	if targetPod == nil {
		recordRoutingError(modelName, "no_target_pod", true)
		return "", fmt.Errorf("no ready pods available for routing")
	}
	// Index the prompt only on a pod that admitted the request.
	if err := ctx.AdmitTarget(targetPod); err != nil {
		return "", err
	}

	selectedPodKey := fmt.Sprintf("%s/%s", targetPod.Namespace, targetPod.Name)

	// Add prefix to sync indexer if we have prefixes
	if len(prefixHashes) > 0 {
		_ = k.syncIndexer.AddPrefix(indexModel, loraID, selectedPodKey, prefixHashes)
	}

	matchPercent := 0
	if len(matchedPods) > 0 {
		if percent, exists := matchedPods[selectedPodKey]; exists {
			matchPercent = percent
		}
	}
	recordRoutingDecision(modelName, matchPercent, true)
	if selection != "" {
		recordRoutingSelection(modelName, selection, true)
	}

	ctx.SetTargetPod(targetPod)
	return ctx.TargetAddress(), nil
}

// getTargetPodFromMatchedPods selects among matchedPods, keyed by pod key (namespace/name).
// stdDevFactor is how many standard deviations above the mean replica request count a candidate
// may sit before it is skipped: the environment default, or the request profile's override.
func getTargetPodFromMatchedPods(cache cache.Cache, readyPods []*v1.Pod, matchedPods map[string]int, stdDevFactor int) *v1.Pod {
	return getTargetPodFromMatchedPodsFromCounts(routeutil.GetRequestCounts(cache, readyPods), readyPods, matchedPods, stdDevFactor)
}

// getTargetPodFromMatchedPodsFromCounts is getTargetPodFromMatchedPods with request counts the
// caller already has, keyed by pod key like matchedPods. A candidate above the bounded-load cap
// (see routeutil.LoadCap) is skipped as well, since the stddev check cannot skip any pod in a
// pool of five or fewer at a factor of 2.
func getTargetPodFromMatchedPodsFromCounts(podRequestCount map[string]int, readyPods []*v1.Pod, matchedPods map[string]int, stdDevFactor int) *v1.Pod {
	var targetPodKey string
	requestCount := make([]float64, 0, len(podRequestCount))

	for _, cnt := range podRequestCount {
		requestCount = append(requestCount, float64(cnt))
	}
	meanRequestCount := routeutil.Mean(requestCount)
	stdDevRequestCount := routeutil.StandardDeviation(requestCount)
	loadCap := routeutil.LoadCap(meanRequestCount, loadCapFactor)

	podKeys := make([]string, 0, len(matchedPods))
	for podKey := range matchedPods {
		podKeys = append(podKeys, podKey)
	}
	rand.Shuffle(len(podKeys), func(i, j int) {
		podKeys[i], podKeys[j] = podKeys[j], podKeys[i]
	})

	// sort pods with decreasing %prefixmatch AND for same %prefixmatch sort by increasing request count
	sort.SliceStable(podKeys, func(i, j int) bool {
		if matchedPods[podKeys[i]] == matchedPods[podKeys[j]] {
			return podRequestCount[podKeys[i]] < podRequestCount[podKeys[j]]
		}
		return matchedPods[podKeys[i]] > matchedPods[podKeys[j]]
	})

	// select targetpod with highest %prefixmatch and request_count within stddev and the load cap
	for _, podKey := range podKeys {
		reqCnt := float64(podRequestCount[podKey])
		if reqCnt <= meanRequestCount+float64(stdDevFactor)*stdDevRequestCount && reqCnt <= loadCap {
			targetPodKey = podKey
			break
		}
	}
	return routeutil.PodsByKey(readyPods)[targetPodKey]
}

// recordRoutingDecision records metrics for routing decisions
func recordRoutingDecision(model string, matchPercent int, usingKVSync bool) {
	metrics := getPrefixCacheMetrics()
	if metrics == nil {
		return
	}

	var bucket string
	switch {
	case matchPercent == 0:
		bucket = "0"
	case matchPercent <= 25:
		bucket = "1-25"
	case matchPercent <= 50:
		bucket = "26-50"
	case matchPercent <= 75:
		bucket = "51-75"
	default:
		bucket = "76-100"
	}

	metrics.prefixCacheRoutingDecisions.WithLabelValues(model, bucket, strconv.FormatBool(usingKVSync)).Inc()
}

func recordRoutingSelection(model, selection string, usingKVSync bool) {
	metrics := getPrefixCacheMetrics()
	if metrics == nil {
		return
	}
	metrics.prefixCacheRoutingSelection.WithLabelValues(model, selection, strconv.FormatBool(usingKVSync)).Inc()
}

func recordRoutingError(model, reason string, usingKVSync bool) {
	metrics := getPrefixCacheMetrics()
	if metrics == nil {
		return
	}
	metrics.prefixCacheRoutingErrors.WithLabelValues(model, reason, strconv.FormatBool(usingKVSync)).Inc()
}
