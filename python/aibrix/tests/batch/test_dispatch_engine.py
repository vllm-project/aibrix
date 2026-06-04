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
from dataclasses import dataclass

import httpx
import pytest

from aibrix.batch.client import (
    CapacitySignal,
    ConcurrencyOutcome,
    DispatchEngine,
    DispatchStats,
    HttpChannel,
    InferenceError,
    InferenceErrorCode,
    InferenceRequest,
    NoopEndpointSource,
    RetryConfig,
)


class _FakeChannel:
    def __init__(self, cid, *, fail_times=0):
        self._id = cid
        self._fail_times = fail_times
        self.calls = []
        self.closed = 0

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
        self.closed += 1


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


class _ErrorChannel(_FakeChannel):
    def __init__(self, cid, error):
        super().__init__(cid)
        self._error = error

    async def send(self, request):
        self.calls.append(request.ref)
        raise self._error


@dataclass(frozen=True)
class _DiscoveredEndpoint:
    base_url: str


@dataclass(frozen=True)
class _DiscoverySnapshot:
    version: int
    endpoints: list[_DiscoveredEndpoint]


class _SequenceDiscovery:
    def __init__(self, snapshots):
        self._snapshots = list(snapshots)
        self.calls = 0

    async def discover_model_endpoints(self, served_model_name, service_id=None):
        self.calls += 1
        index = min(self.calls - 1, len(self._snapshots) - 1)
        return self._snapshots[index]


class _FailingThenSequenceDiscovery:
    def __init__(self, first_snapshot, later_snapshots):
        self._first_snapshot = first_snapshot
        self._later = list(later_snapshots)
        self.calls = 0

    async def discover_model_endpoints(self, served_model_name, service_id=None):
        self.calls += 1
        if self.calls == 1:
            return self._first_snapshot
        if self.calls == 2:
            raise RuntimeError("discovery unavailable")
        index = min(self.calls - 3, len(self._later) - 1)
        return self._later[index]


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
async def test_http_channel_preserves_status_body_and_retryable_hint():
    async def handler(request):
        return httpx.Response(400, json={"error": "bad request"})

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    channel = HttpChannel("http://engine", client=client)
    try:
        with pytest.raises(InferenceError) as excinfo:
            await channel.send(InferenceRequest("/v1/x", {}))
    finally:
        await channel.aclose()
        await client.aclose()

    err = excinfo.value
    assert err.code == InferenceErrorCode.HTTP_ERROR
    assert err.status_code == 400
    assert err.response_body == {"error": "bad request"}
    assert err.retryable is False


@pytest.mark.asyncio
async def test_http_channel_truncates_large_error_body():
    body = "x" * 5000

    async def handler(request):
        return httpx.Response(503, text=body)

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    channel = HttpChannel("http://engine", client=client)
    try:
        with pytest.raises(InferenceError) as excinfo:
            await channel.send(InferenceRequest("/v1/x", {}))
    finally:
        await channel.aclose()
        await client.aclose()

    err = excinfo.value
    assert err.status_code == 503
    assert err.retryable is True
    assert isinstance(err.response_body, str)
    assert len(err.response_body) < len(body)
    assert err.response_body.endswith("...<truncated>")


def test_retry_config_rejects_invalid_values():
    with pytest.raises(ValueError):
        RetryConfig(max_retries=-1)


@pytest.mark.asyncio
async def test_non_retryable_error_is_not_retried():
    bad = _ErrorChannel(
        "bad",
        InferenceError(
            InferenceErrorCode.HTTP_ERROR,
            "bad request",
            status_code=400,
            retryable=False,
        ),
    )
    good = _FakeChannel("good")
    results = await _drain(_FakeSource([bad, good]), _reqs(1), max_retries=2)
    _, resp, err = results[0]
    assert resp is None
    assert err.code == InferenceErrorCode.HTTP_ERROR
    assert err.status_code == 400
    assert len(bad.calls) == 1
    assert len(good.calls) == 0


