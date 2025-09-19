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
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils/tokenizer"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

const (
	// vllmEngine is the constant for vLLM inference engine
	vllmEngine = "vllm"
)

// TokenizerPoolConfig represents configuration for the TokenizerPool
type TokenizerPoolConfig struct {
	EnableVLLMRemote     bool                // Feature flag
	EndpointTemplate     string              // "http://%s:8000"
	HealthCheckPeriod    time.Duration       // Default: 30s
	TokenizerTTL         time.Duration       // Default: 5m
	MaxTokenizersPerPool int                 // Default: 100
	DefaultTokenizer     tokenizer.Tokenizer // Default when remote fails
	ModelServiceMap      map[string]string   // Model -> Service endpoint mapping
	Timeout              time.Duration       // Request timeout
}

// tokenizerEntry represents a cached tokenizer with metadata
type tokenizerEntry struct {
	tokenizer    tokenizer.Tokenizer
	endpoint     string
	lastUsed     time.Time
	lastHealthy  time.Time
	healthStatus bool
}

// TokenizerPool manages model-specific tokenizers with caching and health checking
type TokenizerPool struct {
	mu                sync.RWMutex
	tokenizers        map[string]*tokenizerEntry // model -> tokenizer mapping
	config            TokenizerPoolConfig
	cache             cache.Cache           // for pod discovery
	metrics           *TokenizerPoolMetrics // Can be nil when feature disabled
	metricsRegistered bool
	stopCh            chan struct{}
}

// TokenizerPoolMetrics contains Prometheus metrics for the pool
type TokenizerPoolMetrics struct {
	activeTokenizers           prometheus.Gauge
	tokenizerCreationSuccesses prometheus.Counter
	tokenizerCreationFailures  prometheus.Counter
	unhealthyTokenizers        prometheus.Counter
	tokenizerRequests          *prometheus.CounterVec
	tokenizerLatency           *prometheus.HistogramVec
}

// createTokenizerPoolMetrics creates metrics only when needed
func createTokenizerPoolMetrics() *TokenizerPoolMetrics {
	return &TokenizerPoolMetrics{
		activeTokenizers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aibrix_tokenizer_pool_active_tokenizers",
			Help: "Number of active tokenizers in the pool",
		}),
		tokenizerCreationSuccesses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aibrix_tokenizer_pool_creation_successes_total",
			Help: "Total number of successful tokenizer creations",
		}),
		tokenizerCreationFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aibrix_tokenizer_pool_creation_failures_total",
			Help: "Total number of failed tokenizer creations",
		}),
		unhealthyTokenizers: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aibrix_tokenizer_pool_unhealthy_tokenizers_total",
			Help: "Total number of times tokenizers were marked unhealthy",
		}),
		tokenizerRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aibrix_tokenizer_pool_requests_total",
			Help: "Total number of tokenizer requests by model",
		}, []string{"model"}),
		tokenizerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aibrix_tokenizer_pool_latency_seconds",
			Help:    "Tokenizer request latency in seconds",
			Buckets: prometheus.DefBuckets,
		}, []string{"model"}),
	}
}

