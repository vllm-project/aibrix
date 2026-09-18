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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPDLegStateNilReceiverIsInert(t *testing.T) {
	var leg *PDLegState

	assert.NotPanics(t, func() {
		leg.SetRID("rid")
		leg.MarkDecodeResponded()
		leg.SetDecodeTarget("10.0.0.1:8000", "decode-1")
		assert.False(t, leg.SetPrefillFailure(&PrefillFailure{Class: "timeout"}))
	})
	assert.Empty(t, leg.RID())
	assert.False(t, leg.DecodeResponded())
	assert.Nil(t, leg.PrefillFailure())
	addr, name := leg.DecodeTarget()
	assert.Empty(t, addr)
	assert.Empty(t, name)

	// A nil channel blocks forever in a select, which is the correct "this can
	// never fire" behaviour for a stream that has no PD leg.
	assert.Nil(t, leg.PrefillFailed())

	// There is no abort to bound or to wait for, so the context is the
	// background one and the join point is ready immediately - a caller that
	// waits on it must not hang.
	assert.NotPanics(t, func() { leg.FinishDecodeAbort() })
	assert.NoError(t, leg.AbortContext().Err())
	select {
	case <-leg.AbortDone():
	default:
		t.Fatal("a stream with no PD leg has no abort to wait for")
	}

	// The RoutingContext wrappers are nil-safe too: the gateway builds bare
	// struct literals for metric emission, and those carry no leg.
	bare := &RoutingContext{}
	assert.NotPanics(t, func() {
		bare.MarkDecodeResponded()
		bare.SetPDRequestID("rid")
		bare.SetDecodeTarget("10.0.0.1:8000", "decode-1")
	})
	assert.Empty(t, bare.PDRequestID())
	assert.Nil(t, bare.PrefillFailure())
	assert.Nil(t, bare.PDLeg())
}

func TestPDLegStateFirstFailureWinsAndClosesOnce(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-1", "u")
	leg := ctx.PDLeg()
	require.NotNil(t, leg)

	const racers = 16
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		wins  = make(chan *PrefillFailure, racers)
	)
	start.Add(1)
	for i := 0; i < racers; i++ {
		done.Add(1)
		failure := &PrefillFailure{Class: "transport"}
		go func() {
			defer done.Done()
			start.Wait()
			if leg.SetPrefillFailure(failure) {
				wins <- failure
			}
		}()
	}
	start.Done()
	done.Wait()
	close(wins)

	// Exactly one caller records the failure, so exactly one closes the wakeup
	// channel: a second close would panic and take the gateway down.
	var winners []*PrefillFailure
	for f := range wins {
		winners = append(winners, f)
	}
	require.Len(t, winners, 1)
	assert.Same(t, winners[0], leg.PrefillFailure())

	select {
	case <-ctx.PrefillFailed():
	default:
		t.Fatal("recording a prefill failure must close the wakeup channel")
	}
}