@pytest.mark.asyncio
async def test_retryable_http_error_fails_over_to_next_channel():
    bad = _ErrorChannel(
        "bad",
        InferenceError(
            InferenceErrorCode.HTTP_ERROR,
            "service unavailable",
            status_code=503,
            retryable=True,
        ),
    )
    good = _FakeChannel("good")
    results = await _drain(_FakeSource([bad, good]), _reqs(1), max_retries=2)
    _, resp, err = results[0]
    assert err is None
    assert resp["by"] == "good"
    assert len(bad.calls) == 1
    assert len(good.calls) == 1


@pytest.mark.asyncio
async def test_retry_refreshes_channels_to_recover_from_endpoint_churn():
    bad = _ErrorChannel(
        "bad",
        InferenceError(
            InferenceErrorCode.TRANSPORT_ERROR,
            "pod disappeared",
            retryable=True,
        ),
    )
    good = _FakeChannel("good")

    class _ChurnSource:
        def __init__(self):
            self.calls = 0

        async def channels(self):
            self.calls += 1
            return [bad] if self.calls == 1 else [good]

        async def capacity(self):
            return CapacitySignal(count=1)

        async def wait_capacity_change(self, previous):
            raise AssertionError("unused")

        async def aclose(self):
            return None

    source = _ChurnSource()
    engine = DispatchEngine(source, max_retries=1)

    result = await engine.send_one(InferenceRequest("/test", {}, ref="r1"))

    assert result["by"] == "good"
    assert source.calls == 2
    assert bad.calls == ["r1"]
    assert good.calls == ["r1"]


@pytest.mark.asyncio
async def test_no_endpoint_retries_until_source_recovers():
    good = _FakeChannel("good")

    class _RecoveringSource:
        def __init__(self):
            self.calls = 0
            self.refreshes = 0

        async def channels(self):
            self.calls += 1
            return [] if self.calls < 3 else [good]

        async def capacity(self):
            return CapacitySignal(count=1)

        async def wait_capacity_change(self, previous):
            raise AssertionError("unused")

        async def refresh(self):
            self.refreshes += 1

        async def aclose(self):
            return None

    source = _RecoveringSource()
    engine = DispatchEngine(
        source,
        retry=RetryConfig(max_retries=2, base_delay_seconds=0.001),
    )

    result = await engine.send_one(InferenceRequest("/test", {}, ref="r1"))

    assert result["by"] == "good"
    assert source.calls == 3
    assert source.refreshes == 2


@pytest.mark.asyncio
async def test_no_endpoint_retry_budget_is_separate_from_send_retry_budget():
    good = _FakeChannel("good")

    class _RecoveringSource:
        def __init__(self):
            self.calls = 0

        async def channels(self):
            self.calls += 1
            return [] if self.calls < 4 else [good]

        async def capacity(self):
            return CapacitySignal(count=1)

        async def wait_capacity_change(self, previous):
            raise AssertionError("unused")

        async def refresh(self):
            return None

        async def aclose(self):
            return None

    source = _RecoveringSource()
    engine = DispatchEngine(
        source,
        retry=RetryConfig(
            max_retries=0,
            base_delay_seconds=0.001,
            no_endpoint_max_retries=3,
        ),
    )

    result = await engine.send_one(InferenceRequest("/test", {}, ref="r1"))

    assert result["by"] == "good"
    assert source.calls == 4


@pytest.mark.asyncio
async def test_retryable_error_reports_failed_channel_before_retry():
    bad = _ErrorChannel(
        "bad",
        InferenceError(
            InferenceErrorCode.TRANSPORT_ERROR,
            "pod disappeared",
            retryable=True,
        ),
    )
    good = _FakeChannel("good")

    class _SelfHealingSource:
        def __init__(self):
            self._channels = [bad, good]
            self.reported = []

        async def channels(self):
            return list(self._channels)

        async def capacity(self):
            return CapacitySignal(count=len(self._channels))

        async def wait_capacity_change(self, previous):
            raise AssertionError("unused")

        async def report_channel_error(self, channel_id, error):
            self.reported.append((channel_id, error.code))
            self._channels = [
                channel for channel in self._channels if channel.id != channel_id
            ]

        async def aclose(self):
            return None

    source = _SelfHealingSource()
    engine = DispatchEngine(source, max_retries=1)

    result = await engine.send_one(InferenceRequest("/test", {}, ref="r1"))

    assert result["by"] == "good"
    assert source.reported == [("bad", InferenceErrorCode.TRANSPORT_ERROR)]
    assert bad.calls == ["r1"]
    assert good.calls == ["r1"]