// register registers metrics with Prometheus
func (m *TokenizerPoolMetrics) register() error {
	collectors := []prometheus.Collector{
		m.activeTokenizers,
		m.tokenizerCreationSuccesses,
		m.tokenizerCreationFailures,
		m.unhealthyTokenizers,
		m.tokenizerRequests,
		m.tokenizerLatency,
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

// unregister removes metrics from Prometheus registry
func (m *TokenizerPoolMetrics) unregister() {
	// Best effort unregistration
	prometheus.Unregister(m.activeTokenizers)
	prometheus.Unregister(m.tokenizerCreationSuccesses)
	prometheus.Unregister(m.tokenizerCreationFailures)
	prometheus.Unregister(m.unhealthyTokenizers)
	prometheus.Unregister(m.tokenizerRequests)
	prometheus.Unregister(m.tokenizerLatency)
}

// NewTokenizerPool creates a new TokenizerPool instance
func NewTokenizerPool(config TokenizerPoolConfig, cache cache.Cache) *TokenizerPool {
	pool := &TokenizerPool{
		tokenizers: make(map[string]*tokenizerEntry),
		config:     config,
		cache:      cache,
		stopCh:     make(chan struct{}),
	}

	// Only create and register metrics if feature is enabled
	if config.EnableVLLMRemote {
		metrics := createTokenizerPoolMetrics()
		if err := metrics.register(); err != nil {
			klog.Errorf("Failed to register tokenizer pool metrics: %v", err)
			// Continue without metrics rather than failing
		} else {
			pool.metrics = metrics
			pool.metricsRegistered = true
		}

		// Start health checker only if enabled
		if config.HealthCheckPeriod > 0 {
			pool.startHealthChecker()
		}
	}

	return pool
}

// Helper methods for safe metric operations
func (p *TokenizerPool) incActiveTokenizers() {
	if p.metrics != nil {
		p.metrics.activeTokenizers.Inc()
	}
}

func (p *TokenizerPool) decActiveTokenizers() {
	if p.metrics != nil {
		p.metrics.activeTokenizers.Dec()
	}
}

func (p *TokenizerPool) setActiveTokenizers(count float64) {
	if p.metrics != nil {
		p.metrics.activeTokenizers.Set(count)
	}
}

func (p *TokenizerPool) incTokenizerCreationSuccesses() {
	if p.metrics != nil {
		p.metrics.tokenizerCreationSuccesses.Inc()
	}
}

func (p *TokenizerPool) incTokenizerCreationFailures() {
	if p.metrics != nil {
		p.metrics.tokenizerCreationFailures.Inc()
	}
}

func (p *TokenizerPool) incUnhealthyTokenizers() {
	if p.metrics != nil {
		p.metrics.unhealthyTokenizers.Inc()
	}
}

func (p *TokenizerPool) incTokenizerRequests(model string) {
	if p.metrics != nil {
		p.metrics.tokenizerRequests.WithLabelValues(model).Inc()
	}
}

func (p *TokenizerPool) observeTokenizerLatency(model string, duration time.Duration) {
	if p.metrics != nil {
		p.metrics.tokenizerLatency.WithLabelValues(model).Observe(duration.Seconds())
	}
}

// GetTokenizer returns a tokenizer for the specified model
func (p *TokenizerPool) GetTokenizer(model string, pods []*v1.Pod) tokenizer.Tokenizer {
	// Safe metric increment
	p.incTokenizerRequests(model)
	startTime := time.Now()
	defer func() {
		p.observeTokenizerLatency(model, time.Since(startTime))
	}()

	// If remote tokenizer is disabled, return default immediately
	if !p.config.EnableVLLMRemote {
		return p.config.DefaultTokenizer
	}

	// Acquire write lock directly to avoid race condition
	// TODO: Consider implementing reference counting or double-checked locking
	// to improve concurrency performance while maintaining thread safety
	p.mu.Lock()
	entry, exists := p.tokenizers[model]
	if exists && entry.healthStatus {
		// Update lastUsed while still holding the lock
		entry.lastUsed = time.Now()
		tok := entry.tokenizer
		p.mu.Unlock()
		return tok
	}
	p.mu.Unlock()

	// Slow path: create new tokenizer
	return p.createOrUpdateTokenizer(model, pods)
}

// createOrUpdateTokenizer creates or updates a tokenizer for the model
func (p *TokenizerPool) createOrUpdateTokenizer(model string, pods []*v1.Pod) tokenizer.Tokenizer {
	// First attempt: quick check under write lock
	p.mu.Lock()

	// Double-check after acquiring write lock
	if entry, exists := p.tokenizers[model]; exists && entry.healthStatus {
		entry.lastUsed = time.Now()
		p.mu.Unlock()
		return entry.tokenizer
	}

	// Check pool size limit
	if len(p.tokenizers) >= p.config.MaxTokenizersPerPool {
		p.mu.Unlock()
		klog.Warningf("TokenizerPool reached max size %d, using default tokenizer", p.config.MaxTokenizersPerPool)
		return p.config.DefaultTokenizer
	}

	// Find endpoint for model
	endpoint := p.findVLLMEndpointForModel(model, pods)
	if endpoint == "" {
		p.mu.Unlock()
		klog.V(4).Infof("No vLLM endpoint found for model %s, using default tokenizer", model)
		p.incTokenizerCreationFailures()
		return p.config.DefaultTokenizer
	}

	// Release lock before creating tokenizer and health check
	p.mu.Unlock()

	// Create remote tokenizer (outside of lock)
	config := tokenizer.RemoteTokenizerConfig{
		Engine:             vllmEngine,
		Endpoint:           endpoint,
		Model:              model,
		Timeout:            p.config.Timeout,
		MaxRetries:         3,
		AddSpecialTokens:   true,
		ReturnTokenStrings: false,
	}

	tok, err := tokenizer.NewRemoteTokenizer(config)
	if err != nil {
		klog.Warningf("Failed to create vLLM tokenizer for model %s: %v", model, err)
		p.incTokenizerCreationFailures()
		return p.config.DefaultTokenizer
	}

	// Verify health (outside of lock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if remoteTok, ok := tok.(interface{ IsHealthy(context.Context) bool }); ok {
		if !remoteTok.IsHealthy(ctx) {
			klog.Warningf("Created tokenizer for model %s is not healthy", model)
			p.incTokenizerCreationFailures()
			return p.config.DefaultTokenizer
		}
	}

	// Re-acquire lock to update the pool
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check: another goroutine might have created it while we were checking health
	if entry, exists := p.tokenizers[model]; exists && entry.healthStatus {
		// Another goroutine beat us to it, use theirs and discard ours
		entry.lastUsed = time.Now()
		// Close the tokenizer we just created since we won't use it
		if closer, ok := tok.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
		return entry.tokenizer
	}

	// Add to pool
	now := time.Now()
	p.tokenizers[model] = &tokenizerEntry{
		tokenizer:    tok,
		endpoint:     endpoint,
		lastUsed:     now,
		lastHealthy:  now,
		healthStatus: true,
	}

	p.setActiveTokenizers(float64(len(p.tokenizers)))
	p.incTokenizerCreationSuccesses()
	klog.V(3).Infof("Created vLLM tokenizer for model %s at endpoint %s", model, endpoint)

	return tok
}

// findVLLMEndpointForModel finds the vLLM endpoint for a specific model
func (p *TokenizerPool) findVLLMEndpointForModel(model string, pods []*v1.Pod) string {
	// Priority order for endpoint discovery:
	// 1. Service endpoint (if configured)
	if endpoint, exists := p.config.ModelServiceMap[model]; exists {
		return endpoint
	}

	// 2. Direct pod endpoint
	for _, pod := range pods {
		if !isPodReady(pod) {
			continue
		}

		// Check model match
		podModel := getModelFromPod(pod)
		if podModel != model {
			continue
		}

		// Check if it's a vLLM pod
		if !isVLLMPod(pod) {
			continue
		}

		return fmt.Sprintf(p.config.EndpointTemplate, pod.Status.PodIP)
	}

	return ""
}

// getModelFromPod extracts model information from pod
func getModelFromPod(pod *v1.Pod) string {
	// 1. Check labels (highest priority)
	// Only KV Event Sync constants are defined in pkg/constants
	if model := pod.Labels[constants.ModelLabelName]; model != "" {
		return model
	}

	// 2. Check annotations
	if model := pod.Annotations[constants.ModelLabelName]; model != "" {
		return model
	}

	// 3. Check environment variables
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "MODEL_NAME" || env.Name == "MODEL" {
				return env.Value
			}
		}
	}

	return ""
}

