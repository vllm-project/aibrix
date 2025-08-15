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

// Package kvevent provides KV cache event synchronization functionality.
//
// This package implements the event management system for AIBrix's distributed
// KV cache. It allows multiple vLLM pods to share cached attention computations
// by synchronizing cache events across the cluster.
//
// Architecture:
//
// The package defines interfaces that decouple it from the cache implementation:
//   - PodProvider: Provides access to pod information
//   - SyncIndexProvider: Provides access to the sync indexer
//   - SyncIndexer: Handles cache event processing
//
// This design breaks the circular dependency that existed when the event
// manager was part of the cache package. The cache package implements these
// interfaces through adapter patterns that act as Anti-Corruption Layers,
// maintaining clean architectural boundaries.
//
// Why Adapters are Necessary:
//
// The syncIndexerAdapter in pkg/cache might seem like unnecessary complexity,
// but it serves critical architectural purposes:
//  1. Prevents circular dependencies between packages
//  2. Acts as an Anti-Corruption Layer between domains
//  3. Allows packages to remain independent and testable
//  4. Handles type conversion between nearly-identical structs
//
// Build Requirements:
//
// This package requires ZMQ support. Build with:
//
//	go build -tags=zmq
package kvevent
