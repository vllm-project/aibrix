from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Callable, Mapping, Optional

import aibrix.batch.constant as constant
from aibrix import envs
from aibrix.batch.job_driver.running_jobs import RunningJobs
from aibrix.batch.job_entity import BatchJob, BatchJobError, BatchJobErrorCode
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

logger = init_logger(__name__)

BREAKPOINT_RUNTIME_INITIALIZATION = "runtime_initialization"
BREAKPOINT_VALIDATION = "validation"
BREAKPOINT_PREPARATION = "preparation"
BREAKPOINT_AFTER_N_REQUESTS = "after_n_requests"
BREAKPOINT_BEFORE_FINALIZE_SHUTDOWN = "before_finalize_shutdown"
BREAKPOINT_AFTER_FINALIZE_SHUTDOWN = "after_finalize_shutdown"

ACTION_RAISE = "raise"
ACTION_INTERRUPT_RUNTIME = "interrupt_runtime"
ACTION_CRASH_DRIVER_LOOP = "crash_driver_loop"
_CONSUMED_CRASH_OPT_KEYS = frozenset(
    {
        constant.BATCH_OPTS_CRASH_DURING_VALIDATION,
        constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS,
        constant.BATCH_OPTS_CRASH_BEFORE_FINALIZE_SHUTDOWN,
        constant.BATCH_OPTS_CRASH_AFTER_FINALIZE_SHUTDOWN,
    }
)


@dataclass(frozen=True)
class JobDriverErrorInjectionEvent:
    opt_key: str
    breakpoint: str
    action: str
    configured_n: Optional[int] = None
    current_n: Optional[int] = None

    def to_exception(self) -> Exception:
        if self.opt_key == constant.BATCH_OPTS_FAIL_INIT_RUNTIME:
            return BatchJobError(
                code=BatchJobErrorCode.RESOURCE_CREATION_ERROR,
                message=(
                    "Artificial runtime initialization failure triggered "
                    f"({constant.BATCH_OPTS_FAIL_INIT_RUNTIME})"
                ),
            )
        if self.opt_key == constant.BATCH_OPTS_FAIL_PREPARATION:
            return BatchJobError(
                code=BatchJobErrorCode.PREPARE_OUTPUT_ERROR,
                message=(
                    "Artificial preparation failure triggered "
                    f"({constant.BATCH_OPTS_FAIL_PREPARATION})"
                ),
            )
        if self.opt_key == constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS:
            return RuntimeError(
                "Artificial failure triggered after processing "
                f"{self.current_n} requests "
                f"({constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS}={self.configured_n})"
            )
        raise RuntimeError(
            f"Unsupported error injection event for {self.opt_key} at {self.breakpoint}"
        )


class JobDriverErrorInjectionRaisedAction(RuntimeError):
    def __init__(self, event: JobDriverErrorInjectionEvent) -> None:
        self.event = event
        super().__init__(
            f"Injected action {event.action} triggered for {event.opt_key} "
            f"at {event.breakpoint}"
        )


