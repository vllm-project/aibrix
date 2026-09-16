#pragma once

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
  void **offload_ptrs;
  int offload_dtype;
  int64_t offload_num_blocks;
  int64_t offload_num_layers;
  int64_t offload_block_size;
  void **cache_ptrs;
  int cache_dtype;
  int64_t cache_num_blocks;
  int64_t cache_num_layers;
  int64_t cache_block_size;
  int kv_layout_blocks_first;
  const int64_t *slot_mapping;
  int64_t num_tokens;
  const float **k_scales;
  const float **v_scales;
  int64_t embed_dim;
  int layout;
  void *stream;
} KernelArgs;

/*
 * Get the last error message from a failed operation.
 */
const char *aibrix_get_last_error();
int aibrix_reshape_and_cache_multi_layer(KernelArgs *args);
int aibrix_reshape_and_offload_multi_layer(KernelArgs *args);

#ifdef __cplusplus
}
#endif
