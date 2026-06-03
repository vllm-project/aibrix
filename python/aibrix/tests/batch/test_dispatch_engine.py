# Copyright 2026 The Aibrix Team.
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

import ast
import asyncio
import pkgutil

import pytest

from aibrix.batch.client import (
    CapacitySignal,
    DispatchEngine,
    InferenceError,
    InferenceErrorCode,
    InferenceRequest,
    NoopEndpointSource,
)


class _FakeChannel:
    def __init__(self, cid, *, fail_times=0):
        self._id = cid
        self._fail_times = fail_times
        self.calls = []

    @property
    def id(self):
        return self._id

    async def send(self, request):
        self.calls.append(request.ref)
        if self._fail_times > 0:
            self._fail_times -= 1
            raise InferenceError(InferenceErrorCode.TRANSPORT_ERROR, f"{self._id} boom")
        return {"by": self._id, "echo": request.payload}

    async def aclose(self):
        return None


class _FakeSource:
    def __init__(self, channels):
        self._channels = channels

    async def channels(self):
        return list(self._channels)

    async def capacity(self):
        return CapacitySignal(count=len(self._channels))

    async def wait_capacity_change(self, previous):
        await asyncio.Future()
        raise AssertionError("unreachable")

    async def aclose(self):
        for channel in self._channels:
            await channel.aclose()


async def _drain(source, requests, **kw):
    engine = DispatchEngine(source, **kw)
    results = []

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    async def feed():
        for r in requests:
            yield r

    await engine.run(feed(), on_result)
    return sorted(results, key=lambda t: t[0])


def _reqs(n):
    return [InferenceRequest(path="/v1/x", payload={"i": i}, ref=i) for i in range(n)]


@pytest.mark.asyncio
async def test_round_robin_spreads_across_channels():
    a, b = _FakeChannel("a"), _FakeChannel("b")
    results = await _drain(_FakeSource([a, b]), _reqs(4))
    assert len(results) == 4
    assert all(err is None for _, _, err in results)
    # 4 requests over 2 channels, round-robin -> 2 each
    assert len(a.calls) == 2 and len(b.calls) == 2


@pytest.mark.asyncio
async def test_failover_to_next_channel_on_error():
    bad, good = _FakeChannel("bad", fail_times=1), _FakeChannel("good")
    # one request; first pick (bad) fails, retry picks good
    results = await _drain(_FakeSource([bad, good]), _reqs(1), max_retries=2)
    ref, resp, err = results[0]
    assert err is None and resp["by"] == "good"


@pytest.mark.asyncio
async def test_all_endpoints_failed_reported_per_request_not_raised():
    bad = _FakeChannel("bad", fail_times=99)
    results = await _drain(_FakeSource([bad]), _reqs(1), max_retries=2)
    _, resp, err = results[0]
    assert resp is None
    assert isinstance(err, InferenceError)
    assert err.code == InferenceErrorCode.ALL_ENDPOINTS_FAILED
    assert len(err.causes) == 3  # max_retries + 1 attempts


@pytest.mark.asyncio
async def test_no_endpoint_raises_per_request_error():
    results = await _drain(_FakeSource([]), _reqs(1))
    _, resp, err = results[0]
    assert resp is None and err.code == InferenceErrorCode.NO_ENDPOINT


@pytest.mark.asyncio
async def test_concurrency_cap_is_respected():
    inflight = 0
    peak = 0

    class _SlowChannel(_FakeChannel):
        async def send(self, request):
            nonlocal inflight, peak
            inflight += 1
            peak = max(peak, inflight)
            await asyncio.sleep(0.01)
            inflight -= 1
            return {"ok": True}

    source = _FakeSource([_SlowChannel("s")])
    await _drain(source, _reqs(10), max_retries=0)
    # capacity()==1 -> limit derived as 1
    assert peak == 1


@pytest.mark.asyncio
async def test_explicit_concurrency_overrides_capacity():
    peak = 0
    inflight = 0

    class _SlowChannel(_FakeChannel):
        async def send(self, request):
            nonlocal inflight, peak
            inflight += 1
            peak = max(peak, inflight)
            await asyncio.sleep(0.01)
            inflight -= 1
            return {"ok": True}

    engine = DispatchEngine(_FakeSource([_SlowChannel("s")]))
    results = []

    async def on_result(req, resp, err):
        results.append(req.ref)

    async def feed():
        for r in _reqs(10):
            yield r

    await engine.run(feed(), on_result, max_concurrency=4)
    assert len(results) == 10
    assert 1 < peak <= 4