@pytest.mark.asyncio
async def test_source_is_resolved_before_each_request():
    a = _FakeChannel("a")
    b = _FakeChannel("b")

    class _RotatingSource:
        def __init__(self):
            self.calls = 0

        async def channels(self):
            self.calls += 1
            return [a] if self.calls == 1 else [b]

        async def capacity(self):
            return CapacitySignal(count=1)

        async def wait_capacity_change(self, previous):
            raise AssertionError("unused")

        async def aclose(self):
            return None

    source = _RotatingSource()
    results = await _drain(source, _reqs(2), max_retries=0)

    assert [resp["by"] for _, resp, err in results if err is None] == ["a", "b"]
    assert source.calls == 2
    assert a.calls == [0]
    assert b.calls == [1]


@pytest.mark.asyncio
async def test_retry_reports_only_one_final_result():
    bad = _FakeChannel("bad", fail_times=1)
    good = _FakeChannel("good")
    engine = DispatchEngine(_FakeSource([bad, good]), max_retries=1)
    results = []

    async def feed():
        yield InferenceRequest("/test", {}, ref="r1")

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    await engine.run(feed(), on_result)

    assert len(results) == 1
    assert results[0][0] == "r1"
    assert results[0][1]["by"] == "good"
    assert results[0][2] is None


@pytest.mark.asyncio
async def test_all_endpoints_failed_continues_with_later_requests():
    bad = _FakeChannel("bad", fail_times=99)
    results = await _drain(_FakeSource([bad]), _reqs(3), max_retries=0)

    assert [ref for ref, _, _ in results] == [0, 1, 2]
    assert all(resp is None for _, resp, _ in results)
    assert all(
        err.code == InferenceErrorCode.ALL_ENDPOINTS_FAILED for _, _, err in results
    )


@pytest.mark.asyncio
async def test_qps_gate_limits_request_start_rate():
    starts = []

    class _TimedChannel(_FakeChannel):
        async def send(self, request):
            starts.append(asyncio.get_running_loop().time())
            return {"ok": True}

    engine = DispatchEngine(_FakeSource([_TimedChannel("s")]), max_retries=0)
    results = []

    async def feed():
        for request in _reqs(3):
            yield request

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    await engine.run(feed(), on_result, max_concurrency=3, qps=20)

    assert len(results) == 3
    assert len(starts) == 3
    spacings = [right - left for left, right in zip(starts, starts[1:])]
    assert all(spacing >= 0.04 for spacing in spacings)


@pytest.mark.asyncio
async def test_on_result_failure_stops_pulling_new_requests_after_drain():
    pulled = []
    started = []
    both_started = asyncio.Event()
    release = asyncio.Event()

    class _BlockingChannel(_FakeChannel):
        async def send(self, request):
            started.append(request.ref)
            if len(started) == 2:
                both_started.set()
            await release.wait()
            return {"ok": True}

    async def feed():
        for request in _reqs(5):
            pulled.append(request.ref)
            yield request

    async def on_result(req, resp, err):
        if req.ref == 0:
            raise RuntimeError("result sink failed")

    engine = DispatchEngine(_FakeSource([_BlockingChannel("s")]), max_retries=0)
    task = asyncio.create_task(engine.run(feed(), on_result, max_concurrency=2))
    await both_started.wait()
    await asyncio.sleep(0)
    assert pulled == [0, 1]

    release.set()
    with pytest.raises(RuntimeError, match="result sink failed"):
        await task
    assert pulled == [0, 1]


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
async def test_run_pulls_requests_only_after_admission():
    pulled = []
    results = []
    first_started = asyncio.Event()
    release_first = asyncio.Event()

    class _BlockingChannel(_FakeChannel):
        async def send(self, request):
            first_started.set()
            await release_first.wait()
            return {"ok": True}

    async def feed():
        for request in _reqs(2):
            pulled.append(request.ref)
            yield request

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    engine = DispatchEngine(_FakeSource([_BlockingChannel("s")]), max_retries=0)
    task = asyncio.create_task(engine.run(feed(), on_result, max_concurrency=1))
    await first_started.wait()
    await asyncio.sleep(0)

    assert pulled == [0]

    release_first.set()
    await task
    assert pulled == [0, 1]
    assert len(results) == 2


