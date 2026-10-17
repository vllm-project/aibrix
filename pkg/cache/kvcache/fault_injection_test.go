//go:build zmq

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

package kvcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vllm-project/aibrix/pkg/utils/syncprefixcacheindexer"
)

// kvViewHandler applies decoded KV events to a real prefix index. The harness
// uses it to turn an event stream into the same view the gateway routes on.
type kvViewHandler struct {
	index *syncprefixcacheindexer.SyncPrefixHashTable
	model string
	lora  int64
	pod   string
}

func (h *kvViewHandler) HandleEvent(event KVEvent) error {
	switch e := event.(type) {
	case *BlockStoredEvent:
		return h.index.ProcessBlockStored(syncprefixcacheindexer.BlockStored{
			BlockHashes: e.BlockHashes,
			Tokens:      e.TokenIDs,
			ModelName:   h.model,
			LoraID:      h.lora,
			SourcePod:   h.pod,
		})
	case *BlockRemovedEvent:
		return h.index.ProcessBlockRemoved(syncprefixcacheindexer.BlockRemoved{
			BlockHashes: e.BlockHashes,
			ModelName:   h.model,
			LoraID:      h.lora,
			SourcePod:   h.pod,
			GroupIdx:    -1,
		})
	case *AllBlocksClearedEvent:
		return h.index.RemovePrefix(h.model, h.lora, h.pod)
	}
	return nil
}

type faultArrival struct {
	seq    int64
	replay bool
	events []KVEvent
}

type viewFaultScenario struct {
	name       string
	arrivals   []faultArrival
	recoverGap bool
	// hits lists the prefixes that must still match after the stream, keyed by
	// the prefix token bytes. Every other known prefix is a false hit.
	hits map[string]bool
}

const (
	faultModel = "model"
	faultPod   = "pod-a"
)

func faultPrefix(seed byte) []byte {
	tokens := make([]byte, 32) // two 16-byte blocks
	for i := range tokens {
		tokens[i] = seed
	}
	return tokens
}

func storedEvents(hashes []int64, prefix []byte) []KVEvent {
	tokenIDs := make([][]byte, 0, len(hashes))
	for i := range hashes {
		tokenIDs = append(tokenIDs, prefix[i*16:(i+1)*16])
	}
	return []KVEvent{&BlockStoredEvent{
		Type:        EventTypeBlockStored,
		BlockHashes: hashes,
		TokenIDs:    tokenIDs,
	}}
}

func removedEvents(hashes []int64) []KVEvent {
	return []KVEvent{&BlockRemovedEvent{
		Type:        EventTypeBlockRemoved,
		BlockHashes: hashes,
	}}
}

// guardedHarness runs arrivals through the real ZMQ client guard, including
// replay/duplicate handling, and routes on the resulting index.
type viewHarness struct {
	client *ZMQClient
	index  *syncprefixcacheindexer.SyncPrefixHashTable
	model  string
	lora   int64
	pod    string
}

func newGuardedViewHarness(t *testing.T) *viewHarness {
	t.Helper()
	index := syncprefixcacheindexer.NewSyncPrefixHashTable()
	t.Cleanup(index.Close)
	h := &viewHarness{
		index: index,
		model: faultModel,
		lora:  -1,
		pod:   faultPod,
	}
	client := NewZMQClient(&ZMQClientConfig{
		PodKey:         faultPod,
		ModelName:      faultModel,
		ReconnectDelay: 10 * time.Millisecond,
	}, &kvViewHandler{index: index, model: h.model, lora: h.lora, pod: h.pod})
	t.Cleanup(client.Stop)
	h.client = client
	return h
}

func (h *viewHarness) apply(t *testing.T, arrival faultArrival) {
	t.Helper()
	_, err := h.client.applyEventBatch(arrival.seq, encodeFaultBatch(t, arrival.events...), arrival.replay)
	require.NoError(t, err)
}

func (h *viewHarness) fullHit(prefix []byte) bool {
	matches, hashes := h.index.MatchPrefix(
		h.model, h.lora, prefix, map[string]struct{}{h.pod: {}},
	)
	want := len(h.index.GetPrefixHashes(prefix))
	return len(hashes) == want && matches[h.pod] > 0
}

