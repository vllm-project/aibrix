// Adapted from vLLM
#include "cache.h"
#include <cuda_fp16.h>
#include <cuda_runtime.h>
#include <stdint.h>

#ifdef USE_ROCM
#include "quantization/fp8/amd/quant_utils.cuh"
#else
#include "quantization/fp8/nvidia/quant_utils.cuh"
#endif

/*
 * Layout enumeration for offload KV cache.
 * LCND: [num_blocks, num_layers, 2, block_size, dim]
 * NCLD: [num_blocks, block_size, 2, num_layers, dim]
 */
enum Layout { kLCND = 0, kNCLD = 1 };

/*
 * Calculate offset into KV cache storage.
 * Supports two layouts:
 * - kv_layout_blocks_first=true:  [num_blocks, 2, block_size, num_heads,
 * head_size]
 * - kv_layout_blocks_first=false: [2, num_blocks, block_size, num_heads,
 * head_size]
 *
 * @param kv_type 0 for key, 1 for value
 * @param kv_layout_blocks_first Whether blocks dimension comes first
 * @param num_blocks Total number of blocks in kv cache
 * @param block_size Size of each block
 * @param embed_dim Embedding dimension (num_heads * head_size)
 * @param slot_idx Global slot index (block_idx * block_size + block_offset)
 * @param scalar_offset Offset within the embedding dimension
 */
__device__ __forceinline__ int64_t
get_kv_cache_offset(int64_t kv_type, bool kv_layout_blocks_first,
                    int64_t num_blocks, int64_t block_size, int64_t embed_dim,
                    int64_t slot_idx, int64_t scalar_offset) {
  int64_t block_idx = slot_idx / block_size;
  int64_t block_offset = slot_idx % block_size;
  if (kv_layout_blocks_first) {
    // [num_blocks, kv_type, block_size, embed_dim]
    return block_idx * 2 * block_size * embed_dim +
           kv_type * block_size * embed_dim + block_offset * embed_dim +
           scalar_offset;
  } else {
    // [kv_type, num_blocks, block_size, embed_dim]
    return kv_type * num_blocks * block_size * embed_dim +
           block_idx * block_size * embed_dim + block_offset * embed_dim +
           scalar_offset;
  }
}

/*
 * Calculate offset into offload KV cache with LCND layout.
 * Layout: [num_blocks, num_layers, 2, block_size, dim]
 */
__device__ __forceinline__ int64_t get_offload_offset_lcnd(
    int64_t kv_type, int64_t layer_idx, int64_t block_size, int64_t num_layers,
    int64_t embed_dim, int64_t token_idx, int64_t i) {
  return layer_idx * 2 * block_size * embed_dim +
         kv_type * block_size * embed_dim +
         (token_idx % block_size) * embed_dim + i;
}

/*
 * Calculate offset into offload KV cache with NCLD layout.
 * Layout: [num_blocks, block_size, 2, num_layers, dim]
 */
__device__ __forceinline__ int64_t get_offload_offset_ncld(
    int64_t kv_type, int64_t layer_idx, int64_t block_size, int64_t num_layers,
    int64_t embed_dim, int64_t token_idx, int64_t i) {
  return (token_idx % block_size) * 2 * num_layers * embed_dim +
         kv_type * num_layers * embed_dim + layer_idx * embed_dim + i;
}

/*
 * Type trait to detect FP8 types at compile time.
 */
template <typename T> struct IsFP8 {
  static const bool value = false;
};
template <> struct IsFP8<uint8_t> {
  static const bool value = true;
};

