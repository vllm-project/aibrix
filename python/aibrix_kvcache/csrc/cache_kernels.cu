// Adapted from vLLM
#include <ATen/cuda/CUDAContext.h>
#include <c10/cuda/CUDAGuard.h>
#include <torch/all.h>

#ifdef USE_ROCM
#include "quantization/fp8/amd/quant_utils.cuh"
#else
#include "quantization/fp8/nvidia/quant_utils.cuh"
#endif

#include <algorithm>
#include <cassert>
#include <map>
#include <vector>

namespace aibrix {

template <typename TPtr, typename TTensor>
TPtr *get_device_ptr(TTensor &tensor) {
  torch::Device device = tensor.device();
  bool is_pinned = tensor.is_pinned();

  if (device.is_cuda()) {
    return reinterpret_cast<TPtr *>(tensor.data_ptr());
  } else if (is_pinned) {
    void *ptr;
    cudaHostGetDevicePointer(&ptr, static_cast<void *>(tensor.data_ptr()), 0);
    return static_cast<TPtr *>(ptr);
  }

  TORCH_CHECK(false, "Tensor must be on GPU or be pinned");
  return nullptr;
}

template <typename TTensor>
torch::Tensor get_device_ptrs(const std::vector<TTensor> &tensors) {
  // Create a vector to store the GPU memory pointers
  std::vector<void *> data_ptrs;
  data_ptrs.reserve(tensors.size());

  // Extract data pointers
  for (const auto &tensor : tensors) {
    data_ptrs.push_back(get_device_ptr<void *>(tensor));
  }

  torch::Tensor gpu_data_ptrs =
      torch::from_blob(data_ptrs.data(),
                       {static_cast<int64_t>(data_ptrs.size())}, torch::kInt64)
          .to(torch::kCUDA);

  return gpu_data_ptrs;
}

__device__ __forceinline__ int64_t get_kv_cache_offset(
    const int64_t kv_type, bool kv_layout_blocks_first,
    const int64_t num_blocks, const int64_t block_size, const int64_t embed_dim,
    const int64_t slot_idx, const int64_t scalar_offset) {
  const int64_t block_idx = slot_idx / block_size;
  const int64_t block_offset = slot_idx % block_size;
  if (kv_layout_blocks_first) {
    // [num_blocks, kv_type, block_size, num_heads, head_size]
    return block_idx * 2 * block_size * embed_dim +
           kv_type * block_size * embed_dim + block_offset * embed_dim +
           scalar_offset;
  } else {
    // [kv_type, num_blocks, block_size, num_heads, head_size]
    return kv_type * num_blocks * block_size * embed_dim +
           block_idx * block_size * embed_dim + block_offset * embed_dim +
           scalar_offset;
  }
}

__device__ __forceinline__ int64_t get_offload_kv_cache_offset_lcnd(
    const int64_t kv_type, const int64_t layer_idx, const int64_t block_size,
    const int64_t num_layers, const int64_t embed_dim, const int64_t token_idx,
    const int64_t scalar_offset) {
  const int64_t block_offset = token_idx % block_size;
  return layer_idx * 2 * block_size * embed_dim +
         kv_type * block_size * embed_dim + block_offset * embed_dim +
         scalar_offset;
}

__device__ __forceinline__ int64_t get_offload_kv_cache_offset_ncld(
    const int64_t kv_type, const int64_t layer_idx, const int64_t block_size,
    const int64_t num_layers, const int64_t embed_dim, const int64_t token_idx,
    const int64_t scalar_offset) {
  const int64_t block_offset = token_idx % block_size;
  return block_offset * 2 * num_layers * embed_dim +
         kv_type * num_layers * embed_dim + layer_idx * embed_dim +
         scalar_offset;
}

enum class KVCacheOffloadLayout {
  kLCND = 1,
  kNCLD,
};

/*
 * Template arguments:
 * - TOnload: True if offload_kv_cache to kv_cache.
 * - TLayout: The layout of offload_kv_cache.
 * Args:
 * - offload_kv_cache: Supports LCND and NCLD layouts.
 *                     LCND: [num_blocks, num_layers, 2, block_size, dim]
 *                     NCLD: [num_blocks, block_size, 2, num_layers, dim]
 * - kv_cache: Supports [num_layers, 2, num_blocks, block_size, num_heads,
 * head_size], [num_layers, 2, num_blocks, block_size * num_heads *
 * head_size], [num_layers, num_blocks, 2, block_size, num_heads, head_size],
 * and [num_layers, num_blocks, 2, block_size * num_heads * head_size]
 * - slot_mapping: [num_tokens]
 */
template <typename scalar_t, typename cache_t, vllm::Fp8KVCacheDataType kv_dt,
          bool TOnload, KVCacheOffloadLayout TLayout>
__global__ void reshape_and_cache_multi_layer_kernel(
    scalar_t **__restrict__ offload_kv_cache,
    const int64_t offload_kv_cache_block_size, cache_t **__restrict__ kv_cache,
    bool kv_layout_blocks_first, const int64_t kv_cache_block_size,
    const int64_t kv_cache_num_blocks, const int64_t *__restrict__ slot_mapping,
    const int64_t num_layers, const int64_t embed_dim,
    const float **k_scales, // Scaling factor for keys
    const float **v_scales  // Scaling factor for values
) {
  const int64_t token_idx = blockIdx.x;
  const int64_t layer_idx = blockIdx.y;
  const int64_t kv_type = blockIdx.z;
  const int64_t tid = threadIdx.x;
  const int64_t num_threads = blockDim.x;

  const int64_t slot_idx = slot_mapping[token_idx];
  if (slot_idx < 0)
    return;

  const int64_t offload_kv_cache_block_idx =
      token_idx / offload_kv_cache_block_size;

  scalar_t *offload_kv_cache_block =
      offload_kv_cache[offload_kv_cache_block_idx];
  cache_t *kv_cache_layer = kv_cache[layer_idx];
  const float *k_scale = k_scales[layer_idx];
  const float *v_scale = v_scales[layer_idx];

  // Copy data between kv_cache and offload_kv_cache
  for (int i = tid; i < embed_dim; i += num_threads) {
    int64_t offload_kv_cache_offset = 0;
    if constexpr (TLayout == KVCacheOffloadLayout::kLCND) {
      offload_kv_cache_offset = get_offload_kv_cache_offset_lcnd(
          kv_type, layer_idx, offload_kv_cache_block_size, num_layers,
          embed_dim, token_idx, i);
    } else {
      offload_kv_cache_offset = get_offload_kv_cache_offset_ncld(
          kv_type, layer_idx, offload_kv_cache_block_size, num_layers,
          embed_dim, token_idx, i);
    }

    int64_t kv_cache_offset = get_kv_cache_offset(
        kv_type, kv_layout_blocks_first, kv_cache_num_blocks,
        kv_cache_block_size, embed_dim, slot_idx, i);

    if (TOnload) { // true: offload_kv_cache to kv_cache
      kv_cache_layer[kv_cache_offset] =
          vllm::fp8::scaled_convert<cache_t, scalar_t, kv_dt>(
              offload_kv_cache_block[offload_kv_cache_offset],
              (kv_type == 0) ? *k_scale : *v_scale);
    } else { // false: kv_cache to offload_kv_cache
      offload_kv_cache_block[offload_kv_cache_offset] =
          vllm::fp8::scaled_convert<scalar_t, cache_t, kv_dt>(
              kv_cache_layer[kv_cache_offset],
              (kv_type == 0) ? *k_scale : *v_scale);
    }
  }
}

/*
 * Template arguments:
 * - TOnload: True if offload_kv_cache to kv_cache.
 * - TLayout: The layout of offload_kv_cache.
 * Args:
 * - offload_kv_cache: Supports LCND and NCLD layouts.
 *                     LCND: [num_blocks, num_layers, 2, block_size, dim]
 *                     NCLD: [num_blocks, block_size, 2, num_layers, dim]
 * - kv_cache: Supports [num_layers, 2, num_blocks, block_size, num_heads,
 * head_size], [num_layers, 2, num_blocks, block_size * num_heads *
 * head_size], [num_layers, num_blocks, 2, block_size, num_heads, head_size],
 * and [num_layers, num_blocks, 2, block_size * num_heads * head_size]
 * - slot_mapping: [num_tokens]
 */
template <typename vec_t, bool TOnload, KVCacheOffloadLayout TLayout>
__global__ void reshape_and_cache_multi_layer_vec_kernel(
    vec_t **__restrict__ offload_kv_cache,
    const int64_t offload_kv_cache_block_size, vec_t **__restrict__ kv_cache,
    bool kv_layout_blocks_first, const int64_t kv_cache_block_size,
    const int64_t kv_cache_num_blocks, const int64_t *__restrict__ slot_mapping,
    const int64_t num_layers, const int64_t num_vecs) {
  const int64_t token_idx = blockIdx.x;
  const int64_t layer_idx = blockIdx.y;
  const int64_t kv_type = blockIdx.z;
  const int64_t tid = threadIdx.x;
  const int64_t num_threads = blockDim.x;

  const int64_t slot_idx = slot_mapping[token_idx];
  if (slot_idx < 0)
    return;

  const int64_t offload_kv_cache_block_idx =
      token_idx / offload_kv_cache_block_size;

  vec_t *offload_kv_cache_block = offload_kv_cache[offload_kv_cache_block_idx];
  vec_t *kv_cache_layer = kv_cache[layer_idx];

  // Copy data between kv_cache and offload_kv_cache
  for (int i = tid; i < num_vecs; i += num_threads) {
    int64_t offload_kv_cache_offset = 0;
    if constexpr (TLayout == KVCacheOffloadLayout::kLCND) {
      offload_kv_cache_offset = get_offload_kv_cache_offset_lcnd(
          kv_type, layer_idx, offload_kv_cache_block_size, num_layers, num_vecs,
          token_idx, i);
    } else {
      offload_kv_cache_offset = get_offload_kv_cache_offset_ncld(
          kv_type, layer_idx, offload_kv_cache_block_size, num_layers, num_vecs,
          token_idx, i);
    }

    int64_t kv_cache_offset = get_kv_cache_offset(
        kv_type, kv_layout_blocks_first, kv_cache_num_blocks,
        kv_cache_block_size, num_vecs, slot_idx, i);

    if (TOnload) { // true: offload_kv_cache to kv_cache
      kv_cache_layer[kv_cache_offset] =
          offload_kv_cache_block[offload_kv_cache_offset];
    } else { // false: kv_cache to offload_kv_cache
      offload_kv_cache_block[offload_kv_cache_offset] =
          kv_cache_layer[kv_cache_offset];
    }
  }
}

// KV_T is the data type of offload kv cache.
// CACHE_T is the stored data type of kv cache.
// KV_DTYPE is the real data type of kv cache.
// onload_const: true if offload_kv_cache to kv_cache
// layout_const: LCND, NCLD
#define CALL_RESHAPE_AND_CACHE_MULTI_LAYER(KV_T, CACHE_T, KV_DTYPE)            \
  reshape_and_cache_multi_layer_kernel<KV_T, CACHE_T, KV_DTYPE, onload_const,  \
                                       layout_const>                           \
      <<<grid, block, 0, stream>>>(                                            \
          reinterpret_cast<KV_T **>(offload_kv_cache_ptrs.data_ptr()),         \
          offload_kv_cache_block_size,                                         \
          reinterpret_cast<CACHE_T **>(kv_cache_ptrs.data_ptr()),              \
          kv_layout_blocks_first, block_size, kv_cache_num_blocks,             \
          slot_mapping.data_ptr<int64_t>(), num_layers, embed_dim,             \
          reinterpret_cast<const float **>(k_scale_ptrs.data_ptr()),           \
          reinterpret_cast<const float **>(v_scale_ptrs.data_ptr()));

#define CALL_RESHAPE_AND_CACHE_MULTI_LAYER_VEC                                 \
  reshape_and_cache_multi_layer_vec_kernel<vec_t, onload_const, layout_const>  \
      <<<grid, block, 0, stream>>>(                                            \
          reinterpret_cast<vec_t **>(offload_kv_cache_ptrs.data_ptr()),        \
          offload_kv_cache_block_size,                                         \
          reinterpret_cast<vec_t **>(kv_cache_ptrs.data_ptr()),                \
          kv_layout_blocks_first, block_size, kv_cache_num_blocks,             \
          slot_mapping.data_ptr<int64_t>(), num_layers, num_vecs);

#define DISPATCH_RESHAPE_AND_CACHE_MULTI_LAYER_BY_KV_CACHE_DTYPE               \
  DISPATCH_BY_KV_CACHE_DTYPE(kv_caches[0].dtype(), kv_cache_dtype,             \
                             CALL_RESHAPE_AND_CACHE_MULTI_LAYER);

#define DISPATCH_RESHAPE_AND_CACHE_MULTI_LAYER_BY_ONLOAD_AND_LAYOUT(           \
    ONLOAD_T, LAYOUT_T, FN)                                                    \
  if (LAYOUT_T == aibrix::KVCacheOffloadLayout::kLCND) {                       \
    const auto layout_const = aibrix::KVCacheOffloadLayout::kLCND;             \
    if (ONLOAD_T) {                                                            \
      const auto onload_const = true;                                          \
      FN;                                                                      \
    } else {                                                                   \
      const auto onload_const = false;                                         \
      FN;                                                                      \
    }                                                                          \
  } else {                                                                     \
    const auto layout_const = aibrix::KVCacheOffloadLayout::kNCLD;             \
    if (ONLOAD_T) {                                                            \
      const auto onload_const = true;                                          \
      FN;                                                                      \
    } else {                                                                   \
      const auto onload_const = false;                                         \
      FN;                                                                      \
    }                                                                          \
  }

