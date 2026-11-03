package kvcache

// decoder_vllm_compat_test.go — decoder compatibility with the vLLM wire
// formats, verified against byte-exact payloads generated with msgspec from
// the structs in vllm/distributed/kv_events.py.
//
// vLLM sends one EventBatch per ZMQ message, [ts, events, data_parallel_rank],
// with map-encoded events since vllm-project/vllm#42892 (v0.24) and
// array-encoded events before it. Most fixtures below are single events
// outside a batch, which the decoder also accepts.

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Map-encoded events (vLLM >= #42892), each outside a batch.
const (
	vllmMapMinimal   = "88a474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f6964739401020304aa626c6f636b5f73697a6504a76c6f72615f6964c0a66d656469756dc0a96c6f72615f6e616d65c0"
	vllmMapHybrid    = "89a474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f6964739401020304aa626c6f636b5f73697a6504a76c6f72615f6964c0a66d656469756da3475055a96c6f72615f6e616d65c0a967726f75705f69647801"
	vllmMapExtraKeys = "8aa474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f6964739401020304aa626c6f636b5f73697a6504a76c6f72615f6964c0a66d656469756da3475055a96c6f72615f6e616d65a9616461707465722d31aa65787472615f6b6579739192a4696d6730a4656d6230a967726f75705f69647802"
	vllmMapMaximal   = "8da474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f6964739401020304aa626c6f636b5f73697a6504a76c6f72615f696401a66d656469756da3475055a96c6f72615f6e616d65a9616461707465722d31aa65787472615f6b6579739192a4696d6730a4656d6230a967726f75705f69647805b26b765f63616368655f737065635f6b696e64a6687962726964bc6b765f63616368655f737065635f736c6964696e675f77696e646f77cc80a86c6f63616c697479a54c4f43414c"
	vllmMapRemoved   = "84a474797065ac426c6f636b52656d6f766564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fa66d656469756da3475055a967726f75705f69647801"
	vllmMapCleared   = "81a474797065b0416c6c426c6f636b73436c6561726564"
)

// vllmBatch is a whole message as vLLM v0.31's ZmqEventPublisher encodes it:
// EventBatch(ts=1760000000.5, data_parallel_rank=0, events=[
//
//	BlockStored(block_hashes=[h1], parent_block_hash=None, token_ids=[1, 2, 3, 4],
//	  block_size=4, medium="GPU", extra_keys=[None], group_idx=0,
//	  kv_cache_spec_kind="full_attention"),
//	BlockStored(block_hashes=[h2], parent_block_hash=h1, token_ids=[5, 6, 7, 8],
//	  block_size=4, medium="CPU", ownership="cpu", session_id="s1"),
//	BlockRemoved(block_hashes=[h1], medium="GPU", group_idx=0),
//	AllBlocksCleared(),
//
// ]) with h1 = bytes(range(32)) and h2 = bytes(range(32, 64)).
const vllmBatch = "93cb41da39de00200000948ba474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fb1706172656e745f626c6f636b5f68617368c0a9746f6b656e5f6964739401020304aa626c6f636b5f73697a6504a76c6f72615f6964c0a66d656469756da3475055a96c6f72615f6e616d65c0aa65787472615f6b65797391c0a967726f75705f69647800b26b765f63616368655f737065635f6b696e64ae66756c6c5f617474656e74696f6e8aa474797065ab426c6f636b53746f726564ac626c6f636b5f68617368657391c420202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3fb1706172656e745f626c6f636b5f68617368c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fa9746f6b656e5f6964739405060708aa626c6f636b5f73697a6504a76c6f72615f6964c0a66d656469756da3435055a96c6f72615f6e616d65c0a96f776e657273686970a3637075aa73657373696f6e5f6964a2733184a474797065ac426c6f636b52656d6f766564ac626c6f636b5f68617368657391c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fa66d656469756da3475055a967726f75705f6964780081a474797065b0416c6c426c6f636b73436c656172656400"

// Array-encoded events (vLLM < #42892) in a batch.
const (
	legacyArrayHybrid = "92cb41d954fc40000000919aab426c6f636b53746f72656491c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fc0940102030404c0a3475055c0c001"
	legacyArrayShort  = "92cb41d954fc400000009195ab426c6f636b53746f726564926566c0940102030404"
)

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex: %v", err)
	}
	return b
}

func requireSingleBlockStored(t *testing.T, data []byte) *BlockStoredEvent {
	t.Helper()
	batch, err := DecodeEventBatch(data, "m", "p")
	require.NoError(t, err)
	require.Len(t, batch.Events, 1)
	ev, ok := batch.Events[0].(*BlockStoredEvent)
	require.True(t, ok, "expected *BlockStoredEvent, got %T", batch.Events[0])
	return ev
}

// Current vLLM map encoding: every sample must decode and carry metadata.
func TestDecodeVLLMMapFormatBlockStored(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, vllmMapHybrid))
	assert.Equal(t, "m", ev.ModelName)
	assert.Equal(t, "p", ev.PodName)
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(1), *ev.GroupIdx)
	assert.Nil(t, ev.LoraName)
	assert.Nil(t, ev.LoraID)
}

func TestDecodeVLLMMapFormatExtraKeys(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, vllmMapExtraKeys))
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.LoraName)
	assert.Equal(t, "adapter-1", *ev.LoraName)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(2), *ev.GroupIdx)
	// extra_keys: one entry per block, ["img0", "emb0"] for the single block.
	require.Len(t, ev.ExtraKeys, 1)
	require.Len(t, ev.ExtraKeys[0], 2)
	assert.Equal(t, "img0", ev.ExtraKeys[0][0])
	assert.Equal(t, "emb0", ev.ExtraKeys[0][1])
}

