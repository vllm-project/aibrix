# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
import ctypes
import sys
from pathlib import Path
from typing import List

import torch
import torch.cuda

# Data type mapping (matches enum in cache_kernels.cu)
DTYPE_FLOAT16 = 0
DTYPE_BFLOAT16 = 1
DTYPE_FLOAT32 = 2
DTYPE_FP8 = 3

# Layout mapping
LAYOUT_LCND = 0
LAYOUT_NCLD = 1


class KernelArgs(ctypes.Structure):
    """Structure matching the C KernelArgs struct."""

    _fields_ = [
        ("offload_ptrs", ctypes.c_void_p),  # void**
        ("offload_dtype", ctypes.c_int),
        ("offload_num_blocks", ctypes.c_int64),
        ("offload_num_layers", ctypes.c_int64),
        ("offload_block_size", ctypes.c_int64),
        ("cache_ptrs", ctypes.c_void_p),  # void**
        ("cache_dtype", ctypes.c_int),
        ("cache_num_blocks", ctypes.c_int64),
        ("cache_num_layers", ctypes.c_int64),
        ("cache_block_size", ctypes.c_int64),
        ("kv_layout_blocks_first", ctypes.c_int),
        ("slot_mapping", ctypes.c_void_p),  # int64_t*
        ("num_tokens", ctypes.c_int64),
        ("k_scales", ctypes.c_void_p),  # const float**
        ("v_scales", ctypes.c_void_p),  # const float**
        ("embed_dim", ctypes.c_int64),
        ("layout", ctypes.c_int),
        ("stream", ctypes.c_void_p),
    ]


# Load the shared library
_lib = None


def _load_library():
    """Load the CUDA extension library using ctypes."""
    global _lib

    if _lib is not None:
        return _lib

    # Search for the library
    package_dir = Path(__file__).parent
    _lib_filename = (
        f"_aibrix_C.cpython-{sys.version_info.major}"
        f"{sys.version_info.minor}-x86_64-linux-gnu.so"
    )
    search_paths = [
        package_dir / _lib_filename,
        package_dir / "_aibrix_C.abi3.so",  # Python Stable ABI
        package_dir / "_aibrix_C.so",
    ]

    # Also check in site-packages
    for site_pkg in sys.path:
        if "site-packages" in site_pkg or "dist-packages" in site_pkg:
            site_pkg_path = Path(site_pkg) / "aibrix_kvcache"
            search_paths.append(site_pkg_path / _lib_filename)
            search_paths.append(site_pkg_path / "_aibrix_C.abi3.so")
            search_paths.append(site_pkg_path / "_aibrix_C.so")

    lib_path = None
    for path in search_paths:
        if path.exists():
            lib_path = str(path)
            break

    if lib_path is None:
        raise ImportError(
            f"Could not find _aibrix_C shared library. Searched: {search_paths}"
        )

    try:
        _lib = ctypes.CDLL(lib_path)
    except OSError as e:
        raise ImportError(f"Failed to load library from {lib_path}: {e}")

    # Set up API with struct pointer
    _lib.aibrix_reshape_and_cache_multi_layer.argtypes = [
        ctypes.POINTER(KernelArgs)
    ]
    _lib.aibrix_reshape_and_cache_multi_layer.restype = ctypes.c_int

    _lib.aibrix_reshape_and_offload_multi_layer.argtypes = [
        ctypes.POINTER(KernelArgs)
    ]
    _lib.aibrix_reshape_and_offload_multi_layer.restype = ctypes.c_int

    _lib.aibrix_get_last_error.argtypes = []
    _lib.aibrix_get_last_error.restype = ctypes.c_char_p

    return _lib


# CUDA Runtime library handle for pinned memory device pointer lookup
_cudart = None


def _get_cudart():
    """Lazy load CUDA runtime library."""
    global _cudart
    if _cudart is None:
        _cudart = ctypes.CDLL("libcudart.so")
    return _cudart


def _get_tensor_ptr(tensor: torch.Tensor) -> ctypes.c_void_p:
    """Get device pointer from tensor for CUDA kernel calls."""
    if tensor.device.type == "cuda":
        return ctypes.c_void_p(tensor.data_ptr())
    elif tensor.is_pinned():
        ptr = ctypes.c_void_p()
        _get_cudart().cudaHostGetDevicePointer(
            ctypes.byref(ptr), ctypes.c_void_p(tensor.data_ptr()), 0
        )
        return ptr
    else:
        raise ValueError(
            f"Tensor must be on GPU or pinned, got {tensor.device}"
        )


def _prepare_ptr_array(tensors: List[torch.Tensor]) -> torch.Tensor:
    """Prepare array of device pointers on GPU memory."""
    ptrs = [_get_tensor_ptr(t).value for t in tensors]
    ptr_tensor = torch.tensor(ptrs, dtype=torch.int64, device="cpu")
    ptr_tensor_gpu = ptr_tensor.cuda()
    return ptr_tensor_gpu