void reshape_and_cache_multi_layer_impl(
    const std::vector<torch::Tensor> &offload_kv_cache_blocks, // [num_blocks]
    const std::vector<torch::Tensor> &kv_caches,               // [num_layers]
    bool kv_layout_blocks_first, torch::Tensor &slot_mapping,  // [num_tokens]
    const int64_t block_size, const std::string &kv_cache_dtype,
    const std::vector<torch::Tensor> &k_scales,
    const std::vector<torch::Tensor> &v_scales, bool onload,
    const std::string &layout_str) {
  const auto layout = (layout_str == "LCND")
                          ? aibrix::KVCacheOffloadLayout::kLCND
                          : aibrix::KVCacheOffloadLayout::kNCLD;
  const int64_t num_tokens = slot_mapping.size(0);
  torch::IntArrayRef kv_cache_shape = kv_caches[0].sizes();
  int64_t embed_dim;

  if (kv_cache_shape.size() == 3) {
    // [2, num_blocks, block_size * num_heads * head_size]
    // or [num_blocks, 2, block_size * num_heads * head_size]
    const int64_t block_dim = kv_caches[0].stride(1);
    embed_dim = block_dim / block_size;
  } else {
    // [2, num_blocks, block_size, num_heads, head_size]
    // or [num_blocks, 2, block_size, num_heads, head_size]
    embed_dim = kv_caches[0].stride(2);
  }

  torch::IntArrayRef offload_kv_cache_block_shape =
      offload_kv_cache_blocks[0].sizes();
  const int64_t offload_kv_cache_block_size =
      (layout == aibrix::KVCacheOffloadLayout::kLCND)
          ? offload_kv_cache_block_shape[2]
          : offload_kv_cache_block_shape[0];
  const int64_t offload_kv_cache_num_layers =
      (layout == aibrix::KVCacheOffloadLayout::kLCND)
          ? offload_kv_cache_block_shape[0]
          : offload_kv_cache_block_shape[2];

  // offload_kv_cache_blocks may have padding tokens.
  TORCH_CHECK(num_tokens <=
              offload_kv_cache_blocks.size() * offload_kv_cache_block_size);

  const int64_t num_layers = kv_caches.size();
  TORCH_CHECK(num_layers == offload_kv_cache_num_layers);

  // Assume all layers have the same shape
  for (int64_t i = 0; i < num_layers; i++) {
    TORCH_CHECK(kv_cache_shape == kv_caches[i].sizes());
  }

  const int64_t kv_cache_num_blocks =
      kv_layout_blocks_first ? kv_cache_shape[0] : kv_cache_shape[1];

  torch::Tensor offload_kv_cache_ptrs =
      aibrix::get_device_ptrs(offload_kv_cache_blocks);
  torch::Tensor kv_cache_ptrs = aibrix::get_device_ptrs(kv_caches);
  torch::Tensor k_scale_ptrs = aibrix::get_device_ptrs(k_scales);
  torch::Tensor v_scale_ptrs = aibrix::get_device_ptrs(v_scales);

  const at::cuda::OptionalCUDAGuard device_guard(device_of(kv_caches[0]));
  const cudaStream_t stream = at::cuda::getCurrentCUDAStream();

  dim3 grid(num_tokens, num_layers, 2);
  if (kv_cache_dtype == "auto") {
    auto element_size = kv_caches[0].element_size();
    using vec_t = __int128_t;
    const int64_t num_vecs = embed_dim / sizeof(vec_t) * element_size;
    dim3 block(std::min(num_vecs, static_cast<int64_t>(128)));
    DISPATCH_RESHAPE_AND_CACHE_MULTI_LAYER_BY_ONLOAD_AND_LAYOUT(
        onload, layout, CALL_RESHAPE_AND_CACHE_MULTI_LAYER_VEC);
  } else {
    dim3 block(std::min(embed_dim, static_cast<int64_t>(512)));
    DISPATCH_RESHAPE_AND_CACHE_MULTI_LAYER_BY_ONLOAD_AND_LAYOUT(
        onload, layout,
        DISPATCH_RESHAPE_AND_CACHE_MULTI_LAYER_BY_KV_CACHE_DTYPE);
  }
}
} // namespace aibrix

