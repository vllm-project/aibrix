# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from __future__ import annotations

from fnmatch import fnmatchcase
from typing import Any, Iterable

from aibrix.logger import init_logger
from aibrix.metadata.core.metrics.sink import Sink, Tag, normalize_tags

logger = init_logger(__name__)

_CONTROL_TAGS = {
    "flush",
    "log_context",
    "log_context_bool",
    "log_context_null_if_empty",
    "log_default_map",
    "log_default_null",
    "log_field",
    "log_group",
    "log_inner",
    "log_index",
    "log_key",
    "log_list",
    "log_map",
    "log_nested_map",
    "log_outer",
}


class LogSink(Sink):
    def __init__(
        self,
        *,
        include_patterns: Iterable[str] = (),
        exclude_patterns: Iterable[str] = (),
    ) -> None:
        self._include_patterns = tuple(
            pattern for pattern in include_patterns if pattern
        )
        self._exclude_patterns = tuple(
            pattern for pattern in exclude_patterns if pattern
        )
        self._pending: dict[
            tuple[str, tuple[tuple[str, str | bool | None], ...]],
            dict[str, Any],
        ] = {}

    def counter(self, name: str, value: float, *tags: Tag) -> None:
        self._log_metric("counter", name, value, *tags)

    def gauge(self, name: str, value: float, *tags: Tag) -> None:
        self._log_metric("gauge", name, value, *tags)

    def timer(self, name: str, value: float, *tags: Tag) -> None:
        self._log_metric("timer", name, value, *tags)

    def store(self, name: str, value: float, *tags: Tag) -> None:
        self._log_metric("store", name, value, *tags)

    def rate(self, name: str, value: float, *tags: Tag) -> None:
        self._log_metric("rate", name, value, *tags)

    def close(self) -> None:
        return None

    def _log_metric(
        self, metric_type: str, name: str, value: float, *tags: Tag
    ) -> None:
        if not self._should_log(name):
            return
        tags_dict = self._tag_dict(tags)
        if tags_dict.get("flush") == "true" and "event" in tags_dict:
            if self._has_aggregate_field(tags_dict):
                self._record_pending_metric(metric_type, name, value, tags_dict)
            event = tags_dict["event"]
            payload = self._pop_pending_payload(event, name, tags_dict)
            logger.info(event, **payload)  # type: ignore[call-arg]
            return
        if "event" in tags_dict:
            self._record_pending_metric(metric_type, name, value, tags_dict)
            return
        self._emit_generic_metric(metric_type, name, value, tags_dict)

    def _has_aggregate_field(self, tags: dict[str, str]) -> bool:
        return (
            "log_field" in tags
            or ("log_map" in tags and "log_key" in tags)
            or ("log_list" in tags and "log_index" in tags)
            or (
                "log_nested_map" in tags and "log_outer" in tags and "log_inner" in tags
            )
        )

    def _emit_generic_metric(
        self,
        metric_type: str,
        name: str,
        value: float,
        tags_dict: dict[str, str],
    ) -> None:
        logger.info(
            "Metric sink sample",
            metric_type=metric_type,
            metric_name=name,
            metric_value=value,
            metric_tags=tags_dict,
        )  # type: ignore[call-arg]

    def _record_pending_metric(
        self,
        metric_type: str,
        name: str,
        value: float,
        tags: dict[str, str],
    ) -> None:
        _ = metric_type
        event = tags["event"]
        payload = self._pending.setdefault(
            self._group_key(event, tags),
            self._base_payload(tags),
        )
        if "log_nested_map" in tags and "log_outer" in tags and "log_inner" in tags:
            payload.setdefault(tags["log_nested_map"], {}).setdefault(
                tags["log_outer"],
                {},
            )[tags["log_inner"]] = value
        elif "log_list" in tags and "log_index" in tags:
            log_list = payload.setdefault(tags["log_list"], [])
            if not isinstance(log_list, list):
                log_list = []
                payload[tags["log_list"]] = log_list
            try:
                index = int(tags["log_index"])
            except ValueError:
                payload[name] = value
                return
            while len(log_list) <= index:
                log_list.append(0)
            log_list[index] = value
        elif "log_map" in tags and "log_key" in tags:
            payload.setdefault(tags["log_map"], {})[tags["log_key"]] = value
        elif "log_field" in tags:
            payload[tags["log_field"]] = value
        else:
            payload[name] = value

    def _pop_pending_payload(
        self,
        event: str,
        name: str,
        tags: dict[str, str],
    ) -> dict[str, Any]:
        payload = self._pending.pop(self._group_key(event, tags), None)
        if payload is None:
            payload = self._base_payload(tags)
        payload = dict(payload)
        payload.update(self._base_payload(tags))
        self._apply_flush_defaults(payload, tags)
        payload["metric_name"] = name
        return payload

    def _base_payload(self, tags: dict[str, str]) -> dict[str, Any]:
        if "log_context" in tags:
            context_tags = {
                tag_name.strip()
                for tag_name in tags["log_context"].split(",")
                if tag_name.strip()
            }
            return {
                key: self._context_value(key, value, tags)
                for key, value in tags.items()
                if key in context_tags and key != "event" and key not in _CONTROL_TAGS
            }
        return {
            key: value
            for key, value in tags.items()
            if key != "event" and key not in _CONTROL_TAGS
        }

    def _group_key(
        self,
        event: str,
        tags: dict[str, str],
    ) -> tuple[str, tuple[tuple[str, str | bool | None], ...]]:
        if "log_group" in tags:
            return (event, (("log_group", tags["log_group"]),))
        if "log_context" in tags:
            payload = self._base_payload(tags)
            return (event, tuple(sorted(payload.items())))
        return (
            event,
            tuple(
                sorted(
                    (key, value)
                    for key, value in tags.items()
                    if key != "event" and key not in _CONTROL_TAGS
                )
            ),
        )

    def _context_value(
        self,
        key: str,
        value: str,
        tags: dict[str, str],
    ) -> str | bool | None:
        if key in self._control_tag_set(tags, "log_context_null_if_empty"):
            if value == "":
                return None
        if key in self._control_tag_set(tags, "log_context_bool"):
            if value.lower() == "true":
                return True
            if value.lower() == "false":
                return False
        return value

    def _control_tag_set(self, tags: dict[str, str], name: str) -> set[str]:
        return {
            tag_name.strip()
            for tag_name in tags.get(name, "").split(",")
            if tag_name.strip()
        }

    def _apply_flush_defaults(
        self,
        payload: dict[str, Any],
        tags: dict[str, str],
    ) -> None:
        for field in self._control_tag_set(tags, "log_default_null"):
            payload.setdefault(field, None)
        for field in self._control_tag_set(tags, "log_default_map"):
            payload.setdefault(field, {})

    def _should_log(self, name: str) -> bool:
        if not self._include_patterns:
            return False
        if any(fnmatchcase(name, pattern) for pattern in self._exclude_patterns):
            return False
        return any(fnmatchcase(name, pattern) for pattern in self._include_patterns)

    def _tag_dict(self, tags: Iterable[Tag]) -> dict[str, str]:
        return {tag.name: tag.value for tag in normalize_tags(tags)}