def _raise_kernel_error(func_name: str, lib) -> None:
    error_msg = lib.aibrix_get_last_error()
    error_str = error_msg.decode() if error_msg else "unknown error"
    raise RuntimeError(f"{func_name} failed: {error_str}")


def _get_dtype_enum(dtype):
    """Convert torch dtype or string to kernel dtype enum.

    Supports all vLLM-compatible dtypes:
    - float32, float, half, float16 -> DTYPE_FLOAT32/DTYPE_FLOAT16
    - bfloat16 -> DTYPE_BFLOAT16
    - fp8, fp8_e4m3, fp8_e5m2, fp8_inc, fp8_ds_mla, int8, uint8 -> DTYPE_FP8
    """
    # Handle string input
    if isinstance(dtype, str):
        dtype_str = dtype.lower().strip()
        str_to_enum = {
            "float32": DTYPE_FLOAT32,
            "float": DTYPE_FLOAT32,
            "half": DTYPE_FLOAT16,
            "float16": DTYPE_FLOAT16,
            "bfloat16": DTYPE_BFLOAT16,
            "bf16": DTYPE_BFLOAT16,
            "fp8": DTYPE_FP8,
            "fp8_e4m3": DTYPE_FP8,
            "fp8_e5m2": DTYPE_FP8,
            "fp8_inc": DTYPE_FP8,
            "fp8_ds_mla": DTYPE_FP8,
            "int8": DTYPE_FP8,
            "uint8": DTYPE_FP8,
        }
        if dtype_str in str_to_enum:
            return str_to_enum[dtype_str]
        raise ValueError(f"Unsupported dtype string: {dtype}")

    # Handle torch.dtype input
    if dtype == torch.float16 or dtype == torch.half:
        return DTYPE_FLOAT16
    elif dtype == torch.bfloat16:
        return DTYPE_BFLOAT16
    elif dtype == torch.float32 or dtype == torch.float:
        return DTYPE_FLOAT32
    elif dtype in (torch.int8, torch.uint8):
        return DTYPE_FP8  # 8-bit integer types map to FP8 storage
    elif "fp8" in str(dtype).lower() or "float8" in str(dtype).lower():
        return DTYPE_FP8
    else:
        supported = [
            "float16/half",
            "bfloat16",
            "float32/float",
            "int8",
            "uint8",
            "fp8",
            "fp8_e4m3",
            "fp8_e5m2",
            "fp8_inc",
            "fp8_ds_mla",
        ]
        raise ValueError(f"Unsupported dtype: {dtype}. Supported: {supported}")


def reshape_and_cache_multi_layer(
    offload_kv_cache_blocks: list[torch.Tensor],
    kv_caches: list[torch.Tensor],
    slot_mapping: torch.Tensor,
    block_size: int,
    kv_cache_dtype: str,
    k_scales: list[torch.Tensor],
    v_scales: list[torch.Tensor],
    layout: str,
    kv_layout_blocks_first: bool = False,
) -> None:
    """Reshape and cache multi-layer KV cache (onload)."""
    lib = _load_library()

    # Prepare pointer arrays on GPU
    offload_ptrs_tensor = _prepare_ptr_array(offload_kv_cache_blocks)
    kv_ptrs_tensor = _prepare_ptr_array(kv_caches)
    k_scale_ptrs_tensor = _prepare_ptr_array(k_scales)
    v_scale_ptrs_tensor = _prepare_ptr_array(v_scales)

    offload_dtype = _get_dtype_enum(offload_kv_cache_blocks[0].dtype)

    if kv_cache_dtype == "auto":
        kv_cache_dtype_enum = _get_dtype_enum(kv_caches[0].dtype)
    else:
        kv_cache_dtype_enum = _get_dtype_enum(kv_cache_dtype)

    layout_enum = LAYOUT_LCND if layout == "LCND" else LAYOUT_NCLD

    # Calculate embed_dim
    kcache = kv_caches[0]
    kv_cache_shape = kcache.shape
    if len(kv_cache_shape) == 3:
        embed_dim = kcache.stride(1) // block_size
    else:
        embed_dim = kcache.stride(2)

    # Create KernelArgs struct
    args = KernelArgs()
    args.offload_ptrs = ctypes.c_void_p(offload_ptrs_tensor.data_ptr())
    args.offload_dtype = offload_dtype
    args.offload_num_blocks = len(offload_kv_cache_blocks)
    args.offload_num_layers = (
        offload_kv_cache_blocks[0].shape[2]
        if layout == "NCLD"
        else offload_kv_cache_blocks[0].shape[0]
    )
    args.offload_block_size = (
        offload_kv_cache_blocks[0].shape[0]
        if layout == "NCLD"
        else offload_kv_cache_blocks[0].shape[2]
    )
    args.cache_ptrs = ctypes.c_void_p(kv_ptrs_tensor.data_ptr())
    args.cache_dtype = kv_cache_dtype_enum
    args.cache_num_blocks = kv_cache_shape[1]
    args.cache_num_layers = len(kv_caches)
    args.cache_block_size = block_size
    args.kv_layout_blocks_first = 1 if kv_layout_blocks_first else 0
    args.slot_mapping = ctypes.c_void_p(slot_mapping.data_ptr())
    args.num_tokens = slot_mapping.size(0)
    args.k_scales = ctypes.c_void_p(k_scale_ptrs_tensor.data_ptr())
    args.v_scales = ctypes.c_void_p(v_scale_ptrs_tensor.data_ptr())
    args.embed_dim = embed_dim
    args.layout = layout_enum
    args.stream = ctypes.c_void_p(torch.cuda.current_stream().cuda_stream)

    # Keep tensors alive during kernel execution
    _ptr_tensors = (
        offload_ptrs_tensor,
        kv_ptrs_tensor,
        k_scale_ptrs_tensor,
        v_scale_ptrs_tensor,
        *offload_kv_cache_blocks,
        *kv_caches,
        *k_scales,
        *v_scales,
    )

    result = lib.aibrix_reshape_and_cache_multi_layer(ctypes.byref(args))

    del _ptr_tensors

    if result != 0:
        _raise_kernel_error("reshape_and_cache_multi_layer", lib)


