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
	"testing"

	"github.com/vllm-project/aibrix/pkg/constants"
)

func TestKVEventManagerValidation(t *testing.T) {
	store := &Store{}
	manager := NewKVEventManager(store)

	tests := []struct {
		name      string
		envVars   map[string]string
		wantError bool
	}{
		{
			name: "disabled - no error",
			envVars: map[string]string{
				constants.EnvPrefixCacheKVEventSyncEnabled: "false",
			},
			wantError: false,
		},
		{
			name: "enabled without tokenizer - error",
			envVars: map[string]string{
				constants.EnvPrefixCacheKVEventSyncEnabled: "true",
				constants.EnvPrefixCacheUseRemoteTokenizer: "false",
			},
			wantError: true,
		},
		{
			name: "enabled without tokenizer type - error",
			envVars: map[string]string{
				constants.EnvPrefixCacheKVEventSyncEnabled: "true",
				constants.EnvPrefixCacheUseRemoteTokenizer: "true",
				constants.EnvPrefixCacheTokenizerType:      "local",
			},
			wantError: true,
		},
		{
			name: "enabled without endpoint - error",
			envVars: map[string]string{
				constants.EnvPrefixCacheKVEventSyncEnabled:      "true",
				constants.EnvPrefixCacheUseRemoteTokenizer:      "true",
				constants.EnvPrefixCacheTokenizerType:           "remote",
				constants.EnvPrefixCacheRemoteTokenizerEndpoint: "",
			},
			wantError: true,
		},
		{
			name: "enabled with all config - no error",
			envVars: map[string]string{
				constants.EnvPrefixCacheKVEventSyncEnabled:      "true",
				constants.EnvPrefixCacheUseRemoteTokenizer:      "true",
				constants.EnvPrefixCacheTokenizerType:           "remote",
				constants.EnvPrefixCacheRemoteTokenizerEndpoint: "http://localhost:8080",
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set environment
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			err := manager.validateConfiguration()
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateConfiguration() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}
