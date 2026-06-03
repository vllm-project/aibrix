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
"""Runtime: the single axis of the job lifecycle — *where compute lives*.

A ``Runtime`` provisions compute into a reachable ``Endpoint`` and tears it
down. The job driver owns the phase template (validate / prepare / run_job /
finalize); the runtime only owns provision/wait-ready/connect/teardown. A new
backend (k8s-deployment, k8s-job-sidecar, Lambda, RunPod, OpenAI, local) is a
new ``Runtime`` — never a new driver.

The four sub-phases are explicit (rather than hidden in one ``session`` body) so
that an SSH-launch backend can express "launch vLLM" in ``provision`` and "poll
/health" in ``wait_ready`` as first-class steps instead of an opaque blob.
``RuntimeBase`` implements ``session`` as the four-phase bracket with guaranteed
teardown; subclasses override only the sub-phases they need and inherit a NOOP
for the rest.

NOTE (resource-health watch, deferred): a runtime does NOT continuously monitor
the compute it provisioned. ``wait_ready`` is startup-readiness only. If the
backend dies mid-job, failure surfaces through inference errors (the dispatch
engine) or the job's completion window. A cross-provider ``monitor()`` seam
(k8s watch / EC2 describe / lambda poll) is a documented future extension, not
built here.
"""

from __future__ import annotations

from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import (
    Any,
    AsyncIterator,
    Callable,
    Dict,
    List,
    Optional,
    Protocol,
    runtime_checkable,
)

from aibrix.batch.client import EndpointSource
from aibrix.batch.job_entity import BatchJob


@dataclass(slots=True)
class Completion:
    """Terminal outcome of a self-hosting runtime (Endpoint(source=None) +
    provisions): the worker ran inference itself and the control plane only
    waited. Returned by ``await_completion`` and consumed by the base driver's
    run_job phase."""

    succeeded: bool
    reason: str = ""
    message: str = ""


@dataclass(slots=True)
class Endpoint:
    """What a provisioned runtime hands back to the driver's run_job phase.

    ``source`` is the reachable endpoint set the dispatch engine sends to.
    ``source is None`` means the control plane has no endpoint — the worker
    self-hosts inference (the sidecar case), so the driver sends nothing and
    instead waits for the worker to finish.

    ``model_name`` lets a single-model backend pin requests to the model it
    actually serves (the deployment case), so an input's ``model`` field can't
    misroute.
    """

    source: Optional[EndpointSource]
    model_name: Optional[str] = None


@runtime_checkable
class Runtime(Protocol):
    """Axis A of the job lifecycle. Implementations are registered by key."""

    #: Whether this runtime stands up compute (False for local/openai/sidecar).
    provisions: bool

    def cancelled(self) -> bool:
        """True if teardown was triggered by job deletion (so the driver
        swallows the resulting CancelledError instead of failing the job)."""
        ...

    def session(self, job: BatchJob, job_id: str) -> "AsyncRuntimeSession":
        """Async context manager: provision -> yield Endpoint -> teardown."""
        ...

    async def on_prepared(self) -> None:
        """Driver hook fired after output files are prepared, before run_job; a
        self-hosting runtime starts its provisioned worker here. NOOP for most.
        (See RuntimeBase for the default.)"""
        ...

    async def await_completion(self) -> Completion:
        """Block until the provisioned worker finishes, for the self-hosting
        shape (``Endpoint(source=None)`` + ``provisions``). The base driver
        calls this instead of dispatching; non-self-hosting runtimes need not
        implement it."""
        ...


class AsyncRuntimeSession(Protocol):
    async def __aenter__(self) -> Endpoint: ...

    async def __aexit__(self, *exc: Any) -> Optional[bool]: ...