@pytest.mark.asyncio
async def test_run_stops_admitting_when_adaptive_limit_drops_below_inflight():
    pulled = []
    results = []
    slow_started = asyncio.Event()
    release_slow = asyncio.Event()

    class _DropToOneController:
        def __init__(self):
            self._limit = 2

        def limit(self):
            return self._limit

        def on_complete(self, outcome: ConcurrencyOutcome):
            self._limit = 1

    class _MixedChannel(_FakeChannel):
        async def send(self, request):
            if request.ref == 0:
                slow_started.set()
                await release_slow.wait()
            return {"ok": True}

    async def feed():
        for request in _reqs(3):
            pulled.append(request.ref)
            yield request

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    engine = DispatchEngine(_FakeSource([_MixedChannel("s")]), max_retries=0)
    task = asyncio.create_task(
        engine.run(feed(), on_result, concurrency_controller=_DropToOneController())
    )
    await slow_started.wait()
    await asyncio.sleep(0.01)

    assert pulled == [0, 1]

    release_slow.set()
    await task
    assert pulled == [0, 1, 2]
    assert len(results) == 3


@pytest.mark.asyncio
async def test_adaptive_concurrency_can_grow_above_source_capacity():
    inflight = 0
    peak = 0

    class _SlowChannel(_FakeChannel):
        async def send(self, request):
            nonlocal inflight, peak
            inflight += 1
            peak = max(peak, inflight)
            await asyncio.sleep(0.01)
            inflight -= 1
            return {"usage": {"completion_tokens": 10}}

    source = _FakeSource([_SlowChannel("s")])
    engine = DispatchEngine(source, max_retries=0)
    results = []

    async def feed():
        for request in _reqs(24):
            yield request

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    await engine.run(
        feed(),
        on_result,
        adaptive_concurrency=True,
        adaptive_max_factor=4,
    )

    assert len(results) == 24
    assert peak > 1


@pytest.mark.asyncio
async def test_dispatch_stats_tracks_qps_inputs_and_limit():
    release = asyncio.Event()
    both_started = asyncio.Event()
    starts = 0

    class _BlockingChannel(_FakeChannel):
        async def send(self, request):
            nonlocal starts
            starts += 1
            if starts == 2:
                both_started.set()
            await release.wait()
            return {"usage": {"completion_tokens": 10}}

    stats = DispatchStats()
    engine = DispatchEngine(_FakeSource([_BlockingChannel("s")]), max_retries=0)
    results = []

    async def feed():
        for request in _reqs(2):
            yield request

    async def on_result(req, resp, err):
        results.append((req.ref, resp, err))

    task = asyncio.create_task(
        engine.run(feed(), on_result, max_concurrency=2, stats=stats)
    )
    await both_started.wait()

    running = stats.snapshot()
    assert running.started == 2
    assert running.completed == 0
    assert running.inflight == 2
    assert running.limit == 2

    release.set()
    await task

    final = stats.snapshot(reset_window=True)
    assert final.started == 2
    assert final.completed == 2
    assert final.failed == 0
    assert final.inflight == 0
    assert final.window_started == 2
    assert final.window_completed == 2
    assert final.avg_latency_seconds is not None


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


@pytest.mark.asyncio
async def test_port_forward_source_restarts_when_process_exits(monkeypatch):
    import io as _io

    from aibrix.batch.client.sources import kubernetes as k8s_src

    processes = []

    class _FakeProcess:
        def __init__(self, assigned_port):
            self.stdout = _io.StringIO(
                f"Forwarding from 127.0.0.1:{assigned_port} -> 8000\n"
            )
            self.exited = False
            self.terminated = False

        def poll(self):
            return 1 if self.exited else None

        def terminate(self):
            self.terminated = True
            self.exited = True

        def wait(self, timeout=None):
            return 0

    def _popen(command, **kwargs):
        process = _FakeProcess(39123 + len(processes))
        processes.append(process)
        return process

    monkeypatch.setattr(k8s_src.subprocess, "Popen", _popen)

    source = k8s_src.PortForwardEndpointSource("default", "svc", 8000)
    first = await source.channels()
    assert first[0].id == "http://127.0.0.1:39123"

    processes[0].exited = True
    second = await source.channels()

    assert second[0].id == "http://127.0.0.1:39124"
    assert len(processes) == 2
    await source.aclose()


