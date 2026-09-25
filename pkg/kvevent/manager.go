// Copyright 2025 AIBrix Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build zmq
// +build zmq

package kvevent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// Manager manages KV event subscriptions for vLLM pods
type Manager struct {
	// Dependencies injected via interfaces
	podProvider  PodProvider
	syncProvider SyncIndexProvider

	// Subscriber management
	subscribers utils.SyncMap[string, *kvcache.ZMQClient]

	// sleepStateAwake is the last observed engine_sleep_state "awake" reading
	// per (pod, model), keyed by sleepStateAwakeKey. Backs
	// CheckSleepStateBackstop's edge trigger.
	sleepStateAwake utils.SyncMap[string, bool]

	// Configuration
	enabled bool

	// Lifecycle management
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
	stopped bool
}

// NewManager creates a new KV event manager
func NewManager(podProvider PodProvider, syncProvider SyncIndexProvider) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	// Check configuration
	enabled := validateConfiguration()

	return &Manager{
		podProvider:  podProvider,
		syncProvider: syncProvider,
		enabled:      enabled,
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Start initializes the manager and begins processing
func (m *Manager) Start() error {
	if !m.enabled {
		klog.Info("KV event sync is disabled")
		return nil
	}

	// Verify dependencies with retry logic
	// Use 30s timeout for initialization as sync indexer startup can be slow
	// during controller bootstrap when many resources are being initialized
	initCtx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	// Wait for sync indexer to be ready with polling
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

IndexerReadyLoop:
	for {
		select {
		case <-initCtx.Done():
			return fmt.Errorf("sync indexer not available after timeout: %w", initCtx.Err())
		case <-ticker.C:
			_, err := m.syncProvider.GetSyncIndexer(initCtx)
			if err != nil {
				if errors.Is(err, ErrIndexerNotInitialized) {
					klog.V(2).Info("Sync indexer not yet available, waiting...")
					continue // Keep polling
				}
				return fmt.Errorf("failed to get sync indexer: %w", err)
			}
			// Success - indexer is ready
			break IndexerReadyLoop
		}
	}

	// Process existing pods
	err := m.podProvider.RangePods(initCtx, func(key string, podInfo *PodInfo) bool {
		if canSubscribeToPod(podInfo) {
			// Use anonymous function to properly scope the defer
			func() {
				// Use 5s timeout for individual pod subscriptions as ZMQ
				// connection establishment should be quick for healthy pods
				subCtx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
				defer cancel() // Now properly scoped to this function

				if err := m.subscribeToPod(subCtx, key, podInfo); err != nil {
					klog.Errorf("Failed to subscribe to pod %s: %v", key, err)
				}
			}()
		}
		return true // Continue iteration
	})

	if err != nil {
		return fmt.Errorf("failed to process existing pods: %w", err)
	}

	klog.Info("KV event manager started successfully")
	return nil
}

// Stop gracefully shuts down the manager
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	m.mu.Unlock()

	klog.Info("Stopping KV event manager")

	// Cancel context to signal shutdown
	if m.cancel != nil {
		m.cancel()
	}

	// Stop all subscribers
	m.subscribers.Range(func(key string, client *kvcache.ZMQClient) bool {
		client.Stop()
		return true
	})

	klog.Info("KV event manager stopped")
}

// OnPodAdd handles new pod additions
func (m *Manager) OnPodAdd(pod *v1.Pod) {
	if !m.enabled || !isPodSubscribable(pod) {
		return
	}

	// Check if manager is stopped before using its context
	m.mu.RLock()
	stopped := m.stopped
	m.mu.RUnlock()

	if stopped {
		return
	}

	// Use 5s timeout for pod operations as they involve simple ZMQ ops
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	// Get pod info
	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	podInfo, exists := m.podProvider.GetPod(ctx, podKey)
	if !exists {
		klog.Warningf("Pod %s not found in provider", podKey)
		return
	}

	if err := m.subscribeToPod(ctx, podKey, podInfo); err != nil {
		klog.Errorf("Failed to subscribe to pod %s: %v", podKey, err)
	}
}

// OnPodUpdate handles pod updates
func (m *Manager) OnPodUpdate(oldPod, newPod *v1.Pod) {
	if !m.enabled {
		return
	}

	podKey := utils.GeneratePodKey(newPod.Namespace, newPod.Name)

	oldSubscribable := isPodSubscribable(oldPod)
	newSubscribable := isPodSubscribable(newPod)

	// Resubscription only happens in 2 cases:
	// - Pod Changed
	// - Subscription state (status.Phase) changed, this applies to the same pod or different pods
	if !isSamePod(oldPod, newPod) || oldSubscribable != newSubscribable {
		if oldSubscribable {
			m.unsubscribeFromPod(podKey)
		}
		if newSubscribable {
			m.OnPodAdd(newPod)
		}
	}
}

