// Copyright 2026 The AIBrix Authors
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

// decoder_sglang_compat_test.go — decoder compatibility with the SGLang wire
// format. Each fixture is a payload frame captured from SGLang's
// ZmqEventPublisher, which encodes the structs in
// python/sglang/srt/disaggregation/kv_events.py with msgspec: an
// array-encoded KVEventBatch [ts, events, attn_dp_rank] of map-encoded
// events, here with ts=1760000000.5 and attn_dp_rank=0.

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Page size 16, two pages of one chain coalesced into one event:
	// BlockStored(block_hashes=[101, -7046029254386353131],
	//   parent_block_hash=None, token_ids=list(range(1, 33)), block_size=16,
	//   lora_id=None, medium="GPU")
	sglangPage16 = "93cb41da39de002000009187a474797065ab426c6f636b53746f726564ac626c6f636b5f6861736865739265d39e3779b97f4a7c15b1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f696473dc00200102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20aa626c6f636b5f73697a6510a76c6f72615f6964c0a66d656469756da347505500"
	// Page size 1, SGLang's default on CUDA:
	// BlockStored(block_hashes=[103, 104, 105], parent_block_hash=None,
	//   token_ids=[7, 8, 9], block_size=1, lora_id=None, medium="GPU")
	sglangPage1 = "93cb41da39de002000009187a474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657393676869b1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f69647393070809aa626c6f636b5f73697a6501a76c6f72615f6964c0a66d656469756da347505500"
	// EAGLE keys the radix tree by bigrams, so each position of a page is
	// (token, next token). Raw tokens 11..15 in two pages of size 2:
	// BlockStored(block_hashes=[106, 107], parent_block_hash=None,
	//   token_ids=[(11, 12), (12, 13), (13, 14), (14, 15)], block_size=2,
	//   lora_id=None, medium="GPU")
	sglangEagle = "93cb41da39de002000009187a474797065ab426c6f636b53746f726564ac626c6f636b5f686173686573926a6bb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f69647394920b0c920c0d920d0e920e0faa626c6f636b5f73697a6502a76c6f72615f6964c0a66d656469756da347505500"
	// HiCache tiers, with h1=101 and h2=-7046029254386353131:
	// BlockStored(block_hashes=[h1, h2], parent_block_hash=None,
	//   token_ids=list(range(1, 33)), block_size=16, lora_id=None,
	//   medium="CPU_PINNED"),
	// BlockRemoved(block_hashes=[h1, h2], medium="GPU"),
	// BlockRemoved(block_hashes=[h2], medium="CPU_PINNED"),
	// BlockRemoved(block_hashes=[h1], medium="DISK"),
	// AllBlocksCleared()
	sglangHiCache = "93cb41da39de002000009587a474797065ab426c6f636b53746f726564ac626c6f636b5f6861736865739265d39e3779b97f4a7c15b1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f696473dc00200102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20aa626c6f636b5f73697a6510a76c6f72615f6964c0a66d656469756daa4350555f50494e4e454483a474797065ac426c6f636b52656d6f766564ac626c6f636b5f6861736865739265d39e3779b97f4a7c15a66d656469756da347505583a474797065ac426c6f636b52656d6f766564ac626c6f636b5f68617368657391d39e3779b97f4a7c15a66d656469756daa4350555f50494e4e454483a474797065ac426c6f636b52656d6f766564ac626c6f636b5f6861736865739165a66d656469756da44449534b81a474797065b0416c6c426c6f636b73436c656172656400"
)

// sglangTokens returns tokens from..to-1 as one block of big-endian uint32s.
func sglangTokens(from, to uint32) []byte {
	var b []byte
	for id := from; id < to; id++ {
		b = binary.BigEndian.AppendUint32(b, id)
	}
	return b
}

func TestDecodeSGLangPageSizes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		hashes  []int64
		tokens  [][]byte
	}{
		{"page size 16", sglangPage16, []int64{101, -7046029254386353131},
			[][]byte{sglangTokens(1, 17), sglangTokens(17, 33)}},
		{"page size 1", sglangPage1, []int64{103, 104, 105},
			[][]byte{sglangTokens(7, 8), sglangTokens(8, 9), sglangTokens(9, 10)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			batch, err := DecodeEventBatch(decodeHex(t, tt.payload), "m", "p")
			require.NoError(t, err)
			assert.Equal(t, time.Unix(1760000000, 500_000_000).UTC(), batch.Timestamp)
			require.Len(t, batch.Events, 1)
			ev, ok := batch.Events[0].(*BlockStoredEvent)
			require.True(t, ok, "got %T", batch.Events[0])
			assert.Equal(t, tt.hashes, ev.BlockHashes)
			assert.Nil(t, ev.ParentBlockHash)
			assert.Equal(t, tt.tokens, ev.TokenIDs)
			assert.Nil(t, ev.LoraID)
			require.NotNil(t, ev.Medium)
			assert.Equal(t, "GPU", *ev.Medium)
		})
	}
}

// With EAGLE, a page's tokens are the first elements of its pairs.
func TestDecodeSGLangEagleBigramPages(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, sglangEagle))
	assert.Equal(t, []int64{106, 107}, ev.BlockHashes)
	assert.Equal(t, [][]byte{sglangTokens(11, 13), sglangTokens(13, 15)}, ev.TokenIDs)
}

func TestDecodeSGLangHiCacheEvents(t *testing.T) {
	batch, err := DecodeEventBatch(decodeHex(t, sglangHiCache), "m", "p")
	require.NoError(t, err)
	require.Len(t, batch.Events, 5)

	stored, ok := batch.Events[0].(*BlockStoredEvent)
	require.True(t, ok, "event 0: got %T", batch.Events[0])
	require.NotNil(t, stored.Medium)
	assert.Equal(t, "CPU_PINNED", *stored.Medium)

	removals := []struct {
		hashes []int64
		medium string
	}{
		{[]int64{101, -7046029254386353131}, "GPU"},
		{[]int64{-7046029254386353131}, "CPU_PINNED"},
		{[]int64{101}, "DISK"},
	}
	for i, want := range removals {
		removed, ok := batch.Events[i+1].(*BlockRemovedEvent)
		require.True(t, ok, "event %d: got %T", i+1, batch.Events[i+1])
		assert.Equal(t, want.hashes, removed.BlockHashes)
		require.NotNil(t, removed.Medium)
		assert.Equal(t, want.medium, *removed.Medium)
	}

	_, ok = batch.Events[4].(*AllBlocksClearedEvent)
	assert.True(t, ok, "event 4: got %T", batch.Events[4])
}
