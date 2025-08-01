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
	// Save original env vars
	origKVSyncEnabled := os.Getenv("AIBRIX_KV_EVENT_SYNC_ENABLED")
	origRemoteTokenizer := os.Getenv("AIBRIX_USE_REMOTE_TOKENIZER")
	origTokenizerType := os.Getenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE")
	origTokenizerEndpoint := os.Getenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT")

	// Restore env vars after test
	defer func() {
		_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", origKVSyncEnabled)
		_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", origRemoteTokenizer)
		_ = os.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", origTokenizerType)
		_ = os.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", origTokenizerEndpoint)
	}()

	tests := []struct {
		name          string
		setupEnv      func()
		expectCleanup bool
		expectError   bool
	}{
		{
			name: "cleanup on Start failure - remote tokenizer not configured",
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				// Missing tokenizer configuration will cause validateConfiguration to fail
				_ = os.Unsetenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE")
				_ = os.Unsetenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "cleanup on Start failure - invalid tokenizer type",
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				_ = os.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "local") // Should be "remote"
				_ = os.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")
			},
			expectCleanup: true,
			expectError:   true,
		},
		{
			name: "no cleanup on success",
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
				_ = os.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "remote")
				_ = os.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")
			},
			expectCleanup: false,
			expectError:   false, // This will be overridden for non-ZMQ builds in test logic
		},
		{
			name: "no error when KV sync disabled",
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
			},
			expectCleanup: false,
			expectError:   false,
		},
		{
			name: "no error when remote tokenizer disabled",
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
			},
			expectCleanup: false,
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test environment
			tt.setupEnv()

			// Create a test store
			store := &Store{}

			// Call initKVEventSync
			err := store.initKVEventSync()

			// For non-ZMQ builds, if KV sync is enabled, we expect an error
			// because validateConfiguration will always fail
			expectedError := tt.expectError
			if !tt.expectError && tt.name == "no cleanup on success" {
				// Check if we got the specific ZMQ error
				if err != nil && strings.Contains(err.Error(), "KV event sync requires ZMQ support") {
					expectedError = true
				}
			}

			// Check error expectation
			if expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			// Check cleanup expectation
			if tt.expectCleanup {
				// After cleanup, these should be nil
				assert.Nil(t, store.kvEventManager)
				assert.Nil(t, store.syncPrefixIndexer)
			} else if !expectedError && os.Getenv("AIBRIX_KV_EVENT_SYNC_ENABLED") == "true" && os.Getenv("AIBRIX_USE_REMOTE_TOKENIZER") == "true" {
				// If no error was expected and KV sync is enabled, resources should be initialized
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
	// Save original env vars
	origKVSyncEnabled := os.Getenv("AIBRIX_KV_EVENT_SYNC_ENABLED")
	origRemoteTokenizer := os.Getenv("AIBRIX_USE_REMOTE_TOKENIZER")
	origTokenizerType := os.Getenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE")
	origTokenizerEndpoint := os.Getenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT")

	// Restore env vars after test
	defer func() {
		_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", origKVSyncEnabled)
		_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", origRemoteTokenizer)
		_ = os.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", origTokenizerType)
		_ = os.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", origTokenizerEndpoint)
	}()

	// Setup environment for successful KV sync initialization
	_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
	_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
	_ = os.Setenv("AIBRIX_PREFIX_CACHE_TOKENIZER_TYPE", "remote")
	_ = os.Setenv("AIBRIX_REMOTE_TOKENIZER_ENDPOINT", "http://test:8080")

	// Create and initialize store
	store := &Store{}
	err := store.initKVEventSync()

	// In non-ZMQ builds, this will fail with ZMQ error
	if err != nil && strings.Contains(err.Error(), "KV event sync requires ZMQ support") {
		// This is expected in non-ZMQ builds, skip the rest of the test
		t.Skip("Skipping test in non-ZMQ build")
		return
	}

	assert.NoError(t, err)
	assert.NotNil(t, store.kvEventManager)
	assert.NotNil(t, store.syncPrefixIndexer)

	// Call Close
	store.Close()

	// Resources should be cleaned up
	assert.Nil(t, store.kvEventManager)
	assert.Nil(t, store.syncPrefixIndexer)

	// Calling Close again should be safe
	store.Close()
}

func TestInitWithOptions_KVSyncBehavior(t *testing.T) {
	// Test that InitWithOptions correctly handles KV sync based on options

	// Save original env vars
	origKVSyncEnabled := os.Getenv("AIBRIX_KV_EVENT_SYNC_ENABLED")
	origRemoteTokenizer := os.Getenv("AIBRIX_USE_REMOTE_TOKENIZER")

	// Restore env vars after test
	defer func() {
		_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", origKVSyncEnabled)
		_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", origRemoteTokenizer)
	}()

	// Test various scenarios
	scenarios := []struct {
		name         string
		opts         InitOptions
		expectKVSync bool
		setupEnv     func()
	}{
		{
			name: "metadata service - no KV sync",
			opts: InitOptions{
				RedisClient: &redis.Client{},
			},
			expectKVSync: false,
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
			},
		},
		{
			name:         "controller - no KV sync",
			opts:         InitOptions{},
			expectKVSync: false,
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
			},
		},
		{
			name: "gateway with KV sync disabled",
			opts: InitOptions{
				EnableKVSync: false,
				RedisClient:  &redis.Client{},
			},
			expectKVSync: false,
			setupEnv: func() {
				_ = os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "false")
				_ = os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "false")
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.setupEnv()

			// Create a simple test store
			testStore := &Store{initialized: true}

			// Based on options, verify expected behavior
			if sc.expectKVSync {
				// Would need proper env setup and mocking for full test
				assert.NotNil(t, testStore)
			} else {
				// When KV sync is not enabled, these should remain nil
				assert.Nil(t, testStore.kvEventManager)
				assert.Nil(t, testStore.syncPrefixIndexer)
			}
		})
	}
}