@pytest.mark.asyncio
async def test_endpoint_slice_discovery_returns_ready_endpoint_urls():
    from types import SimpleNamespace

    from aibrix.batch.client.sources import kubernetes as k8s_src

    class _FakeDiscoveryV1Api:
        def __init__(self):
            self.calls = []

        def list_namespaced_endpoint_slice(self, namespace, label_selector):
            self.calls.append((namespace, label_selector))
            return SimpleNamespace(
                items=[
                    SimpleNamespace(
                        metadata=SimpleNamespace(
                            name="slice-a", resource_version="101"
                        ),
                        ports=[SimpleNamespace(port=9000)],
                        endpoints=[
                            SimpleNamespace(
                                addresses=["10.244.0.11"],
                                conditions=SimpleNamespace(ready=True),
                            ),
                            SimpleNamespace(
                                addresses=["10.244.0.12"],
                                conditions=SimpleNamespace(ready=False),
                            ),
                            SimpleNamespace(
                                addresses=["fd00::12"],
                                conditions=SimpleNamespace(ready=None),
                            ),
                        ],
                    )
                ]
            )

    discovery = k8s_src.K8sEndpointSliceDiscovery(
        _FakeDiscoveryV1Api(),
        namespace="default",
        service_name="svc",
        service_port=8000,
    )

    snapshot = await discovery.discover_model_endpoints("unused-model")
    second = await discovery.discover_model_endpoints("unused-model")

    assert [endpoint.base_url for endpoint in snapshot.endpoints] == [
        "http://10.244.0.11:9000",
        "http://[fd00::12]:9000",
    ]
    assert second.version == snapshot.version


@pytest.mark.asyncio
async def test_discovery_source_uses_cache_until_refresh_interval():
    from aibrix.batch.client.sources import DiscoveryEndpointSource

    now = 0.0
    discovery = _SequenceDiscovery(
        [
            _DiscoverySnapshot(1, [_DiscoveredEndpoint("http://one")]),
            _DiscoverySnapshot(
                2,
                [
                    _DiscoveredEndpoint("http://one"),
                    _DiscoveredEndpoint("http://two"),
                ],
            ),
        ]
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
        clock=lambda: now,
    )
    try:
        assert [channel.id for channel in await source.channels()] == ["http://one"]
        assert [channel.id for channel in await source.channels()] == ["http://one"]
        assert discovery.calls == 1

        now = 21.0
        assert [channel.id for channel in await source.channels()] == [
            "http://one",
            "http://two",
        ]
        assert discovery.calls == 2
    finally:
        await source.aclose()


@pytest.mark.asyncio
async def test_discovery_source_refreshes_and_removes_failed_channel():
    from aibrix.batch.client.sources import DiscoveryEndpointSource

    now = 0.0
    discovery = _SequenceDiscovery(
        [
            _DiscoverySnapshot(
                1,
                [
                    _DiscoveredEndpoint("http://bad"),
                    _DiscoveredEndpoint("http://good"),
                ],
            ),
            _DiscoverySnapshot(
                2,
                [
                    _DiscoveredEndpoint("http://bad"),
                    _DiscoveredEndpoint("http://good"),
                    _DiscoveredEndpoint("http://new"),
                ],
            ),
        ]
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
        clock=lambda: now,
    )
    try:
        assert [channel.id for channel in await source.channels()] == [
            "http://bad",
            "http://good",
        ]

        await source.report_channel_error(
            "http://bad",
            InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                "bad endpoint",
                retryable=True,
            ),
        )

        assert [channel.id for channel in await source.channels()] == [
            "http://good",
            "http://new",
        ]
        assert discovery.calls == 2
    finally:
        await source.aclose()


