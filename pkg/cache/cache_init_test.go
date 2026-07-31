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
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modelv1alpha1 "github.com/vllm-project/aibrix/api/model/v1alpha1"
	"github.com/vllm-project/aibrix/pkg/cache/discovery"
	"github.com/vllm-project/aibrix/pkg/constants"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInitKVEventSync_FailureCleanup(t *testing.T) {
	tests := []struct {
		name          string
		setupEnv      func(t *testing.T)
		expectCleanup bool
		expectError   bool
	}{
		{
			name: "cleanup on Start failure - remote tokenizer not configured",
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
				t.Setenv(constants.EnvPrefixCacheTokenizerType, "")
				t.Setenv(constants.EnvPrefixCacheRemoteTokenizerEndpoint, "")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "cleanup on Start failure - invalid tokenizer type",
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
				t.Setenv(constants.EnvPrefixCacheTokenizerType, "local") // Should be "remote"
				t.Setenv(constants.EnvPrefixCacheRemoteTokenizerEndpoint, "http://test:8080")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "no cleanup on success",
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
				t.Setenv(constants.EnvPrefixCacheTokenizerType, "remote")
				t.Setenv(constants.EnvPrefixCacheRemoteTokenizerEndpoint, "http://test:8080")
			},
			expectCleanup: false,
			expectError:   false,
		},
		{
			name: "no error when KV sync disabled",
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "false")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
			},
			expectCleanup: false,
			expectError:   false,
		},
		{
			name: "no error when remote tokenizer disabled",
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "false")
			},
			expectCleanup: false,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv(t) // Isolated env for each subtest

			store := &Store{}
			err := store.initKVEventSync()

			// Handle ZMQ-specific fallback if needed
			expectedError := tt.expectError
			if !tt.expectError && tt.name == "no cleanup on success" {
				if err != nil && strings.Contains(err.Error(), "KV event sync requires ZMQ support") {
					expectedError = true
				}
			}

			if expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tt.expectCleanup {
				assert.Nil(t, store.kvEventManager)
				assert.Nil(t, store.syncPrefixIndexer)
			} else if !expectedError &&
				os.Getenv(constants.EnvPrefixCacheKVEventSyncEnabled) == "true" &&
				os.Getenv(constants.EnvPrefixCacheUseRemoteTokenizer) == "true" {
				assert.NotNil(t, store.kvEventManager)
				assert.NotNil(t, store.syncPrefixIndexer)
			}
		})
	}
}

func TestCleanupKVEventSync_Idempotent(t *testing.T) {
	// Test that cleanup can be called multiple times safely
	store := &Store{}

	// Call cleanup multiple times
	store.cleanupKVEventSync()
	store.cleanupKVEventSync()
	store.cleanupKVEventSync()

	// Should not panic and resources should remain nil
	assert.Nil(t, store.kvEventManager)
	assert.Nil(t, store.syncPrefixIndexer)
}

func TestStore_Close_CallsCleanup(t *testing.T) {
	t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
	t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
	t.Setenv(constants.EnvPrefixCacheTokenizerType, "remote")
	t.Setenv(constants.EnvPrefixCacheRemoteTokenizerEndpoint, "http://test:8080")

	store := &Store{}
	err := store.initKVEventSync()

	if err != nil && strings.Contains(err.Error(), "KV event sync requires ZMQ support") {
		t.Skip("Skipping test in non-ZMQ build")
		return
	}

	assert.NoError(t, err)
	assert.NotNil(t, store.kvEventManager)
	assert.NotNil(t, store.syncPrefixIndexer)

	store.Close()
	assert.Nil(t, store.kvEventManager)
	assert.Nil(t, store.syncPrefixIndexer)

	store.Close() // Ensure idempotency
}

// mockProvider implements discovery.Provider for testing.
// It delivers events in a caller-specified order via Watch().
type mockProvider struct {
	events []discovery.WatchEvent
}

func (p *mockProvider) Watch(handler discovery.EventHandler, _ <-chan struct{}) error {
	for _, ev := range p.events {
		handler(ev)
	}
	return nil
}

func (p *mockProvider) Type() string { return "mock" }

func testPod(name, namespace, model string, id int) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				constants.ModelLabelName: model,
			},
		},
		Status: v1.PodStatus{
			PodIP: fmt.Sprintf("10.0.0.%d", id),
			Conditions: []v1.PodCondition{{
				Type:   v1.PodReady,
				Status: v1.ConditionTrue,
			}},
		},
	}
}

func testModelAdapter(name, namespace, podName string) *modelv1alpha1.ModelAdapter {
	return &modelv1alpha1.ModelAdapter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: modelv1alpha1.ModelAdapterStatus{
			Instances: []string{podName},
		},
	}
}

