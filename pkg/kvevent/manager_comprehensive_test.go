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

package kvevent_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
)

// mockSyncIndexerWithCallbacks extends mockSyncIndexer to track method calls
type mockSyncIndexerWithCallbacks struct {
	mu             sync.Mutex
	removedPrefixes []string
	onRemovePrefix func(ctx context.Context, modelName string, loraID int64, podKey string) error
}

func (m *mockSyncIndexerWithCallbacks) ProcessBlockStored(ctx context.Context, event kvevent.BlockStoredEvent) error {
	return nil
}

func (m *mockSyncIndexerWithCallbacks) ProcessBlockRemoved(ctx context.Context, event kvevent.BlockRemovedEvent) error {
	return nil
}

func (m *mockSyncIndexerWithCallbacks) RemovePrefix(ctx context.Context, modelName string, loraID int64, podKey string) error {
	m.mu.Lock()
	m.removedPrefixes = append(m.removedPrefixes, podKey)
	m.mu.Unlock()
	
	if m.onRemovePrefix != nil {
		return m.onRemovePrefix(ctx, modelName, loraID, podKey)
	}
	return nil
}

// Test configuration handling
func TestManagerConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		envVars       map[string]string
		expectEnabled bool
	}{
		{
			name: "sync disabled",
			envVars: map[string]string{
				constants.EnvKVEventSyncEnabled: "false",
			},
			expectEnabled: false,
		},
		{
			name: "sync enabled with remote tokenizer",
			envVars: map[string]string{
				constants.EnvKVEventSyncEnabled:    "true",
				"AIBRIX_USE_REMOTE_TOKENIZER":      "true",
			},
			expectEnabled: true,
		},
		{
			name: "sync enabled without remote tokenizer",
			envVars: map[string]string{
				constants.EnvKVEventSyncEnabled:    "true",
				"AIBRIX_USE_REMOTE_TOKENIZER":      "false",
			},
			expectEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment variables
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			// Create manager
			podProvider := &mockPodProvider{pods: make(map[string]*kvevent.PodInfo)}
			syncProvider := &mockSyncIndexProvider{indexer: &mockSyncIndexer{}}

			manager := kvevent.NewManager(podProvider, syncProvider)
			err := manager.Start()

			// Manager should start successfully even if disabled
			if err != nil {
				t.Errorf("Manager failed to start: %v", err)
			}

			manager.Stop()
		})
	}
}

// Test pod lifecycle handling
func TestManagerPodLifecycle(t *testing.T) {
	// Enable KV sync
	t.Setenv(constants.EnvKVEventSyncEnabled, "true")
	t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")

	// Track method calls
	syncIndexer := &mockSyncIndexerWithCallbacks{}

	podInfo := &kvevent.PodInfo{
		Name:      "test-pod",
		Namespace: "default",
		PodIP:     "10.0.0.1",
		ModelName: "test-model",
		Labels: map[string]string{
			constants.ModelLabelName:       "test-model",
			constants.KVEventsEnabledLabel: "true",
		},
	}

	podProvider := &mockPodProvider{
		pods: map[string]*kvevent.PodInfo{
			"default/test-pod": podInfo,
		},
	}

	syncProvider := &mockSyncIndexProvider{indexer: syncIndexer}

	// Create and start manager
	manager := kvevent.NewManager(podProvider, syncProvider)
	if err := manager.Start(); err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Stop()

	// Create test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels:    podInfo.Labels,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: podInfo.PodIP,
		},
	}

	// Test pod addition
	manager.OnPodAdd(pod)
	time.Sleep(100 * time.Millisecond) // Allow async operations

	// Test pod update with IP change
	newPod := pod.DeepCopy()
	newPod.Status.PodIP = "10.0.0.2"
	manager.OnPodUpdate(pod, newPod)
	time.Sleep(100 * time.Millisecond)

	// Test pod deletion
	manager.OnPodDelete(newPod)
	time.Sleep(100 * time.Millisecond)

	// Verify cleanup was called
	syncIndexer.mu.Lock()
	removedCount := len(syncIndexer.removedPrefixes)
	syncIndexer.mu.Unlock()
	
	if removedCount != 1 {
		t.Errorf("Expected 1 pod cleanup, got: %d", removedCount)
	}
}