/*
 * CUDA kernel for cache operations.
 * Handles data movement between offload KV cache and engine KV cache.
 *
 * Template arguments:
 *   T: Data type of offload kv cache (uint16_t for FP16/BF16, float for FP32)
 *   CacheT: Data type of engine kv cache (uint16_t, uint8_t for FP8, float)
 *   kIsOnload: true for offload->cache, false for cache->offload
 *   kLayout: Layout of offload cache (LCND or NCLD)
 *
 * Args:
 *   offload_ptrs: Array of device pointers to offload cache blocks [num_blocks]
 *   offload_block_size: Number of tokens per offload block
 *   cache_ptrs: Array of device pointers to engine cache layers [num_layers]
 *   kv_layout_blocks_first: engine cache layout flag
 *   cache_num_blocks: Number of blocks in engine cache
 *   cache_block_size: Block size in engine cache
 *   slot_mapping: Device array mapping tokens to cache slots [num_tokens]
 *   num_layers: Number of model layers
 *   embed_dim: Embedding dimension (num_heads * head_size)
 *   k_scales: Per-layer key quantization scales [num_layers]
 *   v_scales: Per-layer value quantization scales [num_layers]
 */
template <typename T, typename CacheT, bool kIsOnload, Layout kLayout>
__global__ void cache_kernel(T **offload_ptrs, int64_t offload_block_size,
                             CacheT **cache_ptrs, bool kv_layout_blocks_first,
                             int64_t cache_num_blocks, int64_t cache_block_size,
                             const int64_t *slot_mapping, int64_t num_layers,
                             int64_t embed_dim, const float **k_scales,
                             const float **v_scales) {

  int64_t token_idx = blockIdx.x;
  int64_t layer_idx = blockIdx.y;
  int64_t kv_type = blockIdx.z; // 0 = key, 1 = value
  int64_t tid = threadIdx.x;

  int64_t slot_idx = slot_mapping[token_idx];
  if (slot_idx < 0)
    return; // Negative slot means padding/invalid

  int64_t offload_block_idx = token_idx / offload_block_size;
  T *offload_block = offload_ptrs[offload_block_idx];
  CacheT *cache_layer = cache_ptrs[layer_idx];
  const float *scale =
      (kv_type == 0) ? k_scales[layer_idx] : v_scales[layer_idx];

  // Copy data between offload cache and engine cache
  for (int64_t i = tid; i < embed_dim; i += blockDim.x) {
    int64_t offload_offset;
    if constexpr (kLayout == kLCND) {
      offload_offset =
          get_offload_offset_lcnd(kv_type, layer_idx, offload_block_size,
                                  num_layers, embed_dim, token_idx, i);
    } else {
      offload_offset =
          get_offload_offset_ncld(kv_type, layer_idx, offload_block_size,
                                  num_layers, embed_dim, token_idx, i);
    }

    int64_t cache_offset =
        get_kv_cache_offset(kv_type, kv_layout_blocks_first, cache_num_blocks,
                            cache_block_size, embed_dim, slot_idx, i);

    if constexpr (kIsOnload) {
      if constexpr (IsFP8<CacheT>::value) {
        cache_layer[cache_offset] =
            vllm::fp8::scaled_convert<CacheT, T,
                                      vllm::Fp8KVCacheDataType::kAuto>(
                offload_block[offload_offset], *scale);
      } else {
        cache_layer[cache_offset] =
            static_cast<CacheT>(offload_block[offload_offset]);
      }
    } else /* offload */ {
      if constexpr (IsFP8<CacheT>::value) {
        offload_block[offload_offset] =
            vllm::fp8::scaled_convert<T, CacheT,
                                      vllm::Fp8KVCacheDataType::kAuto>(
                cache_layer[cache_offset], *scale);
      } else {
        offload_block[offload_offset] =
            static_cast<T>(cache_layer[cache_offset]);
      }
    }
  }
}

/*
 * Dispatch kernel by data type.
 * Maps dtype enums to actual C++ types and launches appropriate kernel.
 *
 * Dtype enum mapping:
 *   0 = FP16 (uint16_t)
 *   1 = BF16 (uint16_t)
 *   2 = FP32 (float)
 *   3 = FP8  (uint8_t)
 */
