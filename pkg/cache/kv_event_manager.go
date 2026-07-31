//go:build !zmq

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

	v1 "k8s.io/api/core/v1"
)

// KVEventManager stub implementation when ZMQ is not available
type KVEventManager struct {
	store   *Store
	enabled bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// NewKVEventManager creates a stub KV event manager
func NewKVEventManager(store *Store) *KVEventManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &KVEventManager{
		store:   store,
		enabled: false,
		ctx:     ctx,
		cancel:  cancel,
	}
}

// validateConfiguration validates the manager configuration
func (m *KVEventManager) validateConfiguration() error {
	// KV event sync requires ZMQ support which is not available in this build
	return fmt.Errorf("KV event sync requires ZMQ support (build with -tags=zmq)")
}

// Start is a no-op
func (m *KVEventManager) Start() error {
	return nil
}

// Stop is a no-op
func (m *KVEventManager) Stop() {
	m.cancel()
}

// OnPodAdd is a no-op
func (m *KVEventManager) OnPodAdd(pod *v1.Pod) {}

// OnPodUpdate is a no-op
func (m *KVEventManager) OnPodUpdate(oldPod, newPod *v1.Pod) {}

// OnPodDelete is a no-op
func (m *KVEventManager) OnPodDelete(pod *v1.Pod) {}
