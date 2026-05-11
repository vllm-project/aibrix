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

package routingalgorithms

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vllm-project/aibrix/pkg/types"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	RouterNotSet = ""
)

var (
	ErrInitTimeout           = errors.New("router initialization timeout")
	ErrFallbackNotSupported  = errors.New("router not support fallback")
	ErrFallbackNotRegistered = errors.New("fallback router not registered")
	defaultRM                = NewRouterManager()
)

// RouterItem represents a single routing algorithm and its weight coefficient for multi-router config.
type RouterItem struct {
	Name        string
	Coefficient int // Integer weight coefficient (0 to 1000000)
}

// MultiRouterConfig holds the parsed routing algorithms and their weight coefficients.
type MultiRouterConfig struct {
	Items []RouterItem
}

// ParseMultiRouterConfig parses a multi-router string into a MultiRouterConfig.
// Format example: "prefix-cache:2,least-latency:1,least-request"
// - Weight coefficients must be integers in range [0, 1000000].
// - Default weight coefficient is 1 if omitted.
// - Weight coefficient 0 means the routing algorithm should be skipped.
func ParseMultiRouterConfig(routerStr string) (*MultiRouterConfig, error) {
	if routerStr == "" {
		return nil, errors.New("empty routing algorithm")
	}

	parts := strings.Split(routerStr, ",")
	if len(parts) == 0 {
		return nil, errors.New("invalid routing algorithm format")
	}

	var items []RouterItem

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, errors.New("empty algorithm name")
		}

		name := part
		coefInt := 1 // Default weight coefficient is 1

		if strings.Contains(part, ":") {
			subParts := strings.Split(part, ":")
			if len(subParts) != 2 {
				return nil, fmt.Errorf("invalid algorithm format: %s", part)
			}
			name = strings.TrimSpace(subParts[0])
			if name == "" {
				return nil, fmt.Errorf("empty algorithm name in: %s", part)
			}

			coefStr := strings.TrimSpace(subParts[1])
			parsedCoef, err := strconv.Atoi(coefStr)
			if err != nil {
				return nil, fmt.Errorf("invalid weight coefficient in: %s (must be an integer)", part)
			}
			if parsedCoef < 0 || parsedCoef > 1000000 {
				return nil, fmt.Errorf("weight coefficient out of bounds [0, 1000000] in: %s", part)
			}
			coefInt = parsedCoef
		}

		// If weight coefficient is 0, we skip this algorithm entirely
		if coefInt > 0 {
			items = append(items, RouterItem{Name: name, Coefficient: coefInt})
		}
	}

	if len(items) == 0 {
		return nil, errors.New("no valid algorithms found (all weight coefficients were 0)")
	}

	// Check for exclusive strategies (pd, slo*)
	// If found, we ignore other strategies and return just the exclusive one
	// If multiple exclusive strategies are found, the first one encountered wins
	for _, item := range items {
		if item.Name == "pd" || strings.HasPrefix(item.Name, "slo") {
			klog.Infof("Exclusive routing strategy '%s' found in multi-strategy config. Other strategies will be ignored.", item.Name)
			return &MultiRouterConfig{Items: []RouterItem{{Name: item.Name, Coefficient: 1}}}, nil
		}
	}

	return &MultiRouterConfig{Items: items}, nil
}

// multiStrategyRouter coordinates multiple sub-routers and selects a pod via soft-scoring
type multiStrategyRouter struct {
	config  *MultiRouterConfig
	scorers map[string]types.PodScorer
}

// newMultiStrategyRouter initializes a new multiStrategyRouter based on the parsed config
func newMultiStrategyRouter(config *MultiRouterConfig, rm *RouterManager, ctx *types.RoutingContext) (*multiStrategyRouter, error) {
	scorers := make(map[string]types.PodScorer)

	for _, item := range config.Items {
		rm.routerMu.RLock()
		provider, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]
		rm.routerMu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("strategy %s not registered", item.Name)
		}

		router, err := provider(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize strategy %s: %v", item.Name, err)
		}

		scorer, ok := router.(types.PodScorer)
		if !ok {
			return nil, fmt.Errorf("strategy %s does not implement types.PodScorer interface", item.Name)
		}
		scorers[item.Name] = scorer
	}

	return &multiStrategyRouter{
		config:  config,
		scorers: scorers,
	}, nil
}

