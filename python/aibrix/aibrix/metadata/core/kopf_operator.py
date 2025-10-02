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

import threading
from typing import Optional

import kopf

from aibrix.logger import init_logger

logger = init_logger(__name__)


class KopfOperatorWrapper:
    """
    A wrapper class to run kopf operator in a separate thread, managing its lifecycle
    and integrating it with FastAPI application startup and shutdown.

    This class follows the same pattern as HTTPXClientWrapper, providing lifecycle
    management methods that can be called from FastAPI lifespan hooks.
    """

    def __init__(
        self,
        namespace: str = "default",
        startup_timeout: float = 30.0,
        shutdown_timeout: float = 10.0,
        standalone: bool = True,
        peering_name: Optional[str] = None,
    ) -> None:
        """
        Initialize the kopf operator wrapper.

        Args:
            namespace: Kubernetes namespace to monitor (default: "default")
            startup_timeout: Maximum time to wait for operator startup (seconds)
            shutdown_timeout: Maximum time to wait for operator shutdown (seconds)
            standalone: Whether to run kopf in standalone mode
            peering_name: Used for high availability
        """
        self.namespace = namespace
        self.startup_timeout = startup_timeout
        self.shutdown_timeout = shutdown_timeout
        self.standalone = standalone
        self.peering_name = peering_name

        # Threading coordination
        self._operator_thread: Optional[threading.Thread] = None
        self._stop_event: Optional[threading.Event] = None
        self._ready_event: Optional[threading.Event] = None
        self._is_running = False

        # Error tracking
        self._startup_error: Optional[Exception] = None

    def start(self) -> None:
        """
        Start the kopf operator in a background thread.

        This method is synchronous and will block until the operator is ready
        or times out. Call from the FastAPI startup hook.

        Raises:
            RuntimeError: If operator fails to start within timeout
            Exception: Any startup error from the operator thread
        """
        if self._is_running:
            logger.warning("Kopf operator is already running")
            return

        logger.info(
            "Starting kopf operator",
            namespace=self.namespace,
            timeout=self.startup_timeout,
        )  # type: ignore[call-arg]

        # Create threading coordination objects
        self._stop_event = threading.Event()
        self._ready_event = threading.Event()
        self._startup_error = None

        # Start operator thread
        self._operator_thread = threading.Thread(
            target=self._run_operator,
            name="kopf-operator",
            daemon=True,
        )
        self._operator_thread.start()

        # Wait for operator to be ready or fail
        if not self._ready_event.wait(timeout=self.startup_timeout):
            # Startup timeout - clean up and raise error
            self._stop_event.set()
            if self._operator_thread.is_alive():
                self._operator_thread.join(timeout=5.0)

            error_msg = f"Kopf operator did not start within {self.startup_timeout}s"
            if self._startup_error:
                error_msg = f"{error_msg}. Error: {self._startup_error}"

            logger.error("Kopf operator startup failed", reason="timeout")  # type: ignore[call-arg]
            raise RuntimeError(error_msg)

        # Check if startup failed with an error
        if self._startup_error:
            logger.error("Kopf operator startup failed", error=str(self._startup_error))  # type: ignore[call-arg]
            raise self._startup_error

        self._is_running = True
        logger.info(
            "Kopf operator started successfully",
            thread_id=self._operator_thread.ident,
            namespace=self.namespace,
        )  # type: ignore[call-arg]

    def stop(self) -> None:
        """
        Gracefully stop the kopf operator.

        This method is async to match the FastAPI shutdown pattern.
        Call from the FastAPI shutdown hook.
        """
        if not self._is_running:
            logger.debug("Kopf operator is not running")
            return

        logger.info("Stopping kopf operator")  # type: ignore[call-arg]

        # Signal operator to stop
        if self._stop_event:
            self._stop_event.set()

        # Wait for thread to finish
        if self._operator_thread and self._operator_thread.is_alive():
            logger.debug(
                "Waiting for kopf operator thread to finish",
                timeout=self.shutdown_timeout,
            )  # type: ignore[call-arg]

            # Join thread with timeout
            self._operator_thread.join(timeout=self.shutdown_timeout)

            if self._operator_thread.is_alive():
                logger.warning(
                    "Kopf operator thread did not stop within timeout",
                    timeout=self.shutdown_timeout,
                )  # type: ignore[call-arg]
            else:
                logger.info("Kopf operator thread stopped successfully")  # type: ignore[call-arg]

        # Clean up state
        self._is_running = False
        self._operator_thread = None
        self._stop_event = None
        self._ready_event = None
        self._startup_error = None

        logger.info("Kopf operator stopped")  # type: ignore[call-arg]

    def is_running(self) -> bool:
        """
        Check if the kopf operator is currently running.

        Returns:
            bool: True if operator is running, False otherwise
        """
        return self._is_running and (
            self._operator_thread is not None and self._operator_thread.is_alive()
        )

    def _run_operator(self) -> None:
        """
        Target function for the operator thread.

        This runs in a separate thread and handles the kopf.run() call
        with proper error handling and ready signaling.
        """
        try:
            logger.debug("Kopf operator thread started")  # type: ignore[call-arg]

            # Import handlers module to register kopf handlers before running operator
            # This ensures all @kopf.on.* decorated handlers are registered with the default registry
            # logger.debug("Importing kopf handlers for registration")  # type: ignore[call-arg]
            # try:
            #     from aibrix.metadata.cache import job  # noqa: F401

            #     logger.debug("Successfully imported kopf handlers module")  # type: ignore[call-arg]
            # except ImportError as import_error:
            #     logger.warning(
            #         "Failed to import kopf handlers module - handlers may not be available",
            #         error=str(import_error),
            #     )  # type: ignore[call-arg]

            # Run kopf operator with our coordination objects
            # Only set namespace if specified, otherwise watch cluster-wide
            logger.info(
                "Starting kopf operator with namespace restriction",
                namespace=self.namespace,
            )  # type: ignore[call-arg]
            kopf.run(
                standalone=self.standalone,
                namespace=self.namespace,
                ready_flag=self._ready_event,
                stop_flag=self._stop_event,
                # Additional kopf configuration
                peering_name=self.peering_name,  # Unique peering name
            )

            logger.debug("Kopf operator thread finished normally")  # type: ignore[call-arg]

        except Exception as e:
            logger.error(
                "Kopf operator thread failed",
                error=str(e),
                error_type=type(e).__name__,
            )  # type: ignore[call-arg]

            # Store error for main thread
            self._startup_error = e

            # Signal ready even on error so main thread doesn't hang
            if self._ready_event:
                self._ready_event.set()

    def get_status(self) -> dict:
        """
        Get detailed status information about the operator.

        Returns:
            dict: Status information including running state, thread info, etc.
        """
        status = {
            "is_running": self.is_running(),
            "namespace": self.namespace,
            "standalone": self.standalone,
            "startup_timeout": self.startup_timeout,
            "shutdown_timeout": self.shutdown_timeout,
        }

        if self._operator_thread:
            status.update(
                {
                    "thread_name": self._operator_thread.name,
                    "thread_id": self._operator_thread.ident,
                    "thread_alive": self._operator_thread.is_alive(),
                    "thread_daemon": self._operator_thread.daemon,
                }
            )

        if self._startup_error:
            status["startup_error"] = str(self._startup_error)

        return status
