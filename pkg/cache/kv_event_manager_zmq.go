//go:build zmq

/*
Copyright 2025 The Aibrix Team.

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

package cache

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vllm-project/aibrix/pkg/cache/kvcache"
	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/utils"
)

const (
	// kvEventsEnabledValue is the label value that indicates KV events are enabled
	kvEventsEnabledValue = "true"
)

// KVEventManager manages KV event subscriptions for vLLM pods
type KVEventManager struct {
	// Dependencies
	store *Store

	// Subscriber management
	subscribers utils.SyncMap[string, *kvcache.ZMQClient] // podKey -> client

	// Configuration
	enabled bool

	// Lifecycle
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.RWMutex
	stopped bool // Flag to ensure idempotent Stop()
}

// NewKVEventManager creates a new KV event manager
func NewKVEventManager(store *Store) *KVEventManager {
	ctx, cancel := context.WithCancel(context.Background())

	// Check feature dependencies
	kvSyncValue := utils.LoadEnv(constants.EnvKVEventSyncEnabled, "false")
	kvSyncRequested, _ := strconv.ParseBool(kvSyncValue)
	remoteTokenValue := utils.LoadEnv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
	remoteTokenizerEnabled, _ := strconv.ParseBool(remoteTokenValue)

	// Validate configuration
	enabled := kvSyncRequested
	if kvSyncRequested && !remoteTokenizerEnabled {
		klog.Warning("KV event sync requires remote tokenizer to be enabled. " +
			"Please set AIBRIX_USE_REMOTE_TOKENIZER=true to use KV event sync. " +
			"Disabling KV event sync.")
		enabled = false
	}

	return &KVEventManager{
		store:   store,
		enabled: enabled,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start initializes the KV event manager
func (m *KVEventManager) Start() error {
	if !m.enabled {
		klog.Info("KV event sync is disabled")
		return nil
	}

	// Double-check remote tokenizer is available
	if !m.verifyRemoteTokenizer() {
		klog.Error("Remote tokenizer not available, cannot start KV event sync")
		m.enabled = false
		return fmt.Errorf("remote tokenizer required for KV event sync")
	}

	klog.Info("Starting KV event manager with remote tokenizer support")

	// Initialize metrics for KV event sync
	if err := kvcache.InitializeMetrics(); err != nil {
		klog.Errorf("Failed to initialize KV cache metrics: %v", err)
		// Continue without metrics rather than failing
	}

	// Process existing pods
	m.store.metaPods.Range(func(key string, pod *Pod) bool {
		if m.shouldSubscribe(pod.Pod) {
			if err := m.subscribeToPod(pod.Pod); err != nil {
				klog.Errorf("Failed to subscribe to existing pod %s: %v", key, err)
			}
		}
		return true
	})

	return nil
}

// validateConfiguration checks if the manager can start successfully
// without actually starting any goroutines or connections
func (m *KVEventManager) validateConfiguration() error {
	if !m.enabled {
		return fmt.Errorf("KV event sync is disabled")
	}

	// Verify remote tokenizer without side effects
	if !m.verifyRemoteTokenizer() {
		return fmt.Errorf("remote tokenizer not available")
	}

	// Additional validation checks could be added here in the future:
	// - Check ZMQ library is available
	// - Verify network permissions
	// - Validate configuration values

	return nil
}

// verifyRemoteTokenizer checks if remote tokenizer is properly configured
func (m *KVEventManager) verifyRemoteTokenizer() bool {
	// Check if the cache/router has remote tokenizer configured
	if m.store == nil {
		return false
	}

	// Get the prefix cache router configuration
	tokenizerType := utils.LoadEnv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "")
	if tokenizerType != "remote" {
		klog.Warning("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE must be 'remote' for KV sync")
		return false
	}

	// Check remote tokenizer endpoint
	endpoint := utils.LoadEnv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "")
	if endpoint == "" {
		klog.Warning("AIBRIX_REMOTE_TOKENIZER_ENDPOINT not configured")
		return false
	}

	return true
}

// Stop gracefully shuts down the manager
func (m *KVEventManager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return // Already stopped
	}
	m.stopped = true
	m.mu.Unlock()

	klog.Info("Stopping KV event manager")

	if m.cancel != nil {
		m.cancel()
	}

	// Stop all subscribers
	m.subscribers.Range(func(key string, client *kvcache.ZMQClient) bool {
		client.Stop()
		return true
	})

	m.wg.Wait()
}

// OnPodAdd handles new pod additions
func (m *KVEventManager) OnPodAdd(pod *v1.Pod) {
	if !m.enabled || !m.shouldSubscribe(pod) {
		return
	}

	if err := m.subscribeToPod(pod); err != nil {
		klog.Errorf("Failed to subscribe to pod %s: %v",
			utils.GeneratePodKey(pod.Namespace, pod.Name), err)
	}
}

// OnPodUpdate handles pod updates
func (m *KVEventManager) OnPodUpdate(oldPod, newPod *v1.Pod) {
	if !m.enabled {
		return
	}

	podKey := utils.GeneratePodKey(newPod.Namespace, newPod.Name)
	shouldSubscribeOld := m.shouldSubscribe(oldPod)
	shouldSubscribeNew := m.shouldSubscribe(newPod)

	// Handle state transitions
	if !shouldSubscribeOld && shouldSubscribeNew {
		// Pod became eligible for subscription
		if err := m.subscribeToPod(newPod); err != nil {
			klog.Errorf("Failed to subscribe to pod %s: %v", podKey, err)
		}
	} else if shouldSubscribeOld && !shouldSubscribeNew {
		// Pod no longer eligible
		m.unsubscribeFromPod(podKey)
	} else if shouldSubscribeOld && shouldSubscribeNew {
		// Check if IP changed
		if oldPod.Status.PodIP != newPod.Status.PodIP {
			klog.Infof("Pod %s IP changed from %s to %s, resubscribing",
				podKey, oldPod.Status.PodIP, newPod.Status.PodIP)
			m.unsubscribeFromPod(podKey)
			if err := m.subscribeToPod(newPod); err != nil {
				klog.Errorf("Failed to resubscribe to pod %s: %v", podKey, err)
			}
		}
	}
}

// OnPodDelete handles pod deletion
func (m *KVEventManager) OnPodDelete(pod *v1.Pod) {
	if !m.enabled {
		return
	}

	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	m.unsubscribeFromPod(podKey)

	// Clean up from sync indexer
	syncIndexer := m.store.GetSyncPrefixIndexer()
	if syncIndexer != nil {
		modelName := pod.Labels["model.aibrix.ai/name"]
		if modelName != "" {
			loraID := int64(-1) // Default no LoRA
			if loraStr := constants.GetLoraID(pod.Labels); loraStr != "" {
				// Parse LoRA ID if present
				if parsed, err := strconv.ParseInt(loraStr, 10, 64); err == nil {
					loraID = parsed
				}
			}

			if err := syncIndexer.RemovePrefix(modelName, loraID, podKey); err != nil {
				klog.Errorf("Failed to remove prefix for pod %s: %v", podKey, err)
			}
		}
	}
}

// shouldSubscribe checks if a pod should have KV event subscription
func (m *KVEventManager) shouldSubscribe(pod *v1.Pod) bool {
	// Check if KV events are enabled
	if !constants.IsKVEventsEnabled(pod.Labels) {
		return false
	}

	// Check if pod is ready
	if pod.Status.Phase != v1.PodRunning || pod.Status.PodIP == "" {
		return false
	}

	// Check if it's a model pod
	if pod.Labels["model.aibrix.ai/name"] == "" {
		return false
	}

	return true
}

// subscribeToPod creates a ZMQ subscription for a pod
func (m *KVEventManager) subscribeToPod(pod *v1.Pod) error {
	podKey := utils.GeneratePodKey(pod.Namespace, pod.Name)
	modelName := pod.Labels["model.aibrix.ai/name"]

	// Check if already subscribed
	if _, exists := m.subscribers.Load(podKey); exists {
		return nil
	}

	// Create event handler
	handler := &kvEventHandler{
		manager:   m,
		podKey:    podKey,
		modelName: modelName,
	}

	// Create ZMQ client with default config
	config := kvcache.DefaultZMQClientConfig(podKey, pod.Status.PodIP, modelName)
	client := kvcache.NewZMQClient(config, handler)

	// Start subscription
	if err := client.Start(); err != nil {
		return fmt.Errorf("failed to start ZMQ client: %w", err)
	}

	// Store subscriber
	m.subscribers.Store(podKey, client)

	klog.Infof("Subscribed to KV events for pod %s (model: %s, IP: %s)",
		podKey, modelName, pod.Status.PodIP)

	return nil
}

// unsubscribeFromPod removes a ZMQ subscription
func (m *KVEventManager) unsubscribeFromPod(podKey string) {
	client, exists := m.subscribers.LoadAndDelete(podKey)
	if !exists {
		return
	}

	client.Stop()

	klog.Infof("Unsubscribed from KV events for pod %s", podKey)
}