// Route executes the multi-strategy scoring pipeline and returns the best pod IP
func (m *multiStrategyRouter) Route(ctx *types.RoutingContext, readyPodList types.PodList) (string, error) {
	pods := readyPodList.All()
	if len(pods) == 0 {
		return "", errors.New("empty pod list")
	}

	if len(pods) == 1 {
		ctx.SetTargetPod(pods[0])
		m.setTargetPortIfNeeded(ctx, readyPodList, pods[0])
		m.runPostRouteUpdates(ctx, readyPodList, pods[0])
		return ctx.TargetAddress(), nil
	}

	topPod, _, err := m.scoreAndRank(ctx, readyPodList)
	if err != nil {
		return "", err
	}

	// Store target pod for updating cache if needed
	ctx.SetTargetPod(topPod)
	m.setTargetPortIfNeeded(ctx, readyPodList, topPod)
	m.runPostRouteUpdates(ctx, readyPodList, topPod)

	return ctx.TargetAddress(), nil
}

func (m *multiStrategyRouter) setTargetPortIfNeeded(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) {
	if !isMultiPortPods(readyPodList.All()) {
		return
	}
	scorer, ok := m.scorers[string(RouterLeastRequest)]
	if !ok {
		return
	}
	leastRequest, ok := scorer.(*leastRequestRouter)
	if !ok {
		return
	}
	if port := selectTargetPortForPodWithLeastRequestCount(leastRequest.cache, targetPod, readyPodList.ListPortsForPod()); port != 0 {
		ctx.SetTargetPort(port)
	}
}

func (m *multiStrategyRouter) runPostRouteUpdates(ctx *types.RoutingContext, readyPodList types.PodList, targetPod *v1.Pod) {
	for _, item := range m.config.Items {
		scorer := m.scorers[item.Name]
		updater, ok := scorer.(types.PostRouteUpdater)
		if !ok {
			continue
		}
		if err := updater.PostRouteUpdate(ctx, readyPodList, targetPod); err != nil {
			klog.Warningf("post-route update for strategy %s failed: %v", item.Name, err)
		}
	}
}

// scoreAndRank calculates final scores for all pods and returns the winner
func (m *multiStrategyRouter) scoreAndRank(ctx *types.RoutingContext, readyPodList types.PodList) (*v1.Pod, map[*v1.Pod]float64, error) {
	pods := readyPodList.All()
	finalScores := make(map[*v1.Pod]float64)
	logEnabled := klog.V(4).Enabled()

	// To collect diagnostic info for klog
	type podDiag struct {
		StrategyLog []string
	}
	var diags map[*v1.Pod]*podDiag
	if logEnabled {
		diags = make(map[*v1.Pod]*podDiag)
		for _, pod := range pods {
			diags[pod] = &podDiag{}
		}
	}

	// Calculate total weight to act as denominator
	totalWeight := 0.0
	for _, item := range m.config.Items {
		totalWeight += float64(item.Coefficient)
	}

	if totalWeight <= 0 {
		return nil, nil, errors.New("total weight must be greater than zero")
	}

	// Iterate over each sub-strategy
	for _, item := range m.config.Items {
		scorer := m.scorers[item.Name]

		// 1. Collect raw scores
		scores, scored, err := scorer.ScoreAll(ctx, readyPodList)
		if err != nil {
			klog.Warningf("Strategy %s failed to score: %v", item.Name, err)
			scored = make([]bool, len(pods)) // all false
			scores = make([]float64, len(pods))
		}

		// 2. Normalize to [0, 1] based on polarity
		normScores := m.normalizeScoresArray(scores, scored, scorer.Polarity())

		// 3. Aggregate into final sum
		weightFraction := float64(item.Coefficient) / totalWeight
		for i, pod := range pods {
			weightedScore := normScores[i] * weightFraction
			finalScores[pod] += weightedScore

			if logEnabled {
				// Record diagnostic information for this pod and strategy
				rawScoreStr := "N/A"
				if scored[i] {
					rawScoreStr = fmt.Sprintf("%.2f", scores[i])
				}
				diagStr := fmt.Sprintf("%s(raw:%s, norm:%.2f, weight:%.3f)", item.Name, rawScoreStr, normScores[i], weightedScore)
				diags[pod].StrategyLog = append(diags[pod].StrategyLog, diagStr)
			}
		}
	}

	// 4. Find the pod with the highest score
	var topPods []*v1.Pod
	maxScore := -1.0

	for _, pod := range pods {
		score := finalScores[pod]
		// handle floating point precision
		if math.Abs(score-maxScore) < 1e-9 {
			topPods = append(topPods, pod)
		} else if score > maxScore {
			maxScore = score
			topPods = []*v1.Pod{pod}
		}
	}

	if len(topPods) == 0 {
		return nil, nil, errors.New("no valid target pod found after scoring")
	}

	// Tie-break: select the first pod in the shuffled ready list
	winner := topPods[0]

	// 5. Log the routing decision and all candidate metrics to klog
	if logEnabled {
		var logBuilder strings.Builder
		logBuilder.WriteString(fmt.Sprintf("Multi-strategy routing decision for request [%s]. Selected target pod: [%s]. Candidate pod details:\n", ctx.RequestID, winner.Name))
		for _, pod := range pods {
			winnerFlag := " "
			if pod.Name == winner.Name {
				winnerFlag = "*"
			}
			logBuilder.WriteString(fmt.Sprintf("  [%s] Pod: %-30s | FinalScore: %.4f | Details: %s\n",
				winnerFlag, pod.Name, finalScores[pod], strings.Join(diags[pod].StrategyLog, ", ")))
		}
		klog.V(4).Info(logBuilder.String())
	}

	return winner, finalScores, nil
}

