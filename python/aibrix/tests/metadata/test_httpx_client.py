import contextlib
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from aibrix import envs
from aibrix.metadata.core import HTTPClient
from aibrix.metadata.core.httpx_client import HTTPXClientWrapper
from aibrix.metadata.core.metrics.sink import T


def test_http_client_factory_can_select_httpx_backend(monkeypatch):
    monkeypatch.setattr(envs, "BATCH_RUNTIME_HTTP_CLIENT", "httpx_client")

    wrapper = HTTPClient(client_id="test-httpx-backend")

    assert type(wrapper) is HTTPXClientWrapper


def test_http_client_factory_rejects_unknown_backend(monkeypatch):
    monkeypatch.setattr(envs, "BATCH_RUNTIME_HTTP_CLIENT", "unknown")

    with pytest.raises(ValueError, match="AIBRIX_BATCH_RUNTIME_HTTP_CLIENT"):
        HTTPClient(client_id="test-unknown-backend")


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_telemetry():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_httpx_client.success",
        )

    assert response.status_code == 200
    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 0
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_httpx_client.success"
    )
    assert snapshot.avg_latency_seconds is not None
    assert snapshot.p90_latency_seconds is not None
    assert sum(snapshot.latency_histogram) == 1

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_shares_telemetry_stats_by_purpose():
    first = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-1",
        purpose="shared-httpx-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-2",
        purpose="shared-httpx-purpose",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await first.request("GET", "https://example.com/first")
        await second.request("GET", "https://example.com/second")

    assert first._telemetry_stats is second._telemetry_stats
    assert first._telemetry_stats.started == 2
    assert second._telemetry_stats.completed == 2

    await first.stop()
    await second.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_flushes_shared_purpose_final_telemetry_once():
    first = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-final-1",
        purpose="shared-httpx-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    second = HTTPXClientWrapper(
        client_id="test-wrapper-purpose-final-2",
        purpose="shared-httpx-purpose-final",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await first.request("GET", "https://example.com/first")
        await second.request("GET", "https://example.com/second")

    gauges = []
    with patch(
        "aibrix.metadata.core.http_client.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    ):
        await first.stop()
        await second.stop()

    summary_gauges = [
        (value, tags)
        for name, value, tags in gauges
        if name == "metadata.http.client.telemetry.summary"
    ]
    assert len(summary_gauges) == 1
    assert T("purpose", "shared-httpx-purpose-final") in summary_gauges[0][1]
    assert T("flush", "true") in summary_gauges[0][1]
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata.http.client.telemetry.registered_clients"] == 0.0
    assert metric_values["metadata.http.client.telemetry.window_started"] == 2.0
    assert metric_values["metadata.http.client.telemetry.window_completed"] == 2.0


@pytest.mark.asyncio
async def test_httpx_wrapper_request_records_failure():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-failure",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(side_effect=httpx.ConnectError("boom")),
    ):
        with pytest.raises(httpx.ConnectError):
            await wrapper.request(
                "GET",
                "https://example.com/healthz",
                telemetry_call_site="tests.metadata.test_httpx_client.failure",
            )

    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 1
    assert wrapper._telemetry_stats.inflight == 0
    snapshot = wrapper._telemetry_stats.snapshot(reset_window=False)
    assert len(snapshot.call_sites) == 1
    assert (
        snapshot.call_sites[0].call_site == "tests.metadata.test_httpx_client.failure"
    )
    assert snapshot.call_sites[0].exception_types == {"ConnectError": 1}
    assert snapshot.exception_types == {"ConnectError": 1}

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_emits_debug_telemetry_by_call_site():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-debug",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        await wrapper.request(
            "GET",
            "https://example.com/healthz",
            telemetry_call_site="tests.metadata.test_httpx_client.debug",
        )

    with patch("aibrix.metadata.core.http_client.logger.debug") as mock_debug:
        wrapper._emit_telemetry(final=False, elapsed=1.0)

    mock_debug.assert_called_once()
    _, kwargs = mock_debug.call_args
    assert kwargs["client_id"] == "test-wrapper-debug"
    assert kwargs["purpose"] is None
    assert kwargs["call_site"] == "tests.metadata.test_httpx_client.debug"
    assert kwargs["failed"] == 0
    assert kwargs["exception_types"] == {}

    await wrapper.stop()


def test_httpx_wrapper_emits_telemetry_numeric_metrics(monkeypatch):
    gauges = []

    monkeypatch.setattr(
        "aibrix.metadata.core.http_client.Emitter.gauge",
        lambda name, value, *tags: gauges.append((name, value, tags)),
    )
    wrapper = HTTPXClientWrapper(
        client_id="test-httpx-telemetry",
        purpose="batch_runtime_http",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    wrapper._telemetry_stats.record_start(call_site="call.site")
    wrapper._telemetry_stats.record_completion(
        call_site="call.site",
        failed=False,
        latency_seconds=0.123,
        exception_type=None,
    )

    wrapper._emit_telemetry(final=False, elapsed=1.0)

    emitted_names = {name for name, _, _ in gauges}
    assert "metadata.http.client.telemetry.completed" in emitted_names
    assert "metadata.http.client.telemetry.completed_qps" in emitted_names
    assert "metadata.http.client.telemetry.summary" in emitted_names
    completed_tags = next(
        tags
        for name, _, tags in gauges
        if name == "metadata.http.client.telemetry.completed"
    )
    assert T("log_field", "completed") in completed_tags
    summary_gauges = [
        (value, tags)
        for name, value, tags in gauges
        if name == "metadata.http.client.telemetry.summary"
    ]
    assert summary_gauges
    assert summary_gauges[0][0] == 1.0
    assert T("flush", "true") in summary_gauges[0][1]
    assert T("event", "HTTP client telemetry summary") in summary_gauges[0][1]


@pytest.mark.asyncio
async def test_httpx_wrapper_context_manager_starts_and_stops():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-context",
        telemetry_interval_seconds=0,
    )

    async with wrapper as managed:
        assert isinstance(managed.async_client, httpx.AsyncClient)
        assert managed.is_closed is False

    assert wrapper.async_client is None
    assert wrapper.is_closed is True


