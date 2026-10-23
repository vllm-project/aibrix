/*
Copyright 2026 The Aibrix Team.

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

package types

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueueEntryRequesterErr(t *testing.T) {
	if err := (&QueueEntry{EnqueuedAt: time.Now()}).RequesterErr(); err != nil {
		t.Fatalf("entry without a routing context: RequesterErr() = %v, want nil", err)
	}

	waiting := NewQueueEntry(NewRoutingContext(context.Background(), RoutingAlgorithm("slo"), "m", "", "req-waiting", ""), time.Now())
	if err := waiting.RequesterErr(); err != nil {
		t.Fatalf("waiting requester: RequesterErr() = %v, want nil", err)
	}

	requestCtx, cancel := context.WithCancel(context.Background())
	ctx := NewRoutingContext(requestCtx, RoutingAlgorithm("slo"), "m", "", "req-gone", "")
	entry := NewQueueEntry(ctx, time.Now())
	cancel()
	// The pool hands the context to a new request whose own context is live.
	RecycleRoutingContextForTest(ctx, context.Background(), RoutingAlgorithm("slo"), "m", "", "req-next", "")
	if err := entry.RequesterErr(); !errors.Is(err, context.Canceled) {
		t.Fatalf("after recycling: RequesterErr() = %v, want %v", err, context.Canceled)
	}
}
