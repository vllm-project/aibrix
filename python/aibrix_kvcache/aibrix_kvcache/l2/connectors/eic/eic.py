import hashlib
import os
import random
import string
import time
from concurrent.futures import Executor
from dataclasses import dataclass
from datetime import datetime
from typing import Any, List, Sequence, Tuple

import eic
import torch
import yaml

from .... import envs
from ....common import AsyncBase
from ....common.absl_logging import getLogger
from ....memory import MemoryRegion
from ....status import Status, StatusCodes
from .. import Connector, ConnectorFeature

logger = getLogger(__name__)


@dataclass
class EicConfig:
    config_file: str = ""


@AsyncBase.async_wrap(
    exists="_exists",
    get="_get",
    put="_put",
    delete="_delete",
    mput="_mput",
    mget="_mget",
)
class EICConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """EIC connector."""

    def __init__(
        self,
        config: EicConfig,
        key_suffix: str,
        executor: Executor,
    ):
        super().__init__(executor)
        self.conn = None
        config_file = config.config_file
        with open(config_file, "r") as fin:
            config_obj = yaml.safe_load(fin)
        self.config_obj = config_obj
        self.key_suffix = key_suffix
        remote_url = config_obj.get("remote_url", None)
        logger.info(f"eic remote_url: {remote_url}")

        eic_instance_id = config_obj.get("eic_instance_id", None)
        logger.info(f"eic instance_id: {eic_instance_id}")

        eic_thread_num = config_obj.get("eic_thread_num", 6)
        logger.info(f"eic thread_num: {eic_thread_num}")

        eic_log_dir = config_obj.get("eic_log_dir", None)
        logger.info(f"eic log_dir: {eic_log_dir}")

        eic_log_level = config_obj.get("eic_log_level", 1)
        logger.info(f"eic log_level: {eic_log_level}")

        eic_trans_type = config_obj.get("eic_trans_type", 3)
        logger.info(f"eic trans_type: {eic_trans_type}")

        self.eic_kv_ttl = config_obj.get("eic_kv_ttl", -1)
        logger.info(f"eic eic_kv_ttl: {self.eic_kv_ttl}")

        self.eic_kv_ns = config_obj.get("eic_kv_ns", "")
        logger.info(f"eic eic_kv_ns: {self.eic_kv_ns}")

        eic_flag_file = config_obj.get("eic_flag_file", None)
        logger.info(f"eic flag_file: {eic_flag_file}")

    def prebuilt_connection(self) -> None:
        def random_string(N):
            return self.eic_kv_ns.join(
                random.choices(string.ascii_uppercase + string.digits, k=N)
            )

        start_time = time.perf_counter_ns()
        for i in range(2048):
            key = random_string(30)
            self.exists_sync(key)
        end_time = time.perf_counter_ns()
        elapsed_time_us = (end_time - start_time) / 1000
        logger.info(
            f"init eic client, namespace: {self.eic_kv_ns}, prebuilt connection finish - total time: {elapsed_time_us:.2f} us"
        )

    @classmethod
    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "EICConnector":
        config = EicConfig(
            config_file=envs.AIBRIX_KV_CACHE_OL_EIC_CONFIG_FILE,
        )
        return cls(config, conn_id, executor)

    @property
    def name(self) -> str:
        return "eic"

    @property
    def feature(self) -> ConnectorFeature:
        feature = ConnectorFeature(rdma=True, mput_mget=True, prefetch=True)
        return feature

    def __del__(self) -> None:
        self.close()

    def _key(self, key: bytes) -> str:
        return hashlib.md5(key).hexdigest() + self.key_suffix

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.conn is None:
            eic_log_dir = self.config_obj.get("eic_log_dir")
            eic_log_level = self.config_obj.get("eic_log_level")
            eic_trans_type = self.config_obj.get("eic_trans_type")
            eic_flag_file = self.config_obj.get("eic_flag_file")
            eic_instance_id = self.config_obj.get("eic_instance_id")
            endpoint = self.config_obj.get("remote_url").replace("eic://", "")
            self.conn = eic.Client()
            init_option = eic.InitOption()
            init_option.log_dir = eic_log_dir
            init_option.log_level = eic.LogLevel(eic_log_level)
            init_option.transport_type = eic.TransportType(eic_trans_type)
            init_option.flag_file = eic_flag_file
            ret = self.conn.init(eic_instance_id, endpoint, init_option)
            _make_dir(eic_log_dir)
            if ret != 0:
                logger.error(f"fail to init eic client, ret: {ret}")
                return Status.error(
                    StatusCodes.ERROR, f"fail to init eic client, ret: {ret}"
                )
            else:
                logger.info(f"init eic client success, ret: {ret}")
            self.trans_type = eic.TransportType(eic_trans_type)
            self.prebuilt_connection()
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.conn is not None:
            self.conn = None
        return Status.ok()

    @Status.capture_exception
    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        assert self.conn is not None
        vals = eic.IOBuffers()
        meminfo = eic.MemoryInfo()

        if len(slabs) > 0:
            tensor = slabs[0]
            if tensor.is_cuda:
                meminfo.type = eic.MemoryType.MEMORY_CUDA
                meminfo.cuda_id = tensor.device.index
            else:
                meminfo.type = eic.MemoryType.MEMORY_DEFAULT
        else:
            return Status.error(
                StatusCodes.ERROR, "fail to register mixed memory pin buffer"
            )

        # Calculate total size and log registration info
        total_size = 0
        for slab in slabs:
            addr = slab.data_ptr()
            length = slab.numel()
            total_size += length
            vals.append(addr, length, True)

        success = self.conn.register_memory(vals, meminfo)
        if success:
            registration_time = datetime.now()
            logger.info(
                f"registering slabs - total size: {total_size}, registration time: {registration_time}"
            )
            logger.info("register mixed memory pin buffer success")
            return Status.ok()
        else:
            logger.error("fail to register mixed memory pin buffer")
            return Status.error(
                StatusCodes.ERROR, "fail to register mixed memory pin buffer"
            )

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        assert self.conn is not None
        temp = self.exists_sync(self._key(key))
        if temp:
            return Status.ok()
        return Status[Any](StatusCodes.NOT_FOUND)

    def exists_sync(self, key_str: str) -> bool:
        keys = eic.StringVector()
        keys.append(key_str)
        exist_option = eic.ExistOption()
        status_code, exist_outcome = self.conn.mexist(keys, exist_option)
        if status_code != eic.StatusCode.SUCCESS:
            logger.debug(
                f"eic exists {key_str} failed, status_code {status_code}"
            )
        err_code = exist_outcome.status_codes[0]
        success = err_code == eic.StatusCode.SUCCESS
        if success:
            logger.debug(f"eic exists {key_str} success")
        else:
            logger.debug(
                f"eic exists {key_str} failed, status_code {status_code} err_code {err_code}"
            )
        return success

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion],
        batch_size: int,
    ) -> Sequence[Sequence[Tuple[bytes, MemoryRegion]]]:
        lists: List[List[Tuple[bytes, MemoryRegion]]] = []
        for key, mr in zip(keys, mrs):
            if len(lists) == 0 or len(lists[-1]) >= batch_size:
                lists.append([(key, mr)])
            else:
                lists[-1].append((key, mr))
        return lists

    @Status.capture_exception
    def _mget(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        return self._get_values(keys, mrs)

    @Status.capture_exception
    def _mput(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        return self._set_value(keys, mrs)

    @Status.capture_exception
    def _get(self, key: bytes, mr: MemoryRegion) -> Status[torch.Tensor]:
        result = self._get_values([key], [mr])
        return result[0]

    @Status.capture_exception
    def _put(self, key: bytes, mr: MemoryRegion) -> Status:
        result = self._set_value([key], [mr])
        return result[0]

    # TODO: support delete
    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.conn is not None
        logger.debug(f"delete key={self._key(key)}")
        return Status.ok()

    def _get_values(self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]):
        data_keys = eic.StringVector()
        data_vals = eic.IOBuffers()

        for i, mr in enumerate(mrs):
            data_keys.append(self._key(keys[i]))
            data_vals.append(mr.data_ptr(), mr.length, True)

        get_option = eic.GetOption()
        get_option.ns = self.eic_kv_ns
        status_code, data_vals, get_outcome = self.conn.mget(
            data_keys, get_option, data_vals
        )

        if status_code != eic.StatusCode.SUCCESS:
            logger.info(f"eic mget data failed, status_code {status_code}")
            return [Status(StatusCodes.NOT_FOUND)] * len(mrs)

        result = []
        for i, status in enumerate(get_outcome.status_codes):
            if status == eic.StatusCode.SUCCESS:
                result.append(Status.ok())
            else:
                logger.info(
                    f"eic mget {self._key(keys[i])} failed, status_code {status}"
                )
                result.append(
                    Status.error(
                        StatusCodes.ERROR,
                        f"eic mget {self._key(keys[i])} failed, status_code {status}",
                    )
                )
        return result

    def _set_value(self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]):
        assert self.conn is not None
        eic_keys = eic.StringVector()
        eic_vals = eic.IOBuffers()
        for key, mr in zip(keys, mrs):
            eic_keys.append(self._key(key))
            eic_vals.append(mr.data_ptr(), mr.length, False)
        set_option = eic.SetOption()
        set_option.ns = self.eic_kv_ns
        set_option.ttl_second = self.eic_kv_ttl
        status_code, set_outcome = self.conn.mset(
            eic_keys, eic_vals, set_option
        )

        if status_code != eic.StatusCode.SUCCESS:
            logger.info(f"eic mset failed, status_code {status_code}")
            return [
                Status.error(
                    StatusCodes.ERROR,
                    f"eic mset failed, status_code {status_code}",
                )
            ] * len(mrs)
        result = []
        for i, status in enumerate(set_outcome.status_codes):
            if status == eic.StatusCode.SUCCESS:
                result.append(Status.ok())
            else:
                logger.info(
                    f"eic mset {self._key(keys[i])} failed, status_code {status}"
                )
                result.append(
                    Status.error(
                        StatusCodes.ERROR,
                        f"eic mset {self._key(keys[i])} failed, status_code {status}",
                    )
                )
        return result


def _make_dir(path: str):
    try:
        if not os.path.exists(path):
            os.makedirs(path)
        logger.info(f"create dir '{path}' success")
    except OSError as e:
        logger.error(f"create dir '{path}' error {e}")