// normalizeScoresArray maps raw values to a [0, 1] scale.
func (m *multiStrategyRouter) normalizeScoresArray(scores []float64, scored []bool, polarity types.Polarity) []float64 {
	normScores := make([]float64, len(scores))

	minVal := math.MaxFloat64
	maxVal := -math.MaxFloat64
	scoredCount := 0

	// Find min and max for scored pods
	for i, isScored := range scored {
		if isScored && isFiniteScore(scores[i]) {
			if scores[i] < minVal {
				minVal = scores[i]
			}
			if scores[i] > maxVal {
				maxVal = scores[i]
			}
			scoredCount++
		}
	}

	if scoredCount == 0 {
		return normScores // all 0.0
	}

	for i, isScored := range scored {
		if !isScored || !isFiniteScore(scores[i]) {
			normScores[i] = 0.0 // worst score for unscored pods
			continue
		}

		if maxVal == minVal {
			normScores[i] = 1.0 // all scored pods get max score if there's no difference
			continue
		}

		if polarity == types.PolarityLeast {
			// Reverse score if smaller is better: (max - s) / (max - min)
			normScores[i] = (maxVal - scores[i]) / (maxVal - minVal)
		} else {
			// Higher is better: (s - min) / (max - min)
			normScores[i] = (scores[i] - minVal) / (maxVal - minVal)
		}
	}

	return normScores
}

func isFiniteScore(score float64) bool {
	return !math.IsNaN(score) && !math.IsInf(score, 0)
}

type RouterManager struct {
	routerInited      context.Context
	routerDoneInit    context.CancelFunc
	routerFactory     map[types.RoutingAlgorithm]types.RouterProviderFunc
	routerConstructor map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc
	multiRouterCache  map[string]*multiStrategyRouter
	routerMu          sync.RWMutex
}

func NewRouterManager() *RouterManager {
	rm := &RouterManager{}
	rm.routerInited, rm.routerDoneInit = context.WithTimeout(context.Background(), 5*time.Second)
	rm.routerFactory = make(map[types.RoutingAlgorithm]types.RouterProviderFunc)
	rm.routerConstructor = make(map[types.RoutingAlgorithm]types.RouterProviderRegistrationFunc)
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	return rm
}

// Validate validates if user provided routing routers is supported by gateway
func (rm *RouterManager) Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	// Parse the strategy configuration using multi-router parsing logic
	cfg, err := ParseMultiRouterConfig(algorithms)
	if err != nil {
		return RouterNotSet, false
	}

	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()

	// Validate each strategy in the configuration
	for _, item := range cfg.Items {
		provider, ok := rm.routerFactory[types.RoutingAlgorithm(item.Name)]
		if !ok {
			return RouterNotSet, false
		}
		if len(cfg.Items) > 1 {
			if provider == nil {
				return RouterNotSet, false
			}
			router, err := provider(types.RoutingAlgorithm(algorithms).NewContext(context.Background(), "", "", "validate", ""))
			if err != nil {
				return RouterNotSet, false
			}
			if _, ok := router.(types.PodScorer); !ok {
				return RouterNotSet, false
			}
		}
	}

	// Return the original algorithms string to keep it intact in headers
	return types.RoutingAlgorithm(algorithms), true
}
func Validate(algorithms string) (types.RoutingAlgorithm, bool) {
	return defaultRM.Validate(algorithms)
}

