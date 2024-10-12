import os
from typing import List, Optional, Tuple
from urllib.parse import urlparse

import boto3
import torch
import tos


def get_dtype(dtype_str: str):
    # torch.float8 formats require 2.1; we do not support these dtypes on earlier versions
    _float8_e4m3fn = getattr(torch, "float8_e4m3fn", None)
    _float8_e5m2 = getattr(torch, "float8_e5m2", None)
    _TYPES = {
        "F64": torch.float64,
        "F32": torch.float32,
        "F16": torch.float16,
        "BF16": torch.bfloat16,
        "I64": torch.int64,
        # "U64": torch.uint64,
        "I32": torch.int32,
        # "U32": torch.uint32,
        "I16": torch.int16,
        # "U16": torch.uint16,
        "I8": torch.int8,
        "U8": torch.uint8,
        "BOOL": torch.bool,
        "F8_E4M3": _float8_e4m3fn,
        "F8_E5M2": _float8_e5m2,
    }
    return _TYPES[dtype_str]


def _parse_bucket_info_from_uri(uri: str, shcema: str = "s3") -> Tuple[str, str]:
    parsed = urlparse(uri, scheme=shcema)
    bucket_name = parsed.netloc
    bucket_path = parsed.path.lstrip("/")
    return bucket_name, bucket_path


def _create_s3_client():
    ak = os.getenv("AWS_ACCESS_KEY_ID")
    sk = os.getenv("AWS_SECRET_ACCESS_KEY")
    endpoint = os.getenv("AWS_ENDPOINT_URL")
    region = os.getenv("AWS_REGION")
    assert ak is not None and ak != "", "`AWS_ACCESS_KEY_ID` is not set."
    assert sk is not None and sk != "", "`AWS_SECRET_ACCESS_KEY` is not set."

    client = boto3.client(
        service_name="s3",
        region_name=region,
        endpoint_url=endpoint,
        aws_access_key_id=ak,
        aws_secret_access_key=sk,
    )
    return client


def _create_tos_client():
    ak = os.getenv("TOS_ACCESS_KEY")
    sk = os.getenv("TOS_SECRET_KEY")
    endpoint = os.getenv("TOS_ENDPOINT")
    region = os.getenv("TOS_REGION")
    assert ak is not None and ak != "", "`AWS_ACCESS_KEY_ID` is not set."
    assert sk is not None and sk != "", "`AWS_SECRET_ACCESS_KEY` is not set."

    client = tos.TosClientV2(
        ak=ak, sk=sk, endpoint=endpoint, region=region, enable_crc=False
    )
    return client


class TensorMeta:
    def __init__(self, name: str, base_offset: int, dtype: str, shape: List[int], data_offsets: List[int]) -> None:
        self._name = name
        self._base_offset = base_offset
        self._dtype = get_dtype(dtype)
        self._shape = shape
        self._data_offsets = data_offsets
        self._tensor = None

    @property
    def name(self) -> str:
        return self._name

    @property
    def dtype(self) -> torch.dtype:
        return self._dtype

    @property
    def shape(self) -> List[int]:
        return self._shape

    @property
    def data_offsets(self) -> List[int]:
        return self._data_offsets

    @property
    def real_offset(self) -> List[int]:
        return self._data_offsets[0] + self._base_offset

    @property
    def count(self) -> List[int]:
        return self._data_offsets[1] - self._data_offsets[0]

    @property
    def tensor(self) -> Optional[torch.Tensor]:
        return self._tensor

    def set_tensor(self, tensor: torch.Tensor):
        self._tensor = tensor

    def __str__(self) -> str:
        return str(
            {
                "name": self._name,
                "dtype": self._dtype,
                "shape": self._shape,
                "data_offsets": self._data_offsets,
            }
        )

    def __repr__(self) -> str:
        return self.__str__()


def split_continue_tensors(tensor_metas: List[TensorMeta], num_readers:int) -> List[Tuple[TensorMeta]]:
    assert len(tensor_metas) > 0, "tensor_metas should not be empty"
    assert num_readers > 0, "num_readers should be greater than 0"
    
    if len(tensor_metas) <= num_readers:
        return [tuple([item]) for item in tensor_metas]
    
    max_offset = tensor_metas[-1].data_offsets[1]
    avg_size = max_offset // num_readers
    group = []
    groups = []
    current_max_offset = avg_size
    for tensor_meta in tensor_metas:
        start, end = tensor_meta.data_offsets
        while start >= current_max_offset:
            current_max_offset += avg_size
        
        if end <= current_max_offset:
            group.append(tensor_meta)
        else:
            if len(group) != 0:
                groups.append(tuple(group))
            group = [tensor_meta]
            
            current_max_offset += avg_size
    if len(group) != 0:
        groups.append(tuple(group))
    return groups