@pytest.mark.asyncio
async def test_httpx_wrapper_cannot_be_reused_after_stop():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-stopped",
        telemetry_interval_seconds=0,
    )

    with patch(
        "httpx.AsyncClient.request",
        new=AsyncMock(return_value=httpx.Response(200, json={"ok": True})),
    ):
        response = await wrapper.request("GET", "https://example.com/healthz")

    assert response.status_code == 200

    await wrapper.stop()

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        await wrapper.request("GET", "https://example.com/healthz")

    with pytest.raises(RuntimeError, match="cannot be reused after stop"):
        _ = wrapper.headers


@pytest.mark.asyncio
async def test_httpx_wrapper_preserves_httpx_redirect_defaults():
    default_wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-redirect-default",
        telemetry_interval_seconds=0,
    )
    default_wrapper.start()
    assert default_wrapper.async_client is not None
    assert default_wrapper.async_client.follow_redirects is False

    follow_wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-redirect-follow",
        telemetry_interval_seconds=0,
        follow_redirects=True,
    )
    follow_wrapper.start()
    assert follow_wrapper.async_client is not None
    assert follow_wrapper.async_client.follow_redirects is True

    await default_wrapper.stop()
    await follow_wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_uses_httpx_timeout_config():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-timeout",
        timeout=httpx.Timeout(connect=10.0, read=20.0, write=30.0, pool=40.0),
        telemetry_interval_seconds=0,
    )

    wrapper.start()
    assert wrapper.async_client is not None
    timeout = wrapper.async_client.timeout

    assert timeout.connect == 10.0
    assert timeout.read == 20.0
    assert timeout.write == 30.0
    assert timeout.pool == 40.0

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_preserves_httpx_default_timeout():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-default-timeout",
        telemetry_interval_seconds=0,
    )

    try:
        wrapper.start()
        assert wrapper.async_client is not None
        assert wrapper.async_client.timeout.connect == 5.0
        assert wrapper.async_client.timeout.read == 5.0
        assert wrapper.async_client.timeout.write == 5.0
        assert wrapper.async_client.timeout.pool == 5.0
    finally:
        await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_request_timeout_none_disables_timeout():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-request-timeout-none",
        telemetry_interval_seconds=0,
    )

    captured_kwargs = {}

    async def fake_request(*args, **kwargs):
        del args
        captured_kwargs.update(kwargs)
        return httpx.Response(200)

    with patch("httpx.AsyncClient.request", fake_request):
        await wrapper.request("GET", "https://example.com", timeout=None)

    assert captured_kwargs["timeout"] is None
    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_stop_uses_actual_elapsed_time_for_final_telemetry():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-final-elapsed",
        telemetry_enabled=True,
        telemetry_interval_seconds=60,
    )

    wrapper.start()
    assert wrapper.async_client is not None

    wrapper._telemetry_stats.record_start(
        call_site="tests.metadata.test_httpx_client.final_elapsed"
    )
    wrapper._telemetry_stats.record_completion(
        failed=False,
        latency_seconds=0.1,
        call_site="tests.metadata.test_httpx_client.final_elapsed",
    )
    wrapper._last_telemetry_emitted_at = 100.0

    gauges = []
    with (
        patch.object(wrapper.async_client, "aclose", new=AsyncMock()),
        patch(
            "aibrix.metadata.core.http_client.asyncio.get_running_loop",
            return_value=MagicMock(time=MagicMock(return_value=100.25)),
        ),
        patch(
            "aibrix.metadata.core.http_client.Emitter.gauge",
            lambda name, value, *tags: gauges.append((name, value, tags)),
        ),
    ):
        await wrapper.stop()

    summary_gauge = next(
        tags
        for name, _, tags in gauges
        if name == "metadata.http.client.telemetry.summary"
    )
    assert T("final", "true") in summary_gauge
    assert T("purpose", "") in summary_gauge
    metric_values = {name: value for name, value, _ in gauges}
    assert metric_values["metadata.http.client.telemetry.started_qps"] == 4.0
    assert metric_values["metadata.http.client.telemetry.completed_qps"] == 4.0
    assert metric_values["metadata.http.client.telemetry.failed_qps"] == 0.0


@pytest.mark.asyncio
async def test_httpx_wrapper_stream_returns_aiter_bytes_compatible_response():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-stream",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
    )
    response = httpx.Response(200, content=b"abcdef")

    @contextlib.asynccontextmanager
    async def fake_stream(*args, **kwargs):
        del args, kwargs
        yield response

    with patch(
        "httpx.AsyncClient.stream",
        fake_stream,
    ):
        async with wrapper.stream(
            "GET",
            "https://example.com/stream",
            telemetry_call_site="tests.metadata.test_httpx_client.stream",
        ) as streamed:
            chunks = [chunk async for chunk in streamed.aiter_bytes(chunk_size=2)]

    assert chunks == [b"ab", b"cd", b"ef"]
    assert wrapper._telemetry_stats.started == 1
    assert wrapper._telemetry_stats.completed == 1
    assert wrapper._telemetry_stats.failed == 0

    await wrapper.stop()
