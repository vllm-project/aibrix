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

package cache

import (
	"fmt"
	"strconv"

	"github.com/vllm-project/aibrix/pkg/constants"
	"github.com/vllm-project/aibrix/pkg/kvevent"
	"github.com/vllm-project/aibrix/pkg/utils"
)

// KVEventManager wraps the new kvevent.Manager for backward compatibility
type KVEventManager struct {
	*kvevent.Manager
}

// NewKVEventManager creates the event manager with backward compatible API
func NewKVEventManager(store *Store) *KVEventManager {
	// Create adapter to implement provider interfaces
	podProvider, syncProvider := NewStoreProviderAdapter(store)
	manager := kvevent.NewManager(podProvider, syncProvider)
	return &KVEventManager{Manager: manager}
}

// validateKVEventConfiguration checks if KV event sync configuration is valid
func validateKVEventConfiguration() error {
	// Check if KV sync is enabled
	kvSyncValue := utils.LoadEnv(constants.EnvKVEventSyncEnabled, "false")
	kvSyncRequested, err := strconv.ParseBool(kvSyncValue)
	if err != nil {
		return fmt.Errorf("invalid boolean value for %s: %q", constants.EnvKVEventSyncEnabled, kvSyncValue)
	}

	if !kvSyncRequested {
		// Not an error - just disabled
		return nil
	}

	// If enabled, check requirements
	remoteTokenValue := utils.LoadEnv(constants.EnvUseRemoteTokenizer, "false")
	remoteTokenizerEnabled, err := strconv.ParseBool(remoteTokenValue)
	if err != nil {
		return fmt.Errorf("invalid boolean value for %s: %q", constants.EnvUseRemoteTokenizer, remoteTokenValue)
	}

	if !remoteTokenizerEnabled {
		return fmt.Errorf("KV event sync requires remote tokenizer (set %s=true)", constants.EnvUseRemoteTokenizer)
	}

	// Check tokenizer type
	tokenizerType := utils.LoadEnv(constants.EnvPrefixCacheTokenizerType, "")
	if tokenizerType != "remote" {
		return fmt.Errorf("KV event sync requires %s=remote (got %q)", constants.EnvPrefixCacheTokenizerType, tokenizerType)
	}

	// Check remote tokenizer endpoint
	endpoint := utils.LoadEnv(constants.EnvRemoteTokenizerEndpoint, "")
	if endpoint == "" {
		return fmt.Errorf("KV event sync requires %s to be set", constants.EnvRemoteTokenizerEndpoint)
	}

	return nil
}

// validateConfiguration for backward compatibility
func (m *KVEventManager) validateConfiguration() error {
	return validateKVEventConfiguration()
}

// Ensure all methods are delegated (they are embedded, but be explicit if needed)