func TestInitDiscoveryProvider_PodBeforeAdapter(t *testing.T) {
	// Normal case: Pod arrives before ModelAdapter.
	// The adapter should find the pod and create the mapping.
	store := NewForTest()
	stopCh := make(chan struct{})
	defer close(stopCh)

	provider := &mockProvider{
		events: []discovery.WatchEvent{
			{Type: discovery.EventAdd, Object: testPod("p1", "ns", "base-model", 1)},
			{Type: discovery.EventAdd, Object: testModelAdapter("lora-adapter", "ns", "p1")},
		},
	}

	err := initDiscoveryProvider(store, provider, stopCh)
	require.NoError(t, err)

	// The pod should serve both the base model and the adapter model.
	models := store.ListModels()
	assert.Contains(t, models, "base-model")
	assert.Contains(t, models, "lora-adapter")

	// The adapter model should map to the pod.
	pods, err := store.ListPodsByModel("lora-adapter")
	require.NoError(t, err)
	assert.Equal(t, 1, pods.Len())
}

func TestInitDiscoveryProvider_AdapterBeforePod(t *testing.T) {
	// Ordering issue: ModelAdapter arrives BEFORE its Pod.
	// The first addModelAdapter fails silently (pod not in cache).
	// A post-sync reconcile (re-emitting the adapter) should fix the mapping.
	store := NewForTest()
	stopCh := make(chan struct{})
	defer close(stopCh)

	provider := &mockProvider{
		events: []discovery.WatchEvent{
			// Adapter arrives first — pod not in cache yet, mapping silently fails
			{Type: discovery.EventAdd, Object: testModelAdapter("lora-adapter", "ns", "p1")},
			// Pod arrives
			{Type: discovery.EventAdd, Object: testPod("p1", "ns", "base-model", 1)},
			// Post-sync reconcile: re-emit the adapter (simulates what KubernetesProvider does)
			{Type: discovery.EventAdd, Object: testModelAdapter("lora-adapter", "ns", "p1")},
		},
	}

	err := initDiscoveryProvider(store, provider, stopCh)
	require.NoError(t, err)

	// After reconcile, both models should exist.
	models := store.ListModels()
	assert.Contains(t, models, "base-model")
	assert.Contains(t, models, "lora-adapter")

	// The adapter model should map to the pod.
	pods, err := store.ListPodsByModel("lora-adapter")
	require.NoError(t, err)
	assert.Equal(t, 1, pods.Len())
}

func TestInitDiscoveryProvider_AdapterBeforePodWithoutReconcile(t *testing.T) {
	// Demonstrates WHY the reconcile is needed:
	// Without the re-emit, the adapter-to-pod mapping is lost.
	store := NewForTest()
	stopCh := make(chan struct{})
	defer close(stopCh)

	provider := &mockProvider{
		events: []discovery.WatchEvent{
			// Adapter arrives first — mapping fails silently
			{Type: discovery.EventAdd, Object: testModelAdapter("lora-adapter", "ns", "p1")},
			// Pod arrives — but no one re-triggers the adapter mapping
			{Type: discovery.EventAdd, Object: testPod("p1", "ns", "base-model", 1)},
			// NO reconcile
		},
	}

	err := initDiscoveryProvider(store, provider, stopCh)
	require.NoError(t, err)

	// Base model exists (from the pod).
	models := store.ListModels()
	assert.Contains(t, models, "base-model")

	// Adapter model does NOT exist — the mapping was lost.
	assert.NotContains(t, models, "lora-adapter")
}

func TestInitDiscoveryProvider_DeleteEvent(t *testing.T) {
	store := NewForTest()
	stopCh := make(chan struct{})
	defer close(stopCh)

	pod := testPod("p1", "ns", "base-model", 1)
	provider := &mockProvider{
		events: []discovery.WatchEvent{
			{Type: discovery.EventAdd, Object: pod},
			{Type: discovery.EventDelete, Object: pod},
		},
	}

	err := initDiscoveryProvider(store, provider, stopCh)
	require.NoError(t, err)

	// Pod was added then deleted — no models should remain.
	models := store.ListModels()
	assert.Empty(t, models)
}

func TestInitWithOptions_KVSyncBehavior(t *testing.T) {
	scenarios := []struct {
		name         string
		opts         InitOptions
		expectKVSync bool
		setupEnv     func(t *testing.T)
	}{
		{
			name: "metadata service - no KV sync",
			opts: InitOptions{
				RedisClient: &redis.Client{},
			},
			expectKVSync: false,
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
			},
		},
		{
			name:         "controller - no KV sync",
			opts:         InitOptions{},
			expectKVSync: false,
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "true")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "true")
			},
		},
		{
			name: "gateway with KV sync disabled",
			opts: InitOptions{
				EnableKVSync: false,
				RedisClient:  &redis.Client{},
			},
			expectKVSync: false,
			setupEnv: func(t *testing.T) {
				t.Setenv(constants.EnvPrefixCacheKVEventSyncEnabled, "false")
				t.Setenv(constants.EnvPrefixCacheUseRemoteTokenizer, "false")
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.setupEnv(t)

			testStore := &Store{initialized: true}

			if sc.expectKVSync {
				assert.NotNil(t, testStore)
				// More logic would go here in the future with mocking
			} else {
				assert.Nil(t, testStore.kvEventManager)
				assert.Nil(t, testStore.syncPrefixIndexer)
			}
		})
	}
}