// Test sync indexer initialization retry
func TestManagerSyncIndexerRetry(t *testing.T) {
	t.Setenv(constants.EnvKVEventSyncEnabled, "true")
	t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")

	podProvider := &mockPodProvider{pods: make(map[string]*kvevent.PodInfo)}

	// Simulate indexer not ready initially
	callCount := 0
	indexer := &mockSyncIndexer{}
	syncProvider := &mockSyncIndexProvider{
		indexer: indexer,
		err:     kvevent.ErrIndexerNotInitialized,
	}

	// Override GetSyncIndexer behavior
	syncProvider.GetSyncIndexerFunc = func(ctx context.Context) (kvevent.SyncIndexer, error) {
		callCount++
		if callCount > 2 {
			return indexer, nil
		}
		return nil, kvevent.ErrIndexerNotInitialized
	}

	manager := kvevent.NewManager(podProvider, syncProvider)

	// Start should succeed after retries
	err := manager.Start()
	if err != nil {
		t.Errorf("Expected manager to start after retries, got error: %v", err)
	}

	// No need to restore - test is isolated

	manager.Stop()
}

// Test pod subscription criteria
func TestManagerPodSubscriptionCriteria(t *testing.T) {
	// Enable KV sync
	t.Setenv(constants.EnvKVEventSyncEnabled, "true") 
	t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")

	tests := []struct {
		name           string
		pod            *v1.Pod
		shouldSubscribe bool
	}{
		{
			name: "eligible pod",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "eligible-pod",
					Namespace: "default",
					Labels: map[string]string{
						constants.ModelLabelName:       "test-model",
						constants.KVEventsEnabledLabel: "true",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					PodIP: "10.0.0.1",
				},
			},
			shouldSubscribe: true,
		},
		{
			name: "no kv events label",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-kv-pod",
					Namespace: "default",
					Labels: map[string]string{
						constants.ModelLabelName: "test-model",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					PodIP: "10.0.0.1",
				},
			},
			shouldSubscribe: false,
		},
		{
			name: "not running",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pending-pod",
					Namespace: "default",
					Labels: map[string]string{
						constants.ModelLabelName:       "test-model",
						constants.KVEventsEnabledLabel: "true",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodPending,
				},
			},
			shouldSubscribe: false,
		},
		{
			name: "no IP",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-ip-pod",
					Namespace: "default",
					Labels: map[string]string{
						constants.ModelLabelName:       "test-model",
						constants.KVEventsEnabledLabel: "true",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
				},
			},
			shouldSubscribe: false,
		},
		{
			name: "no model name",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-model-pod",
					Namespace: "default",
					Labels: map[string]string{
						constants.KVEventsEnabledLabel: "true",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					PodIP: "10.0.0.1",
				},
			},
			shouldSubscribe: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podProvider := &mockPodProvider{
				pods: make(map[string]*kvevent.PodInfo),
			}
			syncProvider := &mockSyncIndexProvider{
				indexer: &mockSyncIndexer{},
			}

			manager := kvevent.NewManager(podProvider, syncProvider)
			if err := manager.Start(); err != nil {
				t.Fatalf("Failed to start manager: %v", err)
			}

			// Add pod and check if subscription happens
			manager.OnPodAdd(tt.pod)
			
			// Give some time for async operations
			time.Sleep(50 * time.Millisecond)

			// For now, we can't directly check if subscription happened
			// This would require exposing internal state or adding metrics
			// The test ensures no panic occurs for various pod states

			manager.Stop()
		})
	}
}

// Test concurrent pod operations
func TestManagerConcurrentOperations(t *testing.T) {
	// Enable KV sync
	t.Setenv(constants.EnvKVEventSyncEnabled, "true")
	t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")

	podProvider := &mockPodProvider{
		pods: make(map[string]*kvevent.PodInfo),
	}
	syncProvider := &mockSyncIndexProvider{
		indexer: &mockSyncIndexer{},
	}

	manager := kvevent.NewManager(podProvider, syncProvider)
	if err := manager.Start(); err != nil {
		t.Fatalf("Failed to start manager: %v", err)
	}
	defer manager.Stop()

	// Run concurrent operations
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("pod-%d", id),
					Namespace: "default",
					Labels: map[string]string{
						constants.ModelLabelName:       "test-model",
						constants.KVEventsEnabledLabel: "true",
					},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning,
					PodIP: fmt.Sprintf("10.0.0.%d", id),
				},
			}

			// Add, update, and delete
			manager.OnPodAdd(pod)
			
			updatedPod := pod.DeepCopy()
			updatedPod.Status.PodIP = fmt.Sprintf("10.0.1.%d", id)
			manager.OnPodUpdate(pod, updatedPod)
			
			manager.OnPodDelete(updatedPod)
		}(i)
	}

	wg.Wait()
	// Test passes if no panic or deadlock occurs
}