template <bool kIsOnload, Layout kLayout>
void dispatch_dtype(int offload_dtype, int cache_dtype, void **offload_ptrs,
                    int64_t offload_block_size, void **cache_ptrs,
                    bool kv_layout_blocks_first, int64_t cache_num_blocks,
                    int64_t cache_block_size, const int64_t *slot_mapping,
                    int64_t num_tokens, int64_t num_layers, int64_t embed_dim,
                    const float **k_scales, const float **v_scales,
                    cudaStream_t stream) {

  dim3 grid(num_tokens, num_layers, 2); // 2 for key/value
  // Round up to nearest warp size (32) for better occupancy
  int block_size = ((embed_dim + 31) / 32) * 32;
  if (block_size > 512)
    block_size = 512;
  if (block_size < 32)
    block_size = 32;
  dim3 block(block_size);

#define CALL_KERNEL(T, CacheT)                                                 \
  cache_kernel<T, CacheT, kIsOnload, kLayout><<<grid, block, 0, stream>>>(     \
      reinterpret_cast<T **>(offload_ptrs), offload_block_size,                \
      reinterpret_cast<CacheT **>(cache_ptrs), kv_layout_blocks_first,         \
      cache_num_blocks, cache_block_size, slot_mapping, num_layers, embed_dim, \
      k_scales, v_scales)

  if (cache_dtype == 3) {                           // FP8 cache
    if (offload_dtype == 0 || offload_dtype == 1) { // FP16/BF16 offload
      CALL_KERNEL(uint16_t, uint8_t);
    } else if (offload_dtype == 2) { // FP32 offload
      CALL_KERNEL(float, uint8_t);
    } else { // FP8 offload
      CALL_KERNEL(uint8_t, uint8_t);
    }
  } else if (cache_dtype == 2) {                    // FP32 cache
    if (offload_dtype == 0 || offload_dtype == 1) { // FP16/BF16 offload
      CALL_KERNEL(uint16_t, float);
    } else if (offload_dtype == 2) { // FP32 offload
      CALL_KERNEL(float, float);
    } else { // FP8 offload
      CALL_KERNEL(uint8_t, float);
    }
  } else {                                          // FP16/BF16 cache
    if (offload_dtype == 0 || offload_dtype == 1) { // FP16/BF16 offload
      CALL_KERNEL(uint16_t, uint16_t);
    } else if (offload_dtype == 2) { // FP32 offload
      CALL_KERNEL(float, uint16_t);
    } else { // FP8 offload
      CALL_KERNEL(uint8_t, uint16_t);
    }
  }
#undef CALL_KERNEL
}

void dispatch_all(bool onload, int layout, int offload_dtype, int cache_dtype,
                  void **offload_ptrs, int64_t offload_block_size,
                  void **cache_ptrs, bool kv_layout_blocks_first,
                  int64_t cache_num_blocks, int64_t cache_block_size,
                  const int64_t *slot_mapping, int64_t num_tokens,
                  int64_t num_layers, int64_t embed_dim, const float **k_scales,
                  const float **v_scales, cudaStream_t stream) {

  if (layout == kLCND) {
    if (onload) {
      dispatch_dtype<true, kLCND>(
          offload_dtype, cache_dtype, offload_ptrs, offload_block_size,
          cache_ptrs, kv_layout_blocks_first, cache_num_blocks,
          cache_block_size, slot_mapping, num_tokens, num_layers, embed_dim,
          k_scales, v_scales, stream);
    } else {
      dispatch_dtype<false, kLCND>(
          offload_dtype, cache_dtype, offload_ptrs, offload_block_size,
          cache_ptrs, kv_layout_blocks_first, cache_num_blocks,
          cache_block_size, slot_mapping, num_tokens, num_layers, embed_dim,
          k_scales, v_scales, stream);
    }
  } else { // kNCLD
    if (onload) {
      dispatch_dtype<true, kNCLD>(
          offload_dtype, cache_dtype, offload_ptrs, offload_block_size,
          cache_ptrs, kv_layout_blocks_first, cache_num_blocks,
          cache_block_size, slot_mapping, num_tokens, num_layers, embed_dim,
          k_scales, v_scales, stream);
    } else {
      dispatch_dtype<false, kNCLD>(
          offload_dtype, cache_dtype, offload_ptrs, offload_block_size,
          cache_ptrs, kv_layout_blocks_first, cache_num_blocks,
          cache_block_size, slot_mapping, num_tokens, num_layers, embed_dim,
          k_scales, v_scales, stream);
    }
  }
}

static thread_local char last_error[256] = {0};