@pytest.mark.asyncio
async def test_discovery_source_removes_failed_channel_even_if_refresh_fails():
    from aibrix.batch.client.sources import DiscoveryEndpointSource

    discovery = _FailingThenSequenceDiscovery(
        _DiscoverySnapshot(
            1,
            [
                _DiscoveredEndpoint("http://bad"),
                _DiscoveredEndpoint("http://good"),
            ],
        ),
        [
            _DiscoverySnapshot(2, [_DiscoveredEndpoint("http://good")]),
        ],
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
    )
    try:
        assert [channel.id for channel in await source.channels()] == [
            "http://bad",
            "http://good",
        ]

        await source.report_channel_error(
            "http://bad",
            InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                "spot instance reclaimed",
                retryable=True,
            ),
        )

        assert [channel.id for channel in await source.channels()] == ["http://good"]
        assert discovery.calls == 2
    finally:
        await source.aclose()


@pytest.mark.asyncio
async def test_discovery_source_defers_closing_removed_channels(monkeypatch):
    from aibrix.batch.client.sources import DiscoveryEndpointSource
    from aibrix.batch.client.sources import discovery as discovery_module

    created = []

    def make_channel(url, timeout):
        channel = _FakeChannel(url)
        created.append(channel)
        return channel

    monkeypatch.setattr(discovery_module, "HttpChannel", make_channel)
    discovery = _SequenceDiscovery(
        [
            _DiscoverySnapshot(
                1,
                [
                    _DiscoveredEndpoint("http://bad"),
                    _DiscoveredEndpoint("http://good"),
                ],
            ),
            _DiscoverySnapshot(2, [_DiscoveredEndpoint("http://good")]),
        ]
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
    )
    try:
        assert [channel.id for channel in await source.channels()] == [
            "http://bad",
            "http://good",
        ]
        bad, good = created

        await source.report_channel_error(
            "http://bad",
            InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                "bad endpoint",
                retryable=True,
            ),
        )

        assert [channel.id for channel in await source.channels()] == ["http://good"]
        assert bad.closed == 0
        assert good.closed == 0
    finally:
        await source.aclose()

    assert bad.closed == 1
    assert good.closed == 1


@pytest.mark.asyncio
async def test_discovery_source_scale_down_removes_gone_endpoint_after_ttl():
    from aibrix.batch.client.sources import DiscoveryEndpointSource

    now = 0.0
    discovery = _SequenceDiscovery(
        [
            _DiscoverySnapshot(
                1,
                [
                    _DiscoveredEndpoint("http://one"),
                    _DiscoveredEndpoint("http://two"),
                    _DiscoveredEndpoint("http://three"),
                ],
            ),
            _DiscoverySnapshot(
                2,
                [
                    _DiscoveredEndpoint("http://one"),
                    _DiscoveredEndpoint("http://two"),
                ],
            ),
        ]
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
        clock=lambda: now,
    )
    try:
        assert [channel.id for channel in await source.channels()] == [
            "http://one",
            "http://two",
            "http://three",
        ]

        now = 21.0
        assert [channel.id for channel in await source.channels()] == [
            "http://one",
            "http://two",
        ]
        assert (await source.capacity()).count == 2
    finally:
        await source.aclose()


@pytest.mark.asyncio
async def test_discovery_source_scale_up_waits_until_ttl_refresh():
    from aibrix.batch.client.sources import DiscoveryEndpointSource

    now = 0.0
    discovery = _SequenceDiscovery(
        [
            _DiscoverySnapshot(1, [_DiscoveredEndpoint("http://one")]),
            _DiscoverySnapshot(
                2,
                [
                    _DiscoveredEndpoint("http://one"),
                    _DiscoveredEndpoint("http://two"),
                ],
            ),
        ]
    )
    source = DiscoveryEndpointSource(
        discovery,
        "model",
        refresh_interval_seconds=20.0,
        clock=lambda: now,
    )
    try:
        assert [channel.id for channel in await source.channels()] == ["http://one"]

        now = 19.0
        assert [channel.id for channel in await source.channels()] == ["http://one"]
        assert discovery.calls == 1

        now = 20.0
        assert [channel.id for channel in await source.channels()] == [
            "http://one",
            "http://two",
        ]
        assert discovery.calls == 2
    finally:
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
