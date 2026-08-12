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

import pytest

from aibrix.batch.client import (
    DispatchEngine,
    EchoChannel,
    InferenceError,
    InferenceErrorCode,
    InferenceRequest,
    StaticEndpointSource,
)


class TestInferenceClientIntegration:
    """Integration tests for the consolidated inference client/engine layer."""

    @pytest.mark.asyncio
    async def test_echo_channel_echoes_payload(self):
        """EchoChannel (dry-run transport) returns the payload verbatim."""
        channel = EchoChannel()
        payload = {"model": "gpt-3.5-turbo", "messages": [{"role": "user"}]}
        result = await channel.send(InferenceRequest("/v1/chat/completions", payload))
        assert result == payload

    @pytest.mark.asyncio
    async def test_http_transport_error_becomes_inference_error(self):
        """An unreachable endpoint surfaces as InferenceError, not a raw httpx
        error -- the layer speaks only its own error type."""
        source = StaticEndpointSource("http://invalid-host.invalid:9999", timeout=0.5)
        engine = DispatchEngine(source, max_retries=0)
        with pytest.raises(InferenceError) as excinfo:
            await engine.send_one(InferenceRequest("/v1/chat/completions", {"x": 1}))
        assert excinfo.value.code == InferenceErrorCode.ALL_ENDPOINTS_FAILED
        await source.aclose()

    @pytest.mark.asyncio
    async def test_failover_recovers_across_channels(self):
        """The engine fails over to a healthy channel after a transient error,
        replacing the old same-endpoint retry."""

        class _FlakyChannel:
            def __init__(self, cid, fail_times):
                self._id = cid
                self._fail_times = fail_times
                self.calls = 0

            @property
            def id(self):
                return self._id

            async def send(self, request):
                self.calls += 1
                if self._fail_times > 0:
                    self._fail_times -= 1
                    raise InferenceError(InferenceErrorCode.TRANSPORT_ERROR, "boom")
                return {"ok": True, "by": self._id}

            async def aclose(self):
                return None

        from aibrix.batch.client import CapacitySignal

        good = _FlakyChannel("good", fail_times=0)
        bad = _FlakyChannel("bad", fail_times=1)

        class _Source:
            async def channels(self):
                return [bad, good]

            async def capacity(self):
                return CapacitySignal(count=2)

            async def wait_capacity_change(self, previous):
                raise AssertionError("unused")

            async def aclose(self):
                return None

        engine = DispatchEngine(_Source(), max_retries=2)
        result = await engine.send_one(InferenceRequest("/test", {}))
        assert result["ok"] is True
