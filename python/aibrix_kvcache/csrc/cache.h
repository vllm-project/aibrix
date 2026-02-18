#pragma once

#include <torch/all.h>

#include <map>
#include <vector>

void reshape_and_cache_multi_layer(
    const std::vector<torch::Tensor> &offload_kv_cache_blocks,
    const std::vector<torch::Tensor> &kv_caches, bool kv_layout_blocks_first,
    torch::Tensor &slot_mapping, const int64_t block_size,
    const std::string &kv_cache_dtype,
    const std::vector<torch::Tensor> &k_scales,
    const std::vector<torch::Tensor> &v_scales, const std::string &layout_str);

void reshape_and_offload_multi_layer(
    const std::vector<torch::Tensor> &offload_kv_cache_blocks,
    const std::vector<torch::Tensor> &kv_caches, bool kv_layout_blocks_first,
    torch::Tensor &slot_mapping, const int64_t block_size,
    const std::string &kv_cache_dtype,
    const std::vector<torch::Tensor> &k_scales,
    const std::vector<torch::Tensor> &v_scales, const std::string &layout_str);
