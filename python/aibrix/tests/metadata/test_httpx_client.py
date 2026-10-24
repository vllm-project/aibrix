import asyncio
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from aibrix.metadata.core import HTTPXClientWrapper


@pytest.mark.asyncio
@pytest.mark.parametrize("stream", [False, True], ids=["request", "stream"])
@pytest.mark.parametrize("fail_first", [False, True], ids=["success", "failure"])
async def test_httpx_call_site_inflight_survives_telemetry_windows(stream, fail_first):
    entered = [asyncio.Event(), asyncio.Event()]
    release = [asyncio.Event(), asyncio.Event()]

    async def handler(request):
        index = int(request.url.path.rsplit("/", 1)[-1])
        entered[index].set()
        await release[index].wait()
        if index == 0 and fail_first:
            raise httpx.ConnectError("expected failure", request=request)
        return httpx.Response(200, json={"ok": True})

    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-window",
        telemetry_enabled=True,
        telemetry_interval_seconds=0,
        transport=httpx.MockTransport(handler),
    )
    call_site = "tests.metadata.test_httpx_client.window"

    async def send(index):
        url = f"https://example.test/{index}"
        if stream:
            async with wrapper.stream("GET", url, telemetry_call_site=call_site):
                pass
        else:
            await wrapper.get(url, telemetry_call_site=call_site)

    tasks = [asyncio.create_task(send(index)) for index in range(2)]
    try:
        await asyncio.wait_for(
            asyncio.gather(*(event.wait() for event in entered)), timeout=1
        )
        with patch("aibrix.metadata.core.httpx_client.logger.debug") as mock_debug:
            wrapper._emit_telemetry(final=False, elapsed=1.0)
            release[0].set()
            if fail_first:
                with pytest.raises(httpx.ConnectError, match="expected failure"):
                    await tasks[0]
            else:
                await tasks[0]
            assert wrapper._telemetry_stats.inflight == 1
            wrapper._emit_telemetry(final=False, elapsed=1.0)

            release[1].set()
            await tasks[1]
            wrapper._emit_telemetry(final=False, elapsed=1.0)
            wrapper._emit_telemetry(final=False, elapsed=1.0)

            sites = [
                call.kwargs
                for call in mock_debug.call_args_list
                if call.args == ("HTTPX client telemetry by call site",)
            ]
            assert [site["call_site"] for site in sites] == [call_site] * 3
            assert [site["inflight"] for site in sites] == [2, 1, 0]
            assert [site["started"] for site in sites] == [2, 0, 0]
            assert [site["completed"] for site in sites] == [0, 1, 1]
            assert [site["failed"] for site in sites] == [0, int(fail_first), 0]
            assert sites[2]["exception_types"] == {}
    finally:
        for event in release:
            event.set()
        await asyncio.gather(*tasks, return_exceptions=True)
        await wrapper.stop()


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
    assert snapshot.p50_latency_seconds is not None
    assert snapshot.p95_latency_seconds is not None
    assert sum(snapshot.latency_histogram) == 1

    await wrapper.stop()


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

    with patch("aibrix.metadata.core.httpx_client.logger.debug") as mock_debug:
        wrapper._emit_telemetry(final=False, elapsed=1.0)

    mock_debug.assert_called_once()
    _, kwargs = mock_debug.call_args
    assert kwargs["client_id"] == "test-wrapper-debug"
    assert kwargs["call_site"] == "tests.metadata.test_httpx_client.debug"
    assert kwargs["failed"] == 0
    assert kwargs["exception_types"] == {}

    await wrapper.stop()


@pytest.mark.asyncio
async def test_httpx_wrapper_context_manager_starts_and_stops():
    wrapper = HTTPXClientWrapper(
        client_id="test-wrapper-context",
        telemetry_interval_seconds=0,
    )

    async with wrapper as managed:
        assert managed.async_client is not None
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

    with (
        patch.object(wrapper.async_client, "aclose", new=AsyncMock()),
        patch(
            "aibrix.metadata.core.httpx_client.asyncio.get_running_loop",
            return_value=MagicMock(time=MagicMock(return_value=100.25)),
        ),
        patch("aibrix.metadata.core.httpx_client.logger.info") as mock_info,
    ):
        await wrapper.stop()

    telemetry_call = next(
        call
        for call in mock_info.call_args_list
        if call.args and call.args[0] == "HTTPX client telemetry summary"
    )
    assert telemetry_call.kwargs["final"] is True
    assert telemetry_call.kwargs["started_qps"] == 4.0
    assert telemetry_call.kwargs["completed_qps"] == 4.0
    assert telemetry_call.kwargs["failed_qps"] == 0.0
