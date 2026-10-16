import asyncio
import io
import json
import unittest
from unittest.mock import AsyncMock, MagicMock

from client import client as client_module
from client import utils as utils_module
from client.utils import (
    AIBRIX_SESSION_KEY_HEADER,
    GATEWAY_MAX_SESSION_KEY_LEN,
    session_key_headers,
)


class SessionKeyHeadersTest(unittest.TestCase):
    def test_disabled_returns_none(self):
        self.assertIsNone(session_key_headers("session_abc", enabled=False))

    def test_missing_session_id_returns_none(self):
        self.assertIsNone(session_key_headers(None, enabled=True))

    def test_enabled_with_session_id(self):
        self.assertEqual(
            session_key_headers("session_abc", enabled=True),
            {AIBRIX_SESSION_KEY_HEADER: "session_abc"},
        )

    def test_integer_session_id_is_stringified(self):
        self.assertEqual(
            session_key_headers(42, enabled=True),
            {AIBRIX_SESSION_KEY_HEADER: "42"},
        )

    def test_over_long_session_key_warns_once_but_is_still_sent(self):
        utils_module._warned_session_keys.clear()
        long_key = "s" * (GATEWAY_MAX_SESSION_KEY_LEN + 1)
        with self.assertLogs(level="WARNING") as logs:
            headers = session_key_headers(long_key, enabled=True)
            session_key_headers(long_key, enabled=True)
        # Header is still sent unmodified; the client only surfaces the issue.
        self.assertEqual(headers, {AIBRIX_SESSION_KEY_HEADER: long_key})
        # Warned exactly once despite two calls with the same session id.
        self.assertEqual(len(logs.output), 1)
        self.assertIn(str(GATEWAY_MAX_SESSION_KEY_LEN), logs.output[0])

    def test_max_length_session_key_does_not_warn(self):
        utils_module._warned_session_keys.clear()
        max_key = "s" * GATEWAY_MAX_SESSION_KEY_LEN
        headers = session_key_headers(max_key, enabled=True)
        self.assertEqual(headers, {AIBRIX_SESSION_KEY_HEADER: max_key})
        self.assertEqual(utils_module._warned_session_keys, set())


class _FakeStream:
    """Minimal async stream standing in for an OpenAI streaming response."""

    def __aiter__(self):
        return self

    async def __anext__(self):
        raise StopAsyncIteration


def _fake_batch_response():
    response = MagicMock()
    response.response.headers = {}
    response.usage.prompt_tokens = 1
    response.usage.completion_tokens = 1
    response.usage.total_tokens = 2
    response.choices = [MagicMock()]
    response.choices[0].message.content = "ok"
    return response


class SendRequestHeaderInjectionTest(unittest.TestCase):
    def setUp(self):
        client_module.session_history.clear()

    def _run_streaming(self, request, session_key_header):
        create_mock = AsyncMock(return_value=_FakeStream())
        fake_client = MagicMock()
        fake_client.chat.completions.create = create_mock
        output_file = io.StringIO()
        result = asyncio.run(
            client_module.send_request_streaming(
                client=fake_client,
                model="test-model",
                max_output=8,
                request=request,
                output_file=output_file,
                request_id=0,
                session_id=request.get("session_id"),
                target_time=0,
                session_key_header=session_key_header,
            )
        )
        self.assertEqual(result["status"], "success")
        return create_mock

    def test_streaming_default_off_sends_no_extra_headers(self):
        create_mock = self._run_streaming(
            {"prompt": "p", "session_id": "session_abc"}, session_key_header=False
        )
        self.assertIsNone(create_mock.call_args.kwargs.get("extra_headers"))

    def test_streaming_enabled_sends_session_key_header(self):
        create_mock = self._run_streaming(
            {"prompt": "p", "session_id": "session_abc"}, session_key_header=True
        )
        self.assertEqual(
            create_mock.call_args.kwargs["extra_headers"],
            {AIBRIX_SESSION_KEY_HEADER: "session_abc"},
        )

    def test_streaming_enabled_without_session_id_sends_no_extra_headers(self):
        create_mock = self._run_streaming({"prompt": "p"}, session_key_header=True)
        self.assertIsNone(create_mock.call_args.kwargs.get("extra_headers"))

    def test_batch_enabled_sends_session_key_header(self):
        create_mock = AsyncMock(return_value=_fake_batch_response())
        fake_client = MagicMock()
        fake_client.chat.completions.create = create_mock
        output_file = io.StringIO()
        result = asyncio.run(
            client_module.send_request_batch(
                client=fake_client,
                model="test-model",
                max_output=8,
                request={"prompt": "p", "session_id": "session_abc"},
                output_file=output_file,
                request_id=0,
                session_id="session_abc",
                target_time=0,
                session_key_header=True,
            )
        )
        self.assertEqual(result["status"], "success")
        self.assertEqual(
            create_mock.call_args.kwargs["extra_headers"],
            {AIBRIX_SESSION_KEY_HEADER: "session_abc"},
        )

    def test_output_line_is_valid_json(self):
        create_mock = AsyncMock(return_value=_FakeStream())
        fake_client = MagicMock()
        fake_client.chat.completions.create = create_mock
        output_file = io.StringIO()
        asyncio.run(
            client_module.send_request_streaming(
                client=fake_client,
                model="test-model",
                max_output=8,
                request={"prompt": "p", "session_id": "session_abc"},
                output_file=output_file,
                request_id=0,
                session_id="session_abc",
                target_time=0,
                session_key_header=True,
            )
        )
        line = output_file.getvalue().strip().splitlines()[0]
        self.assertEqual(json.loads(line)["status"], "success")


if __name__ == "__main__":
    unittest.main()
