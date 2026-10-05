//go:build !zmq

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
	"testing"
)

func TestBuildModeIsDefault(t *testing.T) {
	t.Log("✅ Verified default build (no ZMQ)")

	// Verify stub behavior
	manager := NewKVEventManager(nil)

	// This should not error in default build
	err := manager.Start()
	if err != nil {
		t.Errorf("Default build Start() should not error, got: %v", err)
	}

	// Verify it's the stub implementation
	if manager.enabled {
		t.Error("Default build should have enabled=false")
	}
}