def reshape_and_offload_multi_layer(
    offload_kv_cache_blocks: list[torch.Tensor],
    kv_caches: list[torch.Tensor],
    slot_mapping: torch.Tensor,
    block_size: int,
    kv_cache_dtype: str,
    k_scales: list[torch.Tensor],
    v_scales: list[torch.Tensor],
    layout: str,
    kv_layout_blocks_first: bool = False,
) -> None:
    """Reshape and offload multi-layer KV cache (offload)."""
    lib = _load_library()

    # Prepare pointer arrays on GPU
    offload_ptrs_tensor = _prepare_ptr_array(offload_kv_cache_blocks)
    kv_ptrs_tensor = _prepare_ptr_array(kv_caches)
    k_scale_ptrs_tensor = _prepare_ptr_array(k_scales)
    v_scale_ptrs_tensor = _prepare_ptr_array(v_scales)

    offload_dtype = _get_dtype_enum(offload_kv_cache_blocks[0].dtype)

    if kv_cache_dtype == "auto":
        kv_cache_dtype_enum = _get_dtype_enum(kv_caches[0].dtype)
    else:
        kv_cache_dtype_enum = _get_dtype_enum(kv_cache_dtype)

    layout_enum = LAYOUT_LCND if layout == "LCND" else LAYOUT_NCLD

    # Calculate embed_dim
    kcache = kv_caches[0]
    kv_cache_shape = kcache.shape
    if len(kv_cache_shape) == 3:
        embed_dim = kcache.stride(1) // block_size
    else:
        embed_dim = kcache.stride(2)

    # Create KernelArgs struct
    args = KernelArgs()
    args.offload_ptrs = ctypes.c_void_p(offload_ptrs_tensor.data_ptr())
    args.offload_dtype = offload_dtype
    args.offload_num_blocks = len(offload_kv_cache_blocks)
    args.offload_num_layers = (
        offload_kv_cache_blocks[0].shape[2]
        if layout == "NCLD"
        else offload_kv_cache_blocks[0].shape[0]
    )
    args.offload_block_size = (
        offload_kv_cache_blocks[0].shape[0]
        if layout == "NCLD"
        else offload_kv_cache_blocks[0].shape[2]
    )
    args.cache_ptrs = ctypes.c_void_p(kv_ptrs_tensor.data_ptr())
    args.cache_dtype = kv_cache_dtype_enum
    args.cache_num_blocks = kv_cache_shape[1]
    args.cache_num_layers = len(kv_caches)
    args.cache_block_size = block_size
    args.kv_layout_blocks_first = 1 if kv_layout_blocks_first else 0
    args.slot_mapping = ctypes.c_void_p(slot_mapping.data_ptr())
    args.num_tokens = slot_mapping.size(0)
    args.k_scales = ctypes.c_void_p(k_scale_ptrs_tensor.data_ptr())
    args.v_scales = ctypes.c_void_p(v_scale_ptrs_tensor.data_ptr())
    args.embed_dim = embed_dim
    args.layout = layout_enum
    args.stream = ctypes.c_void_p(torch.cuda.current_stream().cuda_stream)

    # Keep tensors alive during kernel execution
    _ptr_tensors = (
        offload_ptrs_tensor,
        kv_ptrs_tensor,
        k_scale_ptrs_tensor,
        v_scale_ptrs_tensor,
        *offload_kv_cache_blocks,
        *kv_caches,
        *k_scales,
        *v_scales,
    )

    result = lib.aibrix_reshape_and_offload_multi_layer(ctypes.byref(args))

    del _ptr_tensors

    if result != 0:
        _raise_kernel_error("reshape_and_offload_multi_layer", lib)
