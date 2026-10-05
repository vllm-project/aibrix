import asyncio
import io
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock

import httpx

from client import client as benchmark_client


class InterruptedStream:
    def __init__(self):
        self._yielded = False
        self.response = SimpleNamespace(
            headers={"target-pod": "pod-a", "request-id": "request-a"}
        )

    def __aiter__(self):
        return self

    async def __anext__(self):
        if self._yielded:
            raise httpx.ReadTimeout("stream timed out")

        self._yielded = True
        return SimpleNamespace(
            choices=[SimpleNamespace(delta=SimpleNamespace(content="partial"))],
            usage=None,
        )


class SendRequestStreamingTest(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.original_completed_sessions = benchmark_client.completed_sessions
        self.original_session_history = dict(benchmark_client.session_history)
        self.original_pending_requests = dict(
            benchmark_client.pending_sessioned_requests
        )
        benchmark_client.completed_sessions = asyncio.Queue()
        benchmark_client.session_history.clear()
        benchmark_client.pending_sessioned_requests.clear()

    def tearDown(self):
        benchmark_client.completed_sessions = self.original_completed_sessions
        benchmark_client.session_history.clear()
        benchmark_client.session_history.update(self.original_session_history)
        benchmark_client.pending_sessioned_requests.clear()
        benchmark_client.pending_sessioned_requests.update(
            self.original_pending_requests
        )

    async def test_mid_stream_timeout_is_recorded_once_as_error(self):
        create = AsyncMock(return_value=InterruptedStream())
        fake_client = SimpleNamespace(
            chat=SimpleNamespace(
                completions=SimpleNamespace(create=create),
            )
        )
        output_file = io.StringIO()

        result = await benchmark_client.send_request_streaming(
            client=fake_client,
            model="test-model",
            max_output=8,
            request={"prompt": "hello", "session_id": "session-1"},
            output_file=output_file,
            request_id=7,
            session_id="session-1",
            target_time=0,
        )

        self.assertEqual(result["status"], "error")
        self.assertEqual(result["error_type"], "ReadTimeout")
        self.assertEqual(result["error_message"], "stream timed out")

        records = [json.loads(line) for line in output_file.getvalue().splitlines()]
        self.assertEqual(records, [result])

        self.assertEqual(
            benchmark_client.completed_sessions.get_nowait(), "session-1"
        )
        self.assertTrue(benchmark_client.completed_sessions.empty())
        create.assert_awaited_once()


if __name__ == "__main__":
    unittest.main()