class JobDriverErrorInjector:
    def __init__(
        self,
        job: Optional[BatchJob],
        *,
        progress_manager: Optional[RunningJobs] = None,
        raise_helpers: Optional[
            Mapping[str, Callable[[JobDriverErrorInjectionEvent], BaseException]]
        ] = None,
    ) -> None:
        self._job_id = job.job_id if job is not None else None
        self._progress_manager = progress_manager
        opts = dict(job.spec.opts or {}) if job is not None and job.spec.opts else {}
        if not envs.BATCH_ERROR_INJECTION_ENABLED:
            opts = {}
        crash_already_consumed = bool(
            job is not None and job.status.last_crashed_at is not None
        )
        self._fail_init_runtime = self._parse_enabled(
            opts, constant.BATCH_OPTS_FAIL_INIT_RUNTIME
        )
        self._crash_during_validation = (
            not crash_already_consumed
        ) and self._parse_enabled(opts, constant.BATCH_OPTS_CRASH_DURING_VALIDATION)
        self._fail_preparation = self._parse_enabled(
            opts, constant.BATCH_OPTS_FAIL_PREPARATION
        )
        crash_after_n_requests = self._parse_positive_int(
            opts, constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS
        )
        self._crash_after_n_requests = (
            None if crash_already_consumed else crash_after_n_requests
        )
        self._fail_after_n_requests = self._parse_positive_int(
            opts, constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS
        )
        self._interrupt_runtime_after_n_requests = self._parse_positive_int(
            opts, constant.BATCH_OPTS_INTERRUPT_RUNTIME_AFTER_N_REQUESTS
        )
        self._crash_before_finalize_shutdown = (
            not crash_already_consumed
        ) and self._parse_enabled(
            opts, constant.BATCH_OPTS_CRASH_BEFORE_FINALIZE_SHUTDOWN
        )
        self._crash_after_finalize_shutdown = (
            not crash_already_consumed
        ) and self._parse_enabled(
            opts, constant.BATCH_OPTS_CRASH_AFTER_FINALIZE_SHUTDOWN
        )
        self._consumed_opt_keys: set[str] = set()
        if self._crash_during_validation:
            self._consumed_opt_keys.add(constant.BATCH_OPTS_CRASH_DURING_VALIDATION)
        if self._crash_after_n_requests is not None:
            self._consumed_opt_keys.add(constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS)
        if self._crash_before_finalize_shutdown:
            self._consumed_opt_keys.add(
                constant.BATCH_OPTS_CRASH_BEFORE_FINALIZE_SHUTDOWN
            )
        if self._crash_after_finalize_shutdown:
            self._consumed_opt_keys.add(
                constant.BATCH_OPTS_CRASH_AFTER_FINALIZE_SHUTDOWN
            )
        self._one_shot_triggers: set[str] = set()
        self._skip_session_cleanup_teardown = False
        self._raise_helpers = dict(raise_helpers or {})

    @property
    def job_id(self) -> Optional[str]:
        return self._job_id

    def should_skip_session_cleanup_teardown(
        self,
        error: Optional[BaseException],
    ) -> bool:
        """Only session cleanup consults this flag.

        Crash injections outside a runtime session may also set the flag, but it
        has no effect unless session-finalization teardown reaches this check.
        """
        del error
        return self._skip_session_cleanup_teardown

    def threshold_for(self, opt_key: str) -> Optional[int]:
        if opt_key == constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS:
            return self._crash_after_n_requests
        if opt_key == constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS:
            return self._fail_after_n_requests
        if opt_key == constant.BATCH_OPTS_INTERRUPT_RUNTIME_AFTER_N_REQUESTS:
            return self._interrupt_runtime_after_n_requests
        return None

    def events_at(
        self,
        breakpoint: str,
        **kwargs: Any,
    ) -> tuple[JobDriverErrorInjectionEvent, ...]:
        if breakpoint == BREAKPOINT_RUNTIME_INITIALIZATION:
            return self._flag_events(
                enabled=self._fail_init_runtime,
                opt_key=constant.BATCH_OPTS_FAIL_INIT_RUNTIME,
                breakpoint=breakpoint,
            )
        if breakpoint == BREAKPOINT_VALIDATION:
            return self._one_shot_flag_events(
                enabled=self._crash_during_validation,
                opt_key=constant.BATCH_OPTS_CRASH_DURING_VALIDATION,
                breakpoint=breakpoint,
                action=ACTION_CRASH_DRIVER_LOOP,
            )
        if breakpoint == BREAKPOINT_PREPARATION:
            return self._flag_events(
                enabled=self._fail_preparation,
                opt_key=constant.BATCH_OPTS_FAIL_PREPARATION,
                breakpoint=breakpoint,
            )
        if breakpoint == BREAKPOINT_BEFORE_FINALIZE_SHUTDOWN:
            return self._one_shot_flag_events(
                enabled=self._crash_before_finalize_shutdown,
                opt_key=constant.BATCH_OPTS_CRASH_BEFORE_FINALIZE_SHUTDOWN,
                breakpoint=breakpoint,
                action=ACTION_CRASH_DRIVER_LOOP,
            )
        if breakpoint == BREAKPOINT_AFTER_FINALIZE_SHUTDOWN:
            return self._one_shot_flag_events(
                enabled=self._crash_after_finalize_shutdown,
                opt_key=constant.BATCH_OPTS_CRASH_AFTER_FINALIZE_SHUTDOWN,
                breakpoint=breakpoint,
                action=ACTION_CRASH_DRIVER_LOOP,
            )
        if breakpoint == BREAKPOINT_AFTER_N_REQUESTS:
            current_n = self._coerce_positive_int(kwargs.get("n"))
            if current_n is None:
                return ()

            events: list[JobDriverErrorInjectionEvent] = []
            if (
                self._crash_after_n_requests is not None
                and current_n >= self._crash_after_n_requests
                and self._mark_one_shot(constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS)
            ):
                events.append(
                    JobDriverErrorInjectionEvent(
                        opt_key=constant.BATCH_OPTS_CRASH_AFTER_N_REQUESTS,
                        breakpoint=breakpoint,
                        action=ACTION_CRASH_DRIVER_LOOP,
                        configured_n=self._crash_after_n_requests,
                        current_n=current_n,
                    )
                )
            if (
                self._interrupt_runtime_after_n_requests is not None
                and current_n == self._interrupt_runtime_after_n_requests
                and self._mark_one_shot(
                    constant.BATCH_OPTS_INTERRUPT_RUNTIME_AFTER_N_REQUESTS
                )
            ):
                events.append(
                    JobDriverErrorInjectionEvent(
                        opt_key=constant.BATCH_OPTS_INTERRUPT_RUNTIME_AFTER_N_REQUESTS,
                        breakpoint=breakpoint,
                        action=ACTION_INTERRUPT_RUNTIME,
                        configured_n=self._interrupt_runtime_after_n_requests,
                        current_n=current_n,
                    )
                )
            if (
                self._fail_after_n_requests is not None
                and current_n >= self._fail_after_n_requests
                and self._mark_one_shot(constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS)
            ):
                events.append(
                    JobDriverErrorInjectionEvent(
                        opt_key=constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS,
                        breakpoint=breakpoint,
                        action=ACTION_RAISE,
                        configured_n=self._fail_after_n_requests,
                        current_n=current_n,
                    )
                )
            return tuple(events)
        return ()

    async def raise_for_breakpoint(
        self,
        context: InfrastructureContext,
        job: BatchJob,
        breakpoint: str,
        **kwargs: Any,
    ) -> None:
        if self._job_id != job.job_id:
            raise ValueError(
                f"Error injector is not for job {job.job_id}, but job {self._job_id}"
            )

        for event in self.events_at(breakpoint, **kwargs):
            self._log_event(event)
            if event.action in self._raise_helpers:
                raise self._raise_helpers[event.action](event)
            if event.action == ACTION_CRASH_DRIVER_LOOP:
                self._skip_session_cleanup_teardown = True
                await self.do_crash(context, job)
                raise asyncio.CancelledError(
                    "Injected finalize crash for restart recovery test"
                )
            if event.action != ACTION_RAISE:
                raise RuntimeError(
                    f"No raise helper registered for action {event.action} "
                    f"at breakpoint {breakpoint}"
                )
            raise event.to_exception()

    async def do_crash(self, context: InfrastructureContext, job: BatchJob) -> None:
        await self._consume_job_crash(job)

        driver = context.values.get("batch_driver")
        async_thread_loop = getattr(driver, "_async_thread_loop", None)
        loop = getattr(async_thread_loop, "loop", None)
        if loop is None:
            return
        running_loop = asyncio.get_running_loop()
        if loop is running_loop:
            loop.set_exception_handler(lambda *_: None)
            loop.call_soon(loop.stop)
        else:
            loop.call_soon_threadsafe(loop.set_exception_handler, lambda *_: None)
            loop.call_soon_threadsafe(loop.stop)

    async def _consume_job_crash(self, job: BatchJob) -> None:
        if self._progress_manager is None:
            raise ValueError("progress_manager is not set")

        job.status.last_crashed_at = datetime.now(timezone.utc)
        await self._progress_manager.update_job_status(job.job_id, job.status)

    def _flag_events(
        self,
        *,
        enabled: bool,
        opt_key: str,
        breakpoint: str,
        action: str = ACTION_RAISE,
    ) -> tuple[JobDriverErrorInjectionEvent, ...]:
        if not enabled:
            return ()
        return (
            JobDriverErrorInjectionEvent(
                opt_key=opt_key,
                breakpoint=breakpoint,
                action=action,
            ),
        )

    def _one_shot_flag_events(
        self,
        *,
        enabled: bool,
        opt_key: str,
        breakpoint: str,
        action: str,
    ) -> tuple[JobDriverErrorInjectionEvent, ...]:
        if not enabled or not self._mark_one_shot(opt_key):
            return ()
        return self._flag_events(
            enabled=True,
            opt_key=opt_key,
            breakpoint=breakpoint,
            action=action,
        )

    def _log_event(self, event: JobDriverErrorInjectionEvent) -> None:
        log_data: dict[str, Any] = {
            "job_id": self._job_id,
            "opt_key": event.opt_key,
            "breakpoint": event.breakpoint,
            "action": event.action,
        }
        if event.current_n is not None:
            log_data["current_n"] = event.current_n
        if event.configured_n is not None:
            log_data["configured_n"] = event.configured_n
        logger.info("Triggering error injection", **log_data)  # type: ignore[call-arg]

    @staticmethod
    def _parse_enabled(opts: dict[str, Any], opt_key: str) -> bool:
        if opt_key not in opts:
            return False
        value = opts[opt_key]
        normalized = str(value).strip().lower()
        return normalized not in {"", "0", "false", "no", "off"}

    def _parse_positive_int(self, opts: dict[str, Any], opt_key: str) -> Optional[int]:
        if opt_key not in opts:
            return None
        parsed = self._coerce_positive_int(opts[opt_key])
        if parsed is None:
            logger.warning(
                "Invalid error injection value, ignoring",
                job_id=self._job_id,
                opt_key=opt_key,
                value=opts[opt_key],
            )  # type: ignore[call-arg]
        return parsed

    @staticmethod
    def _coerce_positive_int(value: Any) -> Optional[int]:
        try:
            parsed = int(value)
        except (TypeError, ValueError):
            return None
        return parsed if parsed > 0 else None

    def _mark_one_shot(self, opt_key: str) -> bool:
        if opt_key in self._one_shot_triggers:
            return False
        self._one_shot_triggers.add(opt_key)
        return True
