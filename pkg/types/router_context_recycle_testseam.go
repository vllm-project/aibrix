/*
Copyright 2024 The Aibrix Team.

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
	"flag"
)

// RecycleRoutingContextForTest replays, on this exact object, what happens to a
// pooled RoutingContext when one request ends and the next request picks the
// same object back out of requestPool: a reset for the new request. It returns
// r itself, so the caller holds one object that is simultaneously the retired
// incarnation and the live one - exactly the aliasing that the PD leg state
// must survive.
//
// It exists only because that aliasing cannot be staged reliably through
// requestPool from a test: under the race detector sync.Pool.Put drops roughly
// one in four objects on the floor, so "release it and keep asking until the
// pool hands the same pointer back" fails at random and no retry loop can fix
// it. Tests in other packages also cannot call the unexported reset directly.
//
// This is a test seam, not part of the request lifecycle:
//   - it never goes through requestPool, so nothing guarantees the object is
//     free - the caller must know it is;
//   - it panics outside a test binary (the testing package registers test.v),
//     so a production path cannot reach it even by accident.
//
// Delete remains the one supported way to release a RoutingContext.
func RecycleRoutingContextForTest(r *RoutingContext, ctx context.Context, algorithms RoutingAlgorithm, model, message, requestID, user string) *RoutingContext {
	if flag.Lookup("test.v") == nil {
		panic("types.RecycleRoutingContextForTest is a test-only seam and must not be called outside tests")
	}
	if r == nil {
		return nil
	}
	r.reset(ctx, algorithms, model, message, requestID, user)
	return r
}