// A recycled RoutingContext must carry a brand-new leg, so a goroutine still
// holding the old one cannot report onto the request that took its place.
func TestPDLegStateReplacedOnReset(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-1", "u")
	old := ctx.PDLeg()
	require.NotNil(t, old)
	old.SetRID("req-1-aaaaaaaabbbbbbbb")
	old.MarkDecodeResponded()
	old.SetDecodeTarget("10.0.0.1:8000", "decode-1")
	require.True(t, old.SetPrefillFailure(&PrefillFailure{Class: "timeout"}))

	same := RecycleRoutingContextForTest(ctx, context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-2", "u")
	require.Same(t, ctx, same)

	fresh := ctx.PDLeg()
	require.NotNil(t, fresh)
	require.NotSame(t, old, fresh)
	assert.Empty(t, ctx.PDRequestID())
	assert.False(t, ctx.DecodeResponded())
	assert.Nil(t, ctx.PrefillFailure())
	addr, name := ctx.DecodeTarget()
	assert.Empty(t, addr)
	assert.Empty(t, name)
	select {
	case <-ctx.PrefillFailed():
		t.Fatal("the recycled context inherited the retired leg's wakeup edge")
	default:
	}

	// The retired leg keeps its own state; it is simply no longer reachable
	// from the context.
	assert.NotNil(t, old.PrefillFailure())
	assert.Equal(t, "req-1-aaaaaaaabbbbbbbb", old.RID())
}

// The abort context is built lazily, so the order in which the decode pod
// starts answering and the abort goroutine asks for its context decides which
// half of MarkDecodeResponded/AbortContext has to do the cancelling. Both
// orders must end with a cancelled context, or a decode leg that is already
// streaming keeps being aborted.
func TestPDLegStateAbortContextCancelledBothOrders(t *testing.T) {
	t.Run("context first", func(t *testing.T) {
		leg := newPDLegState()
		abortCtx := leg.AbortContext()
		require.NoError(t, abortCtx.Err())

		leg.MarkDecodeResponded()
		assert.ErrorIs(t, abortCtx.Err(), context.Canceled)
	})

	t.Run("decode first", func(t *testing.T) {
		leg := newPDLegState()
		leg.MarkDecodeResponded()

		// Nothing existed to cancel at the time, so the context has to come
		// back already cancelled.
		assert.ErrorIs(t, leg.AbortContext().Err(), context.Canceled)
	})

	t.Run("same context every time", func(t *testing.T) {
		leg := newPDLegState()
		assert.Same(t, leg.AbortContext(), leg.AbortContext())
	})
}

// FinishDecodeAbort is reached from the goroutine that sent the abort and from
// the deferred "no abort was started after all" path, and both can run for the
// same leg. Closing the channel twice would panic and take the gateway down.
func TestPDLegStateFinishDecodeAbortIsIdempotent(t *testing.T) {
	leg := newPDLegState()
	abortCtx := leg.AbortContext()

	select {
	case <-leg.AbortDone():
		t.Fatal("the abort of a leg that has not finished must not look done")
	default:
	}

	const finishers = 8
	var wg sync.WaitGroup
	for i := 0; i < finishers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leg.FinishDecodeAbort()
		}()
	}
	wg.Wait()

	select {
	case <-leg.AbortDone():
	default:
		t.Fatal("FinishDecodeAbort must make AbortDone ready")
	}
	// Finishing also releases the context, so a straggler that still holds it
	// stops waiting instead of burning its full timeout.
	assert.ErrorIs(t, abortCtx.Err(), context.Canceled)
}

// A recycled context hands out a leg whose abort lifecycle starts over, while
// an abort still running on the retired leg keeps its own context and its own
// join point.
func TestPDLegStateAbortLifecycleIsPerIncarnation(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-1", "u")
	old := ctx.PDLeg()
	require.NotNil(t, old)
	oldAbortCtx := old.AbortContext()

	same := RecycleRoutingContextForTest(ctx, context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-2", "u")
	require.Same(t, ctx, same)
	fresh := ctx.PDLeg()
	require.NotSame(t, old, fresh)

	// Reuse deliberately does not cancel the retired leg: the abort is the one
	// piece of work that has to outlive the client stream, and the stream ends
	// microseconds before the context is handed out again.
	assert.NoError(t, oldAbortCtx.Err())
	assert.NotSame(t, oldAbortCtx, fresh.AbortContext())

	select {
	case <-fresh.AbortDone():
		t.Fatal("the new incarnation inherited the retired leg's join point")
	default:
	}

	// The retired leg's abort still terminates on its own, and only its own
	// join point becomes ready.
	old.FinishDecodeAbort()
	assert.ErrorIs(t, oldAbortCtx.Err(), context.Canceled)
	select {
	case <-old.AbortDone():
	default:
		t.Fatal("the retired leg's abort never finished")
	}
	select {
	case <-fresh.AbortDone():
		t.Fatal("finishing the retired abort must not release the new one")
	default:
	}
}

func TestPodAddressMatchesTargetAddress(t *testing.T) {
	ctx := NewRoutingContext(context.Background(), RoutingAlgorithm("pd"), "m", "msg", "req-1", "u")
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "decode-1",
			Labels: map[string]string{"model.aibrix.ai/port": "18000"},
		},
		Status: v1.PodStatus{PodIP: "10.1.2.3"},
	}

	addr := ctx.PodAddress(pod)
	assert.Equal(t, "10.1.2.3:18000", addr)

	// The abort must land on the very server the decode leg is forwarded to.
	ctx.SetTargetPod(pod)
	assert.Equal(t, ctx.TargetAddress(), addr)

	assert.Empty(t, ctx.PodAddress(nil))
	assert.Empty(t, ctx.PodAddress(&v1.Pod{}), "a pod without an IP has no address to abort")
}