// isVLLMPod checks if a pod is running vLLM engine
func isVLLMPod(pod *v1.Pod) bool {
	// Check labels
	// Only KV Event Sync constants are defined in pkg/constants
	if engine := pod.Labels[constants.ModelLabelEngine]; engine == vllmEngine {
		return true
	}

	// Check annotations
	if engine := pod.Annotations[constants.ModelLabelEngine]; engine == vllmEngine {
		return true
	}

	// Check environment variables
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "INFERENCE_ENGINE" && env.Value == vllmEngine {
				return true
			}
		}
	}

	// Default assumption based on port (vLLM typically runs on 8000)
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.ContainerPort == 8000 {
				return true
			}
		}
	}

	return false
}

// isPodReady checks if a pod is ready to serve requests
func isPodReady(pod *v1.Pod) bool {
	if pod.Status.Phase != v1.PodRunning {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady && condition.Status == v1.ConditionTrue {
			return true
		}
	}

	return false
}

// startHealthChecker starts the background health checking routine
func (p *TokenizerPool) startHealthChecker() {
	ticker := time.NewTicker(p.config.HealthCheckPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.performHealthCheck()
				p.cleanupStaleTokenizers()
			case <-p.stopCh:
				return
			}
		}
	}()
}

// performHealthCheck checks health of all tokenizers
func (p *TokenizerPool) performHealthCheck() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for model, entry := range p.tokenizers {
		if remoteTok, ok := entry.tokenizer.(interface{ IsHealthy(context.Context) bool }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			healthy := remoteTok.IsHealthy(ctx)
			cancel()

			oldStatus := entry.healthStatus
			entry.healthStatus = healthy
			if healthy {
				entry.lastHealthy = time.Now()
			} else if oldStatus {
				// Only log and count when transitioning from healthy to unhealthy
				klog.Warningf("Tokenizer for model %s is now unhealthy", model)
				p.incUnhealthyTokenizers()
			}
		}
	}
}