// legacyHarness reproduces the pre-guard dispatch: every received batch is
// applied in arrival order and lastSeq is overwritten unconditionally.
type legacyViewHarness struct {
	handler *kvViewHandler
}

func newLegacyViewHarness(t *testing.T) *legacyViewHarness {
	t.Helper()
	index := syncprefixcacheindexer.NewSyncPrefixHashTable()
	t.Cleanup(index.Close)
	return &legacyViewHarness{handler: &kvViewHandler{
		index: index, model: faultModel, lora: -1, pod: faultPod,
	}}
}

func (h *legacyViewHarness) apply(t *testing.T, arrival faultArrival) {
	t.Helper()
	for _, event := range arrival.events {
		require.NoError(t, h.handler.HandleEvent(event))
	}
}

func (h *legacyViewHarness) fullHit(prefix []byte) bool {
	matches, hashes := h.handler.index.MatchPrefix(
		h.handler.model, h.handler.lora, prefix, map[string]struct{}{h.handler.pod: {}},
	)
	want := len(h.handler.index.GetPrefixHashes(prefix))
	return len(hashes) == want && matches[h.handler.pod] > 0
}

func encodeFaultBatch(t *testing.T, events ...KVEvent) []byte {
	t.Helper()
	payload, err := EncodeEventBatch(&EventBatch{Events: events})
	require.NoError(t, err)
	return payload
}

// TestKVEventViewFaultInjection drives the real event guard with adversarial
// streams and checks the resulting prefix view against the engine truth. The
// "legacy" column applies the same arrivals with the pre-guard dispatch so the
// false-hit reduction is visible as a number.
func TestKVEventViewFaultInjection(t *testing.T) {
	prefixA := faultPrefix('A')
	prefixB := faultPrefix('B')
	hashesA := []int64{101, 102}
	hashesB := []int64{201, 202}

	tests := []viewFaultScenario{
		{
			name: "stale replay after removal",
			arrivals: []faultArrival{
				{seq: 1, events: storedEvents(hashesA, prefixA)},
				{seq: 2, events: removedEvents(hashesA)},
				{seq: 1, replay: true, events: storedEvents(hashesA, prefixA)},
			},
			hits: map[string]bool{string(prefixA): false, string(prefixB): false},
		},
		{
			name: "publisher restart clears the old view",
			arrivals: []faultArrival{
				{seq: 1, events: storedEvents(hashesA, prefixA)},
				{seq: 1, events: storedEvents(hashesB, prefixB)},
			},
			hits: map[string]bool{string(prefixA): false, string(prefixB): true},
		},
		{
			name: "unrecoverable gap clears the view",
			arrivals: []faultArrival{
				{seq: 1, events: storedEvents(hashesA, prefixA)},
				{seq: 4, events: removedEvents(hashesA)},
			},
			recoverGap: true,
			hits:       map[string]bool{string(prefixA): false},
		},
	}

	totalGuarded, totalLegacy := 0, 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guarded := newGuardedViewHarness(t)
			legacy := newLegacyViewHarness(t)

			for _, arrival := range tt.arrivals {
				guarded.apply(t, arrival)
				legacy.apply(t, arrival)
			}
			if tt.recoverGap {
				guarded.client.recoverStalledGap()
			}

			guardedFalseHits, legacyFalseHits := 0, 0
			for prefix, wantHit := range tt.hits {
				got := guarded.fullHit([]byte(prefix))
				if got != wantHit {
					t.Errorf("guarded view for prefix %q = %v, want %v",
						string(prefix[0]), got, wantHit)
				}
				if !wantHit && got {
					guardedFalseHits++
				}
				legacyHit := legacy.fullHit([]byte(prefix))
				if !wantHit && legacyHit {
					legacyFalseHits++
				}
			}

			t.Logf("%s: guarded false hits=%d, legacy false hits=%d",
				tt.name, guardedFalseHits, legacyFalseHits)
			totalGuarded += guardedFalseHits
			totalLegacy += legacyFalseHits
		})
	}
	t.Logf("total false hits: guarded=%d legacy=%d", totalGuarded, totalLegacy)
	if totalGuarded != 0 {
		t.Errorf("guarded stream produced %d false hits", totalGuarded)
	}
	if totalLegacy == 0 {
		t.Log("note: legacy dispatch did not produce a false hit in these scenarios")
	}
}
