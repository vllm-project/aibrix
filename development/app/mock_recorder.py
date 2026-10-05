"""Thread-safe, bounded request recorder for the mock OpenAI server."""

import base64
from collections import deque
from datetime import datetime, timezone
import math
import threading
import weakref


OUTCOMES = frozenset(("success", "rejected", "failed"))


class _RecordHandle:
    """Opaque identity used to complete exactly one recorder entry."""

    __slots__ = ("_sequence", "__weakref__")

    def __init__(self, sequence):
        self._sequence = sequence

    @property
    def sequence(self):
        return self._sequence


def _timestamp():
    return datetime.now(timezone.utc).isoformat()


def _json_safe(value, active_ids=None):
    """Convert a value to the JSON types exposed by the debug endpoint."""
    if value is None or isinstance(value, (bool, int, str)):
        return value
    if isinstance(value, float):
        return value if math.isfinite(value) else None
    if isinstance(value, bytes):
        return base64.b64encode(value).decode("ascii")

    if active_ids is None:
        active_ids = set()
    value_id = id(value)
    if value_id in active_ids:
        return "<recursive value>"

    active_ids.add(value_id)
    try:
        if isinstance(value, dict):
            return {
                str(key): _json_safe(item, active_ids)
                for key, item in value.items()
            }
        if isinstance(value, (list, tuple, set, frozenset)):
            return [_json_safe(item, active_ids) for item in value]
        return str(value)
    finally:
        active_ids.remove(value_id)


class RequestRecorder:
    """Store a bounded, JSON-serializable history of mock HTTP requests."""

    def __init__(self, capacity=1000):
        if not isinstance(capacity, int) or isinstance(capacity, bool) or capacity <= 0:
            raise ValueError("capacity must be a positive integer")
        self._records = deque(maxlen=capacity)
        self._lock = threading.Lock()
        self._next_sequence = 1
        self._handles = weakref.WeakKeyDictionary()

    def start(
        self,
        *,
        path,
        headers,
        raw_body,
        parsed_json,
        pod,
        engine,
        role,
        request_id=None,
    ):
        """Create and retain a request record, returning its completion handle."""
        if not isinstance(raw_body, bytes):
            raise TypeError("raw_body must be bytes")
        headers = _json_safe(dict(headers))
        if request_id is None:
            request_id = next(
                (value for key, value in headers.items() if key.lower() == "x-request-id"),
                None,
            )

        with self._lock:
            sequence = self._next_sequence
            self._next_sequence += 1
            handle = _RecordHandle(sequence)
            self._handles[handle] = sequence
            self._records.append(
                {
                    "sequence": sequence,
                    "timestamp_started": _timestamp(),
                    "timestamp_finished": None,
                    "request_id": request_id,
                    "path": path,
                    "headers": headers,
                    "raw_body_base64": base64.b64encode(raw_body).decode("ascii"),
                    "parsed_json": _json_safe(parsed_json),
                    "pod": pod,
                    "engine": engine,
                    "role": role,
                    "outcome": None,
                    "status_code": None,
                    "error": None,
                    "response": None,
                    "delay_ms": None,
                }
            )
        return handle

    def finish(
        self,
        handle,
        *,
        outcome,
        status_code,
        response=None,
        error=None,
        delay_ms=None,
    ):
        """Complete a previously started record."""
        if outcome not in OUTCOMES:
            raise ValueError("outcome must be one of: success, rejected, failed")
        if not isinstance(handle, _RecordHandle):
            raise ValueError("record handle belongs to another recorder")

        with self._lock:
            sequence = self._handles.get(handle)
            if sequence is None:
                raise ValueError("record handle belongs to another recorder")
            for record in self._records:
                if record["sequence"] == sequence:
                    if record["outcome"] is not None:
                        raise RuntimeError("record is already finished")
                    record.update(
                        {
                            "timestamp_finished": _timestamp(),
                            "outcome": outcome,
                            "status_code": status_code,
                            "error": _json_safe(error),
                            "response": _json_safe(response),
                            "delay_ms": delay_ms,
                        }
                    )
                    return
        raise KeyError(f"record {sequence} is no longer available")

    def query(self, *, request_id=None):
        """Return independent, sequence-ordered copies of matching records."""
        with self._lock:
            records = [
                _json_safe(record)
                for record in self._records
                if request_id is None or record["request_id"] == request_id
            ]
        return records
