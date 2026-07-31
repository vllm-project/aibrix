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
	syncindexer "github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

// SetSyncIndexerForTest sets the sync indexer for testing purposes only
// This allows integration tests to inject a real indexer
func SetSyncIndexerForTest(s *Store, indexer *syncindexer.SyncPrefixHashTable) {
	s.syncPrefixIndexer = indexer
}