class RuntimeBase:
    """Default runtime: implements ``session`` as the four-phase bracket.

    The public methods here are exactly the ``Runtime`` contract (``provisions``
    / ``cancelled`` / ``session`` / ``on_prepared`` / ``await_completion``); the
    ``_``-prefixed ``_provision`` / ``_wait_ready`` / ``_connect`` / ``_teardown``
    are internal template hooks that only ``session`` calls. A subclass overrides
    the hooks it needs (single underscore keeps them overridable) and inherits a
    NOOP for the rest. The default is a no-op runtime that yields no endpoint
    (the sidecar shape).
    """

    provisions: bool = False

    def cancelled(self) -> bool:
        return False

    async def _provision(self, job: BatchJob, job_id: str) -> Any:
        """Create/lease/launch compute. Returns an opaque handle. NOOP default."""
        return None

    async def _wait_ready(self, handle: Any) -> None:
        """Poll until the compute is serving. NOOP default."""
        return None

    async def _connect(self, handle: Any) -> Endpoint:
        """Build the reachable endpoint. Default: no endpoint (worker self-hosts)."""
        return Endpoint(source=None)

    async def _teardown(self, handle: Any) -> None:
        """Release the compute. NOOP default."""
        return None

    async def on_prepared(self) -> None:
        """Driver hook fired right after output files are prepared, before
        run_job. A self-hosting runtime starts/releases its provisioned worker
        here (e.g. un-suspend a k8s Job, once the files it writes to exist).
        NOOP default — most runtimes are already serving by ``connect``."""
        return None

    async def await_completion(self) -> Completion:
        """Block until the provisioned worker finishes, for the self-hosting
        shape (``connect`` returned ``Endpoint(source=None)`` and
        ``provisions`` is True). The base driver calls this instead of
        dispatching requests. Only self-hosting runtimes implement it."""
        raise NotImplementedError(
            "await_completion is only valid for self-hosting runtimes "
            "(Endpoint(source=None) + provisions=True)"
        )

    @asynccontextmanager
    async def session(self, job: BatchJob, job_id: str) -> AsyncIterator[Endpoint]:
        handle = await self._provision(job, job_id)
        try:
            await self._wait_ready(handle)
            yield await self._connect(handle)
        finally:
            await self._teardown(handle)


class ExternalRuntime(RuntimeBase):
    """No provisioning: the endpoint already exists (an injected source).

    Used by the standalone/in-process path and by OpenAI-style direct-API
    backends — anything where the engine endpoint is known up front.
    """

    provisions = False

    def __init__(self, source: Optional[EndpointSource]) -> None:
        self._source = source

    async def _connect(self, handle: Any) -> Endpoint:
        return Endpoint(source=self._source)


class NoopRuntime(RuntimeBase):
    """No control-plane endpoint at all: the worker provisions and runs
    inference itself (the colocated sidecar / distributed case). ``connect``
    inherits the base NOOP that yields ``Endpoint(source=None)``."""

    provisions = False


# --- Registry (downstream extension point) -------------------------------
#
# Each runtime backend registers under a string key. Upstream registers the
# OSS backends; downstream registers its own (e.g. "k8s-deployment-cr") in its
# own module — no edit to an upstream if/elif or enum, so rebasing upstream
# does not conflict. Selection is by configured string -> registry lookup.

_RUNTIME_FACTORIES: Dict[str, Callable[..., Runtime]] = {}


def register_runtime(key: str, factory: Callable[..., Runtime]) -> None:
    """Register a runtime factory under ``key``. Idempotent overwrite allowed
    so a downstream module can shadow an upstream default if it must."""
    _RUNTIME_FACTORIES[key] = factory


def create_runtime(key: str, *args: Any, **kwargs: Any) -> Runtime:
    """Build a runtime by key. Raises KeyError with the known keys on miss."""
    try:
        factory = _RUNTIME_FACTORIES[key]
    except KeyError:
        raise KeyError(
            f"unknown runtime '{key}'; registered: {registered_runtimes()}"
        ) from None
    return factory(*args, **kwargs)


def registered_runtimes() -> List[str]:
    return sorted(_RUNTIME_FACTORIES)


# OSS built-in runtimes that need no extra dependencies. Provisioning backends
# (Kubernetes / KubernetesJob / LambdaCloud / RunPod) register from their own
# modules, which pull in kubernetes / cloud SDKs. Keys match RuntimeTarget.
# Factories take a uniform keyword bag and pick what they need.
register_runtime(
    "External", lambda *, endpoint_source=None, **_: ExternalRuntime(endpoint_source)
)
register_runtime("noop", lambda **_: NoopRuntime())