static void set_error(const char *msg) {
  strncpy(last_error, msg, sizeof(last_error) - 1);
  last_error[sizeof(last_error) - 1] = '\0';
}

/*
 * CUDA error checking macro.
 * Captures CUDA errors and stores them in the thread-local error buffer.
 */
#define CHECK_CUDA(call)                                                       \
  do {                                                                         \
    cudaError_t err = call;                                                    \
    if (err != cudaSuccess) {                                                  \
      set_error(cudaGetErrorString(err));                                      \
      return 1;                                                                \
    }                                                                          \
  } while (0)

extern "C" {

/*
 * Get the last error message from a failed operation.
 */
const char *aibrix_get_last_error() { return last_error; }

/*
 * Validate kernel arguments.
 * Returns 0 on success, 1 on failure with error message set.
 */
static int validate_args(KernelArgs *args) {
  if (args == nullptr) {
    set_error("args is null");
    return 1;
  }
  if (args->offload_ptrs == nullptr) {
    set_error("offload_ptrs is null");
    return 1;
  }
  if (args->cache_ptrs == nullptr) {
    set_error("cache_ptrs is null");
    return 1;
  }
  if (args->slot_mapping == nullptr) {
    set_error("slot_mapping is null");
    return 1;
  }
  if (args->k_scales == nullptr) {
    set_error("k_scales is null");
    return 1;
  }
  if (args->v_scales == nullptr) {
    set_error("v_scales is null");
    return 1;
  }
  if (args->num_tokens <= 0) {
    set_error("num_tokens must be > 0");
    return 1;
  }
  if (args->cache_num_layers <= 0) {
    set_error("cache_num_layers must be > 0");
    return 1;
  }
  if (args->embed_dim <= 0) {
    set_error("embed_dim must be > 0");
    return 1;
  }
  if (args->layout != 0 && args->layout != 1) {
    set_error("layout must be 0 (LCND) or 1 (NCLD)");
    return 1;
  }
  if (args->offload_dtype < 0 || args->offload_dtype > 3) {
    set_error("offload_dtype must be 0-3");
    return 1;
  }
  if (args->cache_dtype < 0 || args->cache_dtype > 3) {
    set_error("cache_dtype must be 0-3");
    return 1;
  }
  // Check token count does not exceed offload cache capacity
  int64_t offload_capacity =
      args->offload_num_blocks * args->offload_block_size;
  if (args->num_tokens > offload_capacity) {
    set_error("num_tokens exceeds offload cache capacity");
    return 1;
  }
  // Check layer count matches offload layer count (if provided)
  if (args->offload_num_layers > 0 &&
      args->cache_num_layers != args->offload_num_layers) {
    set_error("num_layers does not match offload_num_layers");
    return 1;
  }
  return 0;
}

int aibrix_reshape_and_cache_multi_layer(KernelArgs *args) {
  if (validate_args(args) != 0) {
    return 1;
  }
  dispatch_all(true, args->layout, args->offload_dtype, args->cache_dtype,
               args->offload_ptrs, args->offload_block_size, args->cache_ptrs,
               args->kv_layout_blocks_first != 0, args->cache_num_blocks,
               args->cache_block_size, args->slot_mapping, args->num_tokens,
               args->cache_num_layers, args->embed_dim, args->k_scales,
               args->v_scales, static_cast<cudaStream_t>(args->stream));
  CHECK_CUDA(cudaGetLastError());
  return 0;
}

int aibrix_reshape_and_offload_multi_layer(KernelArgs *args) {
  if (validate_args(args) != 0) {
    return 1;
  }
  dispatch_all(false, args->layout, args->offload_dtype, args->cache_dtype,
               args->offload_ptrs, args->offload_block_size, args->cache_ptrs,
               args->kv_layout_blocks_first != 0, args->cache_num_blocks,
               args->cache_block_size, args->slot_mapping, args->num_tokens,
               args->cache_num_layers, args->embed_dim, args->k_scales,
               args->v_scales, static_cast<cudaStream_t>(args->stream));
  CHECK_CUDA(cudaGetLastError());
  return 0;
}
} // extern "C"

#undef CHECK_CUDA