void reshape_and_cache_multi_layer(
    const std::vector<torch::Tensor> &offload_kv_cache_blocks, // [num_blocks]
    const std::vector<torch::Tensor> &kv_caches,               // [num_layers]
    bool kv_layout_blocks_first, torch::Tensor &slot_mapping,  // [num_tokens]
    const int64_t block_size, const std::string &kv_cache_dtype,
    const std::vector<torch::Tensor> &k_scales,
    const std::vector<torch::Tensor> &v_scales, const std::string &layout_str) {
  aibrix::reshape_and_cache_multi_layer_impl(
      offload_kv_cache_blocks, kv_caches, kv_layout_blocks_first, slot_mapping,
      block_size, kv_cache_dtype, k_scales, v_scales, true, layout_str);
}

void reshape_and_offload_multi_layer(
    const std::vector<torch::Tensor> &offload_kv_cache_blocks, // [num_blocks]
    const std::vector<torch::Tensor> &kv_caches,               // [num_layers]
    bool kv_layout_blocks_first, torch::Tensor &slot_mapping,  // [num_tokens]
    const int64_t block_size, const std::string &kv_cache_dtype,
    const std::vector<torch::Tensor> &k_scales,
    const std::vector<torch::Tensor> &v_scales, const std::string &layout_str) {
  aibrix::reshape_and_cache_multi_layer_impl(
      offload_kv_cache_blocks, kv_caches, kv_layout_blocks_first, slot_mapping,
      block_size, kv_cache_dtype, k_scales, v_scales, false, layout_str);
}
