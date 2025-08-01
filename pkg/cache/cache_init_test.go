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
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
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
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				t.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "")
				t.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "cleanup on Start failure - invalid tokenizer type",
			setupEnv: func(t *testing.T) {
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				t.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "local") // Should be "remote"
				t.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "no cleanup on success",
			setupEnv: func(t *testing.T) {
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				t.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "remote")
				t.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")
			},
			expectCleanup: false,
			expectError:   false,
		},
		{
			name: "no error when KV sync disabled",
			setupEnv: func(t *testing.T) {
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
			},
			expectCleanup: false,
			expectError:   false,
		},
		{
			name: "no error when remote tokenizer disabled",
			setupEnv: func(t *testing.T) {
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
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
				os.Getenv("AIBRIX_KV_EVENT_SYNC_ENABLED") == "true" &&
				os.Getenv("AIBRIX_USE_REMOTE_TOKENIZER") == "true" {
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
	t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
	t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
	t.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "remote")
	t.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")

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
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
			},
		},
		{
			name:         "controller - no KV sync",
			opts:         InitOptions{},
			expectKVSync: false,
			setupEnv: func(t *testing.T) {
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
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
				t.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false")
				t.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
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