@pytest.mark.asyncio
async def test_noop_source_echoes_payload():
    source = NoopEndpointSource()
    results = await _drain(source, _reqs(1))
    _, resp, err = results[0]
    assert err is None and resp == {"i": 0}
    await source.aclose()


@pytest.mark.asyncio
async def test_gateway_capacity_is_not_one_static_is():
    from aibrix.batch.client import GatewayEndpointSource, StaticEndpointSource

    gateway = GatewayEndpointSource("http://gw")
    static = StaticEndpointSource("http://pod")
    try:
        assert (await gateway.capacity()).count > 1  # must not serialize a gateway
        assert (await static.capacity()).count == 1
        assert (
            await GatewayEndpointSource("http://gw", capacity=32).capacity()
        ).count == 32
    finally:
        await gateway.aclose()
        await static.aclose()


@pytest.mark.asyncio
async def test_capacity_drives_concurrency_against_single_channel():
    # Gateway case: one channel, capacity N -> up to N concurrent to that channel.
    inflight = 0
    peak = 0

    class _SlowChannel(_FakeChannel):
        async def send(self, request):
            nonlocal inflight, peak
            inflight += 1
            peak = max(peak, inflight)
            await asyncio.sleep(0.01)
            inflight -= 1
            return {"ok": True}

    class _CapSource(_FakeSource):
        def __init__(self, channels, cap):
            super().__init__(channels)
            self._cap = cap

        async def capacity(self):
            return CapacitySignal(count=self._cap)

    source = _CapSource([_SlowChannel("gw")], 5)
    await _drain(source, _reqs(10), max_retries=0)
    assert peak == 5


@pytest.mark.asyncio
async def test_port_forward_source_parses_assigned_local_port(monkeypatch):
    import io as _io

    from aibrix.batch.client.sources import kubernetes as k8s_src

    assigned_port = 39123
    commands = []

    class _FakeProcess:
        def __init__(self):
            self.stdout = _io.StringIO(
                f"Forwarding from 127.0.0.1:{assigned_port} -> 8000\n"
            )

        def poll(self):
            return None

        def terminate(self):
            return None

        def wait(self, timeout=None):
            return None

    def _popen(command, **kwargs):
        commands.append(command)
        return _FakeProcess()

    monkeypatch.setattr(k8s_src.subprocess, "Popen", _popen)

    source = k8s_src.PortForwardEndpointSource("default", "svc", 8000)
    channels = await source.channels()
    assert commands == [
        ["kubectl", "-n", "default", "port-forward", "service/svc", ":8000"]
    ]
    assert channels[0].id == f"http://127.0.0.1:{assigned_port}"
    await source.aclose()


def _imported_modules(source):
    names = set()
    for node in ast.walk(ast.parse(source)):
        if isinstance(node, ast.Import):
            names.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            names.add(node.module)
    return names


def test_client_layer_has_zero_batch_dependency():
    """Hard boundary: aibrix.batch.client must not import aibrix.batch.*.

    Enforced statically against actual import statements (not text), so the
    lift to aibrix/client stays a pure move.
    """
    import aibrix.batch.client as client_pkg

    offenders = []
    for mod in pkgutil.walk_packages(client_pkg.__path__, client_pkg.__name__ + "."):
        spec = mod.module_finder.find_spec(mod.name)
        if spec is None or spec.origin is None:
            continue
        with open(spec.origin, "r", encoding="utf-8") as fh:
            imported = _imported_modules(fh.read())
        # Intra-client imports (now absolute, e.g. aibrix.batch.client.channel)
        # move with the package, so they are not leaks. A leak is reaching into
        # the rest of batch (job_entity, job_driver, state, ...).
        leaks = [
            name
            for name in imported
            if name.startswith("aibrix.batch")
            and not name.startswith("aibrix.batch.client")
        ]
        if leaks:
            offenders.append((mod.name, leaks))
    assert offenders == [], f"client layer leaked aibrix.batch import: {offenders}"
