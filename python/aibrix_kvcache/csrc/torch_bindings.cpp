// Adapted from vLLM
#include "cache.h"
#include "core/registration.h"

#include <torch/library.h>
#include <torch/version.h>

// Note on op signatures:
// The X_meta signatures are for the meta functions corresponding to op X.
// They must be kept in sync with the signature for X. Generally, only
// functions that return Tensors require a meta function.
//
// See the following links for detailed docs on op registration and function
// schemas.
// https://docs.google.com/document/d/1_W62p8WJOQQUzPsJYa7s701JXt0qf2OfLub2sbkHOaU/edit#heading=h.ptttacy8y1u9
// https://github.com/pytorch/pytorch/blob/main/aten/src/ATen/native/README.md#annotations

TORCH_LIBRARY_EXPAND(CONCAT(TORCH_EXTENSION_NAME, _cache_ops), cache_ops) {
  // Cache ops
  cache_ops.def(
      "reshape_and_cache_multi_layer(Tensor[] offload_kv_cache_blocks,"
      "                              Tensor(b!)[] key_caches,"
      "                              Tensor slot_mapping,"
      "                              SymInt block_size,"
      "                              str kv_cache_dtype,"
      "                              Tensor[] k_scales, Tensor[] v_scales,"
      "                              str layout) -> ()");
  cache_ops.impl("reshape_and_cache_multi_layer", torch::kCUDA,
                 &reshape_and_cache_multi_layer);

  cache_ops.def(
      "reshape_and_offload_multi_layer(Tensor(a!)[] offload_kv_cache_blocks,"
      "                                Tensor[] key_caches,"
      "                                Tensor slot_mapping,"
      "                                SymInt block_size,"
      "                                str kv_cache_dtype,"
      "                                Tensor[] k_scales, Tensor[] v_scales,"
      "                                str layout) -> ()");
  cache_ops.impl("reshape_and_offload_multi_layer", torch::kCUDA,
                 &reshape_and_offload_multi_layer);
}

REGISTER_EXTENSION(TORCH_EXTENSION_NAME)
