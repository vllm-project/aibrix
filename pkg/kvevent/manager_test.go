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
	"testing"

	"github.com/vllm-project/aibrix/pkg/kvevent"
)

// mockPodProvider implements PodProvider interface for testing
type mockPodProvider struct {
	pods map[string]*kvevent.PodInfo
}

func (m *mockPodProvider) GetPod(ctx context.Context, podKey string) (*kvevent.PodInfo, bool) {
	pod, exists := m.pods[podKey]
	return pod, exists
}

func (m *mockPodProvider) RangePods(ctx context.Context, f func(key string, pod *kvevent.PodInfo) bool) error {
	for key, pod := range m.pods {
		if !f(key, pod) {
			break
		}
	}
	return nil
}

// mockSyncIndexProvider implements SyncIndexProvider interface for testing
type mockSyncIndexProvider struct {
	indexer kvevent.SyncIndexer
	err     error
	// Allow overriding GetSyncIndexer for specific test cases
	GetSyncIndexerFunc func(ctx context.Context) (kvevent.SyncIndexer, error)
}

func (m *mockSyncIndexProvider) GetSyncIndexer(ctx context.Context) (kvevent.SyncIndexer, error) {
	if m.GetSyncIndexerFunc != nil {
		return m.GetSyncIndexerFunc(ctx)
	}
	return m.indexer, m.err
}

// mockSyncIndexer implements SyncIndexer interface for testing
type mockSyncIndexer struct{}

func (m *mockSyncIndexer) ProcessBlockStored(ctx context.Context, event kvevent.BlockStoredEvent) error {
	return nil
}

func (m *mockSyncIndexer) ProcessBlockRemoved(ctx context.Context, event kvevent.BlockRemovedEvent) error {
	return nil
}

func (m *mockSyncIndexer) RemovePrefix(ctx context.Context, modelName string, loraID int64, podKey string) error {
	return nil
}

// TestManagerCreation tests basic manager creation
func TestManagerCreation(t *testing.T) {
	podProvider := &mockPodProvider{
		pods: make(map[string]*kvevent.PodInfo),
	}
	syncProvider := &mockSyncIndexProvider{
		indexer: &mockSyncIndexer{},
		err:     nil,
	}

	manager := kvevent.NewManager(podProvider, syncProvider)
	if manager == nil {
		t.Fatal("Expected manager to be created")
	}

	// Test Stop (should not panic)
	manager.Stop()
}