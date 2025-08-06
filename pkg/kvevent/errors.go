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

package kvevent

import (
	"context"
	"errors"
)

// Sentinel errors for known conditions
var (
	// ErrIndexerNotInitialized indicates the sync indexer is not yet ready
	ErrIndexerNotInitialized = errors.New("sync indexer is not yet initialized")

	// ErrPodNotFound indicates the requested pod was not found
	ErrPodNotFound = errors.New("pod not found")

	// ErrManagerStopped indicates the manager has been stopped
	ErrManagerStopped = errors.New("kv event manager has been stopped")

	// ErrZMQNotSupported indicates ZMQ support is not compiled in
	ErrZMQNotSupported = errors.New("ZMQ support not compiled in (build with -tags=zmq)")
)

// IsTemporaryError returns true if the error is temporary and operation can be retried
func IsTemporaryError(err error) bool {
	return errors.Is(err, ErrIndexerNotInitialized) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}