// Select the user provided router provider supported by gateway, no error reported and fallback to random router
// Call Validate before this function to ensure expected behavior.
func (rm *RouterManager) Select(ctx *types.RoutingContext) (types.Router, error) {
	algStr := string(ctx.Algorithm)

	// Check if it's a multi-strategy config.
	cfg, err := ParseMultiRouterConfig(algStr)
	if err == nil {
		if len(cfg.Items) > 1 {
			multiRouter, err := rm.getOrCreateMultiStrategyRouter(algStr, cfg, ctx)
			if err == nil {
				return multiRouter, nil
			}
			// If multi-router initialization fails (e.g. strategy doesn't implement ScoreAll),
			// we log a debug message and fall back to the traditional single strategy provider below.
			klog.V(4).Infof("Cannot use multi-strategy router for %s: %v, falling back to legacy single strategy", algStr, err)
		}

		// If the parser recognized exactly one valid strategy (e.g. it was an exclusive strategy like "pd"
		// or just a single strategy that doesn't support ScoreAll), we fallback to that specific strategy.
		if len(cfg.Items) == 1 {
			algStr = cfg.Items[0].Name
		}
	} else {
		// Log the error but don't fallback to Random. Allow the request to fail down the line,
		// preserving the HTTP 400 Bad Request API contract.
		klog.Warningf("Failed to parse multi-strategy config '%s': %v", algStr, err)
	}

	// Legacy Single strategy fallback
	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()
	if provider, ok := rm.routerFactory[types.RoutingAlgorithm(algStr)]; ok {
		return provider(ctx)
	} else {
		// Return an error rather than falling back to random to preserve 400 Bad Request
		return nil, fmt.Errorf("unsupported router strategy: %s", algStr)
	}
}
func Select(ctx *types.RoutingContext) (types.Router, error) {
	return defaultRM.Select(ctx)
}

func (rm *RouterManager) getOrCreateMultiStrategyRouter(algStr string, cfg *MultiRouterConfig, ctx *types.RoutingContext) (*multiStrategyRouter, error) {
	rm.routerMu.RLock()
	if router, ok := rm.multiRouterCache[algStr]; ok {
		rm.routerMu.RUnlock()
		return router, nil
	}
	rm.routerMu.RUnlock()

	router, err := newMultiStrategyRouter(cfg, rm, ctx)
	if err != nil {
		return nil, err
	}

	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	if cached, ok := rm.multiRouterCache[algStr]; ok {
		return cached, nil
	}
	rm.multiRouterCache[algStr] = router
	return router, nil
}

func (rm *RouterManager) Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.routerConstructor[algorithm] = func() types.RouterProviderFunc {
		router, err := constructor()
		if err != nil {
			klog.Errorf("Failed to construct router for %s: %v", algorithm, err)
			return nil
		}
		return func(_ *types.RoutingContext) (types.Router, error) {
			return router, nil
		}
	}
}
func Register(algorithm types.RoutingAlgorithm, constructor types.RouterConstructor) {
	defaultRM.Register(algorithm, constructor)
}

func (rm *RouterManager) RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	rm.routerFactory[algorithm] = provider
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	klog.Infof("Registered router for %s", algorithm)
}
func RegisterProvider(algorithm types.RoutingAlgorithm, provider types.RouterProviderFunc) {
	defaultRM.RegisterProvider(algorithm, provider)
}

func (rm *RouterManager) SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	r, ok := router.(types.FallbackRouter)
	if !ok {
		return ErrFallbackNotSupported
	}

	<-rm.routerInited.Done()
	initErr := rm.routerInited.Err()
	if initErr != context.Canceled {
		return fmt.Errorf("router did not initialized: %v", initErr)
	}

	rm.routerMu.RLock()
	defer rm.routerMu.RUnlock()

	if provider, ok := rm.routerFactory[fallback]; !ok {
		return ErrFallbackNotRegistered
	} else {
		r.SetFallback(fallback, provider)
	}
	return nil
}
func SetFallback(router types.Router, fallback types.RoutingAlgorithm) error {
	return defaultRM.SetFallback(router, fallback)
}

func (rm *RouterManager) Init() {
	rm.routerMu.Lock()
	defer rm.routerMu.Unlock()
	for algorithm, constructor := range rm.routerConstructor {
		rm.routerFactory[algorithm] = constructor()
		klog.Infof("Registered router for %s", algorithm)
	}
	rm.multiRouterCache = make(map[string]*multiStrategyRouter)
	rm.routerDoneInit()
}
func Init() {
	defaultRM.Init()
}