// OnPodDelete handles pod deletion
func (m *Manager) OnPodDelete(pod *v1.Pod) {
	if !m.enabled {
		return
	}

	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	m.unsubscribeFromPod(podKey)

	// Check if manager is stopped before using its context
	m.mu.RLock()
	stopped := m.stopped
	m.mu.RUnlock()

	if stopped {
		return
	}

	// Clean up from sync indexer
	// Use 5s timeout for cleanup operations as they should be quick
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	syncIndexer, err := m.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		klog.Errorf("Failed to get sync indexer: %v", err)
		return
	}

	modelName, hasModelName := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
	if hasModelName {
		loraID := int64(-1)
		if loraStr := constants.GetLoraID(pod.Labels); loraStr != "" {
			if parsed, err := strconv.ParseInt(loraStr, 10, 64); err == nil {
				loraID = parsed
			}
		}

		if err := syncIndexer.RemovePrefix(ctx, modelName, loraID, podKey); err != nil {
			klog.Errorf("Failed to remove prefix for pod %s: %v", podKey, err)
		}
	}
}

// Internal methods

func canSubscribeToPod(podInfo *PodInfo) bool {
	return podInfo.Labels[constants.KVEventsEnabledLabel] == "true" &&
		podInfo.PodIP != "" &&
		podInfo.ModelName != ""
}

// Check if the pod can be subscribed
func isPodSubscribable(pod *v1.Pod) bool {
	_, hasModelName := constants.ModelNameFromMetadata(pod.Labels, pod.Annotations)
	return constants.IsKVEventsEnabled(pod.Labels) &&
		pod.Status.Phase == v1.PodRunning &&
		pod.Status.PodIP != "" &&
		hasModelName
}

func isSamePod(pod1 *v1.Pod, pod2 *v1.Pod) bool {
	// For now, we just check if PodIP is the same. Other conditions may be added if needed.
	return pod1.Status.PodIP == pod2.Status.PodIP
}

func (m *Manager) subscribeToPod(ctx context.Context, podKey string, podInfo *PodInfo) error {
	// Check if already subscribed
	if _, exists := m.subscribers.Load(podKey); exists {
		return nil
	}

	// Create event handler
	handler := &eventHandler{
		manager:   m,
		podKey:    podKey,
		modelName: podInfo.ModelName,
		loraID:    extractLoraID(podInfo.Labels),
	}

	// Create ZMQ client
	config := kvcache.DefaultZMQClientConfig(podKey, podInfo.PodIP, podInfo.ModelName)
	client := kvcache.NewZMQClient(config, handler)

	// Start subscription
	if err := client.Start(); err != nil {
		return fmt.Errorf("failed to start ZMQ client: %w", err)
	}

	// Store subscriber
	m.subscribers.Store(podKey, client)

	klog.Infof("Subscribed to KV events for pod %s (model: %s, IP: %s)",
		podKey, podInfo.ModelName, podInfo.PodIP)

	return nil
}

// PurgePodPrefixCache removes podKey's cached prefix entries for
// (modelName, loraID) from the sync indexer, so a pod that wiped its KV cache
// stops being preferred by the prefix router (see issue #2287). The
// AllBlocksCleared event handler calls it; CheckSleepStateBackstop does not,
// see its comment. A temporary sync-indexer error is swallowed as nil, since
// the event path has nothing to retry with.
func (m *Manager) PurgePodPrefixCache(ctx context.Context, modelName string, loraID int64, podKey string) error {
	syncIndexer, err := m.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		if IsTemporaryError(err) {
			klog.V(4).Infof("Temporary error getting sync indexer: %v", err)
			return nil
		}
		return fmt.Errorf("failed to get sync indexer: %w", err)
	}
	return syncIndexer.RemovePrefix(ctx, modelName, loraID, podKey)
}

// sleepStateAwakeKey builds the CheckSleepStateBackstop tracker key. A pod
// serving several models is tracked per model, since RemovePrefix itself is
// scoped to one (modelName, loraID, podKey).
func sleepStateAwakeKey(podKey, modelName string) string {
	return podKey + "|" + modelName
}

