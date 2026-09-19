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
