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

import ipaddress
import socket
import struct
from dataclasses import dataclass
from enum import Enum
from typing import List, Sequence

import netifaces
import pyverbs
from pyverbs.addr import GID
from pyverbs.device import (
    Context,
    Device,
    phys_state_to_str,
    port_state_to_str,
    speed_to_str,
    translate_link_layer,
    translate_mtu,
    width_to_str,
)
from pyverbs.enums import (
    ibv_gid_type_sysfs,
    ibv_port_state,
)
from pyverbs.utils import gid_str_to_array
from validators import ipv4

from ..common.absl_logging import getLogger
from ..status import Status, StatusCodes

logger = getLogger(__name__)


class GIDType(Enum):
    IB_ROCE_V1 = "IB_ROCE_V1"
    ROCE_V2 = "ROCE_V2"


class AddrFamily(Enum):
    AF_INET = socket.AF_INET
    AF_INET6 = socket.AF_INET6


class DeviceRequest:
    def __init__(
        self, *, device_name: str = "", port_num: int = -1, gid_index: int = -1
    ):
        """
        Initialize RDMA device request.

        Args:
            device_name (str): Optional specific device name to use
            port_num (int): Optional specific port number to use
            gid_index (int): GID index to use (-1 for auto-detection)
        """
        self.device_name = device_name
        self.port_num = port_num
        self.gid_index = gid_index


@dataclass
class PortAttributes:
    port_num: int
    state: str
    phys_state: str
    active_speed: str
    active_width: str
    bandwidth_score: int
    link_layer: str
    is_active: bool
    lid: int
    sm_lid: int
    cap_flags: int
    max_mtu: str
    active_mtu: str
    gid_tbl_len: int
    pkey_tbl_len: int
    gid_index: int


@dataclass
class RDMADevice:
    device: Device
    port_attrs: PortAttributes

    @property
    def device_name(self) -> str:
        return self.device.name.decode()