// CheckSleepStateBackstop purges podKey's cached prefix entries for
// (modelName, loraID) on an awake-to-asleep transition of the engine's own
// sleep-state metric, backstopping the AllBlocksCleared event handler: an
// idle engine may never flush queued KV events (vLLM only does so from
// inside a scheduler step), and ZMQ delivery is lossy. See issue #2287.
// Callers pass awake as the engine_sleep_state{sleep_state="awake"} gauge
// value: nonzero means awake, zero means asleep at some level.
//
// Edge triggered, not level triggered: RemovePrefix takes the prefix
// router's write lock and is O(P) in cached prefixes for the model, so
// purging on every refresh tick a sleeping pod is observed would hold that
// lock once per refresh interval for as long as it stays asleep. The
// transition is the only interesting event.
//
// A (pod, model) pair not seen before is seeded as having been awake, so a
// gateway restart while a pod is already asleep still self-corrects: the
// first observation after restart reads as a transition and purges once,
// rather than leaving stale entries forever because no transition is ever
// observed.
//
// The tracked state only latches to asleep once RemovePrefix actually runs
// and succeeds. This deliberately does not go through PurgePodPrefixCache:
// that helper swallows a temporary indexer-not-ready error as nil for the
// AllBlocksCleared event handler, which has nothing to retry with. This
// caller does have a next observation to retry with, and needs to tell
// "purged" apart from "swallowed", or latching on either would mark the
// transition handled when nothing was purged, and no later asleep
// observation would ever retry it (review on #2735).
func (m *Manager) CheckSleepStateBackstop(ctx context.Context, podKey, modelName string, loraID int64, awake float64) {
	key := sleepStateAwakeKey(podKey, modelName)
	isAwake := awake != 0

	if isAwake {
		m.sleepStateAwake.Store(key, true)
		return
	}

	wasAwake, hadPrior := m.sleepStateAwake.Load(key)
	if hadPrior && !wasAwake {
		// Already latched asleep by an earlier successful purge.
		return
	}

	syncIndexer, err := m.syncProvider.GetSyncIndexer(ctx)
	if err != nil {
		if !IsTemporaryError(err) {
			klog.Errorf("Sleep-state backstop failed to get sync indexer for pod %s, model %s: %v", podKey, modelName, err)
		}
		// Do not latch asleep either way: leave the tracked state as it was
		// (unset, or awake from the last successful write) so the next
		// asleep observation retries instead of being silently dropped.
		return
	}
	if err := syncIndexer.RemovePrefix(ctx, modelName, loraID, podKey); err != nil {
		klog.Errorf("Sleep-state backstop failed to purge prefix cache for pod %s, model %s: %v", podKey, modelName, err)
		return
	}
	m.sleepStateAwake.Store(key, false)
	klog.V(4).Infof("Sleep-state backstop purged prefix entries for pod %s, model %s", podKey, modelName)
}

// forgetSleepState drops the tracked sleep state for podKey, called when the
// pod is unsubscribed so a future re-add of the same pod key is seeded as
// awake again instead of replaying whatever state it last held.
func (m *Manager) forgetSleepState(podKey string) {
	prefix := podKey + "|"
	for _, key := range m.sleepStateAwake.Keys() {
		if strings.HasPrefix(key, prefix) {
			m.sleepStateAwake.Delete(key)
		}
	}
}

func (m *Manager) unsubscribeFromPod(podKey string) {
	client, exists := m.subscribers.LoadAndDelete(podKey)
	if !exists {
		return
	}

	client.Stop()
	m.forgetSleepState(podKey)
	klog.Infof("Unsubscribed from KV events for pod %s", podKey)
}

func validateConfiguration() bool {
	// Check if KV sync is enabled
	kvSyncRequested := utils.LoadEnvBool(constants.EnvPrefixCacheKVEventSyncEnabled, false)

	// Check remote tokenizer
	remoteTokenizerEnabled := utils.LoadEnvBool(constants.EnvPrefixCacheUseRemoteTokenizer, false)

	if kvSyncRequested && !remoteTokenizerEnabled {
		klog.Warning("KV event sync requires remote tokenizer. Disabling.")
		return false
	}

	return kvSyncRequested
}

func extractLoraID(labels map[string]string) int64 {
	if loraStr := constants.GetLoraID(labels); loraStr != "" {
		if parsed, err := strconv.ParseInt(loraStr, 10, 64); err == nil {
			return parsed
		}
	}
	return -1
}