// cleanupStaleTokenizers removes unused tokenizers
func (p *TokenizerPool) cleanupStaleTokenizers() {
	var staleTokenizers []struct {
		model string
		tok   tokenizer.Tokenizer
	}

	// Collect stale tokenizers under lock
	p.mu.Lock()
	now := time.Now()
	for model, entry := range p.tokenizers {
		// Remove if unused for TTL duration
		if now.Sub(entry.lastUsed) > p.config.TokenizerTTL {
			staleTokenizers = append(staleTokenizers, struct {
				model string
				tok   tokenizer.Tokenizer
			}{model: model, tok: entry.tokenizer})
			delete(p.tokenizers, model)
			klog.V(4).Infof("Removed stale tokenizer for model %s", model)
		}
	}
	p.setActiveTokenizers(float64(len(p.tokenizers)))
	p.mu.Unlock()

	// Close tokenizers outside of lock
	for _, stale := range staleTokenizers {
		if closer, ok := stale.tok.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				klog.Errorf("Error closing tokenizer for model %s: %v", stale.model, err)
			}
		}
	}
}

// Close gracefully shuts down the TokenizerPool
func (p *TokenizerPool) Close() error {
	// Stop health checker
	close(p.stopCh)

	var tokenizersToClose []struct {
		model string
		tok   tokenizer.Tokenizer
	}

	// Collect all tokenizers under lock
	p.mu.Lock()
	for model, entry := range p.tokenizers {
		tokenizersToClose = append(tokenizersToClose, struct {
			model string
			tok   tokenizer.Tokenizer
		}{model: model, tok: entry.tokenizer})
	}
	p.tokenizers = make(map[string]*tokenizerEntry)
	p.setActiveTokenizers(0)
	p.mu.Unlock()

	// Close tokenizers outside of lock
	for _, item := range tokenizersToClose {
		if closer, ok := item.tok.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				klog.Errorf("Error closing tokenizer for model %s: %v", item.model, err)
			}
		}
	}

	// Unregister metrics if they were registered
	if p.metricsRegistered && p.metrics != nil {
		p.metrics.unregister()
	}

	return nil
}