class RDMATransport:
    def __init__(
        self,
        *,
        request: DeviceRequest | Sequence[DeviceRequest] | None = None,
        addr_family: AddrFamily | None = None,
        addr_range: str = "",
        gid_type: GIDType | None = None,
    ):
        """
        Initialize RDMA transport.

        Args:
            request: List of RDMA device request. If None, auto-detection
                     will be used.
            addr_family (AddrFamily | None): Address family preference
                                             (AF_INET/AF_INET6)
            addr_range (str): IP address range in CIDR notation
                              (e.g., "192.168.1.0/24")
            gid_type (GIDType | None): GID type preference (IB_ROCE_V1/ROCE_V2)
        """
        self.requests: Sequence[DeviceRequest] = []
        if request is not None:
            self.requests = (
                [request] if isinstance(request, DeviceRequest) else request
            )
        self.addr_family = addr_family
        self.addr_range = addr_range
        self.gid_type = gid_type

        self.devices: List[RDMADevice] = []

        self._check_params()

    def _check_params(self) -> None:
        if self.addr_range:
            try:
                ipaddress.ip_network(self.addr_range, strict=False)
            except Exception as e:
                raise ValueError(
                    f"Invalid addr_range: {self.addr_range}, error: {e}"
                )

        if self.addr_family and self.addr_range:
            if not self._check_addr_family_addr_range():
                raise ValueError(
                    f"addr_family {self.addr_family} not compatible with "
                    f"addr_range {self.addr_range}"
                )

    def _check_addr_family_addr_range(self) -> bool:
        try:
            network = ipaddress.ip_network(self.addr_range, strict=False)
            if self.addr_family == AddrFamily.AF_INET and isinstance(
                network, ipaddress.IPv4Network
            ):
                return True
            elif self.addr_family == AddrFamily.AF_INET6 and isinstance(
                network, ipaddress.IPv6Network
            ):
                return True
            else:
                return False
        except ValueError:
            return False

    def get_device_list(self) -> Status[List[RDMADevice]]:
        """Get device list"""
        # Get all available devices
        devices = pyverbs.device.get_device_list()
        if not devices:
            raise RuntimeError("No RDMA devices found")

        status = self._select_devices(devices)
        if not status.is_ok():
            return status

        return Status.ok(self.devices)

    def _select_devices(self, devices: List[Device]) -> Status:
        """Select devices"""
        requests = {request.device_name: request for request in self.requests}
        selected_devices = []
        valid_requests = []
        if len(requests) == 0:
            selected_devices = devices
        else:
            # If specific devices requested, find them
            for device in devices:
                device_name = device.name.decode()
                if device_name in requests:
                    selected_devices.append(device)
                    valid_requests.append(requests[device_name])
            if len(selected_devices) == 0:
                logger.error("Requested devices %s not found", requests.keys())
                return Status(
                    StatusCodes.ERROR,
                    f"Requested devices {requests.keys()} not found",
                )

        filtered_devices = []
        for i, device in enumerate(selected_devices):
            request = (
                valid_requests[i]
                if i < len(valid_requests)
                else DeviceRequest()
            )
            status = self._select_port(device, request)
            if not status.is_ok():
                logger.warning(
                    "Failed to select ports for device %s, skip it",
                    device.name.decode(),
                )
                continue
            filtered_devices.append((device, status.get()))

        if len(filtered_devices) == 0:
            logger.error("No valid devices found")
            return Status(StatusCodes.ERROR, "No valid devices found")

        device_names = [pair[0].name.decode() for pair in filtered_devices]
        logger.info("Selected RDMA devices: %s", device_names)

        self.devices = [
            RDMADevice(device=device, port_attrs=attrs)
            for device, attrs in filtered_devices
        ]

        return Status.ok()

    def _select_port(
        self, device: Device, request: DeviceRequest
    ) -> Status[PortAttributes]:
        """Select port"""
        device_name = device.name.decode()
        context = Context(name=device_name)
        active_ports = self._list_all_active_ports(context)
        if len(active_ports) == 0:
            logger.warning("No active ports found for device %s", device_name)
            return Status(StatusCodes.NOT_FOUND)

        if request.port_num in [port.port_num for port in active_ports]:
            logger.info(
                "Got valid port number %d specified for device %s, checking it",
                request.port_num,
                device_name,
            )

            return self._select_port_by_port_num(context, request)
        elif request.gid_index >= 0:
            logger.info(
                "Got GID index %d specified for device %s, checking it",
                request.gid_index,
                device_name,
            )

            return self._select_port_by_gid_index(context, request)

        # auto detect
        return self._select_port_auto(context)

    def _select_port_auto(self, context: Context) -> Status[PortAttributes]:
        device_name = context.name
        port_attrs: PortAttributes | None = None
        logger.info(
            "Trying to select port by bandwidth for device %s",
            device_name,
        )
        status = self._select_port_by_bandwidth(context)

        if not status.is_ok():
            logger.warning(
                "Failed to select port by bandwidth for device %s, "
                "selecting the first active port",
                device_name,
            )
        else:
            port_attrs = status.get()

        if port_attrs is None:
            logger.info(
                "Trying to select the first active port for device %s",
                device_name,
            )
            status = self._select_first_active_port(context)
            if not status.is_ok():
                logger.warning(
                    "No active ports found for device %s", device_name
                )
            else:
                port_attrs = status.get()

        if port_attrs is None:
            logger.warning("Failed to select port for device %s", device_name)
            return Status(StatusCodes.NOT_FOUND)

        logger.info("Selected port for device %s: %s", device_name, port_attrs)

        return Status.ok(port_attrs)

    def _select_port_by_port_num(
        self, context: Context, request: DeviceRequest
    ) -> Status[PortAttributes]:
        port_attrs = self._get_port_attributes(context, request.port_num)

        if not port_attrs.is_active:
            logger.warning(
                "Requested port %d is not active on device %s",
                request.port_num,
                context.name,
            )
            return Status(StatusCodes.NOT_FOUND)

        if port_attrs.gid_index < 0 or (
            request.gid_index >= 0 and port_attrs.gid_index != request.gid_index
        ):  # type: ignore
            logger.warning(
                "Requested port %d on device %s does not have a "
                "valid GID index",
                request.port_num,
                context.name,
            )
            return Status(StatusCodes.NOT_FOUND)

        return Status.ok(port_attrs)

    def _select_port_by_gid_index(
        self, context: Context, request: DeviceRequest
    ) -> Status[PortAttributes]:
        active_ports = [p for p in self._list_all_active_ports(context)]
        if not active_ports:
            return Status(StatusCodes.NOT_FOUND)

        for p in active_ports:
            if p.gid_index == request.gid_index:
                return Status.ok(p)

            if self._is_valid_gid_index(
                context, p.port_num, p.gid_tbl_len, request.gid_index
            ):
                p.gid_index = request.gid_index
                return Status.ok(p)

        return Status(StatusCodes.NOT_FOUND)

    def _get_port_attributes(
        self, context: Context, port_num: int
    ) -> PortAttributes:
        """
        Get detailed information about a specific port.

        Args:
            context (Context): RDMA context
            port_num (int): Port number to query (1-based index)

        Returns:
            Port attributes
        """
        port_attrs = context.query_port(port_num)

        status = self._select_gid_index(
            context, port_num, port_attrs.gid_tbl_len
        )
        gid_index = status.get() if status.is_ok() else -1

        return PortAttributes(
            port_num=port_num,
            state=port_state_to_str(port_attrs.state),
            phys_state=phys_state_to_str(port_attrs.phys_state),
            active_speed=speed_to_str(port_attrs.active_speed, None),
            active_width=width_to_str(port_attrs.active_width),
            bandwidth_score=port_attrs.active_speed * port_attrs.active_width,
            link_layer=translate_link_layer(port_attrs.link_layer),
            is_active=port_attrs.state == ibv_port_state.IBV_PORT_ACTIVE,
            lid=port_attrs.lid,
            sm_lid=port_attrs.sm_lid,
            cap_flags=port_attrs.port_cap_flags,
            max_mtu=translate_mtu(port_attrs.max_mtu),
            active_mtu=translate_mtu(port_attrs.active_mtu),
            gid_tbl_len=port_attrs.gid_tbl_len,
            pkey_tbl_len=port_attrs.pkey_tbl_len,
            gid_index=gid_index,
        )

    def _list_all_active_ports(self, context: Context) -> List[PortAttributes]:
        """
        Get information for all ports on the device.

        Args:
            context (Context): RDMA context
        Returns:
            Port attributes of all active ports
        """
        device_attrs = context.query_device()
        num_ports = device_attrs.phys_port_cnt
        active_ports = []
        for port_num in range(1, num_ports + 1):
            port_attrs = self._get_port_attributes(context, port_num)
            if port_attrs.is_active and port_attrs.gid_index >= 0:
                active_ports.append(port_attrs)
            elif port_attrs.is_active:
                logger.warning(
                    "Port %d on device %s is active but has no GID index",
                    port_num,
                    context.name,
                )
        return active_ports

    def _select_port_by_bandwidth(
        self, context: Context
    ) -> Status[PortAttributes]:
        """
        Select the port with the highest available bandwidth.

        Args:
            context (Context): RDMA context

        Returns:
            Port attributes of the selected port
        """
        active_ports = [p for p in self._list_all_active_ports(context)]
        if not active_ports:
            return Status(StatusCodes.NOT_FOUND)

        # Select port with highest score
        return Status.ok(max(active_ports, key=lambda x: x.bandwidth_score))

    def _select_first_active_port(
        self, context: Context
    ) -> Status[PortAttributes]:
        """
        Select the first active port found.

        Args:
            context (Context): RDMA context

        Returns:
            Port attributes of the selected port
        """
        for port in self._list_all_active_ports(context):
            return Status.ok(port)
        return Status(StatusCodes.NOT_FOUND)

    def _select_gid_index(
        self, context: Context, port_num: int, gid_tbl_len: int
    ) -> Status[int]:
        """Select the GID index"""
        # If no preferences specified, use default (0)
        if not self.addr_family and not self.addr_range and not self.gid_type:
            return Status.ok(0)

        for idx in range(gid_tbl_len):
            if not self._is_valid_gid_index(
                context, port_num, gid_tbl_len, idx
            ):
                continue

            return Status.ok(idx)

        return Status(StatusCodes.NOT_FOUND)

    def _is_valid_gid_index(
        self, context: Context, port_num: int, gid_tbl_len: int, gid_index: int
    ) -> bool:
        """Check the GID index"""
        if gid_index < 0:
            return False
        elif gid_index >= gid_tbl_len:
            return False

        gid = context.query_gid(port_num, gid_index)

        # Check address family match
        gid_family = self._get_gid_addr_family(gid)
        if self.addr_family and gid_family != self.addr_family:
            return False

        # Check subnet match if specified
        if self.addr_range and not self._match_gid_subnet(gid):
            return False

        # Check GID type if specified
        if self.gid_type:
            gid_type = context.query_gid_type(port_num, gid_index)
            device_gid_type = GIDType.IB_ROCE_V1
            if gid_type == ibv_gid_type_sysfs.IBV_GID_TYPE_SYSFS_ROCE_V2:
                device_gid_type = GIDType.ROCE_V2
            if self.gid_type != device_gid_type:
                return False

        # Check if matches with any network interfaces
        ip = self._get_gid_ip(gid)
        for iface in netifaces.interfaces():
            addrs = netifaces.ifaddresses(iface)
            family = (
                netifaces.AF_INET
                if gid_family == AddrFamily.AF_INET
                else netifaces.AF_INET6
            )
            if family not in addrs:
                continue
            for addr_info in addrs[family]:
                if ip == addr_info.get("addr", "").split("%")[0]:
                    return True

        # If no match found, return False
        return False

    def _get_gid_addr_family(self, gid: GID) -> AddrFamily:
        """Determine address family of a GID.

        Adapted from NVSHMEM and NCCL.
        """
        # Interpret as 4 x uint32_t in network byte order (big-endian)
        raw = bytes.fromhex("".join(gid_str_to_array(gid.gid)))
        s6_addr32 = struct.unpack("!4I", raw)

        is_ipv4_mapped = (
            (s6_addr32[0] | s6_addr32[1]) | (s6_addr32[2] ^ 0x0000FFFF)
        ) == 0

        is_ipv4_mapped_multicast = s6_addr32[0] == 0xFF0E0000 and (
            (s6_addr32[1] | (s6_addr32[2] ^ 0x0000FFFF)) == 0
        )

        if is_ipv4_mapped or is_ipv4_mapped_multicast:
            return AddrFamily.AF_INET
        else:
            return AddrFamily.AF_INET6

    def _get_gid_ip(self, gid: GID) -> str:
        """Get the IP address of a GID."""
        gid_family = self._get_gid_addr_family(gid)
        raw = bytes.fromhex("".join(gid_str_to_array(gid.gid)))
        if gid_family == AddrFamily.AF_INET:
            return socket.inet_ntop(socket.AF_INET, raw[12:16])
        else:
            return socket.inet_ntop(socket.AF_INET6, raw)

    def _match_gid_subnet(self, gid: GID) -> bool:
        """Check if GID matches the configured address range"""
        if not self.addr_range:
            return True

        try:
            # Parse address range (e.g., "192.168.1.0/24")
            prefix_len = int(self.addr_range.split("/")[1])

            # Get GID address family
            gid_family = self._get_gid_addr_family(gid)
            ip_addr = self._get_gid_ip(gid)

            if ipv4(self.addr_range):
                if gid_family != AddrFamily.AF_INET:
                    return False

                gid_net4 = ipaddress.IPv4Network(
                    f"{ip_addr}/{prefix_len}", strict=False
                )
                config_net4 = ipaddress.IPv4Network(
                    self.addr_range, strict=False
                )

                return gid_net4.subnet_of(config_net4)

            else:  # AF_INET6
                if gid_family != AddrFamily.AF_INET6:
                    return False

                gid_net6 = ipaddress.IPv6Network(
                    f"{ip_addr}/{prefix_len}", strict=False
                )
                config_net6 = ipaddress.IPv6Network(
                    self.addr_range, strict=False
                )

                return gid_net6.subnet_of(config_net6)

        except Exception:
            return False
