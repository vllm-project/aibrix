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

import os
import threading
from unittest.mock import patch

import pytest

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.metadata.core.kopf_operator import KopfOperatorWrapper


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_lifecycle():
    """Test the basic lifecycle of KopfOperatorWrapper."""
    # Use short timeouts for testing
    wrapper = KopfOperatorWrapper(
        namespace="test-namespace",
        startup_timeout=2.0,
        shutdown_timeout=2.0,
    )

    # Initial state should be not running
    assert not wrapper.is_running()

    status = wrapper.get_status()
    assert status["is_running"] is False
    assert status["namespace"] == "test-namespace"
    assert status["startup_timeout"] == 2.0
    assert status["shutdown_timeout"] == 2.0


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_start_stop_mock():
    """Test kopf operator wrapper start/stop with mocked kopf.run."""

    # Mock kopf.run to avoid actual operator startup
    with patch("aibrix.metadata.core.kopf_operator.kopf.run") as mock_kopf_run:
        # Mock kopf.run to signal ready immediately and then wait for stop
        def mock_run(**kwargs):
            ready_flag = kwargs.get("ready_flag")
            stop_flag = kwargs.get("stop_flag")

            # Signal ready immediately
            if ready_flag:
                ready_flag.set()

            # Wait for stop signal
            if stop_flag:
                stop_flag.wait()

        mock_kopf_run.side_effect = mock_run

        wrapper = KopfOperatorWrapper(
            namespace="test-namespace",
            startup_timeout=1.0,
            shutdown_timeout=1.0,
        )

        # Test start
        wrapper.start()

        # Should be running after start
        assert wrapper.is_running()

        status = wrapper.get_status()
        assert status["is_running"] is True
        assert "thread_name" in status
        assert "thread_id" in status
        assert status["thread_alive"] is True

        # Test stop
        wrapper.stop()

        # Should not be running after stop
        assert not wrapper.is_running()

        # Verify kopf.run was called with correct parameters
        mock_kopf_run.assert_called_once()
        call_args = mock_kopf_run.call_args
        assert call_args.kwargs["standalone"] is True
        assert call_args.kwargs["namespace"] == "test-namespace"
        assert call_args.kwargs["peering_name"] is None


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_startup_timeout():
    """Test that startup timeout works correctly."""

    # Mock kopf.run to never signal ready
    with patch("aibrix.metadata.core.kopf_operator.kopf.run") as mock_kopf_run:

        def mock_run(**kwargs):
            # Never signal ready, just wait
            stop_flag = kwargs.get("stop_flag")
            if stop_flag:
                stop_flag.wait()

        mock_kopf_run.side_effect = mock_run

        wrapper = KopfOperatorWrapper(
            namespace="test-namespace",
            startup_timeout=0.1,  # Very short timeout
            shutdown_timeout=0.1,
        )

        # Start should timeout and raise RuntimeError
        with pytest.raises(RuntimeError, match="did not start within 0.1s"):
            wrapper.start()

        # Should not be running after failed start
        assert not wrapper.is_running()


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_startup_error():
    """Test that startup errors are properly handled."""

    # Mock kopf.run to raise an exception
    with patch("aibrix.metadata.core.kopf_operator.kopf.run") as mock_kopf_run:

        def mock_run(**kwargs):
            ready_flag = kwargs.get("ready_flag")
            if ready_flag:
                ready_flag.set()  # Signal ready first
            raise RuntimeError("Mock startup error")

        mock_kopf_run.side_effect = mock_run

        wrapper = KopfOperatorWrapper(
            namespace="test-namespace",
            startup_timeout=1.0,
            shutdown_timeout=1.0,
        )

        # Start should re-raise the exception
        with pytest.raises(RuntimeError, match="Mock startup error"):
            wrapper.start()

        # Should not be running after failed start
        assert not wrapper.is_running()


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_double_start():
    """Test that calling start twice doesn't cause issues."""

    with patch("aibrix.metadata.core.kopf_operator.kopf.run") as mock_kopf_run:

        def mock_run(**kwargs):
            ready_flag = kwargs.get("ready_flag")
            stop_flag = kwargs.get("stop_flag")

            if ready_flag:
                ready_flag.set()
            if stop_flag:
                stop_flag.wait()

        mock_kopf_run.side_effect = mock_run

        wrapper = KopfOperatorWrapper(
            namespace="test-namespace",
            startup_timeout=1.0,
            shutdown_timeout=1.0,
        )

        # First start should work
        wrapper.start()
        assert wrapper.is_running()

        # Second start should be ignored (no exception)
        wrapper.start()
        assert wrapper.is_running()

        # Cleanup
        wrapper.stop()
        assert not wrapper.is_running()


@pytest.mark.asyncio
async def test_kopf_operator_wrapper_stop_not_running():
    """Test that calling stop when not running doesn't cause issues."""

    wrapper = KopfOperatorWrapper()

    # Stop should work even when not running
    wrapper.stop()
    assert not wrapper.is_running()


def test_kopf_operator_wrapper_threading():
    """Test that the kopf operator runs in a separate thread."""

    main_thread_id = threading.get_ident()
    operator_thread_id = None

    with patch("aibrix.metadata.core.kopf_operator.kopf.run") as mock_kopf_run:

        def mock_run(**kwargs):
            nonlocal operator_thread_id
            operator_thread_id = threading.get_ident()

            ready_flag = kwargs.get("ready_flag")
            stop_flag = kwargs.get("stop_flag")

            if ready_flag:
                ready_flag.set()
            if stop_flag:
                stop_flag.wait()

        mock_kopf_run.side_effect = mock_run

        wrapper = KopfOperatorWrapper(startup_timeout=1.0, shutdown_timeout=1.0)

        wrapper.start()

        # Verify kopf runs in a different thread
        assert operator_thread_id is not None
        assert operator_thread_id != main_thread_id

        # Cleanup
        wrapper.stop()
