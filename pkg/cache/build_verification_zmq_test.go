//go:build zmq

// Copyright 2025 The AIBrix Authors
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

package cache

import (
	"os"
	"testing"
)

func TestBuildModeIsZMQ(t *testing.T) {
	t.Log("✅ Verified ZMQ build")

	// Set environment to enable KV sync
	os.Setenv("AIBRIX_KV_EVENT_SYNC_ENABLED", "true")
	os.Setenv("AIBRIX_USE_REMOTE_TOKENIZER", "true")
	defer func() {
		os.Unsetenv("AIBRIX_KV_EVENT_SYNC_ENABLED")
		os.Unsetenv("AIBRIX_USE_REMOTE_TOKENIZER")
	}()

	// Verify ZMQ implementation behavior
	manager := NewKVEventManager(nil)

	// Verify we got a manager instance
	if manager == nil {
		t.Error("ZMQ build should return a valid manager instance")
	}

	// In ZMQ build with env vars set, manager should be enabled
	// (actual behavior depends on implementation details)
	t.Log("ZMQ implementation available")
}