func TestDecodeVLLMMapFormatMaximal(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, vllmMapMaximal))
	require.NotNil(t, ev.LoraID)
	assert.Equal(t, int64(1), *ev.LoraID)
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.LoraName)
	assert.Equal(t, "adapter-1", *ev.LoraName)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(5), *ev.GroupIdx)
	require.NotNil(t, ev.KVCacheSpecKind)
	assert.Equal(t, "hybrid", *ev.KVCacheSpecKind)
	require.NotNil(t, ev.KVCacheSpecSlidingWindow)
	assert.Equal(t, int64(128), *ev.KVCacheSpecSlidingWindow)
	require.NotNil(t, ev.Locality)
	assert.Equal(t, "LOCAL", *ev.Locality)
}

func TestDecodeVLLMMapFormatMinimal(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, vllmMapMinimal))
	assert.Nil(t, ev.LoraID)
	assert.Nil(t, ev.Medium)
	assert.Nil(t, ev.LoraName)
	assert.Nil(t, ev.GroupIdx)
	assert.Nil(t, ev.ExtraKeys)
	// tokens must still be parsed: 4 tokens, block_size 4 → 1 block
	require.Len(t, ev.TokenIDs, 1)
	assert.Equal(t, []byte{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4}, ev.TokenIDs[0])
}

func TestDecodeVLLMMapFormatBlockRemoved(t *testing.T) {
	batch, err := DecodeEventBatch(decodeHex(t, vllmMapRemoved), "m", "p")
	require.NoError(t, err)
	require.Len(t, batch.Events, 1)
	ev, ok := batch.Events[0].(*BlockRemovedEvent)
	require.True(t, ok)
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(1), *ev.GroupIdx)
}

func TestDecodeVLLMMapFormatAllBlocksCleared(t *testing.T) {
	batch, err := DecodeEventBatch(decodeHex(t, vllmMapCleared), "m", "p")
	require.NoError(t, err)
	require.Len(t, batch.Events, 1)
	_, ok := batch.Events[0].(*AllBlocksClearedEvent)
	require.True(t, ok)
}

// An array-encoded event (pre-#42892) outside a batch, "BlockStored" tag first.
func TestDecodeLegacySingleEventArray(t *testing.T) {
	// Same payload as legacyArrayHybrid but without the [ts, events] wrapper.
	ev := requireSingleBlockStored(t, decodeHex(t, "9aab426c6f636b53746f72656491c420000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1fc0940102030404c0a3475055c0c001"))
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(1), *ev.GroupIdx)
}

// A batch [ts, [event]] with an array-encoded event.
func TestDecodeLegacyBatchWrapper(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, legacyArrayHybrid))
	require.NotNil(t, ev.Medium)
	assert.Equal(t, "GPU", *ev.Medium)
	require.NotNil(t, ev.GroupIdx)
	assert.Equal(t, int64(1), *ev.GroupIdx)
}

func TestDecodeLegacyBatchWrapperShort(t *testing.T) {
	ev := requireSingleBlockStored(t, decodeHex(t, legacyArrayShort))
	assert.Equal(t, []int64{101, 102}, ev.BlockHashes)
	assert.Nil(t, ev.Medium)
	assert.Nil(t, ev.GroupIdx)
}

// TestDecodeVLLMEventBatch decodes a whole message in the shape vLLM sends:
// an array-encoded batch of map-encoded events with data_parallel_rank.
func TestDecodeVLLMEventBatch(t *testing.T) {
	batch, err := DecodeEventBatch(decodeHex(t, vllmBatch), "m", "p")
	require.NoError(t, err)
	assert.Equal(t, time.Unix(1760000000, 500_000_000).UTC(), batch.Timestamp)
	require.Len(t, batch.Events, 4)

	gpu, ok := batch.Events[0].(*BlockStoredEvent)
	require.True(t, ok, "event 0: got %T", batch.Events[0])
	require.Len(t, gpu.BlockHashes, 1)
	assert.Nil(t, gpu.ParentBlockHash)
	assert.Equal(t, [][]byte{{0, 0, 0, 1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0, 4}}, gpu.TokenIDs)
	require.NotNil(t, gpu.Medium)
	assert.Equal(t, "GPU", *gpu.Medium)
	require.NotNil(t, gpu.GroupIdx)
	assert.Equal(t, int64(0), *gpu.GroupIdx)
	require.NotNil(t, gpu.KVCacheSpecKind)
	assert.Equal(t, "full_attention", *gpu.KVCacheSpecKind)
	assert.Equal(t, [][]interface{}{nil}, gpu.ExtraKeys)
	assert.Equal(t, "m", gpu.ModelName)
	assert.Equal(t, "p", gpu.PodName)

	// Fields this decoder does not know (ownership, session_id) are ignored
	cpu, ok := batch.Events[1].(*BlockStoredEvent)
	require.True(t, ok, "event 1: got %T", batch.Events[1])
	require.NotNil(t, cpu.ParentBlockHash)
	assert.Equal(t, gpu.BlockHashes[0], *cpu.ParentBlockHash)
	require.NotNil(t, cpu.Medium)
	assert.Equal(t, "CPU", *cpu.Medium)
	assert.Nil(t, cpu.GroupIdx)

	removed, ok := batch.Events[2].(*BlockRemovedEvent)
	require.True(t, ok, "event 2: got %T", batch.Events[2])
	assert.Equal(t, gpu.BlockHashes, removed.BlockHashes)
	require.NotNil(t, removed.Medium)
	assert.Equal(t, "GPU", *removed.Medium)

	_, ok = batch.Events[3].(*AllBlocksClearedEvent)
	assert.True(t, ok, "event 3: got %T", batch.Events[3])
}
