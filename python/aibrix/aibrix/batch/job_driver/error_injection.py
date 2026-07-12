from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable, Mapping, Optional

import aibrix.batch.constant as constant
from aibrix.batch.job_entity import BatchJob, BatchJobError, BatchJobErrorCode
from aibrix.logger import init_logger

logger = init_logger(__name__)

BREAKPOINT_RUNTIME_INITIALIZATION = "runtime_initialization"
BREAKPOINT_PREPARATION = "preparation"
BREAKPOINT_AFTER_N_REQUESTS = "after_n_requests"

ACTION_RAISE = "raise"
ACTION_INTERRUPT_RUNTIME = "interrupt_runtime"


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
        raise_helpers: Optional[
            Mapping[str, Callable[[JobDriverErrorInjectionEvent], BaseException]]
        ] = None,
    ) -> None:
        self._job_id = job.job_id if job is not None else None
        opts = dict(job.spec.opts or {}) if job is not None and job.spec.opts else {}
        self._fail_init_runtime = self._parse_enabled(
            opts, constant.BATCH_OPTS_FAIL_INIT_RUNTIME
        )
        self._fail_preparation = self._parse_enabled(
            opts, constant.BATCH_OPTS_FAIL_PREPARATION
        )
        self._fail_after_n_requests = self._parse_positive_int(
            opts, constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS
        )
        self._interrupt_runtime_after_n_requests = self._parse_positive_int(
            opts, constant.BATCH_OPTS_INTERRUPT_RUNTIME_AFTER_N_REQUESTS
        )
        self._one_shot_triggers: set[str] = set()
        self._raise_helpers = dict(raise_helpers or {})

    @property
    def job_id(self) -> Optional[str]:
        return self._job_id

    def threshold_for(self, opt_key: str) -> Optional[int]:
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
        if breakpoint == BREAKPOINT_PREPARATION:
            return self._flag_events(
                enabled=self._fail_preparation,
                opt_key=constant.BATCH_OPTS_FAIL_PREPARATION,
                breakpoint=breakpoint,
            )
        if breakpoint == BREAKPOINT_AFTER_N_REQUESTS:
            current_n = self._coerce_positive_int(kwargs.get("n"))
            if current_n is None:
                return ()

            events: list[JobDriverErrorInjectionEvent] = []
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

    def raise_for_breakpoint(
        self,
        breakpoint: str,
        **kwargs: Any,
    ) -> None:
        for event in self.events_at(breakpoint, **kwargs):
            self._log_event(event)
            if event.action in self._raise_helpers:
                raise self._raise_helpers[event.action](event)
            if event.action != ACTION_RAISE:
                raise RuntimeError(
                    f"No raise helper registered for action {event.action} "
                    f"at breakpoint {breakpoint}"
                )
            raise event.to_exception()

    def _flag_events(
        self,
        *,
        enabled: bool,
        opt_key: str,
        breakpoint: str,
    ) -> tuple[JobDriverErrorInjectionEvent, ...]:
        if not enabled:
            return ()
        return (
            JobDriverErrorInjectionEvent(
                opt_key=opt_key,
                breakpoint=breakpoint,
                action=ACTION_RAISE,
            ),
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
